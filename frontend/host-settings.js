const HostSettings = (() => {
  let selected = "hostname", controller = null;
  const tabs = [["hostname", "主机名"], ["cleanup", "自动清理"], ["contact", "联系我们"]];
  const errors = {
    INVALID_HOSTNAME: "主机名格式不支持，请重新填写。",
    HOSTNAME_BUSY: "正在保存，请稍后重试",
    HOSTNAME_CONFLICT: "主机名已被修改，请重新进入",
    HOST_NETWORK_UNSUPPORTED: "当前网络配置暂不支持修改主机名",
    HOSTNAME_ROLLBACK_FAILED: "保存未完成，请检查主机配置",
  };
  function valid(value) {
    return /^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$/.test(value) &&
      /[A-Za-z]/.test(value) && value.toLowerCase() !== "localhost";
  }
  function content() {
    if (selected === "cleanup") return Cleanup.render();
    if (selected === "contact") return ContactSettings.render();
    return '<form id="hostname-form" class="settings-form" novalidate><div class="form-fields">' +
      Forms.field({ prefix: "host", name: "hostname", label: "主机名", labelHidden: true, maxLength: 63, placeholder: "输入主机名", required: true }) +
      '</div><p id="hostname-error" class="host-form-error" role="alert"></p><div class="host-form-actions"><button type="button" class="text-button" data-action="general-back">取消</button><button type="submit" class="primary" disabled>保存</button></div></form>';
  }
  function render() {
    selected = "hostname";
    return '<section class="form-page host-settings-page">' + Forms.header("主机设置", "settings.png") +
      Forms.tabs("主机设置类型", tabs, selected, "host-tab") + '<div id="host-settings-panel">' + content() + '</div></section>';
  }
  async function mount() {
    if (selected === "cleanup") return Cleanup.mount();
    if (selected === "contact") return ContactSettings.mount();
    unmount();
    const form = UI.$("#hostname-form");
    if (!form) return;
    const request = new AbortController();
    controller = request;
    const input = form.elements.namedItem("hostname");
    const button = form.querySelector('[type="submit"]');
    const error = form.querySelector("#hostname-error");
    input.setAttribute("aria-describedby", "hostname-error");
    input.disabled = button.disabled = true;
    input.placeholder = "正在读取…";
    let original = "", ready = false, busy = false;
    const active = () => controller === request && !request.signal.aborted && form.isConnected;
    form.addEventListener("input", () => {
      input.removeAttribute("aria-invalid");
      if (error.textContent === errors.INVALID_HOSTNAME) error.textContent = "";
    }, { signal: request.signal });
    form.addEventListener("submit", async (event) => {
      event.preventDefault();
      if (!ready || busy || !active()) return;
      const value = input.value.trim();
      if (!valid(value)) {
        error.textContent = errors.INVALID_HOSTNAME;
        input.setAttribute("aria-invalid", "true");
        input.focus();
        return;
      }
      busy = true;
      button.disabled = input.disabled = true;
      button.textContent = "保存中…";
      error.textContent = "";
      try {
        const result = await Backend.hostname.update({ body: { hostname: value, expected: original }, signal: request.signal });
        if (result?.hostname !== value) throw new Error("INVALID_RESPONSE");
        if (!active()) return;
        UI.toast(value === original ? "主机名未更改" : "已保存");
        input.value = original = value;
        input.removeAttribute("aria-invalid");
      } catch (failure) {
        if (active()) error.textContent = errors[failure.code] || "保存未确认，请重新进入检查";
      } finally {
        busy = false;
        if (active()) {
          button.disabled = input.disabled = false;
          button.textContent = "保存";
        }
      }
    }, { signal: request.signal });
    try {
      const result = await Backend.hostname.get({ signal: request.signal });
      if (!active()) return;
      if (typeof result?.hostname !== "string") throw new Error("INVALID_RESPONSE");
      input.value = original = result.hostname;
      if (!result.editable) {
        error.textContent = errors.HOST_NETWORK_UNSUPPORTED;
        return;
      }
      ready = true;
      button.disabled = input.disabled = false;
    } catch {
      if (active()) error.textContent = "读取失败，请重新进入重试";
    } finally {
      if (active()) input.placeholder = "输入主机名";
    }
  }
  function unmount() {
    Cleanup.unmount();
    ContactSettings.unmount();
    controller?.abort();
    controller = null;
  }
  document.addEventListener("click", (event) => {
    const action = event.target.closest("[data-host-tab]")?.dataset.hostTab;
    const panel = UI.$("#host-settings-panel");
    if (!panel || !tabs.some(([id]) => id === action) || action === selected) return;
    unmount();
    selected = action;
    document.querySelectorAll("[data-host-tab]").forEach(button => {
      button.setAttribute("aria-pressed", String(button.dataset.hostTab === selected));
    });
    panel.innerHTML = content();
    mount();
  });
  return { render, mount, unmount, valid };
})();
