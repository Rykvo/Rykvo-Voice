const { test } = require("node:test");
const assert = require("node:assert/strict");
const vm = require("node:vm");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");

function setup({ labels = {}, list, update } = {}) {
  const events = {}, timers = new Map(), calls = [];
  let timerId = 0;
  const context = vm.createContext({
    AbortController, Set, Map,
    setTimeout(fn) { const id = ++timerId; timers.set(id, fn); return id; },
    clearTimeout(id) { timers.delete(id); },
    UI: { read: () => labels },
    document: { hidden: false, addEventListener: (name, fn) => { events[name] = fn; } },
    window: { addEventListener: (name, fn) => { events[name] = fn; } },
    location: { replace: (url) => calls.push(["redirect", url]) },
    Http: { home: "/gly" },
    Backend: {
      enabled: (scope) => scope === "modules",
      modules: {
        list: (options) => { calls.push(["list", options]); return list ? list(options) : Promise.resolve({ items: [] }); },
        update: (options) => { calls.push(["update", options]); return update(options); },
      },
    },
  });
  vm.runInContext(readFileSync(join(__dirname, "../module-data.js"), "utf8"), context);
  return { data: vm.runInContext("ModuleData", context), context, events, timers, calls };
}
const settle = () => new Promise((resolve) => setImmediate(resolve));
const record = (label = "模块 01") => ({ id: "module-01", name: "模块 01", label, labelCustom: false, status: "online", sims: [], managed: true });

test("merge retains the shared array and object references, without a device quota", () => {
  const { data } = setup();
  const array = data.items;
  data.merge([record()]);
  const item = data.items[0];
  data.merge([record("工作卡"), ...Array.from({ length: 80 }, (_, i) => ({ ...record(), id: `module-${i + 2}` }))]);
  assert.equal(data.items, array); assert.equal(data.items[0], item);
  assert.equal(item.label, "工作卡"); assert.equal(data.items.length, 81);
});
test("duplicate labels include offline devices, but exclude the edited device", () => {
  const { data } = setup(); data.merge([{ ...record("工作卡"), status: "offline" }]);
  assert.equal(data.duplicate("module-02", " 工作卡 "), true);
  assert.equal(data.duplicate("module-01", "工作卡"), false);
  data.merge([record("ABC")]); assert.equal(data.duplicate("module-02", "Ａbc"), true);
});
test("subscriptions are removed without clearing account storage", () => {
  const { data } = setup(); let count = 0;
  const stop = data.subscribe(() => count++); data.merge([]); stop(); data.merge([]);
  assert.equal(count, 1);
});
test("start is idempotent and schedules only after the request finishes", async () => {
  let resolve; const s = setup({ list: () => new Promise((done) => { resolve = done; }) });
  s.data.start(); s.data.start(); assert.equal(s.calls.length, 1); assert.equal(s.timers.size, 0);
  resolve({ items: [record()] }); await settle(); assert.equal(s.timers.size, 1); assert.equal(s.data.loaded, true);
});
test("hiding cancels the request and discards a late response", async () => {
  let resolve; const s = setup({ list: () => new Promise((done) => { resolve = done; }) });
  s.data.start(); s.context.document.hidden = true; s.events.visibilitychange();
  assert.equal(s.calls[0][1].signal.aborted, true);
  resolve({ items: [record()] }); await settle(); assert.equal(s.data.items.length, 0); assert.equal(s.timers.size, 0);
});
test("saved labels win against an earlier list response", async () => {
  let resolve; const s = setup({ list: () => new Promise((done) => { resolve = done; }), update: async () => record("新标签") });
  s.data.merge([record()]); s.data.start(); await s.data.saveLabel(s.data.items[0], "新标签");
  resolve({ items: [record()] }); await settle(); assert.equal(s.data.items[0].label, "新标签"); assert.equal(s.timers.size, 1);
});
test("existing browser labels migrate once without deleting local storage", async () => {
  const saved = { "module-01": "旧标签" };
  const s = setup({ labels: saved, list: async () => ({ items: [record()] }), update: async (options) => { assert.equal(options.body.ifUnmodified, true); return record("旧标签"); } });
  s.data.start(); await settle(); assert.equal(s.data.items[0].label, "旧标签"); assert.equal(saved["module-01"], "旧标签");
  assert.equal(s.calls.filter(([kind]) => kind === "update").length, 1);
});
test("migration does not overwrite a server-owned custom label", async () => {
  const s = setup({ labels: { "module-01": "旧标签" }, list: async () => ({ items: [{ ...record("服务器标签"), labelCustom: true }] }) });
  s.data.start(); await settle(); assert.equal(s.calls.length, 1); assert.equal(s.data.items[0].label, "服务器标签");
});
test("failed refresh keeps known rows and exposes a connection issue", async () => {
  const s = setup({ list: async () => { throw new Error("network"); } }); s.data.merge([record()]); s.data.start(); await settle();
  assert.equal(s.data.items.length, 1); assert.equal(s.data.issue, "CONNECTION_FAILED");
});
test("authentication expiry redirects to the existing mounted entry", async () => {
  const s = setup({ list: async () => { throw Object.assign(new Error(), { status: 401 }); } });s.data.start();await settle();
  assert.ok(s.calls.some(([kind, url]) => kind === "redirect" && url === "/gly")); assert.equal(s.timers.size, 0);
});
