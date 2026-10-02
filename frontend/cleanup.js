const Cleanup = (() => {
  const valid = value => Number.isSafeInteger(value) && value >= 0 && value <= 36500;
  const validHours = value => Number.isSafeInteger(value) && value >= 1 && value <= 8760;
  const normalize = value => /^\d+$/.test(String(value).trim()) ? Number(value) : NaN;
  function render() {
    return `<form id="cleanup-form" class="settings-form" novalidate>
      <div class="form-fields">
        <div class="cleanup-row"><label class="cleanup-label" for="cleanup-hours">临时附件<small>上传暂存的图片</small></label><input id="cleanup-hours" name="uploadHours" type="number" inputmode="numeric" min="1" max="8760" step="1" disabled aria-describedby="cleanup-error"><span>小时</span></div>
        <div class="cleanup-row"><label class="cleanup-label" for="cleanup-days">历史记录<small>短信、彩信、通话与通知</small></label><input id="cleanup-days" name="days" type="number" inputmode="numeric" min="0" max="36500" step="1" disabled aria-describedby="cleanup-error"><span>天</span></div>
      </div>
      <div class="settings-feedback"><p id="cleanup-error" class="host-form-error" data-settings-error role="alert" hidden></p><button type="button" class="text-button" id="cleanup-retry" data-settings-retry hidden>重试</button></div>
      <div class="host-form-actions"><button type="button" class="text-button" data-action="general-back">取消</button><button type="submit" class="primary" disabled>保存</button></div>
    </form>`;
  }
  const editor = SettingsForm.create({
    selector: "#cleanup-form", api: Backend.retention, issueMessage: "清理暂未完成，后台将重试",
    fields: [
      { name: "uploadHours", normalize, valid: validHours, invalid: "临时附件请输入 1–8760 的整数" },
      { name: "days", normalize, valid, invalid: "历史记录请输入 0–36500 的整数" },
    ],
  });
  return { valid, validHours, render, ...editor };
})();
