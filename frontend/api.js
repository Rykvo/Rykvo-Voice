// 同源接口入口；静态预览不发请求，不保存凭据。
const Backend = (() => {
  const routes = {
    session: {
      get: ["GET", "/session"],
      login: ["POST", "/session"],
      logout: ["DELETE", "/session"],
    },
    modules: {
      list: ["GET", "/modules"],
      get: ["GET", "/modules/:moduleId"],
      update: ["PATCH", "/modules/:moduleId"],
      lines: ["GET", "/modules/:moduleId/lines"],
      updateLine: ["PATCH", "/modules/:moduleId/lines/:lineId"],
      networks: ["GET", "/modules/:moduleId/lines/:lineId/networks"],
      installESIM: ["POST", "/modules/:moduleId/esim"],
      removeLine: ["DELETE", "/modules/:moduleId/lines/:lineId"],
      notifyESIM: ["POST", "/modules/:moduleId/esim/notifications"],
    },
    calls: {
      list: ["GET", "/calls"],
      dial: ["POST", "/calls"],
      get: ["GET", "/calls/:callId"],
      answer: ["POST", "/calls/:callId/answer"],
      reject: ["POST", "/calls/:callId/reject"],
      hangup: ["POST", "/calls/:callId/hangup"],
      dtmf: ["POST", "/calls/:callId/dtmf"],
      media: ["POST", "/calls/:callId/media"],
      ice: ["POST", "/calls/:callId/ice"],
    },
    callRecords: {
      list: ["GET", "/call-records"],
      stats: ["GET", "/call-records/stats"],
      remove: ["DELETE", "/call-records/:recordId"],
      removeAll: ["POST", "/call-records/delete"],
    },
    conversations: {
      list: ["GET", "/conversations"],
      get: ["GET", "/conversations/:conversationId"],
      messages: ["GET", "/conversations/:conversationId/messages"],
      markRead: ["POST", "/conversations/:conversationId/read"],
      remove: ["DELETE", "/conversations/:conversationId"],
      removeAll: ["POST", "/conversations/delete"],
    },
    messages: {
      send: ["POST", "/messages"],
      get: ["GET", "/messages/:messageId"],
    },
    attachments: {
      upload: ["POST", "/attachments"],
      remove: ["DELETE", "/attachments/:attachmentId"],
    },
    sip: {
      list: ["GET", "/sip/accounts"],
      create: ["POST", "/sip/accounts"],
      get: ["GET", "/sip/accounts/:accountId"],
      update: ["PATCH", "/sip/accounts/:accountId"],
      remove: ["DELETE", "/sip/accounts/:accountId"],
    },
    administrator: {
      get: ["GET", "/administrator"],
      update: ["PATCH", "/administrator"],
    },
    developer: {
      get: ["GET", "/settings/developer"],
      update: ["PATCH", "/settings/developer"],
    },
    retention: {
      get: ["GET", "/settings/retention"],
      update: ["PUT", "/settings/retention"],
    },
    preferences: {
      get: ["GET", "/settings/preferences"],
      update: ["PATCH", "/settings/preferences"],
    },
    visibility: {
      get: ["GET", "/settings/visibility"],
      update: ["PUT", "/settings/visibility"],
      unlock: ["POST", "/settings/visibility/unlock"],
      access: ["GET", "/settings/visibility/access"],
      lock: ["DELETE", "/settings/visibility/access"],
      changePassword: ["PUT", "/settings/visibility/password"],
    },
    ui: { activate: ["POST", "/ui/activation"] },
    updates: {
      get: ["GET", "/updates"],
      check: ["POST", "/updates/check"],
      install: ["POST", "/updates/install"],
    },
    jobs: { get: ["GET", "/jobs/:jobId"] },
    tunnel: {
      get: ["GET", "/tunnel"],
      connect: ["POST", "/tunnel/connect"],
      disconnect: ["POST", "/tunnel/disconnect"],
    },
  };
  let stream = null;
  const listeners = new Set();
  const enabled = (scope) =>
    document.querySelector('meta[name="backend-api"]')?.content === "enabled" ||
    (["session", "visibility", "ui", "administrator"].includes(scope) &&
      document.querySelector('meta[name="session-api"]')?.content ===
        "enabled") ||
    (scope === "modules" &&
      document.querySelector('meta[name="modules-api"]')?.content === "enabled") ||
    (scope === "tunnel" &&
      document.querySelector('meta[name="tunnel-api"]')?.content === "enabled");
  const failure = Http.failure;
  function request(method, template, options, scope) {
    if (!enabled(scope))
      return Promise.reject(failure("NOT_CONNECTED", "服务尚未连接"));
    return Http.request(method, template, options);
  }
  function subscribe(listener) {
    if (!enabled()) throw failure("NOT_CONNECTED", "服务尚未连接");
    if (typeof listener !== "function")
      throw failure("INVALID_ARGUMENT", "缺少事件处理函数");
    const subscription = (value) => listener(value);
    if (!stream) {
      stream = new EventSource(Http.apiURL("/events"));
      stream.onmessage = (event) => {
        let value;
        try {
          value = JSON.parse(event.data);
        } catch {
          return;
        }
        if (
          !value ||
          typeof value.type !== "string" ||
          typeof value.id !== "string"
        )
          return;
        for (const handler of listeners) {
          try {
            handler(value);
          } catch {
            /* 单个订阅失败不影响其他页面。 */
          }
        }
      };
    }
    listeners.add(subscription);
    return () => {
      listeners.delete(subscription);
      if (!listeners.size) {
        stream?.close();
        stream = null;
      }
    };
  }
  return Object.freeze({
    enabled,
    setCSRFToken(value) {
      Http.setCSRFToken(value);
    },
    subscribe,
    ...Object.fromEntries(
      Object.entries(routes).map(([group, operations]) => [
        group,
        Object.freeze(
          Object.fromEntries(
            Object.entries(operations).map(([name, [method, path]]) => [
              name,
              (options) => request(method, path, options, group),
            ]),
          ),
        ),
      ]),
    ),
  });
})();
