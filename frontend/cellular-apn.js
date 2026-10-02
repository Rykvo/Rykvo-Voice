const CellularAPN = (() => {
  let session = null;
  const protocolName = (v) => ({ IP: "IPv4", IPV6: "IPv6", IPV4V6: "IPv4 / IPv6" })[v] || v || "—";
  const authName = (v) => ({ NONE: "无", PAP: "PAP", CHAP: "CHAP", PAP_OR_CHAP: "PAP / CHAP" })[v] || "—";
  const current = (s) => session === s && !s.controller.signal.aborted;
  function close() { session?.controller.abort(); session = null; }
  const back = (s, list = false) => `<button type="button" class="text-button cellular-back" data-apn-back="${list ? "list" : "line"}">‹ ${UI.escape(list ? "APN" : s.line.label || "SIM")}</button>`;
  function shell(s, html, list = false) {
    UI.modal("蜂窝数据网络", `<section class="cellular-page cellular-apn">${back(s, list)}${html}</section>`);
  }
  function list(s) {
    const rows = s.data?.current || [], profiles = s.data?.profiles || [];
    const issue = s.error || s.data?.issue;
    const state = issue ? ModuleData.issueText(issue) || "APN 读取未完成" : "";
    shell(s, `<h3 class="cellular-section-title">模块当前 APN</h3><div class="cellular-group">${rows.length ? rows.map(row => `<div class="cellular-setting"><span>${UI.escape(row.apn || "运营商自动配置")}<small class="apn-description">CID ${UI.escape(row.cid)} · ${UI.escape(protocolName(row.protocol))}</small></span><span class="cellular-setting-value">模块已有</span></div>`).join("") : `<p class="cellular-empty" role="status">${UI.escape(s.loading ? "正在读取" : state || "暂无读取结果")}</p>`}</div>
      ${rows.length && state ? `<p class="cellular-section-title" role="status">${UI.escape(state)}</p>` : ""}
      <h3 class="cellular-section-title apn-section">已保存的 APN</h3><div class="cellular-group">${profiles.length ? profiles.map(p => `<button type="button" class="cellular-setting" data-apn-edit="${UI.escape(p.id)}"><span>${UI.escape(p.apn || "运营商自动配置")}<small class="apn-description">${UI.escape(protocolName(p.protocol))} · ${UI.escape(authName(p.auth))}</small></span><span class="cellular-setting-value">已保存</span><span class="chevron" aria-hidden="true">›</span></button>`).join("") : '<p class="cellular-empty">暂无自定义 APN</p>'}</div>
      <button type="button" class="cellular-add" data-apn-add ${s.loading || !s.item.managed ? "disabled" : ""}>＋ 新增 APN</button>
      <button type="button" class="text-button cellular-notify" data-apn-refresh ${s.loading ? "disabled" : ""}>重新读取</button>`);
  }
  async function load(s) {
    if (!current(s)) return;
    s.loading = true; s.error = ""; list(s);
    try {
      if (!s.item.managed || !s.line.id) throw { code: "NOT_CONNECTED" };
      const data = await Backend.modules.apns({ params: s.params, signal: s.controller.signal });
      if (current(s)) s.data = data;
    } catch (e) { if (current(s)) s.error = e.code; }
    finally { if (current(s)) { s.loading = false; list(s); } }
  }
  function open(item, line, onBack) {
    close();
    const s = { item, line: { ...line }, onBack, controller: new AbortController(), params: { moduleId: item.id, lineId: line.id }, data: null, busy: false };
    session = s; load(s);
  }
  function form(s, profile = null) {
    s.edit = profile; s.formID = profile?.id || Http.id();
    const select = (name, label, options, selected) => `<div class="form-field"><label for="apn-${name}">${label}</label><select id="apn-${name}" name="${name}">${options.map(([value, text]) => `<option value="${value}" ${value === selected ? "selected" : ""}>${text}</option>`).join("")}</select></div>`;
    shell(s, `<form id="cellular-apn-form" class="settings-form" novalidate><div class="form-fields">
      ${Forms.field({ prefix: "apn", name: "apn", label: "APN", value: profile?.apn || "", placeholder: "留空使用运营商默认", maxLength: 100 })}
      ${select("protocol", "协议", [["IP", "IPv4"], ["IPV6", "IPv6"], ["IPV4V6", "IPv4 / IPv6"]], profile?.protocol || "IPV4V6")}
      ${select("auth", "认证", [["NONE", "无"], ["PAP", "PAP"], ["CHAP", "CHAP"], ["PAP_OR_CHAP", "PAP / CHAP"]], profile?.auth || "NONE")}
      ${Forms.field({ prefix: "apn", name: "username", label: "用户名", value: profile?.username || "", maxLength: 128 })}
      ${Forms.field({ prefix: "apn", name: "password", label: "密码", type: "password", autocomplete: "new-password", placeholder: profile?.hasPassword ? "已保存，留空保持不变" : "选填", maxLength: 128 })}
      </div><p class="cellular-section-title apn-note" role="status">${profile ? "编辑后先保存，再从列表重新打开并应用。" : "仅保存配置，不会自动开启蜂窝数据。"}</p><div class="form-footer"><button type="submit" class="primary">保存</button></div></form>
      ${profile ? `<div class="cellular-group cellular-settings apn-section"><button type="button" class="cellular-setting" data-apn-apply>应用已保存配置</button><button type="button" class="cellular-setting danger-button" data-apn-delete>删除已保存配置</button></div>` : ""}`, true);
  }
  function confirm(s, action) {
    shell(s, `<div class="cellular-group"><p class="cellular-empty">${action === "apply" ? "将已保存配置写入当前卡的数据 APN，不开启流量。请先关闭 Wi-Fi 通话并断开该模块的数据连接。" : "删除此卡的已保存 APN？模块当前配置不会被删除。"}</p></div><div class="form-footer"><button type="button" class="primary" data-apn-confirm="${action}">${action === "apply" ? "应用配置" : "删除配置"}</button></div>`, true);
  }
  async function mutate(s, operation, body) {
    if (!current(s) || s.busy) return;
    s.busy = true;
    document.querySelectorAll("[data-apn-confirm], #cellular-apn-form button[type=submit]").forEach(b => { b.disabled = true; });
    try {
      await Backend.modules[operation]({ params: { ...s.params, apnId: s.formID }, body, signal: s.controller.signal });
      if (current(s)) { UI.toast(operation === "applyAPN" ? "APN 已写入，未启用流量" : operation === "removeAPN" ? "配置已删除" : "APN 已保存"); await load(s); }
    } catch (e) { if (current(s)) UI.toast(ModuleData.issueText(e.code) || "请求未完成，请重新读取核实结果"); }
    finally { if (current(s)) { s.busy = false; document.querySelectorAll("[data-apn-confirm], #cellular-apn-form button[type=submit]").forEach(b => { b.disabled = false; }); } }
  }
  document.addEventListener("click", event => {
    const s = session;
    if (!s || !event.target.closest("#dialog-content") || s.busy) return;
    const b = event.target.closest("[data-apn-back]");
    if (b) { if (b.dataset.apnBack === "line") { const go = s.onBack; close(); go(); } else load(s); return; }
    if (event.target.closest("[data-apn-refresh]")) { if (!s.loading) load(s); return; }
    if (s.loading) return;
    if (event.target.closest("[data-apn-add]")) { if (s.item.managed) form(s); return; }
    const edit = event.target.closest("[data-apn-edit]");
    if (edit) { const p = s.data?.profiles.find(p => p.id === edit.dataset.apnEdit); if (p) form(s, p); return; }
    if (s.edit && event.target.closest("[data-apn-apply]")) { confirm(s,"apply"); return; }
    if (s.edit && event.target.closest("[data-apn-delete]")) { confirm(s,"delete"); return; }
    const confirmed = event.target.closest("[data-apn-confirm]");
    if (s.edit && confirmed) mutate(s, confirmed.dataset.apnConfirm === "apply" ? "applyAPN" : "removeAPN");
  });
  document.addEventListener("submit", event => {
    if (event.target.id !== "cellular-apn-form") return;
    event.preventDefault(); const s = session; if (!s || s.busy) return;
    const get = name => event.target.elements.namedItem(name).value;
    const auth = get("auth"), password = get("password");
    mutate(s,"saveAPN", { apn: get("apn").trim(), protocol: get("protocol"), auth, username: auth === "NONE" ? "" : get("username"), password: auth === "NONE" ? "" : password, preservePassword: auth !== "NONE" && !password && s.edit?.hasPassword === true });
  });
  document.getElementById("dialog").addEventListener("close", close);
  return { open, close };
})();
