const { test } = require("node:test");
const assert = require("node:assert/strict");
const vm = require("node:vm");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
const source = readFileSync(join(__dirname, "..", "api.js"), "utf8");

function setup(mode = "all", base = "") {
  const requests = [],
    timers = new Map(),
    streams = [];
  let next = 0,
    respond = async () =>
      new Response(JSON.stringify({ data: { ok: true } }), {
        headers: { "Content-Type": "application/json" },
      });
  const context = vm.createContext({
    document: {
      querySelector: (selector) =>
        selector === "base"
          ? { getAttribute: () => base }
          : mode === "all" ||
              (mode === "session" && selector.includes('"session-api"')) ||
              (mode === "tunnel" && selector.includes('"tunnel-api"')) ||
              (mode === "sipNetwork" && selector.includes('"sip-network-api"')) ||
              (mode === "sipAccounts" && selector.includes('"sip-accounts-api"'))
            ? { content: "enabled" }
            : null,
    },
    URLSearchParams,
    FormData,
    AbortController,
    crypto: require("node:crypto").webcrypto,
    setTimeout(fn) {
      timers.set(++next, fn);
      return next;
    },
    clearTimeout(id) {
      timers.delete(id);
    },
    fetch(url, options) {
      requests.push({ url, ...options });
      return respond(url, options);
    },
    EventSource: class {
      constructor(url) {
        this.url = url;
        streams.push(this);
      }
      close() {
        this.closed = true;
      }
    },
  });
  vm.runInContext(
    readFileSync(join(__dirname, "..", "http.js"), "utf8") +
      "\n" +
      readFileSync(join(__dirname, "..", "shared.js"), "utf8"),
    context,
  );
  vm.runInContext(source, context);
  return {
    api: vm.runInContext("Backend", context),
    requests,
    timers,
    streams,
    respond(fn) {
      respond = fn;
    },
  };
}

test("public mount prefixes API and event stream while LAN keeps root", async () => {
  for (const base of ["", "/gly/"]) {
    const f = setup("all", base);
    await f.api.session.get();
    await f.api.tunnel.get();
    const unsubscribe = f.api.subscribe(() => {});
    const prefix = base ? "/gly" : "";
    assert.equal(f.requests[0].url, `${prefix}/api/session`);
    assert.equal(f.requests[1].url, `${prefix}/api/tunnel`);
    assert.equal(f.streams[0].url, `${prefix}/api/events`);
    unsubscribe();
  }
});

test("module API omits retired search and selection endpoints", () => {
  const { api } = setup();
  assert.equal(api.modules.networks, undefined);
  assert.equal(api.modules.scanNetworks, undefined);
  assert.equal(typeof api.modules.updateLine, "function");
  assert.equal(typeof api.modules.installESIM, "function");
});

test("static mode never sends requests or opens event streams", async () => {
  const f = setup("off");
  assert.equal(f.api.enabled(), false);
  await assert.rejects(f.api.modules.list(), { code: "NOT_CONNECTED" });
  assert.throws(() => f.api.subscribe(() => {}), { code: "NOT_CONNECTED" });
  assert.equal(f.requests.length, 0);
  assert.equal(f.streams.length, 0);
});

test("session-only mode does not enable unfinished business APIs", async () => {
  const f = setup("session");
  assert.equal(f.api.enabled("session"), true);
  assert.equal(f.api.enabled(), false);
  await f.api.session.get();
  await assert.rejects(f.api.modules.list(), { code: "NOT_CONNECTED" });
  await assert.rejects(f.api.tunnel.get(), { code: "NOT_CONNECTED" });
  assert.equal(f.requests.length, 1);
});
test("all feature groups expose callable frozen methods", async () => {
  const f = setup();
  const groups = [
    "session",
    "modules",
    "calls",
    "callRecords",
    "conversations",
    "messages",
    "attachments",
    "sip",
    "administrator",
    "developer",
    "retention",
    "preferences",
    "visibility",
    "updates",
    "jobs",
    "tunnel",
  ];
  const params = Object.fromEntries(
    [
      "moduleId",
      "lineId",
      "apnId",
      "callId",
      "recordId",
      "conversationId",
      "messageId",
      "attachmentId",
      "accountId",
      "jobId",
    ].map((id) => [id, "test-id"]),
  );
  for (const group of groups) {
    assert.ok(Object.isFrozen(f.api[group]));
    for (const fn of Object.values(f.api[group])) await fn({ params });
  }
  assert.ok(f.requests.length >= 50);
  assert.ok(
    f.requests.every((r) => r.url.startsWith("/api/") && !r.url.includes(":")),
  );
  assert.equal(f.timers.size, 0);
});
test("tunnel-only enablement does not enable unrelated endpoints", async () => {
  const f = setup("tunnel");
  await f.api.tunnel.get();
  await assert.rejects(f.api.sip.list(), { code: "NOT_CONNECTED" });
  assert.equal(f.requests.length, 1);
});
test("path and query parameters are encoded with same-origin credentials", async () => {
  const f = setup();
  await f.api.modules.get({
    params: { moduleId: "module/一" },
    query: { search: "a&b", cursor: "", limit: 0, unused: undefined },
  });
  assert.equal(
    f.requests[0].url,
    "/api/modules/module%2F%E4%B8%80?search=a%26b&limit=0",
  );
  assert.equal(f.requests[0].credentials, "same-origin");
  assert.equal(f.requests[0].cache, "no-store");
  assert.equal(f.requests[0].redirect, "error");
  for (const moduleId of [undefined, "", ".", ".."])
    await assert.rejects(f.api.modules.get({ params: { moduleId } }), {
      code: "INVALID_ARGUMENT",
    });
  await assert.rejects(f.api.modules.list({ body: {} }), {
    code: "INVALID_ARGUMENT",
  });
  assert.equal(f.requests.length, 1);
});
test("writes carry CSRF and reusable idempotency keys without storing secrets", async () => {
  const f = setup();
  f.api.setCSRFToken("test-csrf");
  await f.api.messages.send({
    body: { number: "+12025550123", text: "hello" },
    idempotencyKey: "same-operation",
  });
  const r = f.requests[0];
  assert.equal(r.headers["X-CSRF-Token"], "test-csrf");
  assert.equal(r.headers["Idempotency-Key"], "same-operation");
  assert.equal(r.headers["Content-Type"], "application/json");
  assert.equal(JSON.parse(r.body).text, "hello");
  f.api.setCSRFToken(null);
  await f.api.session.logout();
  assert.ok(f.requests[1].headers["Idempotency-Key"]);
  assert.equal(f.requests[1].headers["X-CSRF-Token"], undefined);
});
test("attachment uploads preserve multipart boundary generation", async () => {
  const f = setup(),
    body = new FormData();
  body.append("file", "test");
  await f.api.attachments.upload({ body });
  assert.equal(f.requests[0].body, body);
  assert.equal(f.requests[0].headers["Content-Type"], undefined);
});
test("204 and JSON envelopes are accepted, malformed responses rejected", async () => {
  const f = setup();
  f.respond(async () => new Response(null, { status: 204 }));
  assert.equal(await f.api.session.logout(), null);
  for (const body of ["broken", "null", '"text"', '{"error":{}}']) {
    f.respond(
      async () =>
        new Response(body, { headers: { "Content-Type": "application/json" } }),
    );
    await assert.rejects(f.api.session.get(), { code: "INVALID_RESPONSE" });
  }
  assert.equal(f.timers.size, 0);
});
test("HTTP errors keep status and field but never display server secrets", async () => {
  const f = setup();
  f.respond(
    async () =>
      new Response(
        JSON.stringify({
          error: {
            code: "DENIED",
            message: "private-token",
            field: "username",
          },
        }),
        { status: 403, headers: { "Content-Type": "application/json" } },
      ),
  );
  await assert.rejects(
    f.api.session.get(),
    (e) =>
      e.status === 403 &&
      e.code === "DENIED" &&
      e.field === "username" &&
      !e.message.includes("private-token"),
  );
  assert.equal(f.requests.length, 1);
});
test("timeouts and caller aborts do not retry writes and clear timers", async () => {
  const f = setup();
  f.respond(
    (url, options) =>
      new Promise((resolve, reject) =>
        options.signal.addEventListener("abort", () =>
          reject(new Error("aborted")),
        ),
      ),
  );
  const pending = f.api.messages.send({ body: {} });
  const checked = assert.rejects(pending, { code: "TIMEOUT" });
  [...f.timers.values()][0]();
  await checked;
  assert.equal(f.requests.length, 1);
  assert.equal(f.timers.size, 0);
  const controller = new AbortController();
  const aborted = assert.rejects(
    f.api.calls.list({ signal: controller.signal }),
    { code: "ABORTED" },
  );
  controller.abort();
  await aborted;
  await assert.rejects(f.api.calls.list({ signal: controller.signal }), {
    code: "ABORTED",
  });
  assert.equal(f.requests.length, 2);
  assert.equal(f.timers.size, 0);
});
test("serialization and network failures release request resources", async () => {
  const f = setup(),
    circular = {};
  circular.self = circular;
  await assert.rejects(f.api.messages.send({ body: circular }));
  assert.equal(f.timers.size, 0);
  assert.equal(f.requests.length, 0);
  f.respond(async () => {
    throw Error("offline");
  });
  await assert.rejects(f.api.calls.dial({ body: {} }), { code: "NETWORK" });
  assert.equal(f.requests.length, 1);
  assert.equal(f.timers.size, 0);
});
test("event subscribers share one connection and dispose independently", () => {
  const f = setup(),
    received = [],
    listener = (e) => received.push(e.id);
  const one = f.api.subscribe(listener),
    two = f.api.subscribe(listener);
  const bad = f.api.subscribe(() => {
    throw Error("view failed");
  });
  assert.equal(f.streams.length, 1);
  const stream = f.streams[0];
  stream.onmessage({ data: "bad" });
  stream.onmessage({ data: "{}" });
  stream.onmessage({ data: '{"id":"1","type":"call.updated"}' });
  assert.deepEqual(received, ["1", "1"]);
  one();
  one();
  stream.onmessage({ data: '{"id":"2","type":"call.updated"}' });
  assert.deepEqual(received, ["1", "1", "2"]);
  assert.equal(stream.closed, undefined);
  two();
  bad();
  assert.equal(stream.closed, true);
});

test("reserved integration scopes stay offline with existing production flags", async () => {
  for (const mode of ["session", "tunnel", "none"]) {
    const f = setup(mode);
    await assert.rejects(f.api.sipServer.connect({body:{address:"sip.example.com",accessCode:"test"}}), {code:"NOT_CONNECTED"});
    await assert.rejects(f.api.emergencyAddress.start({params:{moduleId:"module-03",lineId:"line-03"}}), {code:"NOT_CONNECTED"});
    assert.equal(f.requests.length, 0);
  }
});
test("reserved interface contracts retain line scope and credentials stay out of URL", async () => {
  const f = setup();
  await f.api.sipServer.connect({body:{address:"sip.example.com",accessCode:"secret-fixture"}});
  await f.api.emergencyAddress.start({params:{moduleId:"module-03",lineId:"line/03"}});
  assert.equal(f.requests[0].url, "/api/settings/sip-server/connect");
  assert.equal(JSON.parse(f.requests[0].body).accessCode, "secret-fixture");
  assert.equal(f.requests[1].url, "/api/modules/module-03/lines/line%2F03/emergency-address/session");
  assert.equal(f.requests[1].method, "POST");
});

 test("SIP network scope leaves host tunnel and phone APIs independent",async()=>{
 const f=setup("sipNetwork");await f.api.sipServer.get();await f.api.sipServer.logout({body:{}});
 assert.equal(f.requests[0].url,"/api/settings/sip-server");assert.equal(f.requests[1].url,"/api/settings/sip-server/logout");
 await assert.rejects(f.api.tunnel.connect({body:{}}),{code:"NOT_CONNECTED"});await assert.rejects(f.api.calls.list(),{code:"NOT_CONNECTED"});
 });

test("SIP account management does not enable calling or global tunnel APIs", async () => {
  const f = setup("sipAccounts");
  await f.api.sip.list();
  assert.equal(f.requests[0].url, "/api/sip/accounts");
  await assert.rejects(f.api.calls.dial({body:{number:"1001"}}),{code:"NOT_CONNECTED"});
  await assert.rejects(f.api.tunnel.connect({body:{}}),{code:"NOT_CONNECTED"});
});
