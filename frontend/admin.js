const Admin = (() => {
  const fields = [
    ["account", "账号", "text", "留空不修改", "username"],
    [
      "currentPassword",
      "当前密码",
      "password",
      "输入当前密码",
      "current-password",
    ],
    ["newPassword", "新密码", "password", "留空不修改", "new-password"],
    [
      "confirmPassword",
      "确认新密码",
      "password",
      "再次输入新密码",
      "new-password",
    ],
  ];
  function render() {
    return `<section class="form-page">${Forms.header("管理员", "privacy.svg")}<form id="admin-form" class="settings-form" novalidate><div class="form-fields">${fields.map(([name, label, type, placeholder, autocomplete]) => Forms.field({ prefix: "admin", name, label, type, placeholder, autocomplete, required: name === "currentPassword", maxLength: name === "account" ? 64 : 128 })).join("")}</div><div class="form-footer"><button class="primary" type="submit">保存修改</button></div></form></section>`;
  }
  function validate(values) {
    if (
      !values.account.trim() &&
      !values.newPassword &&
      !values.confirmPassword
    )
      return { field: "account", message: "请填写新账号或新密码" };
    if (!values.currentPassword)
      return { field: "currentPassword", message: "请输入当前密码" };
    if (values.newPassword && values.newPassword.length < 8)
      return { field: "newPassword", message: "新密码至少需要 8 位" };
    if (values.newPassword !== values.confirmPassword)
      return { field: "confirmPassword", message: "两次输入的新密码不一致" };
    if (values.newPassword && values.newPassword === values.currentPassword)
      return { field: "newPassword", message: "新密码需与当前密码不同" };
    return null;
  }
  document.addEventListener("submit", async (event) => {
    if (event.target.id !== "admin-form") return;
    event.preventDefault();
    const form = event.target;
    if (form.dataset.busy === "true") return;
    const values = Object.fromEntries(new FormData(form));
    if (Forms.report(event.target, validate(values))) return;
    form.dataset.busy = "true";
    const button = form.querySelector('[type="submit"]');
    button.disabled = true;
    try {
      await Backend.administrator.update({ body: values });
      form.reset();
      location.replace(Http.home);
    } catch (error) {
      if (error.status === 401) location.replace(Http.home);
      else if (error.code === "INVALID_PASSWORD")
        Forms.report(form, {
          field: "currentPassword",
          message: "当前密码不正确",
        });
      else
        UI.toast(
          error.status === 429 ? "尝试过多，请稍后重试" : "保存失败，请重试",
        );
    } finally {
      form.dataset.busy = "false";
      button.disabled = false;
    }
  });
  return { render, validate };
})();
