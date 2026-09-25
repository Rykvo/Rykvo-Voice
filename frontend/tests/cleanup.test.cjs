const { test } = require("node:test");
const assert = require("node:assert/strict");
const vm = require("node:vm");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
const C = "rykvo-voice-calls-v1",
  M = "rykvo-voice-messages-v1",
  R = "rykvo-voice-retention-days-v1";
const now = Date.now(),
  day = 86400000;
const call = (id, at) => ({
  id,
  number: "13800000001",
  kind: "outgoing",
  duration: 10,
  at,
});
const message = (id, at) => ({ id, text: id, mine: false, at });
function setup(calls = [], threads = [], days = 0) {
  const storage = new Map([
    [C, JSON.stringify(calls)],
    [M, JSON.stringify(threads)],
    [R, JSON.stringify(days)],
    ["rykvo-voice-demo-calls-20-v1", "done"],
    ["rykvo-voice-global-numbers-v1", "true"],
    ["rykvo-voice-v1", '{"theme":"dark"}'],
  ]);
  const events = {},
    writes = [],
    timers = new Set();
  let fail = false,
    tick = 0;
  const nodes = new Map();
  for (const name of [
    "#toast",
    "#dialog",
    "#dialog-content",
    "#cleanup-days",
    "#cleanup-error",
  ])
    nodes.set(name, {
      value: "",
      hidden: true,
      style: {},
      focus() {},
      setAttribute() {},
      removeAttribute() {},
      close() {},
      showModal() {},
      classList: { add() {}, remove() {} },
    });
  const document = {
    hidden: false,
    querySelector: (s) => nodes.get(s) || null,
    querySelectorAll: () => [],
    addEventListener: (name, fn) => (events[name] ||= []).push(fn),
  };
  const context = vm.createContext({
    document,
    window: { addEventListener() {} },
    localStorage: {
      getItem: (key) => storage.get(key) ?? null,
      setItem(key, value) {
        if (fail) throw Error("quota");
        writes.push(key);
        storage.set(key, value);
      },
    },
    setTimeout(fn) {
      timers.add(++tick);
      return tick;
    },
    clearTimeout(id) {
      timers.delete(id);
    },
    SIPHistory: { update() {} },
  });
  for (const file of [
    "http.js",
    "shared.js",
    "countries.js",
    "module-data.js",
    "lines.js",
    "phone.js",
    "message-identity.js",
    "messages.js",
    "cleanup.js",
  ])
    vm.runInContext(readFileSync(join(__dirname, "..", file), "utf8"), context);
  const cleanup = vm.runInContext("Cleanup", context);
  return {
    cleanup,
    storage,
    writes,
    timers,
    nodes,
    document,
    read: (key) => JSON.parse(storage.get(key)),
    fail() {
      fail = true;
    },
    emit(name, target) {
      for (const fn of events[name] || []) fn({ target, preventDefault() {} });
    },
    html: () => vm.runInContext("Messages.render()", context),
  };
}
test("0 disables retention without touching records, settings or timers", () => {
  const app = setup([call("old", now - 100 * day)], []);
  app.cleanup.start();
  assert.equal(app.read(C).length, 1);
  assert.equal(app.writes.length, 0);
  assert.equal(app.timers.size, 0);
});
test("retention covers all accounts and directions, strictly older than the cutoff", () => {
  const records = [
    call("old", now - 3 * day),
    { ...call("incoming", now - 3 * day), kind: "incoming" },
    {
      ...call("sip-old", now - 3 * day),
      sipAccountId: "a",
      status: "connected",
    },
    {
      id: "sip-new",
      number: "555",
      direction: "outgoing",
      status: "failed",
      sipAccountId: "b",
      at: now,
    },
    call("edge", now - 2 * day),
    call("future", now + day),
    { id: "unknown", at: "invalid" },
  ];
  const app = setup(records, [], 2);
  assert.equal(app.cleanup.run(now), true);
  assert.deepEqual(
    app.read(C).map((x) => x.id),
    ["sip-new", "edge", "future", "unknown"],
  );
  assert.equal(app.storage.get("rykvo-voice-v1"), '{"theme":"dark"}');
  const count = app.writes.length;
  app.cleanup.run(now);
  assert.equal(app.writes.length, count);
});
test("SMS cleanup removes old messages and attachments, retaining recent messages and empty contacts", () => {
  const app = setup(
    [],
    [
      {
        id: "mixed",
        number: "13800000001",
        messages: [
          {
            ...message("old", now - 4 * day),
            image: "data:image/png;base64,AA==",
          },
          message("recent", now - day),
        ],
      },
      {
        id: "expired",
        number: "13800000002",
        messages: [message("old2", now - 5 * day)],
      },
      { id: "contact", number: "13800000003", messages: [] },
    ],
    2,
  );
  app.cleanup.run(now);
  assert.deepEqual(
    app.read(M).map((x) => x.id),
    ["mixed", "contact"],
  );
  assert.deepEqual(
    app.read(M)[0].messages.map((x) => x.id),
    ["recent"],
  );
  assert.doesNotMatch(app.html(), /data-msg-thread="expired"/);
});
test("failed record writes leave memory and persisted histories unchanged", () => {
  const app = setup(
    [call("old", now - 3 * day)],
    [
      {
        id: "old-thread",
        number: "13800000001",
        messages: [message("old", now - 3 * day)],
      },
    ],
    1,
  );
  app.fail();
  assert.equal(app.cleanup.run(now), false);
  assert.equal(app.read(C).length, 1);
  assert.equal(app.read(M).length, 1);
  assert.match(app.html(), /data-msg-thread="old-thread"/);
});
test("records beyond the previous 50-row limit survive unless actually expired", () => {
  const app = setup(
    Array.from({ length: 100 }, (_, i) =>
      call(String(i), now - (i === 99 ? 3 : 1) * day),
    ),
    [],
    2,
  );
  app.cleanup.run(now);
  assert.equal(app.read(C).length, 99);
});
test("invalid saved values default off and only nonnegative integer days are valid", () => {
  for (const value of [-1, 1.5, "7", null, Infinity, 36501]) {
    const app = setup([], [], value);
    assert.equal(app.cleanup.days(), 0);
    assert.equal(app.cleanup.valid(value), false);
  }
  for (const value of [0, 1, 365, 36500])
    assert.equal(setup().cleanup.valid(value), true);
});
test("settings open without clearing; invalid input is rejected; zero saves and cancels timer", () => {
  const app = setup([call("recent", now)], [], 10);
  app.cleanup.start();
  assert.equal(app.timers.size, 1);
  app.cleanup.open();
  assert.match(app.nodes.get("#dialog-content").innerHTML, /保留天数/);
  app.nodes.get("#cleanup-days").value = "-1";
  app.emit("submit", { id: "cleanup-form" });
  assert.equal(app.read(R), 10);
  assert.equal(app.nodes.get("#cleanup-error").hidden, false);
  app.nodes.get("#cleanup-days").value = "0";
  app.emit("submit", { id: "cleanup-form" });
  assert.equal(app.read(R), 0);
  // Toast is the only short-lived timer remaining.
  assert.equal(app.timers.size, 1);
  assert.equal(app.read(C).length, 1);
});
test("hidden pages suspend checks; returning to the page resumes one timer", () => {
  const app = setup([], [], 10);
  app.cleanup.start();
  app.document.hidden = true;
  app.emit("visibilitychange", {});
  assert.equal(app.timers.size, 0);
  app.document.hidden = false;
  app.emit("visibilitychange", {});
  assert.equal(app.timers.size, 1);
  app.emit("visibilitychange", {});
  assert.equal(app.timers.size, 1);
});
