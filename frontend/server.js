const Server = (() => {
  const labels = {
    disconnected: ["未连接", "连接"],
    authorizing: ["等待授权", "授权"],
    connecting: ["连接中", "连接"],
    connected: ["已连接", "注销"],
    disconnecting: ["正在注销", "注销中"],
    failed: ["连接失败", "重试"],
  };
  const errors = {
    AUTH_REQUIRED: "请重新授权",
    AUTH_INVALID: "授权无效，请注销后重试",
    AUTH_EXPIRED: "授权已过期，请重试",
    AUTH_FAILED: "授权失败，请重试",
    DOMAIN_NOT_AUTHORIZED: "授权域名不匹配，请注销后重新授权",
    DNS_CONFLICT: "域名已有 DNS 记录，请更换域名或先在 Cloudflare 处理",
    CLOUDFLARE_PERMISSION: "Cloudflare 权限不足，请检查授权",
    CLOUDFLARE_UNAVAILABLE: "Cloudflare 连接失败，请重试",
    CLOUDFLARE_RATE_LIMITED: "Cloudflare 请求频繁，请稍后重试",
    CLOUDFLARE_REQUEST_FAILED: "Cloudflare 请求失败，请重试",
    CLOUDFLARE_RESPONSE_INVALID: "Cloudflare 响应异常，请重试",
    CREDENTIALS_INVALID: "连接凭据异常，请注销后重试",
    TUNNEL_START_FAILED: "隧道启动失败，请重试",
    TUNNEL_NOT_CONFIGURED: "隧道服务尚未配置",
    TUNNEL_UNAVAILABLE: "隧道服务暂时不可用",
    TUNNEL_OWNER_REQUIRED: "请使用发起连接的管理员账号",
    DISCONNECT_FIRST: "请先注销当前连接",
    DISCONNECT_FAILED: "注销未完成，请重试",
    RETRY_REQUIRED: "连接已中断，请重试",
    INVALID_DOMAIN: "请输入有效域名",
    DATABASE_UNAVAILABLE: "状态保存失败，请重试",
    RESOURCE_NOT_FOUND: "Cloudflare 资源不存在，请注销后重试",
    RESOURCE_OWNERSHIP_MISMATCH: "资源归属异常，请检查 Cloudflare 配置",
  };
  const cleanupErrors = {
    TUNNEL_CONNECTIONS_ACTIVE: "连接仍在释放，请稍后重试",
    CLOUDFLARE_PERMISSION: "Cloudflare 授权不足，请检查授权",
    CLOUDFLARE_UNAVAILABLE: "Cloudflare 暂时不可用，请重试",
    CLOUDFLARE_RATE_LIMITED: "Cloudflare 请求频繁，请稍后重试",
    AUTH_REQUIRED: "授权文件缺失，请检查服务配置",
    AUTH_INVALID: "授权文件异常，请检查服务配置",
    RESOURCE_OWNERSHIP_MISMATCH: "资源归属异常，请检查 Cloudflare 配置",
    DATABASE_UNAVAILABLE: "状态保存失败，请重试",
    FILE_PERMISSION: "清理权限不足，请检查服务配置",
    CLEANUP_TIMEOUT: "注销超时，请重试",
  };
  const pending = () => ["connecting", "disconnecting"].includes(state.status);
  const cleanupFailed = () => state.errorCode === "DISCONNECT_FAILED";
  const failureMessage = (error) => errors[error.code] || error.message;
  let state = { status: "disconnected", domain: "" };
  let form,
    controller,
    timer,
    busy = false,
    loaded = false,
    refreshFailures = 0;
  const enabled = () => Backend.enabled("tunnel");

  function normalizeDomain(value) {
    const text = value.trim().replace(/\.$/, "");
    if (!text || /[\s/:@?#\\]/.test(text)) return "";
    try {
      const host = new URL(`https://${text}`).hostname;
      const parts = host.split(".");
      return host.length <= 253 &&
        parts.length > 1 &&
        ![
          "localhost",
          "local",
          "internal",
          "test",
          "invalid",
          "example",
        ].includes(parts.at(-1)) &&
        !/^\d+(\.\d+){3}$/.test(host) &&
        parts.every((part) =>
          /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/i.test(part),
        )
        ? host
        : "";
    } catch {
      return "";
    }
  }
  function authURL(value) {
    try {
      const url = new URL(value);
      return url.protocol === "https:" &&
        url.hostname === "dash.cloudflare.com" &&
        !url.username &&
        !url.password &&
        !url.port &&
        url.pathname === "/argotunnel" &&
        Boolean(url.search)
        ? url.href
        : "";
    } catch {
      return "";
    }
  }
  function parseState(value) {
    if (!value || !Object.hasOwn(labels, value.status))
      throw Error("连接状态异常，请重试");
    const domain = normalizeDomain(value.domain || "");
    if (value.status !== "disconnected" && !domain)
      throw Error("连接状态异常，请重试");
    const authorizationUrl = authURL(value.authorizationUrl);
    if (value.status === "authorizing" && !authorizationUrl)
      throw Error("授权地址异常，请重试");
    return {
      status: value.status,
      domain,
      authorizationUrl,
      errorCode: typeof value.errorCode === "string" ? value.errorCode : "",
      cleanupCode:
        typeof value.cleanupCode === "string" ? value.cleanupCode : "",
    };
  }
  function render() {
    return `<section class="form-page server-page">${Forms.header("云服务器", "cloud.svg")}<form id="server-form" class="settings-form" novalidate><div class="form-fields">${Forms.field({ prefix: "server", name: "domain", label: "域名", placeholder: "panel.example.com", maxLength: 253, required: true })}<div class="server-status-row"><span>状态</span><span id="server-status" class="server-status" role="status" aria-live="polite"><i aria-hidden="true"></i><span>未连接</span></span></div></div><p id="server-error" class="server-error" role="alert" hidden></p><div class="form-footer"><button id="server-button" class="primary" type="submit">连接</button><button id="server-reset" type="button" class="text-button" hidden>取消连接</button></div></form></section>`;
  }
  function update(message) {
    if (!form) return;
    const [status, action] = labels[state.status];
    const input = form.elements.namedItem("domain");
    const button = form.querySelector("#server-button");
    input.readOnly = busy || state.status !== "disconnected";
    form.setAttribute("aria-busy", String(busy));
    button.textContent = cleanupFailed() ? "重试注销" : action;
    button.disabled = busy || (loaded && pending());
    button.classList.toggle("server-disconnect", state.status === "connected");
    button.classList.toggle("is-pending", busy || pending());
    const reset = form.querySelector("#server-reset");
    reset.hidden =
      !["authorizing", "connecting", "failed"].includes(state.status) ||
      cleanupFailed();
    reset.disabled = busy;
    const error = form.querySelector("#server-error");
    error.textContent = cleanupFailed()
      ? cleanupErrors[state.cleanupCode] || errors.DISCONNECT_FAILED
      : errors[state.errorCode] ||
        (state.status === "failed" ? "连接失败，请重试" : "");
    error.hidden = !error.textContent;
    const indicator = form.querySelector("#server-status");
    indicator.dataset.status = state.status;
    indicator.lastElementChild.textContent =
      message || (cleanupFailed() ? "注销未完成" : status);
  }
  async function request(path = "", body) {
    controller = new AbortController();
    const active = controller;
    try {
      const action =
        path === "/connect"
          ? "connect"
          : path === "/disconnect"
            ? "disconnect"
            : "get";
      return parseState(
        await Backend.tunnel[action]({ body, signal: active.signal }),
      );
    } finally {
      if (controller === active) controller = null;
    }
  }
  function accept(next) {
    const cleared =
      next.status === "disconnected" && state.status !== "disconnected";
    state = next;
    loaded = true;
    refreshFailures = 0;
    if (next.domain || cleared)
      form.elements.namedItem("domain").value = next.domain;
  }
  function schedule(recover = false) {
    clearTimeout(timer);
    if (
      form &&
      !document.hidden &&
      (recover ||
        ["authorizing", "connecting", "disconnecting", "connected"].includes(
          state.status,
        ))
    )
      timer = setTimeout(
        refresh,
        recover
          ? Math.min(2500 * 2 ** refreshFailures, 30000)
          : state.status === "connected"
            ? 15000
            : 2500,
      );
  }
  async function refresh() {
    if (!form || !enabled() || busy) return;
    clearTimeout(timer);
    const current = form;
    busy = true;
    update(loaded ? undefined : "正在检查");
    let failure;
    try {
      const next = await request();
      if (form === current) accept(next);
    } catch (error) {
      failure =
        error.name === "AbortError" ? "连接超时" : failureMessage(error);
    } finally {
      if (form === current) {
        busy = false;
        if (failure) loaded = false;
        update(failure);
        if (failure) refreshFailures++;
        if (!failure || refreshFailures < 5) schedule(Boolean(failure));
      }
    }
  }
  async function act() {
    if (!form || busy || (loaded && pending())) return;
    const domain = normalizeDomain(form.elements.namedItem("domain").value);
    if (
      Forms.report(
        form,
        domain
          ? null
          : { field: "domain", message: "请输入域名，例如 panel.example.com" },
      )
    )
      return;
    if (!enabled()) {
      UI.toast("服务器接口尚未接入");
      return;
    }
    if (!loaded) {
      await refresh();
      return;
    }
    if (cleanupFailed()) {
      await change("/disconnect", {});
      return;
    }
    if (state.status === "authorizing") {
      window.open(state.authorizationUrl, "_blank", "noopener,noreferrer");
      schedule();
      return;
    }
    if (state.status === "connected") {
      UI.modal(
        "注销云连接？",
        '<div class="confirm-actions"><button type="button" data-server-cancel>取消</button><button type="button" class="danger-button" data-server-disconnect>注销</button></div>',
      );
      return;
    }
    await change("/connect", { domain });
  }
  async function change(path, body) {
    if (!form || busy) return;
    const current = form;
    busy = true;
    clearTimeout(timer);
    update(path === "/disconnect" ? "正在注销" : "正在连接");
    let failure;
    try {
      const next = await request(path, body);
      if (form === current) {
        accept(next);
        if (path === "/disconnect" && next.status === "disconnected") {
          form.elements.namedItem("domain").value = "";
          UI.toast("已注销连接");
        } else if (next.status === "authorizing") {
          UI.toast("请点击授权，前往 Cloudflare");
        }
      }
    } catch (error) {
      failure =
        error.name === "AbortError"
          ? "连接超时，请检查状态后重试"
          : failureMessage(error);
      if (form === current) UI.toast(failure);
    } finally {
      if (form === current) {
        busy = false;
        if (failure) loaded = false;
        update(failure);
        schedule(Boolean(failure));
      }
    }
  }
  function mount() {
    form = document.getElementById("server-form");
    busy = loaded = false;
    refreshFailures = 0;
    state = { status: "disconnected", domain: "" };
    update(enabled() ? "正在检查" : "未接入");
    if (enabled()) refresh();
  }
  function unmount() {
    form = null;
    controller?.abort();
    clearTimeout(timer);
    busy = false;
  }
  document.addEventListener("submit", (event) => {
    if (event.target.id !== "server-form") return;
    event.preventDefault();
    act();
  });
  document.addEventListener("click", (event) => {
    if (
      event.target.closest("#server-reset") &&
      ["authorizing", "connecting", "failed"].includes(state.status)
    )
      change("/disconnect", {});
    if (event.target.closest("[data-server-cancel]"))
      document.getElementById("dialog").close();
    if (event.target.closest("[data-server-disconnect]")) {
      document.getElementById("dialog").close();
      if (state.status === "connected") change("/disconnect", {});
    }
  });
  document.addEventListener("visibilitychange", () => {
    clearTimeout(timer);
    if (form && !document.hidden) refresh();
  });
  return { render, mount, unmount, normalizeDomain, authURL, parseState };
})();
