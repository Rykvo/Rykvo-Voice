const Auth = (() => {
  let authenticated = false,
    started = false;
  async function start(onReady) {
    if (started) return;
    started = true;
    try {
      const session = await Backend.session.get();
      if (!session?.user?.id || !session.csrfToken)
        throw new Error("Invalid session");
      Backend.setCSRFToken(session.csrfToken);
      authenticated = true;
      onReady();
      document.querySelector("#app-window").hidden = false;
    } catch (error) {
      if (error.status === 401) location.replace(Http.home);
      else UI.toast("连接失败，请刷新重试");
    }
  }
  async function logout(button) {
    if (button.disabled) return;
    button.disabled = true;
    try {
      await Backend.session.logout();
      Backend.setCSRFToken("");
      location.replace(Http.home);
    } catch (error) {
      if (error.status === 401) location.replace(Http.home);
      else UI.toast("退出失败，请重试");
    } finally {
      button.disabled = false;
    }
  }
  return {
    start,
    logout,
    get authenticated() {
      return authenticated;
    },
  };
})();
