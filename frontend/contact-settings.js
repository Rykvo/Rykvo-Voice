const ContactSettings = (() => {
  function render() {
    return `<form id="contact-form" class="settings-form" novalidate><div class="form-fields">${Forms.field({ prefix: "contact", name: "url", label: "联系链接", type: "url", placeholder: "https://", maxLength: 2048 })}</div><div class="settings-feedback"><p id="contact-error" class="host-form-error" data-settings-error role="alert" hidden></p><button type="button" class="text-button" data-settings-retry hidden>重试</button></div><div class="host-form-actions"><button type="button" class="text-button" data-action="general-back">取消</button><button type="submit" class="primary" disabled>保存</button></div></form>`;
  }
  const editor = SettingsForm.create({
    selector: "#contact-form", api: Backend.contact,
    fields: [{ name: "url", normalize: value => value.trim(), valid: ContactLink.valid, invalid: "请输入有效的联系链接" }],
  });
  return { render, ...editor };
})();
