const { test } = require("node:test");
const assert = require("node:assert/strict");
const vm = require("node:vm");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
function setup(records = []) {
  const events = {},
    nodes = new Map();
  const node = (key) => {
    if (!nodes.has(key))
      nodes.set(key, {
        value: "",
        innerHTML: "",
        attributes: {},
        setAttribute(k, v) {
          this.attributes[k] = v;
        },
      });
    return nodes.get(key);
  };
  const context = vm.createContext({
    window: { addEventListener() {} },
    UI: { escape: String, $: node, read: () => records, time: () => "12:00" },
    Countries: { format: String },
    ModuleData: { items: [] },
    document: {
      addEventListener: (type, fn) => (events[type] ||= []).push(fn),
    },
  });
  vm.runInContext(
    readFileSync(join(__dirname, "..", "calendar.js"), "utf8"),
    context,
  );
  vm.runInContext(
    readFileSync(join(__dirname, "..", "sip-history.js"), "utf8"),
    context,
  );
  return {
    h: vm.runInContext("SIPHistory", context),
    node,
    emit(type, target) {
      for (const fn of events[type] || []) fn({ target });
    },
  };
}
const base = {
  sipAccountId: "a",
  direction: "outgoing",
  number: "10001",
  at: Date.now(),
  status: "connected",
  duration: 61,
};
test("history counts only this account outgoing calls on the selected local day", () => {
  const { h } = setup();
  const today = h.dateKey();
  const records = [
    base,
    { ...base, sipAccountId: "b" },
    { ...base, direction: "incoming" },
    {
      ...base,
      at: new Date(h.dateKey(Date.now() - 86400000) + "T12:00:00").getTime(),
    },
    { ...base, direction: undefined, kind: "missed" },
    { ...base, direction: undefined, kind: "cancelled" },
    null,
    { ...base, at: NaN },
  ];
  const selected = h.select(records, "a", today);
  assert.equal(selected.length, 2);
  assert.equal(h.select(null, "a", today).length, 0);
  assert.equal(
    h.isOutgoing({ ...base, direction: "incoming", kind: "outgoing" }),
    false,
  );
});
test("repeated outgoing numbers remain separate and each row counts one call", () => {
  const { h } = setup([
    base,
    { ...base, status: "busy" },
    { ...base, duration: 3601 },
  ]);
  const html = h.render("a");
  assert.equal((html.match(/<th scope="row">10001/g) || []).length, 3);
  assert.match(html, /<td>2分钟<\/td>/);
  assert.match(html, /<td>61分钟<\/td>/);
  assert.match(html, /data-connected="false">未接通<\/span><\/td><td>—/);
  assert.match(html, /data-connected="true">已接通<\/span>/);
  assert.doesNotMatch(html, /<td>1<\/td>/);
  assert.doesNotMatch(html, /秒|\d{2}:\d{2}:\d{2}/);
});
test("local date boundaries and reverse chronological sorting are stable", () => {
  const { h } = setup();
  const start = new Date(2026, 8, 24, 0, 0, 0).getTime();
  const calls = [
    { ...base, at: start },
    { ...base, at: start - 1 },
    { ...base, at: start + 3600000 },
  ];
  const selected = h.select(calls, "a", "2026-09-24");
  assert.equal(selected.length, 2);
  assert.equal(selected[0].at, start + 3600000);
  assert.equal(calls[0].at, start);
  assert.equal(h.dateKey(NaN), "");
});
test("custom calendar filters individual rows without statistics or quick filters", () => {
  const { h, node, emit } = setup([base]);
  const html = h.render("a");
  assert.match(html, /data-calendar-open="sip-history-date"/);
  assert.doesNotMatch(html, /type="date"/);
  assert.doesNotMatch(
    html,
    /sip-history-stats|sip-history-status|总通话|今天|昨天/,
  );
  assert.deepEqual(
    [...html.matchAll(/<th scope="col">([^<]+)<\/th>/g)].map((x) => x[1]),
    ["对方号码", "模块", "状态", "时长"],
  );
  emit("change", {
    id: "sip-history-date",
    value: h.dateKey(Date.now() - 86400000),
  });
  assert.match(node("#sip-history-rows").innerHTML, /暂无通话记录/);
  emit("change", { id: "sip-history-date", value: h.dateKey() });
  assert.match(node("#sip-history-rows").innerHTML, /10001/);
  const invalid = { id: "sip-history-date", value: "2026-02-30" };
  emit("change", invalid);
  assert.equal(invalid.value, h.dateKey());
});
test("SIP and cellular use the same iOS switch, native checkboxes stay hidden", () => {
  const dir = join(__dirname, "..");
  for (const file of ["sip.js", "cellular.js"]) {
    const js = readFileSync(join(dir, file), "utf8");
    assert.match(js, /class="form-switch"/);
    assert.match(js, /role="switch"/);
  }
  const css = readFileSync(join(dir, "forms.css"), "utf8");
  assert.match(
    css,
    /\.form-switch input \{[^}]*appearance: none;[^}]*opacity: 0;/,
  );
  assert.match(css, /background: #34c759;/);
});

test("footer totals connected outgoing duration for the selected account and date", () => {
  const today = new Date();
  today.setHours(12, 0, 0, 0);
  const yesterday = new Date(today);
  yesterday.setDate(yesterday.getDate() - 1);
  const records = [
    { ...base, at: today.getTime(), duration: 3601 },
    { ...base, at: today.getTime(), duration: 59 },
    { ...base, at: today.getTime(), duration: 999, status: "busy" },
    { ...base, at: today.getTime(), duration: 999, direction: "incoming" },
    { ...base, at: today.getTime(), duration: 999, sipAccountId: "other" },
    { ...base, at: yesterday.getTime(), duration: 75 },
  ];
  const { h, node, emit } = setup(records);
  assert.match(h.render("a"), /id="sip-history-duration">1小时2分/);
  emit("change", { id: "sip-history-date", value: h.dateKey(yesterday) });
  assert.equal(node("#sip-history-duration").textContent, "0小时2分");
  emit("change", { id: "sip-history-date", value: "2000-01-01" });
  assert.equal(node("#sip-history-duration").textContent, "0小时0分");
  emit("change", { id: "sip-history-date", value: h.dateKey(today) });
  assert.equal(node("#sip-history-duration").textContent, "1小时2分");
});
test("duration includes offscreen records, handles invalid seconds and never wraps at 24 hours", () => {
  const { h } = setup();
  assert.equal(
    h.totalDuration(
      Array.from({ length: 100 }, () => ({ ...base, duration: 3600 })),
    ),
    "100小时0分",
  );
  assert.equal(
    h.totalDuration([
      { ...base, duration: 61.9 },
      { ...base, duration: NaN },
      { ...base, duration: -1 },
      { ...base, duration: Infinity },
      { ...base, duration: "99" },
      { ...base, duration: 99, status: "failed" },
    ]),
    "0小时6分",
  );
  assert.equal(h.totalDuration([]), "0小时0分");
});

test("each connected call rounds up separately with a one-minute minimum", () => {
  const { h } = setup();
  for (const [duration, expected] of [
    [0, 1],
    [1, 1],
    [59, 1],
    [60, 1],
    [60.1, 2],
    [61, 2],
    [119, 2],
    [120, 2],
    [121, 3],
  ]) {
    assert.equal(h.billedMinutes({ ...base, duration }), expected);
  }
  assert.equal(
    h.totalDuration([
      { ...base, duration: 1 },
      { ...base, duration: 1 },
    ]),
    "0小时2分",
  );
  assert.equal(
    h.billedMinutes({ ...base, status: "failed", duration: 100 }),
    0,
  );
});
