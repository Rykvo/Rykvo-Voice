(() => {
  let started = false;
  let authenticated = false;
  function validSession(session) {
    return (
      typeof session?.user?.id === "string" &&
      session.user.id.trim() !== "" &&
      typeof session.csrfToken === "string" &&
      session.csrfToken.trim() !== ""
    );
  }
  async function start() {
    if (started) return;
    started = true;
    const screen = document.querySelector("#login-screen");
    const form = document.querySelector("#login-form");
    const username = form.elements.namedItem("username");
    const password = form.elements.namedItem("password");
    const submit = document.querySelector("#login-submit");
    const status = document.querySelector("#login-status");
    let busy = false;
    for (const input of [username, password])
      input.setAttribute("aria-describedby", "login-status");
    function message(text = "", error = false) {
      status.textContent = text;
      status.dataset.error = String(error);
    }
    function pending(value, label = "登录") {
      busy = value;
      form.setAttribute("aria-busy", String(value));
      username.disabled = password.disabled = submit.disabled = value;
      submit.textContent = label;
    }
    function accept(session) {
      if (!validSession(session)) throw { code: "INVALID_RESPONSE" };
      password.value = "";
      authenticated = true;
      location.replace(Http.home);
    }
    function failed(error, restoring = false) {
      if (restoring && error.status === 401) return message();
      const text =
        error.status === 401
          ? "账号或密码不正确"
          : error.status === 429
            ? "尝试过多，请稍后重试"
            : error.code === "NOT_CONNECTED"
              ? "登录服务尚未连接"
              : error.code === "INVALID_RESPONSE"
                ? "登录服务响应异常"
                : "连接失败，请重试";
      message(text, true);
    }
    form.addEventListener("input", (event) => {
      event.target.removeAttribute("aria-invalid");
      if (!busy) message();
    });
    form.addEventListener("submit", async (event) => {
      event.preventDefault();
      if (busy || authenticated) return;
      const account = username.value.trim();
      const secret = password.value;
      const missing = !account ? username : !secret ? password : null;
      if (missing) {
        message(missing === username ? "请输入账号" : "请输入密码", true);
        missing.setAttribute("aria-invalid", "true");
        missing.focus();
        return;
      }
      message();
      pending(true, "正在登录…");
      try {
        accept(
          await Http.request("POST", "/session", {
            body: { username: account, password: secret },
          }),
        );
      } catch (error) {
        failed(error);
      } finally {
        pending(false);
      }
    });
    pending(true, "正在连接…");
    try {
      accept(await Http.request("GET", "/session"));
    } catch (error) {
      failed(error, true);
      screen.hidden = false;
    } finally {
      pending(false);
    }
  }
  try {
    const preferences =
      JSON.parse(localStorage.getItem("rykvo-voice-v1")) || {};
    document.body.classList.toggle("theme-dark", preferences.theme === "dark");
    document.body.classList.toggle(
      "reduce-motion",
      preferences.motion === true,
    );
  } catch {}
  start();
})();
