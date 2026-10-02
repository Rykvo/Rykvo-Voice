const { test } = require("node:test");
const assert = require("node:assert/strict");
const vm = require("node:vm");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
const tick = () => new Promise(resolve => setImmediate(resolve));
function setup() {
  const events = {}, toasts = [], reads = [], requests = [];
  let get = async () => ({ hostname: "rykvo", editable: true });
  let put = async options => ({ hostname: options.body.hostname, editable: true });
  let form;
  const node = () => ({
    value: "", disabled: false, textContent: "", attributes: {}, focus() { this.focused = true; },
    setAttribute(name, value) { this.attributes[name] = value; },
    removeAttribute(name) { delete this.attributes[name]; },
  });
  const buttons = ["hostname", "cleanup"].map(hostTab => ({ ...node(), dataset: { hostTab } }));
  const panel = {
    set innerHTML(html) {
      this.html = html;
      if (form) form.isConnected = false;
      form = null;
      if (!html.includes('id="hostname-form"')) return;
      const input = node(), button = node(), error = node(), listeners = {};
      button.disabled = true;
      form = {
        input, button, error, isConnected: true,
        elements: { namedItem: () => input },
        querySelector: selector => selector.includes("submit") ? button : error,
        addEventListener: (event, handler) => { listeners[event] = handler; },
        submit: () => listeners.submit({ preventDefault() {} }),
        edit(value) { input.value = value; listeners.input(); },
      };
    },
  };
  const context = vm.createContext({
    AbortController,
    document: {
      addEventListener: (key, fn) => { (events[key] ||= []).push(fn); },
      querySelectorAll: () => buttons,
    },
    ContactSettings: { render: () => '<form id="contact-form"></form>', mount() {}, unmount() {} },
    Cleanup: { mount() {}, unmount() {}, render: () => '<form id="cleanup-form"><input value="7"></form>' },
    Backend: { hostname: {
      get: options => { reads.push(options); return get(options); },
      update: options => { requests.push(options); return put(options); },
    } },
    UI: {
      escape: text => String(text).replaceAll("&", "&amp;").replaceAll('"', "&quot;").replaceAll("<", "&lt;"),
      $: selector => selector === "#host-settings-panel" ? panel : form,
      toast: text => toasts.push(text),
    },
  });
  for (const name of ["forms.js", "host-settings.js"]) {
    vm.runInContext(readFileSync(join(__dirname, "..", name), "utf8"), context);
  }
  const api = vm.runInContext("HostSettings", context);
  return {
    api, panel, buttons, toasts, reads, requests,
    get form() { return form; },
    get(fn) { get = fn; }, put(fn) { put = fn; },
    open() { panel.innerHTML = api.render(); return api.mount(); },
    click(action) {
      for (const fn of events.click || []) fn({ target: { closest: () => ({ dataset: { hostTab: action } }) } });
    },
  };
}

test("host settings is a compact page with shared tabs, hidden accessible label and no explanatory paragraph", () => {
  const f = setup(), html = f.api.render();
  assert.match(html, /host-settings-page/);
  assert.match(html, /class="settings-tabs"/);
  assert.match(html, /data-host-tab="hostname" aria-pressed="true"/);
  assert.match(html, /data-host-tab="cleanup" aria-pressed="false"/);
  assert.match(html, /data-host-tab="contact" aria-pressed="false"/);
  assert.match(html, /<label for="host-hostname" class="sr-only">主机名<\/label>/);
  assert.match(html, /data-action="general-back">取消/);
  assert.match(html, />保存<\/button>/);
  assert.doesNotMatch(html, /重启|字母|连字符|路由器|dialog|modal|主机名格式/);
  assert.equal(f.requests.length, 0);
  assert.equal(f.reads.length, 0);
});

test("hostname reads real value before enabling save and tracks the last confirmed name", async () => {
  const f = setup(), pending = f.open();
  assert.equal(f.form.input.disabled, true);
  assert.equal(f.form.button.disabled, true);
  await pending;
  assert.equal(f.form.input.value, "rykvo");
  assert.equal(f.form.button.disabled, false);
  f.form.edit(" RYKVO-02 ");
  await f.form.submit();
  assert.equal(JSON.stringify(f.requests[0].body), '{"hostname":"RYKVO-02","expected":"rykvo"}');
  assert.deepEqual(f.toasts, ["已保存"]);
  assert.equal(f.form.input.value, "RYKVO-02");
  f.form.edit("rykvo-03");
  await f.form.submit();
  assert.equal(f.requests[1].body.expected, "RYKVO-02");
});

test("case-only changes preserve the confirmed spelling and detect normalized responses", async () => {
  const f = setup(); f.get(async () => ({ hostname: "a01", editable: true }));
  await f.open();
  f.form.edit("A01"); await f.form.submit();
  assert.equal(f.requests[0].body.expected, "a01");
  assert.equal(f.requests[0].body.hostname, "A01");
  assert.equal(f.form.input.value, "A01");
  f.form.edit("Host-01");
  f.put(async () => ({ hostname: "host-01", editable: true }));
  await f.form.submit();
  assert.equal(f.requests[1].body.expected, "A01");
  assert.deepEqual(f.toasts, ["已保存"]);
  assert.match(f.form.error.textContent, /保存未确认/);
});

test("invalid names show only the requested error and never reach the backend", async () => {
  const f = setup(); await f.open();
  for (const value of ["", "-abc", "abc-", "a.b", "123", "localhost", "LOCALHOST", "LocalHost", "中文", "İ01", "x;id", "a".repeat(64)]) {
    assert.equal(f.api.valid(value), false);
    f.form.edit(value); await f.form.submit();
    assert.equal(f.form.error.textContent, "主机名格式不支持，请重新填写。");
    assert.equal(f.form.input.attributes["aria-invalid"], "true");
    assert.equal(f.form.input.focused, true);
  }
  f.form.edit("rykvo-02");
  assert.equal(f.form.error.textContent, "");
  assert.equal(f.form.input.attributes["aria-invalid"], undefined);
  assert.equal(f.requests.length, 0);
  for (const value of ["a", "A", "a".repeat(63), "A".repeat(63), "host-42", "A01", "a01", "Host-01"]) assert.equal(f.api.valid(value), true);
});

test("read failures and unsupported hosts leave saving disabled", async () => {
  for (const get of [
    async () => { throw Error("offline"); },
    async () => ({ hostname: "rykvo", editable: false }),
    async () => ({}),
  ]) {
    const f = setup(); f.get(get); await f.open();
    assert.equal(f.form.button.disabled, true);
    assert.equal(f.form.input.disabled, true);
    await f.form.submit(); assert.equal(f.requests.length, 0);
    assert.ok(f.form.error.textContent);
  }
});

test("double submit sends one mutation; conflict does not claim success", async () => {
  const f = setup(); await f.open();
  let fail; f.put(() => new Promise((_resolve, reject) => { fail = reject; }));
  f.form.edit("rykvo-02");
  const pending = f.form.submit(); await f.form.submit();
  assert.equal(f.requests.length, 1);
  fail({ code: "HOSTNAME_CONFLICT" }); await pending;
  assert.equal(f.toasts.length, 0);
  assert.match(f.form.error.textContent, /重新进入/);
  assert.equal(f.form.input.disabled, false);
  assert.equal(f.form.button.textContent, "保存");
});

test("malformed success is not presented as saved and backend validation keeps the exact wording", async () => {
  for (const put of [
    async () => ({}),
    async () => { throw { code: "INVALID_HOSTNAME" }; },
  ]) {
    const f = setup(); await f.open(); f.put(put);
    f.form.edit("rykvo-02"); await f.form.submit();
    assert.equal(f.toasts.length, 0);
    assert.match(f.form.error.textContent, /保存未确认|主机名格式不支持，请重新填写。/);
  }
});

test("same tab preserves edits and does not refetch; switching retains cleanup values without saving", async () => {
  const f = setup(); await f.open();
  f.form.edit("draft"); f.click("hostname"); f.click("invalid");
  assert.equal(f.form.input.value, "draft");
  assert.equal(f.reads.length, 1);
  f.click("cleanup");
  assert.match(f.panel.html, /value="7"/);
  assert.equal(f.buttons[1].attributes["aria-pressed"], "true");
  assert.equal(f.requests.length, 0);
  assert.equal(f.reads[0].signal.aborted, true);
  f.click("hostname"); await tick();
  assert.equal(f.reads.length, 2);
  assert.equal(f.form.input.value, "rykvo");
});

test("late read after tab switch cannot overwrite a fresh view", async () => {
  const f = setup(); let done;
  f.get(() => new Promise(resolve => { done = resolve; }));
  const pending = f.open(), old = f.form;
  f.click("cleanup");
  f.get(async () => ({ hostname: "fresh", editable: true }));
  f.click("hostname"); await tick();
  done({ hostname: "stale", editable: true }); await pending;
  assert.equal(old.input.value, "");
  assert.equal(f.form.input.value, "fresh");
});

test("unmount or tab switch aborts save and suppresses stale toast or updates", async () => {
  for (const leave of [f => f.api.unmount(), f => f.click("cleanup")]) {
    const f = setup(); await f.open(); let done;
    f.put(() => new Promise(resolve => { done = resolve; }));
    f.form.edit("rykvo-02"); const pending = f.form.submit();
    leave(f);
    assert.equal(f.requests[0].signal.aborted, true);
    done({ hostname: "rykvo-02" }); await pending;
    assert.equal(f.toasts.length, 0);
  }
});

test("unmount aborts pending read and never enables the detached form", async () => {
  const f = setup(); let done;
  f.get(() => new Promise(resolve => { done = resolve; }));
  const pending = f.open(), old = f.form;
  f.api.unmount(); old.isConnected = false;
  done({ hostname: "stale", editable: true }); await pending;
  assert.equal(f.reads[0].signal.aborted, true);
  assert.equal(old.input.value, "");
  assert.equal(old.button.disabled, true);
});
