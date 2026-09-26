const SIPHistory = (() => {
  const { escape: esc, $ } = UI;
  let accountId = "", date = "", request = null, generation = 0;
  let cursors = [""], nextCursor = "", page = 0, items = [], total = null, issue = "";
  const labels = {
    connected: "已接通", active: "通话中", dialing: "拨号中", ringing: "响铃中",
    no_answer: "无人接听", rejected: "对方拒接", busy: "对方占线", cancelled: "已取消",
    radio_unavailable: "蜂窝网络不可用", card_error: "卡异常", not_registered: "网络未注册", module_error: "模块异常",
    module_busy: "模块忙碌", account_busy: "账号忙碌", no_available_module: "无可用模块",
    number_not_found: "号码不存在", carrier_rejected: "运营商拒绝", peer_unavailable: "对方暂不可用",
    carrier_unavailable: "运营商服务异常", unsupported_audio: "音频格式不支持",
    media_network_error: "语音网络异常", media_error: "音频异常", account_revoked: "账号授权已撤销",
    call_timeout: "呼叫超时", interrupted: "服务中断", service_unavailable: "电话服务异常",
    answered_elsewhere: "其他用户已接听", unconnected: "未接通", failed: "拨打失败",
  };
  function status(call) { return labels[call.status] || "拨打失败"; }
  function minutes(call) {
    return call.answeredAt && Number.isFinite(call.duration) && call.duration >= 0 ? Math.max(1, Math.ceil(call.duration / 60)) : null;
  }
  function totalDuration(value) {
    return Number.isSafeInteger(value) && value >= 0 ? `${Math.floor(value / 60)}小时${value % 60}分` : "—";
  }
  function bounds(day) {
    const start = Calendar.parse(day);
    if (!start) return null;
    start.setHours(0, 0, 0, 0);
    const end = new Date(start); end.setDate(end.getDate() + 1);
    return { from: start.toISOString(), to: end.toISOString() };
  }
  function rows() {
    if (issue) return `<tr><td colspan="4" class="data-empty">${esc(issue)} <button type="button" class="text-button" data-sip-history-retry>重试</button></td></tr>`;
    if (!items.length) return `<tr><td colspan="4" class="data-empty">${request ? "加载中" : "暂无通话记录"}</td></tr>`;
    return items.map(call => {
      const module = ModuleData.items.find(item => item.id === call.moduleId);
      const label = module?.label || module?.name || (/^module-\d+$/.test(call.moduleId) ? `模块 ${call.moduleId.slice(7)}` : "—");
      const at = new Date(call.startedAt), time = Number.isFinite(at.getTime()) ? at.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false }) : "";
      const duration = minutes(call), connected = !!call.answeredAt;
      return `<tr><th scope="row">${esc(call.number ? Countries.format(call.number) : "未知号码")}<small class="sip-call-meta">${call.direction === "incoming" ? "呼入" : "呼出"} · ${esc(time)}</small></th><td>${esc(label)}</td><td><span class="sip-call-result" data-connected="${connected}">${esc(status(call))}</span></td><td>${duration === null ? "—" : `${duration}分钟`}</td></tr>`;
    }).join("");
  }
  function paint() {
    const body = $("#sip-history-rows");
    if (!body) return;
    const html = rows(); if (body.innerHTML !== html) body.innerHTML = html;
    $("#sip-history-duration").textContent = totalDuration(total);
    const prev = $("[data-sip-history-prev]"), next = $("[data-sip-history-next]");
    if (prev) { prev.disabled = !!request || page === 0; prev.hidden = page === 0; }
    if (next) { next.disabled = !!request || !nextCursor; next.hidden = !nextCursor; }
  }
  function close() {
    generation++; request?.abort(); request = null; accountId = ""; items = []; total = null;
  }
  function render(id) {
    close(); accountId = id; date = Calendar.dateKey(); cursors = [""]; page = 0; nextCursor = ""; issue = "";
    return `${Calendar.render("sip-history-date", date)}<div class="sip-history-list data-table-wrap" role="region" aria-label="当前账号通话记录" tabindex="0"><table class="data-table sip-call-table" aria-label="当前账号通话记录"><colgroup><col style="width:36%"><col style="width:20%"><col style="width:24%"><col style="width:20%"></colgroup><thead><tr><th scope="col">对方号码</th><th scope="col">模块</th><th scope="col">状态</th><th scope="col">时长</th></tr></thead><tbody id="sip-history-rows"><tr><td colspan="4" class="data-empty">加载中</td></tr></tbody></table></div><div class="sip-history-total"><nav aria-label="通话记录分页"><button type="button" class="text-button" data-sip-history-prev hidden>上一页</button><button type="button" class="text-button" data-sip-history-next hidden>下一页</button></nav><span>通话统计</span><strong id="sip-history-duration" role="status" aria-live="polite">—</strong></div>`;
  }
  async function update(reset = false) {
    if (!accountId || !$("#sip-history-rows") || document.hidden || (request && !reset) || (page > 0 && !reset)) return;
    request?.abort();
    const pending = new AbortController(), epoch = ++generation;
    request = pending; issue = ""; paint();
    try {
      const data = await Backend.callRecords.list({ query: { accountId, ...bounds(date), before: cursors[page] }, signal: pending.signal });
      if (pending.signal.aborted || epoch !== generation || !accountId) return;
      if (!Array.isArray(data?.items) || !Number.isSafeInteger(data.totalMinutes) || typeof data.nextCursor !== "string") throw new Error("invalid response");
      items = data.items; total = data.totalMinutes; nextCursor = data.nextCursor;
    } catch (error) {
      if (pending.signal.aborted || epoch !== generation) return;
      items = []; total = null; nextCursor = ""; issue = error.code === "NOT_CONNECTED" ? "服务尚未连接" : "加载失败";
    } finally {
      if (request === pending) request = null;
      if (epoch === generation && accountId) paint();
    }
  }
  function resetPage() { items = []; total = null; issue = ""; nextCursor = ""; const list = $(".sip-history-list"); if (list) list.scrollTop = 0; }
  document.addEventListener("change", event => {
    if (event.target.id !== "sip-history-date" || !accountId) return;
    if (!Calendar.parse(event.target.value)) { event.target.value = date; return; }
    date = event.target.value; cursors = [""]; page = 0; resetPage(); update(true);
  });
  document.addEventListener("click", event => {
    if (!accountId) return;
    if (event.target.closest("[data-sip-history-retry]")) return update(true);
    if (request) return;
    if (event.target.closest("[data-sip-history-next]") && nextCursor) { cursors[++page] = nextCursor; }
    else if (event.target.closest("[data-sip-history-prev]") && page > 0) page--;
    else return;
    resetPage(); update(true);
  });
  document.addEventListener("visibilitychange", () => {
    if (document.hidden && request) { generation++; request.abort(); request = null; }
  });
  return { render, update, close, status, minutes, totalDuration, bounds };
})();
