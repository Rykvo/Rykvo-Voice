const SIPHistory = (() => {
  const { escape: esc, $ } = UI;
  let accountId = "",
    date = "";
  const dateKey = Calendar.dateKey;
  function isOutgoing(call) {
    return call.direction
      ? call.direction === "outgoing"
      : ["outgoing", "cancelled"].includes(call.kind);
  }
  function result(call) {
    if (
      [
        "connected",
        "missed",
        "busy",
        "cancelled",
        "failed",
        "unconnected",
      ].includes(call.status)
    )
      return call.status;
    if (call.status) return "unconnected";
    return ["incoming", "outgoing"].includes(call.kind)
      ? "connected"
      : ["missed", "busy", "cancelled", "failed"].includes(call.kind)
        ? call.kind
        : "unconnected";
  }
  function billedMinutes(call) {
    if (result(call) !== "connected") return 0;
    const duration = Number.isFinite(call.duration) ? call.duration : 0;
    return Math.max(1, Math.ceil(duration / 60));
  }
  function totalDuration(records) {
    const minutes = records.reduce((sum, call) => sum + billedMinutes(call), 0);
    return `${Math.floor(minutes / 60)}小时${minutes % 60}分`;
  }
  function select(records, id, day) {
    return (Array.isArray(records) ? records : [])
      .filter(
        (call) =>
          call &&
          isOutgoing(call) &&
          call.sipAccountId === id &&
          typeof call.number === "string" &&
          Number.isFinite(call.at) &&
          dateKey(call.at) === day,
      )
      .sort((a, b) => b.at - a.at);
  }
  function rows(calls) {
    return calls.length
      ? calls
          .map((call) => {
            const module = ModuleData.items.find(
              (item) => item.id === (call.moduleId || call.lineId),
            );
            const connected = result(call) === "connected";
            return `<tr><th scope="row">${esc(Countries.format(call.number))}</th><td>${esc(module?.label || module?.name || "—")}</td><td><span class="sip-call-result" data-connected="${connected}">${connected ? "已接通" : "未接通"}</span></td><td>${connected ? `${billedMinutes(call)}分钟` : "—"}</td></tr>`;
          })
          .join("")
      : '<tr><td colspan="4" class="data-empty">暂无通话记录</td></tr>';
  }
  function calls() {
    return select(UI.read("rykvo-voice-calls-v1", []), accountId, date);
  }
  function render(id) {
    accountId = id;
    date = dateKey();
    const records = calls();
    return `${Calendar.render("sip-history-date", date)}<div class="sip-history-list data-table-wrap" role="region" aria-label="当前账号通话记录" tabindex="0"><table class="data-table sip-call-table" aria-label="当前账号通话记录"><colgroup><col style="width:36%"><col style="width:20%"><col style="width:24%"><col style="width:20%"></colgroup><thead><tr><th scope="col">对方号码</th><th scope="col">模块</th><th scope="col">状态</th><th scope="col">时长</th></tr></thead><tbody id="sip-history-rows">${rows(records)}</tbody></table></div><div class="sip-history-total" role="status" aria-live="polite" aria-atomic="true"><span>通话统计</span><strong id="sip-history-duration">${totalDuration(records)}</strong></div>`;
  }
  function update() {
    if (!$("#sip-history-rows")) return;
    const records = calls();
    $("#sip-history-rows").innerHTML = rows(records);
    $("#sip-history-duration").textContent = totalDuration(records);
    const list = $(".sip-history-list");
    if (list) list.scrollTop = 0;
  }
  document.addEventListener("change", (event) => {
    if (event.target.id === "sip-history-date") {
      const value = event.target.value;
      if (!Calendar.parse(value)) {
        event.target.value = date;
        return;
      }
      date = value;
    } else return;
    update();
  });
  return {
    render,
    update,
    dateKey,
    isOutgoing,
    result,
    select,
    billedMinutes,
    totalDuration,
  };
})();
