const Cellular = (() => {
  let module = null;
  let activeLine = -1, activeID = null, unsubscribe = null;
  let view = "overview", rendered = "", submitting = false, manualNetwork = false;
  const dismissedJobs = new Set();
  const editable = (item) => !item.managed || (item.capabilities?.esim && !submitting);
  const downloadable = (item) => !item.managed || item.capabilities?.esimDownload === true;
  const networkEditable = (item, sim = active()) => item.capabilities?.network && sim?.enabled && sim.networkAvailable && !submitting;
  function active() {
    if (activeID) activeLine = module?.sims?.findIndex((sim) => sim.id === activeID) ?? -1;
    return module?.sims?.[activeLine];
  }
  function jobNote(item) {
    const job = item.job;
    if (!job?.id || job.state === "succeeded" || dismissedJobs.has(job.id)) return "";
    const busy = ["queued", "running"].includes(job.state);
    const stage = { waiting: "等待设备", checking: "检查设备", scanning: "正在搜索网络", selecting: "正在选择网络", writing: job.action === "enable" ? "正在切换号码" : "正在处理", authenticating: "正在验证", downloading: "正在下载", installing: "正在写入", verifying: "正在确认卡片状态", notifying: "正在上报状态" };
    const text = busy ? stage[job.stage] || "正在处理" : ModuleData.issueText(job.issue) || "操作未完成";
    return `<div class="cellular-job"><div class="cellular-job-heading"><span role="status">${UI.escape(text)}</span>${busy ? "" : '<button type="button" class="text-button" data-cellular-dismiss aria-label="关闭操作提示">×</button>'}</div>${busy ? `<progress aria-label="${UI.escape(text)}"></progress>` : ""}</div>`;
  }
  function refreshDialog() {
    if (!module || !["overview", "detail", "network"].includes(view)) return;
    active();
    if (view !== "overview" && !active()) view = "overview";
    const render = (showJob) => view === "network" ? networkPage(module, showJob) : view === "detail" ? detail(module, activeLine, showJob) : overview(module, showJob);
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
    const title = view === "network" ? "网络选择" : view === "detail" ? active().label : "蜂窝网络";
    content.innerHTML = `<h2 id="dialog-title">${UI.escape(title)}</h2>${render(true)}`;
    dialog.scrollTop = scroll;
    if (setting) content.querySelector(`[data-cellular-setting="${setting}"]`)?.focus({ preventScroll: true });
    else if (action) content.querySelector(`[data-cellular-action="${action}"]`)?.focus({ preventScroll: true });
  }
  async function control(item, operation, body, lineId, requestId) {
    const network = operation === "scanNetworks" || body.networkAutomatic !== undefined;
    if (network ? !networkEditable(item) : !editable(item)) return false;
    submitting = true;
    try {
      await ModuleData.control(item, operation, body, lineId, requestId);
      return true;
    } catch (error) {
      UI.toast(ModuleData.issueText(error.code) || "请求失败，请核实操作状态");
      return false;
    } finally { submitting = false; refreshDialog(); }
  }
  const simIcon =
    '<svg viewBox="0 0 28 32" aria-hidden="true"><path d="M6 2h12l6 6v21H4V4a2 2 0 0 1 2-2Z"/><rect x="8" y="13" width="12" height="11" rx="2"/><path d="M12 13v11m4-11v11M8 18.5h12"/></svg>';
  function lines(item) {
    return (item.sims || []).map((sim, index) => ({
      ...sim,
      networkAutomatic: sim.networkAutomatic !== false,
      wifiCalling: sim.wifiCalling === true,
      roaming: sim.roaming === true,
      number: item.managed ? sim.number : index === 0 ? item.number : sim.number,
      state: !sim.enabled
        ? "已关闭"
        : item.status === "online"
          ? "已启用"
          : "无服务",
    }));
  }
  function back(toDetail = false) {
    const label = toDetail ? active()?.label || "蜂窝网络" : "蜂窝网络";
    return `<button type="button" class="text-button cellular-back" data-cellular-back="${toDetail ? "detail" : "overview"}">‹ ${UI.escape(label)}</button>`;
  }
  function overview(item, showJob = true) {
    const cards = lines(item);
    return `<section class="cellular-page">
      <h3 class="cellular-section-title">${UI.escape(item.label || item.name)}</h3>
      <div class="cellular-group">${cards.length ? cards.map((sim, index) => `
        <button type="button" class="cellular-sim" data-cellular-sim="${index}">
          <span class="cellular-sim-icon">${simIcon}</span>
          <span class="cellular-sim-copy"><strong>${UI.escape(sim.label)}</strong>${sim.number || sim.iccid ? `<small>${UI.escape(ModuleData.identity(sim.number, sim.iccid).caption)}</small>` : ""}</span>
          <span class="cellular-sim-meta">${UI.escape(sim.state)}</span>
          <span class="chevron" aria-hidden="true">›</span>
        </button>`).join("") : `<p class="cellular-empty">${UI.escape(item.issue ? ModuleData.issueText(item.issue) : item.cardReading ? "正在读取卡片" : "无 SIM 卡")}</p>`}</div>
      ${downloadable(item) ? `<button type="button" class="cellular-add" data-cellular-add ${editable(item) ? "" : "disabled"}>
        <span aria-hidden="true">＋</span>添加 eSIM<span class="chevron" aria-hidden="true">›</span>
      </button>` : ""}
      ${item.hardware?.esim?.pending > 0 ? `<button type="button" class="text-button cellular-notify" data-cellular-notify ${editable(item) ? "" : "disabled"}>重试状态上报</button>` : ""}<div data-cellular-job>${showJob ? jobNote(item) : ""}</div>
    </section>`;
  }
  function open(item) {
    unsubscribe?.();
    module = item;
    activeLine = -1; activeID = null; view = "overview"; manualNetwork = false;
    rendered = overview(item, false);
    UI.modal("蜂窝网络", overview(item));
    unsubscribe = ModuleData.subscribe(refreshDialog);
  }
  function switchRow(key, label, checked, disabled = false, status = "") {
    return `<label class="cellular-setting"><span>${label}</span>${status ? `<span class="cellular-setting-value">${UI.escape(status)}</span>` : ""}<span class="form-switch"><input type="checkbox" role="switch" data-cellular-setting="${key}" aria-label="${label}" ${checked ? "checked" : ""} ${disabled ? "disabled" : ""}><span aria-hidden="true"></span></span></label>`;
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
    const networkDisabled = item.managed || !sim.enabled;
    const pending = item.managed ? "待接入" : "";
    const remove = realESIM
      ? `<div class="cellular-group cellular-settings"><button type="button" class="cellular-setting danger-button" data-cellular-delete ${disabled || sim.enabled || !sim.canDelete ? "disabled" : ""}>删除 eSIM</button></div>`
      : "";
    return `<section class="cellular-page">${back()}
      <div class="cellular-group cellular-settings">
        ${valueRow("号码标签", sim.label, "label", disabled)}
        ${switchRow("enabled", "启用此号码", sim.enabled, disabled || (realESIM && sim.enabled && !sim.canDisable))}
      </div>
      <div class="cellular-group cellular-settings">
        ${valueRow("网络选择", item.managed && !sim.networkAvailable ? "待接入" : sim.networkAutomatic ? "自动" : "手动", "network", item.managed ? !networkEditable(item, sim) : networkDisabled)}
        ${valueRow(identity.label, identity.value)}
        ${valueRow("Wi-Fi 通话", pending || (sim.wifiCalling ? "开启" : "关闭"), "wifi", networkDisabled)}
        ${switchRow("roaming", "数据漫游", sim.roaming, networkDisabled, pending)}
      </div>${remove}<div data-cellular-job>${showJob ? jobNote(item) : ""}</div></section>`;
  }

  function networkPage(item, showJob = true) {
    const sim = active(), job = item.job;
    const busy = !networkEditable(item, sim);
    const manual = manualNetwork || sim?.networkAutomatic === false;
    const operators = job?.action === "network-scan" && job.state === "succeeded" ? job.networks || [] : [];
    return `<section class="cellular-page">${back(true)}<div class="cellular-group cellular-settings">${switchRow("networkAutomatic", "自动", !manual, busy)}</div>${manual ? `<div class="cellular-group cellular-settings">${operators.map((op, index) => `<button type="button" class="cellular-setting" data-network-index="${index}" ${busy || op.status === 3 ? "disabled" : ""}><span>${UI.escape(op.name)}</span><span class="cellular-setting-value">${UI.escape(op.technology === 7 ? "LTE" : op.technology === 2 ? "UMTS" : op.technology === 0 ? "GSM" : op.plmn)}${op.status === 2 ? " ✓" : ""}</span></button>`).join("")}<button type="button" class="cellular-setting text-button" data-network-scan ${busy ? "disabled" : ""}>搜索网络</button></div>` : ""}<div data-cellular-job>${showJob ? jobNote(item) : ""}</div></section>`;
  }
  function showNetwork() {
    view = "network"; rendered = networkPage(module, false);
    UI.modal("网络选择", networkPage(module));
  }
  function showDetail() {
    active();
    const sim = lines(module)[activeLine];
    if (sim) { view = "detail"; rendered = detail(module, activeLine, false); UI.modal(sim.label, detail(module, activeLine)); }
  }
  const { validLabel } = ModuleData;
  function validActivation(value) {
    return /^LPA:1\$[a-z0-9.-]+\$[^\s$]+(?:\$[^\s$]*)?(?:\$1)?$/.test(value.trim());
  }
  document.addEventListener("click", (event) => {
    if (!module || !event.target.closest("#dialog-content")) return;
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
    const operator = event.target.closest("[data-network-index]");
    if (operator) {
      if (!networkEditable(module) || module.job?.action !== "network-scan" || module.job.state !== "succeeded") return;
      const op = module.job.networks?.[Number(operator.dataset.networkIndex)];
      if (op && op.status !== 3) control(module, "updateLine", { networkAutomatic: false, operator: op.plmn, accessTechnology: op.technology }, active().id);
      return;
    }
    if (event.target.closest("[data-network-scan]")) { control(module, "scanNetworks", {}, active()?.id); return; }
    const action = event.target.closest("[data-cellular-action]");
    if (action) {
      active();
      const sim = lines(module)[activeLine];
      if (module.managed && action.dataset.cellularAction === "network") {
        if (networkEditable(module, sim)) { manualNetwork = false; showNetwork(); }
        return;
      }
      if (module.managed && (!editable(module) || !sim?.esim || action.dataset.cellularAction !== "label")) return;
      if (!sim) return;
      if (action.dataset.cellularAction === "label") {
        view = "form";
        UI.modal(
          "号码标签",
          `<section class="cellular-page">${back(true)}<form id="cellular-label-form" class="settings-form" novalidate><div class="form-fields">${Forms.field({ prefix: "cellular", name: "label", label: "标签", value: sim.label, placeholder: "主号 / 副号", maxLength: 20, required: true })}</div><div class="form-footer"><button type="submit" class="primary">完成</button></div></form></section>`,
        );
        document.getElementById("cellular-label").focus();
      } else if (action.dataset.cellularAction === "wifi" && sim.enabled) {
        view = "form";
        UI.modal(
          "Wi-Fi 通话",
          `<section class="cellular-page">${back(true)}<div class="cellular-group cellular-settings">${switchRow("wifiCalling", "在此号码上使用 Wi-Fi 通话", sim.wifiCalling)}</div></section>`,
        );
      } else if (action.dataset.cellularAction === "network" && sim.enabled) {
        view = "form";
        UI.modal(
          "网络选择",
          `<section class="cellular-page">${back(true)}<div class="cellular-group cellular-settings">${switchRow("networkAutomatic", "自动", sim.networkAutomatic)}</div><p id="cellular-network-empty" class="cellular-empty" ${sim.networkAutomatic ? "hidden" : ""}>运营商服务尚未接入</p></section>`,
        );
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
      !["enabled", "wifiCalling", "roaming", "networkAutomatic"].includes(key)
    )
      return;
    const sim = active();
    if (!sim || (key !== "enabled" && !sim.enabled)) return;
    if (module.managed) {
      const enabled = input.checked;
      input.checked = Boolean(sim[key]);
      if (key === "networkAutomatic" && networkEditable(module, sim)) {
        manualNetwork = !enabled;
        if (enabled) control(module, "updateLine", { networkAutomatic: true }, sim.id);
        else control(module, "scanNetworks", {}, sim.id);
        refreshDialog(); return;
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
          '[data-cellular-action="wifi"], [data-cellular-setting="roaming"], [data-cellular-action="network"]',
        )
        .forEach((control) => {
          control.disabled = !sim.enabled;
        });
    } else if (key === "networkAutomatic") {
      document.getElementById("cellular-network-empty").hidden =
        sim.networkAutomatic;
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
    if (!module?.managed) { UI.toast("eSIM 服务尚未接入，未添加"); return; }
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
    unsubscribe?.(); unsubscribe = null;
    module = null;
    activeLine = -1; activeID = null;
    const form = document.getElementById("esim-form");
    form?.reset();
  });
  return { open, overview, detail, lines, validActivation, validLabel };
})();
