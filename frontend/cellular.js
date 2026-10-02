const Cellular = (() => {
  let module = null;
  let activeLine = -1, activeID = null, unsubscribe = null;
  let view = "overview", rendered = "", submitting = false;
  let emergencyRequest = null;
  let emergencySession = null, emergencyTimer = null, emergencyExpiry = null, emergencyLoad = null;
  const dismissedJobs = new Set();
  const editable = (item) => !item.managed || (item.capabilities?.esim && !submitting);
  const wifiEditable = (item) => !submitting && (!item.managed || item.capabilities?.wifiCalling === true) && item.wifi?.state !== "stopping";
  const roamingEditable = (item) => !submitting && (!item.managed || item.capabilities?.roaming === true) && item.wifi?.enabled !== true && item.wifi?.state !== "stopping" && !(item.sims || []).some((sim) => sim.enabled && sim.wifiCalling === true);
  const downloadable = (item) => !item.managed || item.capabilities?.esimDownload === true;
  function active() {
    if (activeID) activeLine = module?.sims?.findIndex((sim) => sim.id === activeID) ?? -1;
    return module?.sims?.[activeLine];
  }
  function jobNote(item) {
    const job = item.job;
    if (!job?.id || job.state === "succeeded" || dismissedJobs.has(job.id)) return "";
    const busy = ["queued", "running"].includes(job.state);
    const stage = { waiting: "等待设备", checking: "检查设备", writing: job.action === "enable" ? "正在切换号码" : "正在处理", authenticating: "正在验证", downloading: "正在下载", installing: "正在写入", verifying: "正在确认卡片状态", notifying: "正在上报状态" };
    const text = busy ? stage[job.stage] || "正在处理" : ModuleData.issueText(job.issue) || "操作未完成";
    return `<div class="cellular-job"><div class="cellular-job-heading"><span role="status">${UI.escape(text)}</span>${busy ? "" : '<button type="button" class="text-button" data-cellular-dismiss aria-label="关闭操作提示">×</button>'}</div>${busy ? `<progress aria-label="${UI.escape(text)}"></progress>` : ""}</div>`;
  }
  function refreshDialog() {
    if (view === "emergency") {
      if (!active()?.enabled || active().iccid !== emergencySession?.card || module.issue || module.cardReading) closeEmergency();
      return;
    }
    if (!module || !["overview", "detail", "wifi"].includes(view)) return;
    active();
    if (view !== "overview" && !active()) view = "overview";
    const render = (showJob) => view === "wifi" ? wifiPage(module) : view === "detail" ? detail(module, activeLine, showJob) : overview(module, showJob);
    const html = render(false);
    const content = document.getElementById("dialog-content"), dialog = document.getElementById("dialog");
    if (html === rendered) {
      const slot = content.querySelector("[data-cellular-job]");
      const note = jobNote(module);
      if (slot && slot.innerHTML !== note) slot.innerHTML = note;
      return;
    }
    rendered = html;
    const scroll = dialog.scrollTop;
    const focus = document.activeElement;
    const setting = focus?.dataset?.cellularSetting, action = focus?.dataset?.cellularAction;
    const title = view === "wifi" ? "Wi-Fi 通话" : view === "detail" ? active().label : "蜂窝网络";
    content.innerHTML = `<h2 id="dialog-title">${UI.escape(title)}</h2>${render(true)}`;
    dialog.scrollTop = scroll;
    if (setting) content.querySelector(`[data-cellular-setting="${setting}"]`)?.focus({ preventScroll: true });
    else if (action) content.querySelector(`[data-cellular-action="${action}"]`)?.focus({ preventScroll: true });
  }
  async function control(item, operation, body, lineId, requestId) {
    if (typeof body.wifiCalling === "boolean" ? !wifiEditable(item) : typeof body.roaming === "boolean" ? !roamingEditable(item) : !editable(item)) return false;
    submitting = true;
    try {
      await ModuleData.control(item, operation, body, lineId, requestId);
      return true;
    } catch (error) {
      UI.toast(ModuleData.issueText(error.code) || "请求失败，请核实操作状态");
      return false;
    } finally { submitting = false; rendered = ""; refreshDialog(); }
  }
  const simIcon =
    '<svg viewBox="0 0 28 32" aria-hidden="true"><path d="M6 2h12l6 6v21H4V4a2 2 0 0 1 2-2Z"/><rect x="8" y="13" width="12" height="11" rx="2"/><path d="M12 13v11m4-11v11M8 18.5h12"/></svg>';
  function lines(item) {
    return (item.sims || []).map((sim, index) => ({
      ...sim,
      wifiCalling: sim.wifiCalling === true,
      roaming: sim.roaming === true,
      number: item.managed ? sim.number : index === 0 ? item.number : sim.number,
      state: sim.enabled ? "打开" : "关闭",
    }));
  }
  function back(toDetail = false) {
    const label = toDetail ? active()?.label || "蜂窝网络" : "蜂窝网络";
    return `<button type="button" class="text-button cellular-back" data-cellular-back="${toDetail ? "detail" : "overview"}">‹ ${UI.escape(label)}</button>`;
  }
  function overview(item, showJob = true) {
    const cards = lines(item);
    const cardIssue = item.issue || (item.cardReading && item.hardware?.esim?.issue);
    return `<section class="cellular-page">
      <h3 class="cellular-section-title">${UI.escape(item.label || item.name)}</h3>
      <div class="cellular-group">${cards.length ? cards.map((sim, index) => `
        <button type="button" class="cellular-sim" data-cellular-sim="${index}">
          <span class="cellular-sim-icon">${simIcon}</span>
          <span class="cellular-sim-copy"><strong>${UI.escape(sim.label)}</strong>${sim.number || sim.iccid ? `<small>${UI.escape(ModuleData.identity(sim.number, sim.iccid).caption)}</small>` : ""}</span>
          <span class="cellular-sim-meta" data-enabled="${sim.enabled === true}">${UI.escape(sim.state)}</span>
          <span class="chevron" aria-hidden="true">›</span>
        </button>`).join("") : `<p class="cellular-empty">${UI.escape(cardIssue ? ModuleData.issueText(cardIssue) : item.cardReading ? "正在读取卡片" : "无 SIM 卡")}</p>`}</div>
      ${typeof Alerts === "undefined" ? "" : Alerts.details(item)}
      ${downloadable(item) ? `<button type="button" class="cellular-add" data-cellular-add ${editable(item) ? "" : "disabled"}>
        <span aria-hidden="true">＋</span>添加 eSIM<span class="chevron" aria-hidden="true">›</span>
      </button>` : ""}
      ${item.hardware?.esim?.pending > 0 ? `<button type="button" class="text-button cellular-notify" data-cellular-notify ${editable(item) ? "" : "disabled"}>重试状态上报</button>` : ""}<div data-cellular-job>${showJob ? jobNote(item) : ""}</div>
    </section>`;
  }
  function open(item) {
    clearEmergency();
    unsubscribe?.();
    module = item;
    activeLine = -1; activeID = null; view = "overview";
    rendered = overview(item, false);
    UI.modal("蜂窝网络", overview(item));
    unsubscribe = ModuleData.subscribe(refreshDialog);
  }
  function switchRow(key, label, checked, disabled = false, status = "") {
    return `<div class="cellular-setting"><span>${label}</span>${status ? `<span class="cellular-setting-value">${UI.escape(status)}</span>` : ""}<span class="form-switch"><input type="checkbox" role="switch" data-cellular-setting="${key}" aria-label="${label}" ${checked ? "checked" : ""} ${disabled ? "disabled" : ""}><span aria-hidden="true"></span></span></div>`;
  }
  function valueRow(label, value, action = "", disabled = false) {
    const content = `<span>${label}</span><span class="cellular-setting-value">${UI.escape(value)}</span>${action ? '<span class="chevron" aria-hidden="true">›</span>' : ""}`;
    return action
      ? `<button type="button" class="cellular-setting" data-cellular-action="${action}" ${disabled ? "disabled" : ""}>${content}</button>`
      : `<div class="cellular-setting">${content}</div>`;
  }
  function detail(item, index, showJob = true) {
    const sim = lines(item)[index];
    if (!sim) return "";
    const identity = ModuleData.identity(sim.number, sim.iccid);
    const realESIM = item.managed && sim.esim;
    const disabled = item.managed && (!realESIM || !editable(item));
    const settingsDisabled = !sim.enabled || !roamingEditable(item);
    const pending = item.managed && item.capabilities?.roaming !== true ? "不可用" : "";
    const remove = realESIM
      ? `<div class="cellular-group cellular-settings"><button type="button" class="cellular-setting danger-button" data-cellular-delete ${disabled || sim.enabled || !sim.canDelete ? "disabled" : ""}>删除 eSIM</button></div>`
      : "";
    return `<section class="cellular-page">${back()}
      <div class="cellular-group cellular-settings">
        ${valueRow("号码标签", sim.label, "label", disabled)}
        ${switchRow("enabled", "启用此号码", sim.enabled, disabled || (realESIM && sim.enabled && !sim.canDisable))}
      </div>
      <div class="cellular-group cellular-settings">
        ${valueRow(identity.label, identity.value)}
        ${valueRow("Wi-Fi 通话", !item.managed || item.capabilities?.wifiCalling ? (sim.wifiCalling ? (item.wifi?.registered ? "已连接" : "开启") : "关闭") : "不可用", "wifi", !sim.enabled || !wifiEditable(item))}
        ${switchRow("roaming", "数据漫游", sim.roaming, settingsDisabled, pending)}
      </div>${remove}<div data-cellular-job>${showJob ? jobNote(item) : ""}</div></section>`;
  }

  function wifiPage(item) {
    const sim = active();
    const state = item.wifi?.state;
    const status = state === "connected" ? "已连接" : state === "connecting" || state === "waiting" ? "连接中" : state === "stopping" ? "关闭中" : "";
    const issue = item.wifi?.issue;
    const emergency = item.managed && item.carrierConfiguration?.emergencyAddress;
    return `<section class="cellular-page">${back(true)}<div class="cellular-group cellular-settings">${switchRow("wifiCalling", "Wi-Fi 通话", sim?.wifiCalling === true, !sim?.enabled || !wifiEditable(item), status)}</div>${emergency ? `<div class="cellular-group cellular-settings">${valueRow("更新紧急联系地址", "", "emergency-address", !sim?.enabled)}</div>` : ""}${issue ? `<p class="cellular-empty" role="status">${UI.escape(ModuleData.issueText(issue))}</p>` : ""}</section>`;
  }
  function clearEmergency() {
    emergencyRequest?.abort(); emergencyRequest = null;
    clearTimeout(emergencyTimer); emergencyTimer = null;
    clearTimeout(emergencyExpiry); emergencyExpiry = null;
    clearTimeout(emergencyLoad); emergencyLoad = null;
    document.getElementById("emergency-frame")?.remove();
    const old = emergencySession; emergencySession = null;
    if (old?.id) Backend.emergencyAddress.cancel({ params: { ...old.params, sessionId: old.id } }).catch(() => {});
  }
  function closeEmergency() {
    clearEmergency();
    if (!module) return;
    if (!active()?.enabled) { open(module); return; }
    view = "wifi"; rendered = wifiPage(module); UI.modal("Wi-Fi 通话", rendered);
    document.querySelector('[data-cellular-action="emergency-address"]')?.focus();
  }
  function emergencyError(code) {
    return ({ EMERGENCY_UNSUPPORTED: "此卡暂未接入紧急地址", EMERGENCY_REJECTED: "运营商未接受验证", EMERGENCY_SESSION_EXPIRED: "会话已失效，请重新打开", DEVICE_CHANGED: "卡片已变化，请重新打开", DEVICE_BUSY: "模块忙碌，请稍后重试" })[code] || "运营商页面暂未打开，请稍后重试";
  }
  function showEmergencyPage(page, expiresAt) {
    const url = new URL(page?.url || "");
    if (url.protocol !== "https:" || url.username || url.password || url.search || url.hash || typeof page.token !== "string" || !page.token || page.token.length > 16384) throw Error("invalid carrier page");
    const remaining = new Date(expiresAt).getTime() - Date.now();
    if (!Number.isFinite(remaining) || remaining <= 0 || remaining > 300000) throw Error("invalid session expiry");
    const frame = document.getElementById("emergency-frame");
    if (!frame || !emergencySession) return;
    const sessionID = emergencySession.id;
    frame.hidden = false;
    frame.addEventListener("load", () => {
      if (emergencySession?.id !== sessionID || !frame.isConnected) return;
      clearTimeout(emergencyLoad); emergencyLoad = null;
      document.getElementById("emergency-loading")?.remove();
    }, { once: true });
    emergencyLoad = setTimeout(() => { closeEmergency(); UI.toast(emergencyError()); }, 30000);
    const form = document.createElement("form"), input = document.createElement("input");
    form.method = "POST"; form.action = url.href; form.target = frame.name; form.hidden = true;
    input.type = "hidden"; input.name = "token"; input.value = page.token;
    form.append(input); document.getElementById("dialog-content").append(form);
    form.submit(); input.value = ""; form.remove();
    emergencyExpiry = setTimeout(() => { closeEmergency(); UI.toast(emergencyError("EMERGENCY_SESSION_EXPIRED")); }, remaining);
  }
  async function emergencyAddress(sim) {
    if (!sim?.enabled || !module.carrierConfiguration?.emergencyAddress || emergencyRequest) return;
    const item = module, lineID = sim.id, controller = new AbortController();
    const params = { moduleId: item.id, lineId: lineID };
    emergencyRequest = controller;
    emergencySession = { params, card: sim.iccid, id: null };
    view = "emergency";
    UI.modal("更新紧急联系地址", `<section class="cellular-page emergency-sheet"><div class="emergency-toolbar"><button type="button" class="text-button" data-emergency-cancel>取消</button></div><div id="emergency-loading" class="emergency-loading" role="status" aria-label="正在打开运营商页面"><span class="emergency-spinner" aria-hidden="true"></span></div><iframe id="emergency-frame" name="emergency-carrier" title="运营商紧急地址页面" sandbox="allow-scripts allow-forms allow-same-origin" referrerpolicy="no-referrer" hidden></iframe></section>`);
    document.querySelector('[data-emergency-cancel]')?.focus();
    const current = () => module === item && active()?.id === lineID && !controller.signal.aborted && view === "emergency";
    const poll = async () => {
      try {
        const state = await Backend.emergencyAddress.get({ params: { ...params, sessionId: emergencySession.id }, signal: controller.signal });
        if (!current()) return;
        if (state.state === "ready") {
          if (!emergencySession.opened) {
            showEmergencyPage(state.page, state.expiresAt);
            emergencySession.opened = true;
          }
        } else if (state.state !== "pending" || emergencySession.opened) throw Object.assign(Error("carrier session failed"), { code: state.issue });
        emergencyTimer = setTimeout(poll, emergencySession.opened ? 5000 : 1000);
      } catch (error) { if (current()) { closeEmergency(); UI.toast(emergencyError(error.code)); } }
    };
    try {
      const state = await Backend.emergencyAddress.start({ params, signal: controller.signal });
      if (!current()) { if (state?.id) Backend.emergencyAddress.cancel({ params: { ...params, sessionId: state.id } }).catch(() => {}); return; }
      if (typeof state?.id !== "string" || !state.id || state.id.length > 256) throw Error("invalid carrier session");
      emergencySession.id = state.id;
      await poll();
    } catch (error) {
      if (current()) { closeEmergency(); UI.toast(emergencyError(error.code)); }
    }
  }
  function showDetail() {
    active();
    const sim = lines(module)[activeLine];
    if (sim) { view = "detail"; rendered = detail(module, activeLine, false); UI.modal(sim.label, detail(module, activeLine)); }
  }
  const { validLabel } = ModuleData;
  function validActivation(value) {
    return /^LPA:1\$[A-Za-z0-9.-]+\$[^\s$]+(?:\$[^\s$]*)?(?:\$1)?$/.test(value.trim());
  }
  document.addEventListener("click", (event) => {
    if (!module || !event.target.closest("#dialog-content")) return;
    if (event.target.closest("[data-emergency-cancel]")) { closeEmergency(); return; }
    if (event.target.closest("[data-cellular-dismiss]")) {
      if (module.job?.id && !["queued", "running"].includes(module.job.state)) dismissedJobs.add(module.job.id);
      refreshDialog();
      return;
    }
    const backButton = event.target.closest("[data-cellular-back]");
    if (backButton) {
      if (backButton.dataset.cellularBack === "detail") showDetail();
      else open(module);
      return;
    }
    const action = event.target.closest("[data-cellular-action]");
    if (action) {
      active();
      const sim = lines(module)[activeLine];

      if (action.dataset.cellularAction === "emergency-address") {
        if (view === "wifi") emergencyAddress(sim);
        return;
      }
      if (module.managed && (action.dataset.cellularAction === "wifi" ? !wifiEditable(module) : !editable(module) || !sim?.esim || action.dataset.cellularAction !== "label")) return;
      if (!sim) return;
      if (action.dataset.cellularAction === "label") {
        view = "form";
        UI.modal(
          "号码标签",
          `<section class="cellular-page">${back(true)}<form id="cellular-label-form" class="settings-form" novalidate><div class="form-fields">${Forms.field({ prefix: "cellular", name: "label", label: "标签", value: sim.label, placeholder: "主号 / 副号", maxLength: 20, required: true })}</div><div class="form-footer"><button type="submit" class="primary">完成</button></div></form></section>`,
        );
        document.getElementById("cellular-label").focus();
      } else if (action.dataset.cellularAction === "wifi" && sim.enabled) {
        view = "wifi"; rendered = wifiPage(module);
        UI.modal("Wi-Fi 通话", rendered);
      }
      return;
    }
    if (event.target.closest("[data-cellular-add]")) {
      if (!downloadable(module) || !editable(module)) return;
      view = "form";
      UI.modal(
        "添加 eSIM",
        `<section class="cellular-page">${back()}<form id="esim-form" class="settings-form" novalidate><div class="form-fields">${Forms.field({ prefix: "esim", name: "activation", label: "激活码", type: "password", placeholder: "LPA:1$…", maxLength: 2048, required: true })}${Forms.field({ prefix: "esim", name: "confirmation", label: "确认码", type: "password", placeholder: "选填", maxLength: 128 })}${module.managed && !module.hardware?.imei ? Forms.field({ prefix: "esim", name: "imei", label: "设备 IMEI", inputmode: "numeric", maxLength: 15, required: true }) : ""}</div><div class="form-footer"><button type="submit" class="primary">添加 eSIM</button></div></form></section>`,
      );
      document.getElementById("esim-activation").focus();
      return;
    }
    if (event.target.closest("[data-cellular-delete]")) {
      const item = module, sim = active();
      if (!sim?.esim || !editable(item) || sim.enabled || !sim.canDelete) return;
      view = "confirm";
      ContextMenu.confirm(`删除“${sim.label}” eSIM？`, () => control(item, "removeLine", {}, sim.id));
      return;
    }
    if (event.target.closest("[data-cellular-notify]")) { control(module, "notifyESIM", {}); return; }
    const button = event.target.closest("[data-cellular-sim]");
    if (!button) return;
    const index = Number(button.dataset.cellularSim);
    const sim = lines(module)[index];
    if (!sim) return;
    activeLine = index; activeID = sim.id || null;
    showDetail();
  });
  document.addEventListener("change", (event) => {
    const input = event.target;
    const key = input.dataset.cellularSetting;
    if (
      !module ||
      !input.closest("#dialog-content") ||
      !["enabled", "wifiCalling", "roaming"].includes(key)
    )
      return;
    const sim = active();
    if (!sim || (key !== "enabled" && !sim.enabled)) return;
    if (module.managed) {
      const enabled = input.checked;
      input.checked = Boolean(sim[key]);
      if (key === "roaming") {
        if (!roamingEditable(module)) return;
        input.disabled = true;
        control(module, "updateLine", { roaming: enabled }, sim.id);
        return;
      }
      if (key === "wifiCalling") {
        if (!wifiEditable(module)) return;
        input.disabled = true;
        control(module, "updateLine", { wifiCalling: enabled }, sim.id);
        return;
      }
      if (key !== "enabled" || !sim.esim || !editable(module) || (!enabled && !sim.canDisable)) return;
      input.disabled = true;
      control(module, "updateLine", { enabled }, sim.id);
      return;
    }
    // 仅更新本次页面的样本，设备配置由后端接入后执行。
    sim[key] = input.checked;
    if (key === "enabled") {
      document
        .querySelectorAll(
          '[data-cellular-action="wifi"], [data-cellular-setting="roaming"]',
        )
        .forEach((control) => {
          control.disabled = !sim.enabled;
        });
    }
  });
  document.addEventListener("submit", async (event) => {
    if (event.target.id === "cellular-label-form") {
      event.preventDefault();
      if (!module || !active()) return;

      const label = event.target.elements.namedItem("label").value.trim();
      if (
        Forms.report(
          event.target,
          validLabel(label)
            ? null
            : { field: "label", message: "标签需为 1–20 个字符" },
        )
      )
        return;
      if (module.managed) {
        if (!active().esim || !editable(module)) return;
        const item = module, form = event.target, line = active().id;
        form.querySelector('[type="submit"]').disabled = true;
        const ok = await control(item, "updateLine", { label }, line, form.dataset.requestId ||= Http.id());
        if (module === item && form.isConnected) { form.querySelector('[type="submit"]').disabled = false; if (ok) showDetail(); }
        return;
      }
      module.sims[activeLine].label = label;
      showDetail();
      return;
    }
    if (event.target.id !== "esim-form") return;
    event.preventDefault();
    const input = event.target.elements.namedItem("activation");
    if (
      Forms.report(
        event.target,
        validActivation(input.value)
          ? null
          : { field: "activation", message: "请输入有效的 eSIM 激活码" },
      )
    )
      return;
    if (!module?.managed) { UI.toast("eSIM 服务暂不可用"); return; }
    if (!downloadable(module) || !editable(module)) return;
    const item = module, form = event.target;
    const button = form.querySelector('[type="submit"]');
    button.disabled = true;
    const ok = await control(item, "installESIM", {
      activation: input.value.trim(),
      confirmation: form.elements.namedItem("confirmation")?.value || "",
      imei: form.elements.namedItem("imei")?.value.trim() || "",
    }, undefined, form.dataset.requestId ||= Http.id());
    if (module === item && form.isConnected) {
      button.disabled = false;
      if (ok) { form.reset(); open(item); }
    }
  });
  document.getElementById("dialog").addEventListener("close", () => {
    clearEmergency();
    unsubscribe?.(); unsubscribe = null;
    module = null;
    activeLine = -1; activeID = null;
    const form = document.getElementById("esim-form");
    form?.reset();
  });
  return { open, overview, detail, lines, validActivation, validLabel };
})();
