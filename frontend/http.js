const Http = (() => {
  let csrfToken = "";
  const base = () =>
    document.querySelector("base")?.getAttribute?.("href") === "/gly/"
      ? "/gly"
      : "";
  const apiURL = (path) => `${base()}/api${path}`;
  function id() {
    if (crypto.randomUUID) return crypto.randomUUID();
    // 局域网 HTTP 仍使用安全随机数生成 UUID。
    const bytes = crypto.getRandomValues(new Uint8Array(16));
    bytes[6] = (bytes[6] & 15) | 64;
    bytes[8] = (bytes[8] & 63) | 128;
    const hex = Array.from(bytes, (byte) =>
      byte.toString(16).padStart(2, "0"),
    ).join("");
    return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
  }
  function failure(code, message, status = 0, field = "") {
    return Object.assign(new Error(message), { code, status, field });
  }
  async function request(method, template, options = {}) {
    if (method === "GET" && options.body !== undefined)
      throw failure("INVALID_ARGUMENT", "查询请求不接受正文");
    const path = template.replace(/:([a-zA-Z]+)/g, (_, key) => {
      const value = options.params?.[key];
      if (
        typeof value !== "string" ||
        !value.trim() ||
        [".", ".."].includes(value)
      )
        throw failure("INVALID_ARGUMENT", `缺少参数 ${key}`);
      return encodeURIComponent(value);
    });
    const query = new URLSearchParams();
    for (const [key, value] of Object.entries(options.query || {})) {
      if (value !== undefined && value !== null && value !== "")
        query.set(key, String(value));
    }
    const controller = new AbortController();
    const abort = () => controller.abort();
    if (options.signal?.aborted) throw failure("ABORTED", "请求已取消");
    options.signal?.addEventListener("abort", abort, { once: true });
    let timedOut = false;
    const timer = setTimeout(() => {
      timedOut = true;
      controller.abort();
    }, 15000);
    const headers = { Accept: "application/json" };
    const write = method !== "GET";
    try {
      if (write) {
        headers["Idempotency-Key"] = options.idempotencyKey || id();
        if (csrfToken) headers["X-CSRF-Token"] = csrfToken;
      }
      let body;
      if (options.body instanceof FormData) body = options.body;
      else if (options.body !== undefined) {
        headers["Content-Type"] = "application/json";
        body = JSON.stringify(options.body);
      }
      const response = await fetch(
        `${apiURL(path)}${query.size ? `?${query}` : ""}`,
        {
          method,
          headers,
          body,
          credentials: "same-origin",
          cache: "no-store",
          redirect: "error",
          signal: controller.signal,
        },
      );
      if (response.status === 204 && response.ok) return null;
      const json = response.headers
        .get("Content-Type")
        ?.includes("application/json");
      let payload = null;
      if (json) {
        try {
          payload = await response.json();
        } catch {
          throw failure("INVALID_RESPONSE", "服务响应异常", response.status);
        }
      }
      if (!response.ok) {
        const code = payload?.error?.code || `HTTP_${response.status}`;
        const messages = {
          401: "请先登录",
          403: "没有操作权限",
          409: "数据已更新，请刷新后重试",
          429: "操作频繁，请稍后重试",
        };
        throw failure(
          code,
          messages[response.status] || "请求失败，请重试",
          response.status,
          payload?.error?.field || "",
        );
      }
      if (!json || !payload || typeof payload !== "object")
        throw failure("INVALID_RESPONSE", "服务响应异常");
      if (Object.hasOwn(payload, "error"))
        throw failure("INVALID_RESPONSE", "服务响应异常");
      return Object.hasOwn(payload, "data") ? payload.data : payload;
    } catch (error) {
      if (timedOut) throw failure("TIMEOUT", "请求超时，请核实操作结果");
      if (controller.signal.aborted) throw failure("ABORTED", "请求已取消");
      if (error.code) throw error;
      throw failure("NETWORK", "连接失败，请重试");
    } finally {
      clearTimeout(timer);
      options.signal?.removeEventListener("abort", abort);
    }
  }
  return {
    id,
    apiURL,
    get home() {
      return base() || "/";
    },
    request,
    failure,
    setCSRFToken(value) {
      csrfToken = typeof value === "string" ? value : "";
    },
  };
})();
