const Cleanup = (() => {
  const key = "rykvo-voice-retention-days-v1";
  let timer;
  function valid(value) {
    return Number.isSafeInteger(value) && value >= 0 && value <= 36500;
  }
  function days() {
    const value = UI.read(key, 0);
    return valid(value) ? value : 0;
  }
  function expired(record, cutoff) {
    return Number.isFinite(record?.at) && record.at < cutoff;
  }
  function run(now = Date.now()) {
    const value = days();
    if (!value) return true;
    const cutoff = now - value * 86400000;
    const calls = Phone.prune(cutoff);
    const messages = Messages.prune(cutoff);
    SIPHistory.update();
    return calls && messages;
  }
  function schedule() {
    clearTimeout(timer);
    if (!days() || document.hidden) return;
    // 仅启用且页面可见时，每小时检查一次。
    timer = setTimeout(() => {
      run();
      schedule();
    }, 3600000);
  }
  function open() {
    UI.modal(
      "自动清理",
      `<form id="cleanup-form" novalidate><div class="cleanup-days"><label for="cleanup-days">保留天数</label><input id="cleanup-days" type="number" inputmode="numeric" min="0" max="36500" step="1" value="${days()}" aria-describedby="cleanup-note cleanup-error"><span>天</span></div><p id="cleanup-note">0 为关闭</p><p id="cleanup-error" role="alert" hidden></p><div class="cleanup-actions"><button type="button" class="text-button" data-cleanup-cancel>取消</button><button type="submit" class="primary">保存</button></div></form>`,
    );
  }
  function start() {
    run();
    schedule();
    document.addEventListener("visibilitychange", () => {
      if (!document.hidden) run();
      schedule();
    });
    document.addEventListener("click", (event) => {
      if (event.target.closest("[data-cleanup-cancel]"))
        UI.$("#dialog").close();
    });
    document.addEventListener("submit", (event) => {
      if (event.target.id !== "cleanup-form") return;
      event.preventDefault();
      const input = UI.$("#cleanup-days");
      const raw = input.value.trim();
      const value = Number(raw);
      const error = UI.$("#cleanup-error");
      if (!/^\d+$/.test(raw) || !valid(value)) {
        error.textContent = "请输入 0–36500 的整数";
        error.hidden = false;
        input.setAttribute("aria-invalid", "true");
        input.focus();
        return;
      }
      error.hidden = true;
      input.removeAttribute("aria-invalid");
      if (!UI.write(key, value)) return;
      const complete = run();
      schedule();
      UI.$("#dialog").close();
      UI.toast(
        !complete
          ? "规则已保存，部分记录清理失败，将重试"
          : value
            ? "自动清理已开启"
            : "自动清理已关闭",
      );
    });
  }
  return { valid, days, expired, run, open, start };
})();
