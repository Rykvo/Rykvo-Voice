// 同源接口入口；静态预览不发请求，不保存凭据。
const Backend = (() => {
  const routes = {
    version: { get: ["GET", "/version"] },
    softwareUpdate: { get: ["GET", "/software-update"], apply: ["POST", "/software-update/apply"] },
    session: {
      get: ["GET", "/session"],
      login: ["POST", "/session"],
      logout: ["DELETE", "/session"],
    },
    modules: {
      list: ["GET", "/modules"],
      restart: ["POST", "/modules/:moduleId/restart"],
      restartAll: ["POST", "/modules/restart"],
      restartHost: ["POST", "/modules/host-restart"],
      get: ["GET", "/modules/:moduleId"],
      update: ["PATCH", "/modules/:moduleId"],
      lines: ["GET", "/modules/:moduleId/lines"],
      updateLine: ["PATCH", "/modules/:moduleId/lines/:lineId"],
      installESIM: ["POST", "/modules/:moduleId/esim"],
      removeLine: ["DELETE", "/modules/:moduleId/lines/:lineId"],
      notifyESIM: ["POST", "/modules/:moduleId/esim/notifications"],
      apns: ["GET", "/modules/:moduleId/lines/:lineId/apns"],
      saveAPN: ["PUT", "/modules/:moduleId/lines/:lineId/apns/:apnId"],
      removeAPN: ["DELETE", "/modules/:moduleId/lines/:lineId/apns/:apnId"],
      applyAPN: ["POST", "/modules/:moduleId/lines/:lineId/apns/:apnId/apply"],
    },
    callRecords: {
      list: ["GET", "/call-records"],
      stats: ["GET", "/call-records/stats"],
    },
    messages: {
      threads: ["GET", "/messages/threads"],
      search: ["GET", "/messages/search"],
      window: ["GET", "/messages/window"],
      removeThreads: ["DELETE", "/messages/threads"],
      saveContact: ["PUT", "/messages/contacts"],
      send: ["POST", "/messages"],
    },
    sip: {
      list: ["GET", "/sip/accounts"],
      create: ["POST", "/sip/accounts"],
      get: ["GET", "/sip/accounts/:accountId"],
      update: ["PATCH", "/sip/accounts/:accountId"],
      remove: ["DELETE", "/sip/accounts/:accountId"],
    },
    sipServer: {
      get: ["GET", "/settings/sip-server"],
      logout: ["POST", "/settings/sip-server/logout"],
      connect: ["POST", "/settings/sip-server/connect"],
    },
    emergencyAddress: {
      start: ["POST", "/modules/:moduleId/lines/:lineId/emergency-address/session"],
      get: ["GET", "/modules/:moduleId/lines/:lineId/emergency-address/session/:sessionId"],
      cancel: ["DELETE", "/modules/:moduleId/lines/:lineId/emergency-address/session/:sessionId"],
    },
    administrator: {
      get: ["GET", "/administrator"],
      update: ["PATCH", "/administrator"],
    },
    developer: {
      get: ["GET", "/settings/developer"],
      update: ["PATCH", "/settings/developer"],
    },
    alerts: {
      get: ["GET", "/settings/alerts"],
      update: ["PUT", "/settings/alerts"],
    },
    networks: { get: ["GET", "/settings/networks"], update: ["PUT", "/settings/networks"] },
    hostname: {
      get: ["GET", "/settings/hostname"],
      update: ["PUT", "/settings/hostname"],
    },
    contact: {
      get: ["GET", "/settings/contact"],
      update: ["PUT", "/settings/contact"],
    },
    retention: {
      get: ["GET", "/settings/retention"],
      update: ["PUT", "/settings/retention"],
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
    tunnel: {
      get: ["GET", "/tunnel"],
      connect: ["POST", "/tunnel/connect"],
      disconnect: ["POST", "/tunnel/disconnect"],
    },
  };
  const enabled = (scope) =>
    document.querySelector('meta[name="backend-api"]')?.content === "enabled" ||
    (["session", "visibility", "ui", "administrator", "hostname", "networks", "developer", "alerts", "retention", "contact", "version", "softwareUpdate"].includes(scope) &&
      document.querySelector('meta[name="session-api"]')?.content ===
        "enabled") ||
    (scope === "sipServer" && document.querySelector('meta[name="sip-network-api"]')?.content === "enabled") ||
    (["sip", "callRecords"].includes(scope) && document.querySelector('meta[name="sip-accounts-api"]')?.content === "enabled") ||
    (scope === "messages" && document.querySelector('meta[name="messages-api"]')?.content === "enabled") ||
    (["modules", "emergencyAddress"].includes(scope) &&
      document.querySelector('meta[name="modules-api"]')?.content === "enabled") ||
    (scope === "tunnel" &&
      document.querySelector('meta[name="tunnel-api"]')?.content === "enabled");
  const failure = Http.failure;
  function request(method, template, options, scope) {
    if (!enabled(scope))
      return Promise.reject(failure("NOT_CONNECTED", "服务尚未连接"));
    return Http.request(method, template, options);
  }
  return Object.freeze({
    enabled,
    setCSRFToken(value) {
      Http.setCSRFToken(value);
    },
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
