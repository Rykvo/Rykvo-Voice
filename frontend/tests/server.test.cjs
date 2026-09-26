const { test } = require("node:test");
const assert = require("node:assert/strict");
const vm = require("node:vm");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");

function setup(enabled = true) {
  const events = {},
    notices = [],
    requests = [],
    responses = [],
    opened = [],
    timers = new Map();
  let sequence = 0;
  const input = { value: "panel.example.com", readOnly: false };
  const button = { classList: { toggle() {} } };
  const reset = {},
    error = {};
  const indicator = { dataset: {}, lastElementChild: {} };
  const form = {
    id: "server-form",
    elements: { namedItem: () => input },
    setAttribute() {},
    querySelector: (selector) =>
      ({
        "#server-button": button,
        "#server-reset": reset,
        "#server-error": error,
        "#server-status": indicator,
      })[selector],
  };
  const context = vm.createContext({
    ServerNavigation: {header:()=>"服务器 主机服务器 SIP 电话服务器"},
    URL,
    URLSearchParams,
    FormData,
    crypto: require("node:crypto").webcrypto,
    AbortController,
    setTimeout: (fn, ms) => {
      timers.set(++sequence, { fn, ms });
      return sequence;
    },
    clearTimeout: (id) => timers.delete(id),
    document: {
      hidden: false,
      addEventListener: (name, fn) => (events[name] = fn),
      querySelector: () => (enabled ? { content: "enabled" } : null),
      getElementById: (id) => (id === "server-form" ? form : { close() {} }),
    },
    Forms: {
      header: () => "",
      field: () => "",
      report: (_, error) => Boolean(error),
    },
    UI: {
      id: () => require("node:crypto").randomUUID(),
      toast: (text) => notices.push(text),
      modal: (...args) => notices.push(args),
    },
    window: { open: (...args) => opened.push(args) },
    fetch: async (url, options) => {
      requests.push({ url, options });
      const value = responses.shift();
      if (value instanceof Error) throw value;
      return {
        ok: true,
        headers: { get: () => "application/json" },
        json: async () => value,
      };
    },
  });
  vm.runInContext(
    readFileSync(join(__dirname, "..", "http.js"), "utf8") +
      "\n" +
      readFileSync(join(__dirname, "..", "api.js"), "utf8"),
    context,
  );
  vm.runInContext(
    readFileSync(join(__dirname, "..", "server.js"), "utf8"),
    context,
  );
  return {
    server: vm.runInContext("Server", context),
    events,
    input,
    button,
    indicator,
    reset,
    error,
    form,
    requests,
    responses,
    notices,
    timers,
    opened,
  };
}
const settle = () => new Promise(setImmediate);
const submit = (fixture) =>
  fixture.events.submit({ target: fixture.form, preventDefault() {} });

test("domain normalization rejects URL, credentials, path, IP and malformed labels", () => {
  const { server } = setup();
  assert.equal(
    server.normalizeDomain(" Panel.Example.com. "),
    "panel.example.com",
  );
  assert.equal(server.normalizeDomain("例子.中国"), "xn--fsqu00a.xn--fiqs8s");
  for (const value of [
    "",
    "localhost",
    "https://example.com",
    "a@b.com",
    "example.com/a",
    "127.0.0.1",
    "a..com",
    "a_.com",
    "-a.com",
    "a.com:80",
    "a.com?x",
    "a".repeat(64) + ".com",
  ])
    assert.equal(server.normalizeDomain(value), "", value);
});
test("authorization URL must use exact Cloudflare HTTPS host", () => {
  const { server } = setup();
  assert.equal(
    server.authURL("https://dash.cloudflare.com/argotunnel?token=fixture"),
    "https://dash.cloudflare.com/argotunnel?token=fixture",
  );
  for (const url of [
    "javascript:alert(1)",
    "https://dash.cloudflare.com.evil.test",
    "http://dash.cloudflare.com",
    "https://user@dash.cloudflare.com",
    "https://dash.cloudflare.com:8443",
    "/login",
  ])
    assert.equal(server.authURL(url), "");
});
test("rejects malformed backend state rather than claiming success", () => {
  const { server } = setup();
  for (const value of [
    null,
    {},
    { status: "unknown" },
    { status: "connected" },
    {
      status: "authorizing",
      domain: "example.com",
      authorizationUrl: "https://evil.test",
    },
  ])
    assert.throws(() => server.parseState(value));
});
test("static mode does not make requests or pretend to connect", async () => {
  const f = setup(false);
  f.server.mount();
  submit(f);
  await settle();
  assert.equal(f.indicator.lastElementChild.textContent, "未接入");
  assert.equal(f.button.textContent, "连接");
  assert.equal(f.requests.length, 0);
  assert.deepEqual(f.notices, ["服务器暂不可用"]);
  f.server.unmount();
});
test("one button transitions through connect, authorize and disconnect", async () => {
  const f = setup();
  f.responses.push({ status: "disconnected" });
  f.server.mount();
  await settle();
  assert.equal(f.button.textContent, "连接");
  f.responses.push({
    status: "authorizing",
    domain: "panel.example.com",
    authorizationUrl: "https://dash.cloudflare.com/argotunnel?fixture",
  });
  submit(f);
  submit(f);
  await settle();
  assert.equal(f.requests.filter((r) => r.options.method === "POST").length, 1);
  assert.equal(f.button.textContent, "授权");
  assert.equal(f.input.readOnly, true);
  submit(f);
  await settle();
  assert.equal(f.opened.length, 1);
  assert.equal(f.opened[0][2], "noopener,noreferrer");
  f.responses.push({ status: "connected", domain: "panel.example.com" });
  await [...f.timers.values()].find((t) => t.ms === 2500).fn();
  assert.equal(f.button.textContent, "注销");
  submit(f);
  assert.equal(f.notices.at(-1)[0], "注销云连接？");
  assert.match(f.notices.at(-1)[1], /class="confirm-actions"/);
  assert.doesNotMatch(f.notices.at(-1)[1], /<p>|server-confirm/);
  assert.equal(f.requests.filter((r) => r.options.method === "POST").length, 1);
  f.events.click({
    target: { closest: (selector) => selector === "[data-server-cancel]" },
  });
  assert.equal(f.requests.filter((r) => r.options.method === "POST").length, 1);
  assert.equal(f.button.textContent, "注销");
  submit(f);
  f.responses.push({ status: "disconnected", domain: "" });
  f.events.click({
    target: { closest: (selector) => selector === "[data-server-disconnect]" },
  });
  await settle();
  assert.equal(f.button.textContent, "连接");
  assert.equal(f.input.value, "");
  assert.equal(f.input.readOnly, false);
  assert.equal(f.timers.size, 0);
});
test("disconnect failure preserves binding and allows retry", async () => {
  const f = setup();
  f.responses.push({ status: "connected", domain: "panel.example.com" });
  f.server.mount();
  await settle();
  f.responses.push(new Error("Network offline"));
  f.events.click({
    target: { closest: (selector) => selector === "[data-server-disconnect]" },
  });
  await settle();
  assert.equal(f.button.textContent, "注销");
  assert.equal(f.input.readOnly, true);
  assert.equal(f.input.value, "panel.example.com");
  f.server.unmount();
  assert.equal(f.timers.size, 0);
});
test("mount/unmount clears pending authorization polling", async () => {
  const f = setup();
  f.responses.push({
    status: "authorizing",
    domain: "panel.example.com",
    authorizationUrl: "https://dash.cloudflare.com/argotunnel?fixture",
  });
  f.server.mount();
  await settle();
  assert.equal(f.timers.size, 1);
  f.server.unmount();
  assert.equal(f.timers.size, 0);
});

test("failed connection keeps its domain and offers retry or cancellation", async () => {
  const f = setup();
  f.responses.push({
    status: "failed",
    domain: "panel.example.com",
    errorCode: "DNS_CONFLICT",
  });
  f.server.mount();
  await settle();
  assert.equal(f.button.textContent, "重试");
  assert.equal(f.input.readOnly, true);
  assert.equal(f.reset.hidden, false);
  assert.match(f.error.textContent, /DNS/);
  assert.equal(f.timers.size, 0);
  f.responses.push({ status: "disconnecting", domain: "panel.example.com" });
  f.events.click({
    target: { closest: (selector) => selector === "#server-reset" },
  });
  await settle();
  assert.equal(f.button.disabled, true);
  assert.equal(f.reset.hidden, true);
  f.responses.push({ status: "disconnected", domain: "" });
  await [...f.timers.values()].find((t) => t.ms === 2500).fn();
  assert.equal(f.input.value, "");
  assert.equal(f.button.textContent, "连接");
  assert.equal(f.error.hidden, true);
  f.server.unmount();
});

test("interrupted cleanup retries disconnect, never reconnects", async () => {
  const f = setup();
  f.responses.push({
    status: "failed",
    domain: "panel.example.com",
    errorCode: "DISCONNECT_FAILED",
  });
  f.server.mount();
  await settle();
  assert.equal(f.button.textContent, "重试注销");
  assert.equal(f.indicator.lastElementChild.textContent, "注销未完成");
  assert.equal(f.reset.hidden, true);
  f.responses.push({ status: "disconnecting", domain: "panel.example.com" });
  submit(f);
  await settle();
  assert.equal(f.requests.at(-1).url, "/api/tunnel/disconnect");
  f.server.unmount();
});

test("connected status is periodically verified and unmount cancels the timer", async () => {
  const f = setup();
  f.responses.push({ status: "connected", domain: "panel.example.com" });
  f.server.mount();
  await settle();
  assert.equal([...f.timers.values()][0].ms, 15000);
  f.server.unmount();
  assert.equal(f.timers.size, 0);
});

test("cleanup failure shows a concise specific cause", async () => {
  const f = setup();
  f.responses.push({
    status: "failed",
    domain: "panel.example.com",
    errorCode: "DISCONNECT_FAILED",
    cleanupCode: "CLOUDFLARE_PERMISSION",
  });
  f.server.mount();
  await settle();
  assert.equal(f.error.textContent, "Cloudflare 授权不足，请检查授权");
  assert.equal(f.indicator.lastElementChild.textContent, "注销未完成");
  assert.equal(f.button.textContent, "重试注销");
  f.server.unmount();
});

test("temporary status failure recovers without replaying disconnect", async () => {
  const f = setup();
  f.responses.push({ status: "disconnecting", domain: "panel.example.com" });
  f.server.mount();
  await settle();
  f.responses.push(new Error("Offline"));
  await [...f.timers.values()][0].fn();
  assert.equal([...f.timers.values()][0].ms, 5000);
  f.responses.push({ status: "disconnected", domain: "" });
  await [...f.timers.values()][0].fn();
  assert.equal(f.button.textContent, "连接");
  assert.equal(f.input.value, "");
  assert.equal(f.timers.size, 0);
  assert.equal(f.requests.filter((r) => r.options.method === "POST").length, 0);
  f.server.unmount();
});

test("repeated status failures stop background retries", async () => {
  const f = setup();
  f.responses.push({ status: "connected", domain: "panel.example.com" });
  f.server.mount();
  await settle();
  for (let i = 0; i < 5; i++) {
    f.responses.push(new Error("Offline"));
    await [...f.timers.values()][0].fn();
  }
  assert.equal(f.timers.size, 0);
  f.server.unmount();
});

test("lost disconnect response only retries status reads", async () => {
  const f = setup();
  f.responses.push({ status: "connected", domain: "panel.example.com" });
  f.server.mount();
  await settle();
  f.responses.push(new Error("Response lost"));
  f.events.click({
    target: { closest: (s) => s === "[data-server-disconnect]" },
  });
  await settle();
  f.responses.push({ status: "disconnected", domain: "" });
  await [...f.timers.values()][0].fn();
  assert.equal(f.requests.filter((r) => r.options.method === "POST").length, 1);
  assert.equal(f.button.textContent, "连接");
  assert.equal(f.timers.size, 0);
  f.server.unmount();
});
