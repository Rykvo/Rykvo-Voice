const NetworkSettings = (() => {
  let controller = null, timer = null, snapshot = null, loading = false, saving = false, needsSync = false, generation = 0;
  const markupCache = new WeakMap();
  const states = { configured: "已连接", unavailable: "不可用", unconfigured: "待配置", "no-carrier": "未连接", "address-pending": "获取地址", "gateway-missing": "缺少网关", "dns-missing": "缺少 DNS", "strict-rpf": "路由限制", ambiguous: "设备冲突" };
  const errors = {
    INVALID_VLESS: "请检查 VLESS 链接",
    VLESS_OPTIONS_UNSUPPORTED: "仅支持 VLESS TCP / TCP+TLS",
    VPN_TEST_BUSY: "正在检测，请稍后",
    VPN_TEST_FAILED: "节点连接失败，请检查节点和网络",
    VPN_CORE_FAILED: "代理服务启动失败",
    PROBE_ROUTE_BUSY: "检测路由忙，请稍后",
    PROBE_CLEANUP_FAILED: "检测路由清理异常，请联系维护",
    TRAFFIC_CONTROL_NOT_READY: "网络服务尚未就绪",
    NETWORK_ROUTE_CONFLICT: "路由配置冲突，请检查现有网络配置",
    NETWORK_APPLY_FAILED: "切换未完成，请检查网络",
    NETWORK_DNS_UNAVAILABLE: "请先启用系统 DNS 服务 systemd-resolved",
    NETWORK_CONTROL_UNAVAILABLE: "网络服务暂不可用",
    VPN_DISABLE_FIRST: "请先关闭 VPN 再修改节点",
    DEVICE_BUSY: "模块使用中，请稍后修改",
    NETWORK_ASSIGNED: "请先在原网络关闭分配",
    NETWORK_CONFLICT: "配置已更新，请重新打开",
    NETWORK_UNAVAILABLE: "指定网络不可用",
    INVALID_NETWORK: "请检查网络和模块选择",
    NETWORK_INVENTORY_UNAVAILABLE: "网络读取失败",
  };
  function render() {
    return '<section class="form-page network-page">' + Forms.header("网络配置", "ethernet.svg") +
      '<p id="network-error" role="status"></p><div id="network-list" aria-live="polite">正在读取…</div></section>';
  }
  function valid(data) {
    return Number.isSafeInteger(data?.revision) && data.revision > 0 && Array.isArray(data.networks) && Array.isArray(data.modules) &&
      data.networks.every(n => /^[a-f0-9]{32}$/.test(n.id) && typeof n.label === "string" && Object.hasOwn(states, n.state)) &&
      data.modules.every(m => typeof m.id === "string" && typeof m.label === "string" && typeof m.network === "string" && typeof m.busy === "boolean");
  }
  function hostDefault(network) {
    return (network.hostDefault || []).some(f => f === "IPv4" || f === "IPv6");
  }
  function visibleNetwork(n, modules = []) {
    return n.state === "configured" || n.state === "unavailable" || (n.addresses || []).some(a => a.scope === "global") ||
      n.vpn?.configured || modules.some(m => m.network === n.id);
  }
  function table(data) {
    const networks = data.networks.filter(n => visibleNetwork(n, data.modules));
    if (!networks.length) return '<div class="network-empty">暂无已配置网络</div>';
    const icon = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="8" y="3" width="8" height="6" rx="1.5"/><path d="M12 9v4M5 16v-3h14v3"/><rect x="2" y="16" width="6" height="5" rx="1.5"/><rect x="16" y="16" width="6" height="5" rx="1.5"/></svg>';
    return '<div class="data-table-wrap"><table class="data-table"><thead><tr><th>网络</th><th>主网络</th><th>VPN</th><th>操作</th></tr></thead><tbody>' + networks.map(n => {
      const primary = n.primary ?? hostDefault(n), vpn = n.vpn || {};
      const ready = n.routingAvailable && !saving && !needsSync;
      const ip = (n.addresses || []).find(a => a.family === "inet" && a.scope === "global")?.local;
      const status = [n.fallback ? "已回退" : states[n.state], ip].filter(Boolean).join(" · ");
      const vpnStatus = vpn.enabled ? (vpn.state === "blocked" ? "连接异常" : "已开启") : vpn.testState === "testing" ? "检测中" : vpn.configured ? "未开启" : "未配置";
      return `<tr><th scope="row"><div class="network-identity"><span class="network-symbol ${primary ? "primary-network" : ""}">${icon}</span><div class="network-copy"><span class="network-label">${UI.escape(n.label)}${(n.systemDefault ?? hostDefault(n)) ? '<small class="network-default">系统默认</small>' : ""}</span><small class="network-role"><i class="network-dot ${n.state === "configured" ? "connected" : ""}" aria-hidden="true"></i>${UI.escape(status)}</small></div></div></th>` +
        `<td class="network-main-cell"><span class="network-mobile-label" aria-hidden="true">主网络</span><div class="network-control"><label class="form-switch" title="主网络"><input type="checkbox" role="switch" aria-label="${UI.escape(n.label)} 主网络" ${primary ? "checked" : ""} data-network-toggle="primary" data-network="${n.id}" ${ready && n.state === "configured" ? "" : "disabled"}><span aria-hidden="true"></span></label></div></td>` +
        `<td class="network-vpn-cell"><span class="network-mobile-label" aria-hidden="true">VPN</span><div class="network-control"><label class="form-switch" title="VPN"><input type="checkbox" role="switch" aria-label="${UI.escape(n.label)} VPN" data-network-toggle="vpn" data-network="${n.id}" ${vpn.enabled ? "checked" : ""} ${ready && (vpn.enabled || vpn.configured && n.state === "configured") ? "" : "disabled"}><span aria-hidden="true"></span></label><small class="network-role">${vpnStatus}</small></div></td>` +
        `<td class="network-actions-cell"><div class="network-actions"><button type="button" class="text-button network-assign" data-network-assign="${n.id}">分配</button><button type="button" class="text-button network-detail" data-network-detail="${n.id}">详情<span aria-hidden="true">›</span></button></div></td></tr>`;
    }).join("") + '</tbody></table></div>';
  }
  function vpnStatus(network) {
    const v = network.vpn || {};
    if (v.enabled) return v.state === "blocked" ? "连接异常 · 已阻止直连" : "已开启";
    if (v.testState === "testing") return "正在检测";
    if (v.testState === "passed") return "检测通过 · 未接管流量";
    if (v.testState === "failed") return errors[v.testError] || "检测失败";
    return v.configured ? "未开启" : "未配置";
  }
  function paintVPNStatus(network, form) {
    const status = form.querySelector("[data-vpn-status]");
    const exit = form.querySelector("[data-vpn-exit]");
    setText(status, vpnStatus(network));
    setText(exit, network.vpn?.testExitIP || "—");
    const remove = form.querySelector('[data-vpn-action="remove"]');
    if (remove && remove.hidden !== !network.vpn?.configured) remove.hidden = !network.vpn?.configured;
  }
  function setText(element, value) {
    if (element && element.textContent !== value) element.textContent = value;
  }
  function paint(force = false) {
    if (!snapshot || !controller) return;
    const list = UI.$("#network-list"), markup = table(snapshot);
    if (list && markupCache.get(list) !== markup) { list.innerHTML = markup; markupCache.set(list, markup); }
    const form = UI.$("#network-edit");
    if (!form) { lockForm(); return; }
    const network = snapshot.networks.find(n => n.id === form.dataset.network);
    if (!network) { UI.$("#dialog").close(); return; }
    setText(UI.$("#dialog-title"), form.dataset.details ? "网络详情" : network.label);
    if (form.dataset.details && markupCache.has(form) && !force) { paintVPNStatus(network, form); lockForm(); return; }
    const body = form.dataset.details ? detailRows(network, snapshot.modules) : '<div class="network-modules" tabindex="0" aria-label="模块分配">' + assignmentRows(snapshot.modules, network, snapshot.networks) + '</div>';
    if (force || markupCache.get(form) !== body) {
      const scroll = form.querySelector(".network-modules")?.scrollTop || 0;
      const focused = form.contains(document.activeElement) ? document.activeElement?.dataset.module : null;
      form.innerHTML = body;
      markupCache.set(form, body);
      const region = form.querySelector(".network-modules");
      if (region) region.scrollTop = scroll;
      if (focused) [...form.querySelectorAll("[data-module]")].find(i => i.dataset.module === focused)?.focus({ preventScroll: true });
      if (form.dataset.details) loadVPN(form);
    }
    lockForm();
  }
  function lockForm() {
    document.querySelectorAll("[data-network-toggle]").forEach(input => {
      const n = snapshot?.networks.find(n => n.id === input.dataset.network);
      input.disabled = !n?.routingAvailable || saving || needsSync || (input.dataset.networkToggle === "primary" ? n.state !== "configured" : !n.vpn?.enabled && (!n.vpn?.configured || n.state !== "configured"));
    });
    const form = UI.$("#network-edit");
    if (!form) return;
    if (form.getAttribute("aria-busy") !== String(saving)) form.setAttribute("aria-busy", String(saving));
    form.querySelectorAll("input, textarea, button").forEach(input => {
      const loadingNode = input.closest(".network-vpn") && !input.hasAttribute("data-vpn-retry") && form.dataset.vpnReady !== "true";
      const vpnActive = snapshot?.networks.find(n => n.id === form.dataset.network)?.vpn?.enabled;
      const editsNode = ["vpnLink", "vpnLabel"].includes(input.name) || ["save", "remove"].includes(input.dataset.vpnAction);
      const disabled = Boolean(saving || needsSync || loadingNode || input.dataset.locked === "true" || vpnActive && editsNode);
      if (editsNode) input.title = vpnActive ? "先关闭 VPN 再修改节点" : "";
      if (input.disabled !== disabled) input.disabled = disabled;
    });
  }
  async function loadVPN(form) {
    const active = controller, request = String(Number(form.dataset.vpnRequest || 0) + 1);
    form.dataset.vpnRequest = request;
    delete form.dataset.vpnReady;
    lockForm();
    try {
      const data = await Http.request("GET", "/settings/network-control", { query: { node: form.dataset.network }, signal: active.signal });
      if (controller !== active || active.signal.aborted || !form.isConnected || form.dataset.vpnRequest !== request) return;
      if (typeof data.link !== "string" || typeof data.label !== "string" || !Number.isSafeInteger(data.revision)) throw new Error("INVALID_RESPONSE");
      form.elements.namedItem("vpnLink").value = data.link;
      form.elements.namedItem("vpnLabel").value = data.label;
      form.dataset.vpnRevision = String(data.revision);
      form.dataset.vpnReady = "true";
      form.querySelector("[data-vpn-read-error]").hidden = true;
    } catch {
      if (controller === active && !active.signal.aborted && form.isConnected && form.dataset.vpnRequest === request) form.querySelector("[data-vpn-read-error]").hidden = false;
    } finally {
      if (controller === active) lockForm();
    }
  }
  async function refresh() {
    const active = controller;
    if (!active || active.signal.aborted || loading || saving || document.hidden) return;
    loading = true;
    const version = generation;
    try {
      const data = await Http.request("GET", "/settings/network-control", { signal: active.signal });
      if (controller !== active || active.signal.aborted || version !== generation) return;
      if (!valid(data)) throw new Error("INVALID_RESPONSE");
      snapshot = data; needsSync = false;
      paint();
      setText(UI.$("#network-error"), errors[data.routingError] || "");
    } catch (e) {
      if (controller === active && !active.signal.aborted && version === generation) {
        needsSync = true; lockForm();
        setText(UI.$("#network-error"), errors[e.code] || "读取失败，正在重试");
        if (!snapshot) UI.$("#network-list").textContent = "";
      }
    } finally {
      if (controller === active) loading = false;
    }
  }
  function assignmentLocked(module, network) {
    return module.busy || Boolean(module.network && module.network !== network.id) || network.state !== "configured" && module.network !== network.id;
  }
  function assignmentUpdate(modules, network, moduleID, enabled) {
    const target = modules.find(m => m.id === moduleID);
    if (!target || assignmentLocked(target, network)) return null;
    return modules.filter(m => m.id === moduleID ? enabled : m.network === network.id).map(m => m.id);
  }
  function assignmentRows(modules, network, networks) {
    const names = new Map(networks.map(n => [n.id, n.label]));
    return modules.map(m => {
      const current = m.network ? names.get(m.network) || "网络未连接" : "主机网络";
      const disabled = assignmentLocked(m, network);
      return `<div class="network-module"><span class="network-module-id">${UI.escape(m.label)}</span><span class="network-module-number">${UI.escape(m.number || "—")}</span><small title="${UI.escape(current)}">${UI.escape(current)}</small><label class="form-switch"><input type="checkbox" role="switch" data-module="${UI.escape(m.id)}" data-locked="${disabled}" aria-label="模块 ${UI.escape(m.label)} 分配到 ${UI.escape(network.label)}" ${m.network === network.id ? "checked" : ""} ${disabled ? "disabled" : ""}><span aria-hidden="true"></span></label></div>`;
    }).join("") || '<p class="network-empty">暂无模块</p>';
  }
  function detailRows(network, modules = []) {
    const addresses = network.addresses || [];
    const ipv4 = addresses.filter(a => a.family === "inet").map(a => a.local).join("、");
    const ipv6 = addresses.filter(a => a.family === "inet6").map(a => a.local).join("、");
    const rows = [["接口", network.name], ["局域网 IP", ipv4 || ipv6], ["网关", (network.gateways || []).join("、")]];
    const vpn = network.vpn || {};
    return `<div class="network-info"><div data-network-name-row>${nameRow(network)}</div>` +
      '<dl class="network-details">' + rows.map(([k,v]) => `<div><dt>${k}</dt><dd>${UI.escape(v || "—")}</dd></div>`).join("") + '</dl></div>' +
      '<section class="network-vpn"><h3>VPN</h3><label class="network-vpn-field">节点链接<textarea name="vpnLink" rows="3" autocomplete="off" autocapitalize="none" spellcheck="false" maxlength="4096" placeholder="粘贴 vless:// 链接"></textarea></label>' +
      `<label class="network-vpn-field">节点名称<input name="vpnLabel" maxlength="40" value="${UI.escape(vpn.label || "")}" placeholder="自动识别，可修改"></label>` +
      '<p class="network-load-error" data-vpn-read-error hidden>节点读取失败 <button type="button" class="text-button" data-vpn-retry>重试</button></p>' +
      `<dl class="network-details"><div><dt>检测出口 IP</dt><dd data-vpn-exit>${UI.escape(vpn.testExitIP || "—")}</dd></div><div><dt>连接检测</dt><dd data-vpn-status>${UI.escape(vpnStatus(network))}</dd></div></dl>` +
      `<div class="network-vpn-actions"><button type="button" class="text-button" data-vpn-action="test">检测连接</button><button type="button" class="text-button network-vpn-remove" data-vpn-action="remove" ${vpn.configured ? "" : "hidden"}>移除</button><button type="button" class="network-vpn-save" data-vpn-action="save">保存</button></div>` +
      '</section><details class="network-diagnostics"><summary>诊断信息</summary><dl class="network-details">' +
      `<div><dt>网络状态</dt><dd>${UI.escape(states[network.state])}</dd></div>${ipv4 && ipv6 ? `<div><dt>IPv6</dt><dd>${UI.escape(ipv6)}</dd></div>` : ""}<div><dt>DNS</dt><dd>${UI.escape((network.dns || []).join("、") || "—")}</dd></div>` +
      `<div><dt>传输保护</dt><dd>${vpn.security === "none" ? "未启用 TLS / REALITY" : vpn.security === "tls" ? "TLS" : "—"}</dd></div></dl></details>`;
  }
  function nameRow(network) {
    return `<button type="button" class="network-name" data-network-rename><span>名称</span><span>${UI.escape(network.label)}<span class="network-chevron" aria-hidden="true">›</span></span></button>`;
  }
  function finishRename(form) {
    const network = snapshot.networks.find(n => n.id === form.dataset.network);
    const row = form.querySelector("[data-network-name-row]");
    if (network && row) {
      const focused = row.contains(document.activeElement);
      row.innerHTML = nameRow(network);
      if (focused) row.querySelector("button").focus({ preventScroll: true });
    }
    delete form.dataset.renaming;
  }
  async function vpnAction(form, action) {
    if (saving || needsSync || !controller || form.dataset.vpnReady !== "true") return;
    const active = controller;
    saving = true; generation++; lockForm();
    const body = { id: form.dataset.network, revision: Number(form.dataset.vpnRevision), action };
    if (action !== "remove") {
      body.link = form.elements.namedItem("vpnLink").value.trim();
      body.label = form.elements.namedItem("vpnLabel").value.trim();
    }
    try {
      const result = await Http.request("POST", "/settings/network-control", { body, signal: active.signal });
      if (controller !== active || active.signal.aborted) return;
      if (action !== "test") {
        form.dataset.vpnRevision = String(result.revision);
        if (action === "remove") {
          form.elements.namedItem("vpnLink").value = "";
          form.elements.namedItem("vpnLabel").value = "";
        }
      }
      UI.toast(action === "test" ? "正在检测节点，未切换业务流量" : action === "save" ? "节点已保存" : "节点已移除");
    } catch (error) {
      if (controller === active && !active.signal.aborted) UI.toast(errors[error.code] || "操作未确认，请核对后重试");
    } finally {
      if (controller === active) { saving = false; lockForm(); await refresh(); }
    }
  }
  async function save(form, body, apply) {
    if (saving || needsSync || !controller) { paint(); return; }
    const active = controller, renaming = Boolean(form.dataset.renaming);
    saving = true; generation++; lockForm();
    try {
      await Backend.networks.update({ body, signal: active.signal });
      if (controller !== active || active.signal.aborted) return;
      // Only a confirmed write changes the local snapshot; never resend an uncertain write.
      apply(); snapshot.revision = body.revision + 1;
      if (renaming) finishRename(form);
    } catch (error) {
      if (controller === active && !active.signal.aborted) {
        needsSync = true;
        UI.toast(errors[error.code] || "保存未确认，正在核对");
      }
    } finally {
      if (controller === active) {
        saving = false;
        paint(!renaming); lockForm();
        await refresh();
      }
    }
  }
  function edit(id, details) {
    if (!snapshot || !controller) return;
    const network = snapshot.networks.find(n => n.id === id);
    if (!network) return;
    UI.modal(details ? "网络详情" : network.label, `<form id="network-edit" class="settings-form" data-network="${id}" ${details ? 'data-details="true"' : ""}></form>`);
    paint();
    const form = UI.$("#network-edit"), active = controller;
    function submitName() {
      if (!form.dataset.renaming || saving || needsSync) return;
      const label = form.elements.namedItem("label").value.trim();
      if (!label) { UI.toast("请输入名称"); return; }
      save(form, { id, revision: Number(form.dataset.renaming), label }, () => {
        snapshot.networks.find(n => n.id === id).label = label;
      });
    }
    form.addEventListener("input", e => {
      if (!["vpnLink", "vpnLabel"].includes(e.target.name)) return;
      if (e.target.name === "vpnLink" && !form.elements.namedItem("vpnLabel").value.trim()) {
        try {
          const url = new URL(e.target.value.trim());
          if (url.protocol === "vless:") form.elements.namedItem("vpnLabel").value = decodeURIComponent(url.hash.slice(1)).slice(0,40);
        } catch {}
      }
    }, { signal: active.signal });
    form.addEventListener("change", e => {
      const input = e.target.closest("[data-module]");
      if (!input) return;
      if (saving || needsSync) { paint(); return; }
      const current = snapshot.networks.find(n => n.id === id);
      if (!current) return;
      const modules = assignmentUpdate(snapshot.modules, current, input.dataset.module, input.checked);
      if (!modules) { paint(true); return; }
      const enabled = input.checked, moduleID = input.dataset.module;
      save(form, { id, revision: snapshot.revision, modules }, () => {
        snapshot.modules.find(m => m.id === moduleID).network = enabled ? id : "";
      });
    }, { signal: active.signal });
    form.addEventListener("click", e => {
      if (saving || needsSync) return;
      if (e.target.closest("[data-network-name-cancel]")) { finishRename(form); return; }
      if (e.target.closest("[data-network-name-save]")) { submitName(); return; }
      if (e.target.closest("button[data-vpn-retry]")) { loadVPN(form); return; }
      const vpnButton = e.target.closest("[data-vpn-action]");
      if (vpnButton) { vpnAction(form, vpnButton.dataset.vpnAction); return; }
      if (!e.target.closest("[data-network-rename]")) return;
      const current = snapshot.networks.find(n => n.id === id);
      form.dataset.renaming = String(snapshot.revision);
      form.querySelector("[data-network-name-row]").innerHTML = `<div class="network-rename"><label for="network-label">名称</label><input id="network-label" name="label" value="${UI.escape(current.label)}" maxlength="20" autocomplete="off"><button type="button" class="text-button" data-network-name-cancel>取消</button><button type="button" class="text-button" data-network-name-save>确定</button></div>`;
      form.elements.namedItem("label").focus({ preventScroll: true });
      form.elements.namedItem("label").select();
    }, { signal: active.signal });
    form.addEventListener("keydown", e => {
      if (e.target.name !== "label" || !form.dataset.renaming) return;
      if (e.key === "Enter") { e.preventDefault(); submitName(); }
      if (e.key === "Escape" && !saving) { e.preventDefault(); e.stopPropagation(); finishRename(form); }
    }, { signal: active.signal });
    form.addEventListener("submit", e => e.preventDefault(), { signal: active.signal });
  }
  async function routingAction(input) {
    const network = snapshot?.networks.find(n => n.id === input.dataset.network);
    if (!network) return;
    const enabled = input.checked, action = input.dataset.networkToggle;
    input.checked = action === "primary" ? Boolean(network.primary) : Boolean(network.vpn?.enabled);
    if (saving || needsSync || !controller) return;
    const active = controller;
    saving = true; generation++; lockForm();
    try {
      await Http.request("POST", "/settings/network-control", { body: { id: network.id, action, enabled, revision: snapshot.routingRevision }, signal: active.signal });
      if (controller !== active || active.signal.aborted) return;
    } catch (error) {
      if (controller === active && !active.signal.aborted) UI.toast(errors[error.code] || "切换未确认，正在核对");
    } finally {
      if (controller === active) {
        saving = false;
        await refresh(); lockForm();
        document.querySelector(`[data-network-toggle="${action}"][data-network="${network.id}"]`)?.focus({ preventScroll: true });
      }
    }
  }
  function outsideDialog(event, dialog) {
    const rect = dialog.getBoundingClientRect();
    return event.target === dialog && (event.clientX < rect.left || event.clientX > rect.right || event.clientY < rect.top || event.clientY > rect.bottom);
  }
  function mount() {
    unmount();
    controller = new AbortController();
    document.addEventListener("click", e => {
      const assign = e.target.closest("[data-network-assign]");
      const detail = e.target.closest("[data-network-detail]");
      if (assign) edit(assign.dataset.networkAssign, false);
      if (detail) edit(detail.dataset.networkDetail, true);
    }, { signal: controller.signal });
    document.addEventListener("change", e => {
      const input = e.target.closest("[data-network-toggle]");
      if (input) routingAction(input);
    }, { signal: controller.signal });
    const dialog = UI.$("#dialog");
    let pressedOutside = false;
    dialog.addEventListener("pointerdown", e => { pressedOutside = Boolean(UI.$("#network-edit")) && e.button === 0 && outsideDialog(e, dialog); }, { signal: controller.signal });
    dialog.addEventListener("pointercancel", () => { pressedOutside = false; }, { signal: controller.signal });
    dialog.addEventListener("click", e => {
      const dismiss = pressedOutside && outsideDialog(e, dialog);
      pressedOutside = false;
      if (dismiss && UI.$("#network-edit")) dialog.close();
    }, { signal: controller.signal });
    dialog.addEventListener("close", () => { pressedOutside = false; if (!dialog.open) UI.$("#network-edit")?.remove(); }, { signal: controller.signal });
    timer = setInterval(refresh, 5000);
    return refresh();
  }
  function unmount() {
    clearInterval(timer); timer = null;
    controller?.abort(); controller = null;
    snapshot = null; loading = false; saving = false; needsSync = false;
    if (UI.$("#network-edit")) { UI.$("#dialog").close(); UI.$("#network-edit")?.remove(); }
  }
  return { render, mount, unmount, valid, table, assignmentUpdate, assignmentRows, detailRows, visibleNetwork, vpnStatus, outsideDialog, setText };
})();
