const Alerts = (() => {
  const names = { sip: "SIP 电话", sms: "短信", mms: "彩信", module: "异常" };
  const limits = { sip: "SIP 电话", message: "短信／彩信", module: "异常" };
  let request = null;
  function active(item) { return (item.alerts || []).filter(a => a.active); }
  function sim(item) { return active(item).some(a => a.kind !== "module"); }
  function details(item) {
    const rows = active(item);
    if (!rows.length) return "";
    return `<div class="cellular-group alert-details">${rows.map(a => `<div><strong>${UI.escape(names[a.kind] || "异常")} · 连续${a.kind === "module" ? "检测" : "失败"} ${a.failures} 次</strong><p>${UI.escape(a.description || "原因待确认")}</p></div>`).join("")}</div>`;
  }
  function badge(item) {
    const rows = active(item).filter(a => a.kind !== "module");
    return rows.length ? `<span class="module-sim-alert" title="${UI.escape(rows.map(a => `${names[a.kind]}：连续 ${a.failures} 次`).join("；"))}">SIM 异常</span>` : "";
  }
  function validate(values) {
    for (const field of Object.keys(limits)) {
      if (!/^\d{1,3}$/.test(String(values[field])) || +values[field] < 0 || +values[field] > 100)
        return { field, message: "请填写 0–100 的整数" };
    }
    return null;
  }
  async function open() {
    request?.abort();
    const controller = request = new AbortController();
    UI.modal("异常设置", '<p role="status" class="cellular-empty">正在读取</p>');
    const dialog = document.getElementById("dialog");
    dialog.addEventListener("close", () => controller.abort(), { once: true });
    try {
      const data = await Backend.alerts.get({ signal: controller.signal });
      if (controller.signal.aborted || !dialog.open || request !== controller) return;
      UI.modal("异常设置", `<form id="alert-settings-form" class="settings-form alert-settings-form" data-revision="${data.revision}" aria-describedby="alert-limit-hint" novalidate><div class="form-fields">${Object.entries(limits).map(([name, label]) => Forms.field({ prefix: "alert", name, label, value: data[name], inputmode: "numeric", maxLength: 3 })).join("")}</div><p id="alert-limit-hint">0 为关闭</p><div class="host-form-actions"><button type="button" class="text-button" data-alert-cancel>取消</button><button type="submit" class="primary">保存</button></div></form>`);
    } catch (error) {
      if (!controller.signal.aborted) {
        dialog.close();
        UI.toast("读取失败，请重试");
      }
    }
  }
  document.addEventListener("click", event => {
    if (event.target.closest("[data-alert-settings]")) open();
    if (event.target.closest("[data-alert-cancel]")) document.getElementById("dialog").close();
  });
  document.addEventListener("submit", async event => {
    const form = event.target;
    if (form.id !== "alert-settings-form") return;
    event.preventDefault();
    const values = Object.fromEntries(new FormData(form));
    if (Forms.report(form, validate(values))) return;
    const button = form.querySelector('[type="submit"]');
    if (button.disabled) return;
    button.disabled = true;
    try {
      await Backend.alerts.update({ body: { ...Object.fromEntries(Object.keys(limits).map(k => [k, +values[k]])), revision: +form.dataset.revision } });
      if (form.isConnected) document.getElementById("dialog").close();
      UI.toast("已保存");
    } catch (error) {
      UI.toast(error.code === "SETTINGS_CHANGED" ? "设置已更新，请重新打开" : "保存失败，请重试");
    } finally { button.disabled = false; }
  });
  return { active, sim, details, badge, validate };
})();
