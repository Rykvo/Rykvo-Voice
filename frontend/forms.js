const Forms = (() => {
  function icon(name, className = "row-icon") {
    const file = name.includes(".") ? name : `${name}.png`;
    return `<img class="${UI.escape(className)}" src="assets/${UI.escape(file)}" alt="">`;
  }
  function header(title, iconName) {
    return `<div class="form-toolbar"><button type="button" class="text-button form-back" data-action="general-back">‹ 通用</button></div><header class="form-header"><span class="form-emblem" aria-hidden="true">${icon(iconName, "")}</span><h1>${UI.escape(title)}</h1></header>`;
  }
  function field({
    prefix,
    name,
    label,
    type = "text",
    placeholder = "",
    autocomplete = "off",
    inputmode,
    value,
    maxLength = 128,
    required = false,
  }) {
    const id = `${prefix}-${name}`;
    return `<div class="form-field"><label for="${id}">${UI.escape(label)}</label><div class="form-input"><input id="${id}" name="${name}" type="${type}" placeholder="${UI.escape(placeholder)}" autocomplete="${autocomplete}" ${inputmode ? `inputmode="${UI.escape(inputmode)}"` : ""} ${required ? "required" : ""} ${value === undefined ? "" : `value="${UI.escape(value)}"`} maxlength="${maxLength}" autocapitalize="none" spellcheck="false">${type === "password" ? `<button type="button" class="password-toggle" data-secret-toggle="${id}" aria-label="显示${UI.escape(label)}" aria-pressed="false"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M2 12s3.5-6 10-6 10 6 10 6-3.5 6-10 6-10-6-10-6Z"/><circle cx="12" cy="12" r="3"/><path class="eye-slash" d="m4 4 16 16"/></svg></button>` : ""}</div></div>`;
  }
  function report(form, error) {
    if (!error) return false;
    const input = form.elements.namedItem(error.field);
    input.setAttribute("aria-invalid", "true");
    input.focus();
    UI.toast(error.message);
    return true;
  }
  document.addEventListener("input", (event) => {
    if (event.target.closest(".settings-form"))
      event.target.removeAttribute("aria-invalid");
  });
  return { icon, header, field, report };
})();
