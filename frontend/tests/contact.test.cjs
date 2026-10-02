const { test } = require("node:test");
const assert = require("node:assert/strict");
const vm = require("node:vm");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
function setup() {
  const events = {}, requests = [], toasts = [];
  const input = { value: "", disabled: true, setAttribute() {}, removeAttribute() {}, focus() {} };
  const button = {}, error = { id: "contact-error" }, retry = { addEventListener: (_, fn) => { events.retry = fn; } };
  const form = { isConnected: true, elements: { namedItem: () => input },
    querySelector: s => s.includes("submit") ? button : s.includes("retry") ? retry : error,
    addEventListener: (name, fn) => { events[name] = fn; } };
  let get = async () => ({ url: "", revision: 1 });
  const context = vm.createContext({ URL, AbortController, UI: { $: () => form, toast: v => toasts.push(v), escape: v => String(v) },
    document: { addEventListener() {} }, Backend: { contact: {
      get: o => { requests.push(o); return get(o); },
      update: async o => { requests.push(o); return { url: o.body.url, revision: o.body.revision + 1 }; },
    } } });
  for (const file of ["forms.js", "settings-form.js", "contact-link.js", "contact-settings.js"]) vm.runInContext(readFileSync(join(__dirname,"..",file),"utf8"),context);
  return { api: vm.runInContext("ContactSettings",context), valid: vm.runInContext("ContactLink.valid",context), input, button, error, retry, form, requests, toasts,
    get(fn) { get = fn; }, submit() { return events.submit({ preventDefault() {} }); }, retryRead() { return events.retry(); } };
}
test("contact URL allows web, email and telephone only", () => {
  const f = setup();
  for (const url of ["", "https://example.com/help?a=1&b=2", "https://t.me/help", "http://example.com", "mailto:help@example.com", "tel:+12025550123"]) assert.equal(f.valid(url),true,url);
  for (const url of [null, "javascript:alert(1)", "data:text/html,a", "//example.com", "https:example.com", "https://", "mailto:", "tel:", "https://user:pass@example.com", "https://example.com/\\evil", "https://example.com/ x", "https://example.com/<script>", "https://example.com/"+"a".repeat(2048)]) assert.equal(f.valid(url),false,String(url));
});
test("contact edits load before saving, trim links and support clearing", async () => {
  const f = setup(); await f.api.mount(); f.input.value = " https://t.me/example "; await f.submit();
  assert.deepEqual(JSON.parse(JSON.stringify(f.requests[1].body)), { url: "https://t.me/example", revision: 1 });
  f.input.value = ""; await f.submit(); assert.equal(f.requests[2].body.url, ""); assert.equal(f.requests[2].body.revision, 2);
  assert.deepEqual(f.toasts, ["已保存", "已保存"]);
  assert.doesNotMatch(f.api.render(), /显示在登录页|留空隐藏|settings-note/);
});
test("unsafe contact never reaches the server; failed read can retry", async () => {
  const f = setup(); f.get(async () => { throw Error("offline"); }); await f.api.mount(); assert.equal(f.button.disabled, true);
  f.get(async () => ({ url: "", revision: 2 })); await f.retryRead(); f.input.value = "javascript:alert(1)";
  await f.submit(); assert.equal(f.requests.length, 2); assert.equal(f.toasts.length, 0); assert.match(f.error.textContent, /有效的联系链接/);
});
test("leaving contact settings aborts pending load", async () => {
  const f = setup(); let resolve; f.get(() => new Promise(r => { resolve = r; })); const loading = f.api.mount();
  f.api.unmount(); resolve({ url: "https://example.com", revision: 1 }); await loading;
  assert.equal(f.requests[0].signal.aborted, true); assert.equal(f.input.value, ""); assert.equal(f.button.disabled, true);
});
