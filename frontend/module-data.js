const ModuleData = (() => {
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
    AT_PORT_MISSING: "未找到 AT 串口", PERMISSION_DENIED: "设备访问权限不足",
    DEVICE_BUSY: "设备正在被占用", DEVICE_UNAVAILABLE: "设备暂不可用",
    READ_TIMEOUT: "设备读取超时", QMI_UNAVAILABLE: "QMI 读取工具未就绪",
    QMI_READ_FAILED: "QMI 状态读取失败", PCSC_UNAVAILABLE: "读卡器服务或驱动未就绪",
    CARD_READ_FAILED: "SIM 读取失败", CARD_UNSUPPORTED: "SIM 类型暂未识别",
    IDENTITY_CONFLICT: "设备身份冲突", READING: "正在读取", STATE_STALE: "状态待更新",
    IDENTITY_PENDING: "正在确认设备身份",
    NO_EUICC: "当前卡片未识别为 eSIM", EUICC_CHANNEL_UNAVAILABLE: "卡片通道暂不可用",
    DEVICE_CHANGED: "设备或卡片已变化，请刷新", PROFILE_NOT_FOUND: "配置已变化，请刷新",
    PROFILE_DELETE_BLOCKED: "请先停用此配置", PROFILE_POLICY: "运营商限制此操作",
    ESIM_CARD_REJECTED: "卡片未接受操作", ESIM_UNAVAILABLE: "eSIM 服务暂未就绪",
    ESIM_OPERATION_FAILED: "eSIM 操作失败", ESIM_RESULT_UNKNOWN: "结果待核实，请刷新卡片状态",
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
        if (started && !document.hidden) timer = setTimeout(refresh, 5000);
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
      item.job = job;
      if (item.capabilities) item.capabilities.esim = false;
      notify();
      return job;
    } finally {
      if (started && !document.hidden) timer = setTimeout(refresh, 1000);
    }
  }
  return {
    items, labels, labelKey, validLabel, duplicate, connected, merge, start, saveLabel, control, issueText,
    subscribe(listener) { listeners.add(listener); return () => listeners.delete(listener); },
    get issue() { return issue; },
    get loaded() { return loaded; },
  };
})();
