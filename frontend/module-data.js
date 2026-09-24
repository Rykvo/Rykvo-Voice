const ModuleData = (() => {
  function identity(number, iccid) {
    const phone = String(number || "").trim(), card = String(iccid || "").trim();
    const label = phone || !card ? "本机号码" : "ICCID";
    const value = phone ? Countries.format(phone) : card || "—";
    return { label, value, caption: label === "ICCID" ? `ICCID ${value}` : value };
  }
  function validLabel(value) {
    const label = value.trim();
    return (
      [...label].length > 0 && [...label].length <= 20 && !/[\p{Cc}\p{Cf}]/u.test(label)
    );
  }
  const items = [];
  const labelKey = "rykvo-voice-module-labels-v1";
  const saved = UI.read(labelKey, {});
  const labels = Object.fromEntries(
    Object.entries(
      saved && typeof saved === "object" && !Array.isArray(saved) ? saved : {},
    ).filter(([, label]) => typeof label === "string" && validLabel(label)),
  );
  const listeners = new Set();
  const migrated = new Set();
  let started = false, timer = null, controller = null, generation = 0;
  let issue = "", loaded = false;
  const connected = () => typeof Backend !== "undefined" && Backend.enabled("modules");
  const issueText = (code) => ({
    APN_INVALID: "请检查 APN、协议和认证信息；IMS / SOS 配置保留给系统",
    APN_READ_FAILED: "APN 读取未完成，请稍后重新读取",
    APN_NOT_FOUND: "APN 配置已变化，请重新读取",
    APN_SYSTEM_CONTEXT: "当前数据上下文用于 IMS / SOS，已保留原配置",
    APN_APPLY_UNCONFIRMED: "APN 写入结果待确认，请重新读取，勿连续重复应用",
    NOT_CONNECTED: "服务尚未接入",
    WIFI_MODEM_UNSUPPORTED: "模块暂未支持 Wi-Fi 通话",
    WIFI_DATA_ACTIVE: "请先断开该模块的主机数据连接", WIFI_DATA_STATE_UNKNOWN: "蜂窝数据状态待确认",
    WIFI_IMS_SERVICE_UNAVAILABLE: "运营商 IMS 暂不可用，等待重试", WIFI_IMS_CONTACT_UNCONFIRMED: "IMS 联系地址未确认", WIFI_IMS_TIMEOUT: "IMS 注册响应超时", WIFI_TCP_CONNECT_FAILED: "IMS TCP 连接失败", WIFI_TCP_CLOSED: "IMS TCP 连接中断", WIFI_TCP_WRITE_FAILED: "IMS TCP 发送失败", WIFI_IMS_PROTECTED_FAILED: "IMS 加密注册失败", WIFI_CARRIER_CONFIG_INVALID: "运营商配置无效", WIFI_CARRIER_UNSUPPORTED: "运营商鉴权方式待适配", WIFI_IMS_ADDRESS_MISSING: "IMS 地址不可用", WIFI_IMS_PEER_AUTH_FAILED: "IMS 对端校验失败",
    WIFI_WORKER_UNAVAILABLE: "Wi-Fi 连接服务未就绪", WIFI_NETWORK_UNAVAILABLE: "网络暂不可用", WIFI_CONNECTION_FAILED: "连接失败",
    WIFI_IMS_REJECTED: "运营商未接受注册", WIFI_AUTH_REJECTED: "运营商鉴权未通过", AKA_REJECTED: "SIM 鉴权未通过",
    WIFI_CERTIFICATE_INVALID: "运营商证书验证失败", WIFI_PEER_AUTH_FAILED: "运营商身份验证失败",
    WIFI_IMS_AKA_RESYNC_REQUIRED: "SIM 鉴权需要重试", WIFI_IMS_SECURITY_UNSUPPORTED: "运营商安全协议暂未支持",
    WIFI_IMS_AUTH_UNSUPPORTED: "运营商鉴权协议暂未支持",
    WIFI_RADIO_RESTORE_UNCONFIRMED: "射频恢复待确认", WIFI_RADIO_UNCONFIRMED: "飞行模式待确认",
    WIFI_SIM_CLEANUP_UNCONFIRMED: "SIM 通道关闭待确认", WIFI_IMS_DEREGISTER_UNCONFIRMED: "运营商注销待确认", SIM_NOT_READY: "SIM 未就绪",
    COMMAND_UNSUPPORTED: "模块未支持此操作",
    AT_PORT_MISSING: "未找到 AT 串口", PERMISSION_DENIED: "设备访问权限不足",
    DEVICE_BUSY: "设备正在被占用", DEVICE_UNAVAILABLE: "设备暂不可用",
    READ_TIMEOUT: "设备读取超时", QMI_UNAVAILABLE: "QMI 读取工具未就绪",
    QMI_READ_FAILED: "QMI 状态读取失败", QMI_STATUS_FAILED: "网络状态读取失败",
    PCSC_UNAVAILABLE: "读卡器服务或驱动未就绪",
    CARD_READ_FAILED: "SIM 读取失败", CARD_UNSUPPORTED: "SIM 类型暂未识别",
    IDENTITY_CONFLICT: "设备身份冲突", READING: "正在读取", RECOVERING: "正在恢复", STATE_STALE: "状态待更新",
    IDENTITY_PENDING: "正在确认设备身份",
    NO_EUICC: "当前卡片未识别为 eSIM", EUICC_CHANNEL_UNAVAILABLE: "卡片通道暂不可用",
    DEVICE_CHANGED: "设备或卡片已变化，请刷新", PROFILE_NOT_FOUND: "配置已变化，请刷新",
    PROFILE_DELETE_BLOCKED: "请先停用此配置", PROFILE_POLICY: "运营商限制此操作",
    ESIM_CARD_REJECTED: "卡片未接受操作", ESIM_UNAVAILABLE: "eSIM 服务暂未就绪",
    ESIM_OPERATION_FAILED: "eSIM 操作失败", ESIM_RESULT_UNKNOWN: "等待卡片确认，请勿重复操作",
    ESIM_INTERRUPTED: "操作中断，请核实卡片状态", ESIM_NETWORK_FAILED: "运营商服务连接失败",
    ESIM_REMOTE_ADDRESS: "运营商服务地址无效", ESIM_NOTIFICATION_PENDING: "配置已处理，运营商通知待重试",
    CONFIRMATION_REQUIRED: "请输入运营商确认码", INVALID_ESIM_REQUEST: "请检查卡片和输入内容",
  })[code] || (code ? "设备读取异常" : "");
  const key = (value) => value.trim().normalize("NFKC").toLowerCase();
  const duplicate = (id, value) => items.some((item) => item.id !== id && key(item.label || item.name) === key(value));
  function notify() { for (const listener of listeners) listener(); }
  function merge(records) {
    const existing = new Map(items.map((item) => [item.id, item]));
    const next = records.map((record) => Object.assign(existing.get(record.id) || {}, record));
    items.splice(0, items.length, ...next);
    notify();
  }
  function stopRequest() {
    generation++;
    clearTimeout(timer);
    timer = null;
    controller?.abort();
    controller = null;
  }
  async function refresh() {
    if (!connected() || controller || document.hidden) return;
    const request = new AbortController();
    const version = ++generation;
    controller = request;
    try {
      const result = await Backend.modules.list({ signal: request.signal });
      if (version !== generation) return;
      if (!Array.isArray(result?.items)) throw new Error("Invalid modules response");
      issue = result.discoveryIssue || "";
      loaded = true;
      merge(result.items);
      for (const item of items) {
        const label = labels[item.id];
        if (!label || migrated.has(item.id)) continue;
        migrated.add(item.id);
        if (item.labelCustom || duplicate(item.id, label)) continue;
        try {
          const saved = await Backend.modules.update({ params: { moduleId: item.id }, body: { label, ifUnmodified: true }, signal: request.signal });
          if (version !== generation) return;
          Object.assign(item, saved);
          notify();
        } catch (error) {
          if (!error.code?.startsWith("LABEL_")) migrated.delete(item.id);
        }
      }
    } catch (error) {
      if (version !== generation || error.code === "ABORTED") return;
      if (error.status === 401) { stopRequest(); location.replace(Http.home); return; }
      issue = "CONNECTION_FAILED";
      notify();
    } finally {
      if (controller === request) {
        controller = null;
        if (started && !document.hidden) timer = setTimeout(refresh, items.some(item => ["queued", "running"].includes(item.job?.state)) ? 1000 : 5000);
      }
    }
  }
  function start() {
    if (started || !connected()) return;
    started = true;
    document.addEventListener("visibilitychange", () => {
      stopRequest();
      if (!document.hidden) refresh();
    });
    window.addEventListener("pagehide", stopRequest);
    window.addEventListener("pageshow", () => { if (!document.hidden) refresh(); });
    refresh();
  }
  async function saveLabel(item, label) {
    // Discard an older in-flight list so it cannot undo this edit.
    stopRequest();
    try {
      const saved = await Backend.modules.update({ params: { moduleId: item.id }, body: { label } });
      Object.assign(item, saved);
      migrated.add(item.id);
      notify();
    } finally {
      if (started && !document.hidden) timer = setTimeout(refresh, 5000);
    }
  }
  async function control(item, operation, body, lineId, requestId = Http.id()) {
    stopRequest();
    try {
      const job = await Backend.modules[operation]({
        params: { moduleId: item.id, lineId },
        body: { ...body, eid: item.hardware?.esim?.eid, requestId },
        idempotencyKey: requestId,
      });
      if (typeof body.wifiCalling === "boolean") Object.assign(item, job);
      else {
        item.job = job;
        if (item.capabilities) item.capabilities.esim = false;
      }
      notify();
      return job;
    } finally {
      if (started && !document.hidden) timer = setTimeout(refresh, 1000);
    }
  }
  return {
    items, labels, labelKey, validLabel, identity, duplicate, connected, merge, start, saveLabel, control, issueText,
    subscribe(listener) { listeners.add(listener); return () => listeners.delete(listener); },
    get issue() { return issue; },
    get loaded() { return loaded; },
  };
})();
