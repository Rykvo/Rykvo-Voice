const { test } = require("node:test");
const assert = require("node:assert/strict");
const vm = require("node:vm");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
function setup() {
  const requests = [], toasts = [], events = {};
  let get = async () => ({ uploadHours: 2, days: 3, revision: 1, issue: "" });
  let update = async ({ body }) => ({ uploadHours: body.uploadHours, days: body.days, revision: body.revision + 1 });
  const input = { value: "", disabled: true, attrs: {}, focus() { this.focused = true; }, setAttribute(k,v) { this.attrs[k]=v; }, removeAttribute(k) { delete this.attrs[k]; } };
  const hours = { ...input, attrs: {} };
  const retry = { hidden: true, disabled: false, addEventListener: (_, fn) => { events.retry = fn; } };
  const button = { disabled: true }, error = { hidden: true, textContent: "" };
  const form = { isConnected: true, elements: { namedItem: name => name === "uploadHours" ? hours : input }, querySelector: s => s.includes("submit") ? button : s === "[data-settings-retry]" ? retry : error,
    addEventListener: (name, fn) => { events[name] = fn; } };
  const context = vm.createContext({ AbortController, UI: { $: () => form, toast: value => toasts.push(value) },
    Backend: { retention: { get: options => { requests.push(options); return get(options); }, update: options => { requests.push(options); return update(options); } } },
    localStorage: { getItem() { throw Error("Must not read local retention"); }, setItem() { throw Error("Must not modify browser settings"); } },
    setTimeout() { throw Error("No browser cleanup timers"); }, Messages: { prune() { throw Error("No browser deletion"); } },
  });
  for (const file of ["settings-form.js", "cleanup.js"]) vm.runInContext(readFileSync(join(__dirname, "..", file), "utf8"), context);
  const api = vm.runInContext("Cleanup", context);
  return { api, input, hours, button, error, retry, form, retryRead() { return events.retry(); }, requests, toasts, get(fn) { get=fn; }, put(fn) { update=fn; }, submit() { return events.submit({ preventDefault() {} }); } };
}
test("server retention loads before save, supports zero and uses revision", async () => {
  const f=setup(); f.get(async () => ({ uploadHours: 2, days: 7, revision: 3 }));
  const mounted=f.api.mount(); assert.equal(f.input.disabled,true); await mounted;
  assert.equal(f.input.value,7); assert.equal(f.button.disabled,false);
  f.input.value="0"; await f.submit();
  assert.deepEqual(JSON.parse(JSON.stringify(f.requests[1].body)),{ uploadHours: 2, days: 0, revision: 3 });
  assert.deepEqual(f.toasts,["已保存"]); assert.equal(f.input.value,0);
  f.input.value="30"; await f.submit(); assert.equal(f.requests[2].body.revision,4);
});
test("invalid settings never trigger cleanup or save", async () => {
  const f=setup(); await f.api.mount();
  for(const value of ["", "-1", "1.5", "1e2", "36501"]) { f.input.value=value; await f.submit(); assert.equal(f.requests.length,1); assert.equal(f.error.hidden,false); }
  for(const value of [0,1,365,36500]) assert.equal(f.api.valid(value),true);
  for(const value of [-1,1.5,"7",NaN,null,36501]) assert.equal(f.api.valid(value),false);
});
test("concurrent settings conflict does not falsely report saved", async () => {
  const f=setup(); await f.api.mount(); f.put(async () => { throw {code:"SETTINGS_CHANGED"}; });
  f.input.value="2"; await f.submit(); assert.match(f.error.textContent,/重试读取/); assert.equal(f.button.disabled,true); assert.equal(f.toasts.length,0);
});
test("unmount aborts reads and prevents late changes", async () => {
  const f=setup(); let finish; f.get(() => new Promise(resolve => { finish=resolve; }));
  const work=f.api.mount(); f.api.unmount(); finish({ uploadHours: 2, days:7,revision:2 }); await work;
  assert.equal(f.requests[0].signal.aborted,true); assert.equal(f.input.value,""); assert.equal(f.button.disabled,true);
});
test("failure leaves saving disabled; page distinguishes temporary attachments from history", async () => {
  const f=setup(); f.get(async()=>{throw Error("offline");}); await f.api.mount();
  assert.equal(f.button.disabled,true); assert.match(f.error.textContent,/读取失败/);
  const html=f.api.render(); assert.doesNotMatch(html,/0 保留全部历史|cleanup-note/); assert.match(html,/临时附件/); assert.match(html,/name="uploadHours"/); assert.match(html,/>小时</); assert.match(html,/短信、彩信、通话与通知/); assert.match(html,/disabled/);
  assert.doesNotMatch(html,/缓存|localStorage|setTimeout/);
});

test("read failure retries in place without submitting a default", async () => {
  const f = setup(); f.get(async () => { throw Error("offline"); });
  await f.api.mount(); assert.equal(f.retry.hidden, false); assert.equal(f.input.disabled, true);
  f.input.value = "0"; await f.submit(); assert.equal(f.requests.length, 1);
  f.get(async () => ({ uploadHours: 2, days: 90, revision: 8 })); await f.retryRead();
  assert.equal(f.input.value, 90); assert.equal(f.retry.hidden, true); assert.equal(f.button.disabled, false);
  f.input.value = "30"; await f.submit(); assert.equal(f.requests[2].body.revision, 8);
});

test("ambiguous save requires a fresh read and never retries the write", async () => {
  const f = setup(); await f.api.mount();
  f.put(async () => { throw { code: "TIMEOUT" }; }); f.input.value = "14"; await f.submit();
  assert.equal(f.button.disabled, true); assert.equal(f.retry.hidden, false);
  await f.submit(); assert.equal(f.requests.length, 2); assert.equal(f.toasts.length, 0);
  f.get(async () => ({ uploadHours: 2, days: 14, revision: 2 })); await f.retryRead();
  assert.equal(f.input.value, 14); assert.equal(f.button.disabled, false); assert.equal(f.toasts.length, 0);
});

test("invalid revisions never enable save or claim success", async () => {
  for (const revision of [0, -1, 1.5, "1", undefined]) {
    const f = setup(); f.get(async () => ({ uploadHours: 2, days: 0, revision })); await f.api.mount();
    assert.equal(f.button.disabled, true); assert.equal(f.retry.hidden, false);
  }
  const f = setup(); await f.api.mount(); f.put(async () => ({ uploadHours: 2, days: 7, revision: 0 }));
  f.input.value = "7"; await f.submit(); assert.equal(f.toasts.length, 0); assert.equal(f.button.disabled, true);
});

test("double retry and submit stay single-flight; late save does not change a closed page", async () => {
  const f = setup(); let finish; f.get(() => new Promise(resolve => { finish = resolve; }));
  const mounting = f.api.mount(); await f.retryRead(); await f.submit(); assert.equal(f.requests.length, 1);
  finish({ uploadHours: 2, days: 0, revision: 1 }); await mounting;
  f.put(() => new Promise(resolve => { finish = resolve; })); f.input.value = "30";
  const saving = f.submit(); await f.submit(); await f.retryRead(); assert.equal(f.requests.length, 2);
  f.api.unmount(); finish({ uploadHours: 2, days: 30, revision: 2 }); await saving;
  assert.equal(f.requests[1].signal.aborted, true); assert.equal(f.toasts.length, 0);
});

test("worker issue remains visible without blocking retention configuration", async () => {
  const f = setup(); f.get(async () => ({ uploadHours: 2, days: 0, revision: 1, issue: "RETENTION_FAILED" }));
  await f.api.mount(); assert.match(f.error.textContent, /后台将重试/); assert.equal(f.button.disabled, false);
});


test("temporary attachment hours are independently editable and saved atomically with history", async () => {
  const f = setup(); await f.api.mount();
  assert.equal(f.hours.value, 2); assert.equal(f.hours.disabled, false);
  f.hours.value = "24"; f.input.value = "7"; await f.submit();
  assert.deepEqual(JSON.parse(JSON.stringify(f.requests[1].body)), { revision: 1, uploadHours: 24, days: 7 });
  assert.equal(f.hours.value, 24); assert.equal(f.input.value, 7);
  f.hours.value = "1"; await f.submit(); assert.equal(f.requests[2].body.revision, 2);
});

test("invalid or missing hours cannot overwrite either setting", async () => {
  const f = setup(); await f.api.mount();
  for (const value of ["", "0", "-1", "1.5", "1e2", "8761"]) {
    f.hours.value = value; await f.submit(); assert.equal(f.requests.length, 1); assert.equal(f.hours.attrs["aria-invalid"], "true");
  }
  for (const value of [1, 2, 24, 8760]) assert.equal(f.api.validHours(value), true);
  for (const value of [undefined, null, "2", 0, 8761, 1.5]) {
    const fixture = setup(); fixture.get(async () => ({ days: 3, uploadHours: value, revision: 1 })); await fixture.api.mount();
    assert.equal(fixture.button.disabled, true); assert.equal(fixture.hours.disabled, true); assert.equal(fixture.input.disabled, true);
  }
});

test("changed server values are not claimed as a successful save", async () => {
  const f = setup(); await f.api.mount();
  f.hours.value = "12"; f.put(async () => ({ days: 3, uploadHours: 2, revision: 2 })); await f.submit();
  assert.equal(f.toasts.length, 0); assert.equal(f.hours.disabled, true); assert.equal(f.retry.hidden, false);
});
