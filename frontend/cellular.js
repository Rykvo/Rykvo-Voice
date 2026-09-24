const Cellular = (() => {
  let module = null;
  let activeLine = -1;
  const simIcon =
    '<svg viewBox="0 0 28 32" aria-hidden="true"><path d="M6 2h12l6 6v21H4V4a2 2 0 0 1 2-2Z"/><rect x="8" y="13" width="12" height="11" rx="2"/><path d="M12 13v11m4-11v11M8 18.5h12"/></svg>';
  function lines(item) {
    return (item.sims || []).map((sim, index) => ({
      ...sim,
      networkAutomatic: sim.networkAutomatic !== false,
      wifiCalling: sim.wifiCalling === true,
      roaming: sim.roaming === true,
      number: index === 0 ? item.number : sim.number,
      state: !sim.enabled
        ? "已关闭"
        : item.status === "online"
          ? "已启用"
          : "无服务",
    }));
  }
  function back(toDetail = false) {
    const label = toDetail ? lines(module)[activeLine].label : "蜂窝网络";
    return `<button type="button" class="text-button cellular-back" data-cellular-back="${toDetail ? "detail" : "overview"}">‹ ${UI.escape(label)}</button>`;
  }
  function overview(item) {
    const cards = lines(item);
    return `<section class="cellular-page"><p class="cellular-module">${UI.escape(item.label || item.name)}</p><div class="cellular-section-title"><h3>SIM</h3><span>${cards.length} 张</span></div><div class="cellular-group">${cards.length ? cards.map((sim, index) => `<button type="button" class="cellular-sim" data-cellular-sim="${index}"><span class="cellular-sim-icon">${simIcon}</span><span class="cellular-sim-copy"><strong>${UI.escape(sim.label)}</strong><small>${UI.escape(Countries.format(sim.number))}</small></span><span class="cellular-sim-meta">${UI.escape(sim.state)}</span><span class="chevron" aria-hidden="true">›</span></button>`).join("") : '<p class="cellular-empty">无 SIM 卡</p>'}</div><button type="button" class="cellular-add" data-cellular-add><span aria-hidden="true">＋</span>添加 eSIM<span class="chevron" aria-hidden="true">›</span></button></section>`;
  }
  function open(item) {
    module = item;
    activeLine = -1;
    UI.modal("蜂窝网络", overview(item));
  }
  function switchRow(key, label, checked, disabled = false) {
    return `<label class="cellular-setting"><span>${label}</span><span class="form-switch"><input type="checkbox" role="switch" data-cellular-setting="${key}" aria-label="${label}" ${checked ? "checked" : ""} ${disabled ? "disabled" : ""}><span aria-hidden="true"></span></span></label>`;
  }
  function valueRow(label, value, action = "", disabled = false) {
    const content = `<span>${label}</span><span class="cellular-setting-value">${UI.escape(value)}</span>${action ? '<span class="chevron" aria-hidden="true">›</span>' : ""}`;
    return action
      ? `<button type="button" class="cellular-setting" data-cellular-action="${action}" ${disabled ? "disabled" : ""}>${content}</button>`
      : `<div class="cellular-setting">${content}</div>`;
  }
  function detail(item, index) {
    const sim = lines(item)[index];
    if (!sim) return "";
    return `<section class="cellular-page">${back()}<div class="cellular-group cellular-settings">${valueRow("号码标签", sim.label, "label")}${switchRow("enabled", "启用此号码", sim.enabled)}</div><div class="cellular-group cellular-settings">${valueRow("网络选择", sim.networkAutomatic ? "自动" : "手动", "network", !sim.enabled)}${valueRow("本机号码", Countries.format(sim.number))}${switchRow("wifiCalling", "Wi-Fi 通话", sim.wifiCalling, !sim.enabled)}${switchRow("roaming", "数据漫游", sim.roaming, !sim.enabled)}</div></section>`;
  }
  function showDetail() {
    const sim = lines(module)[activeLine];
    if (sim) UI.modal(sim.label, detail(module, activeLine));
  }
  const { validLabel } = ModuleData;
  function validActivation(value) {
    return /^LPA:1\$[a-z0-9.-]+\$[^\s$]+(?:\$[^\s$]*)?$/i.test(value.trim());
  }
  document.addEventListener("click", (event) => {
    if (!module || !event.target.closest("#dialog-content")) return;
    const backButton = event.target.closest("[data-cellular-back]");
    if (backButton) {
      if (backButton.dataset.cellularBack === "detail") showDetail();
      else open(module);
      return;
    }
    const action = event.target.closest("[data-cellular-action]");
    if (action) {
      const sim = lines(module)[activeLine];
      if (!sim) return;
      if (action.dataset.cellularAction === "label") {
        UI.modal(
          "号码标签",
          `<section class="cellular-page">${back(true)}<form id="cellular-label-form" class="settings-form" novalidate><div class="form-fields">${Forms.field({ prefix: "cellular", name: "label", label: "标签", value: sim.label, placeholder: "主号 / 副号", maxLength: 20, required: true })}</div><div class="form-footer"><button type="submit" class="primary">完成</button></div></form></section>`,
        );
        document.getElementById("cellular-label").focus();
      } else if (action.dataset.cellularAction === "network" && sim.enabled) {
        UI.modal(
          "网络选择",
          `<section class="cellular-page">${back(true)}<div class="cellular-group cellular-settings">${switchRow("networkAutomatic", "自动", sim.networkAutomatic)}</div><p id="cellular-network-empty" class="cellular-empty" ${sim.networkAutomatic ? "hidden" : ""}>运营商服务尚未接入</p></section>`,
        );
      }
      return;
    }
    if (event.target.closest("[data-cellular-add]")) {
      UI.modal(
        "添加 eSIM",
        `<section class="cellular-page">${back()}<form id="esim-form" class="settings-form" novalidate><div class="form-fields">${Forms.field({ prefix: "esim", name: "activation", label: "激活码", type: "password", placeholder: "LPA:1$…", maxLength: 2048, required: true })}</div><div class="form-footer"><button type="submit" class="primary">添加 eSIM</button></div></form></section>`,
      );
      document.getElementById("esim-activation").focus();
      return;
    }
    const button = event.target.closest("[data-cellular-sim]");
    if (!button) return;
    const index = Number(button.dataset.cellularSim);
    const sim = lines(module)[index];
    if (!sim) return;
    activeLine = index;
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
    const sim = module.sims[activeLine];
    if (!sim || (key !== "enabled" && !sim.enabled)) return;
    // 仅更新本次页面的样本，设备配置由后端接入后执行。
    sim[key] = input.checked;
    if (key === "enabled") {
      document
        .querySelectorAll(
          '[data-cellular-setting="wifiCalling"], [data-cellular-setting="roaming"], [data-cellular-action="network"]',
        )
        .forEach((control) => {
          control.disabled = !sim.enabled;
        });
    } else if (key === "networkAutomatic") {
      document.getElementById("cellular-network-empty").hidden =
        sim.networkAutomatic;
    }
  });
  document.addEventListener("submit", (event) => {
    if (event.target.id === "cellular-label-form") {
      event.preventDefault();
      if (!module || !module.sims[activeLine]) return;
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
    // 安装由模块后端执行，激活码不缓存、不记录。
    UI.toast("eSIM 服务尚未接入，未添加");
  });
  document.getElementById("dialog").addEventListener("close", () => {
    module = null;
    activeLine = -1;
    const form = document.getElementById("esim-form");
    form?.reset();
  });
  return { open, overview, detail, lines, validActivation, validLabel };
})();
