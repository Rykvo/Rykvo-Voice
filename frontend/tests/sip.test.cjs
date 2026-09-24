const { test } = require("node:test");
const assert = require("node:assert/strict");
const vm = require("node:vm");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
function setup(hostname = "192.0.2.1", calls = []) {
  const events = {},
    nodes = new Map(),
    dialogs = [],
    errors = [];
  let confirmation,
    uid = 0;
  const node = (id) => {
    if (!nodes.has(id))
      nodes.set(id, {
        value: "",
        innerHTML: "",
        focus() {},
        addEventListener: (name, fn) =>
          (events["dialog-" + name] ||= []).push(fn),
        close() {
          for (const fn of events["dialog-close"] || []) fn();
        },
      });
    return nodes.get(id);
  };
  const emit = (type, target) => {
    for (const fn of events[type] || []) fn({ target, preventDefault() {} });
  };
  const context = vm.createContext({
    window: { addEventListener() {} },
    location: { hostname },
    crypto: { randomUUID: () => `sip-${++uid}` },
    document: {
      addEventListener: (name, fn) => (events[name] ||= []).push(fn),
      getElementById: (id) => node("#" + id),
    },
    ModuleData: {
      items: [
        { id: "module-01", name: "模块 01", number: "+8613800000001" },
        {
          id: "module-02",
          label: "办公室",
          name: "模块 02",
          number: "+8613800000002",
        },
      ],
    },
    Countries: { format: (value) => value },
    UI: {
      id: () => `sip-${++uid}`,
      read: () => calls,
      day: () => "今天",
      time: () => "12:00",
      $: node,
      escape: (value) =>
        String(value)
          .replaceAll("&", "&amp;")
          .replaceAll("<", "&lt;")
          .replaceAll('"', "&quot;"),
      modal: (...args) => dialogs.push(args),
      toast() {},
    },
    Forms: {
      field: ({ name, type = "text", value = "" }) =>
        `<input name="${name}" type="${type}" value="${String(value).replaceAll('"', "&quot;")}">`,
      report: (_, error) => {
        if (error) errors.push(error);
        return !!error;
      },
    },
    ContextMenu: { confirm: (...args) => (confirmation = args[1]) },
    fetch() {
      throw Error("Must not send credentials");
    },
    localStorage: {
      setItem() {
        throw Error("Must not persist passwords");
      },
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
  vm.runInContext(
    readFileSync(join(__dirname, "..", "sip.js"), "utf8"),
    context,
  );
  const sip = vm.runInContext("SIP", context);
  function click(selector, dataset = {}) {
    emit("click", {
      closest: (value) => (value === selector ? { dataset } : null),
    });
  }
  function submit(values) {
    const form = {
      id: "sip-form",
      elements: {
        namedItem: (name) => ({
          checked: values[name] === true,
          value: values[name] ?? (name === "allocation" ? "all" : ""),
        }),
      },
      querySelectorAll: () =>
        (values.moduleIds || []).map((value) => ({ value })),
      reset() {},
      remove() {
        nodes.delete("#sip-form");
      },
    };
    nodes.set("#sip-form", form);
    emit("submit", form);
  }
  // There is no form in the initial dialog.
  nodes.set("#sip-form", null);
  return {
    sip,
    node,
    dialogs,
    errors,
    click,
    submit,
    confirm: () => confirmation?.(),
    search(value) {
      emit("input", { id: "sip-search", value });
    },
    html: () => node("#sip-rows").innerHTML,
  };
}
const valid = {
  port: "5060",
  username: "1001",
  password: "test-secret",
};
test("SIP page includes search, create and the five requested columns", () => {
  const app = setup(),
    html = app.sip.render();
  assert.match(html, /搜索用户名/);
  assert.match(html, /data-sip-create>创建/);
  assert.deepEqual(
    [...html.matchAll(/<th scope="col">([^<]+)<\/th>/g)].map((x) => x[1]),
    ["IP", "用户名", "密码", "模块", "操作"],
  );
  assert.match(html, /暂无 SIP 账号/);
});
test("creation has username, password, numeric port and no server field or explanatory note", () => {
  const app = setup();
  app.click("[data-sip-create]");
  const html = app.dialogs[0][1];
  assert.deepEqual(
    [...html.matchAll(/name="([^"]+)"/g)].map((match) => match[1]),
    ["username", "password", "port"],
  );
  assert.match(html, /name="port"[^>]+value="5060"/);
  assert.match(html, /type="password"/);
  assert.doesNotMatch(
    html,
    /sip-note|仅当前预览|刷新后清除|尚未接入|name="server"/,
  );
});
test("port accepts 1–65535, rejects malformed input and requires username/password", () => {
  const { sip } = setup();
  for (const port of ["1", "5060", "65535", "05060"])
    assert.equal(sip.validate({ ...valid, port }), null);
  for (const port of [
    "",
    "0",
    "65536",
    "-1",
    "1.5",
    "5e3",
    "abc",
    "80:90",
    "123456",
  ])
    assert.equal(sip.validate({ ...valid, port })?.field, "port");
  assert.equal(sip.validate({ ...valid, username: " " })?.field, "username");
  assert.equal(sip.validate({ ...valid, password: "" })?.field, "password");
});

test("create masks credentials, shows endpoint and searches usernames only", () => {
  const app = setup();
  app.click("[data-sip-create]");
  app.submit(valid);
  assert.match(app.html(), /192.0.2.1<wbr>:5060<\/th>/);
  assert.match(app.html(), />全部<\/td>/);
  assert.doesNotMatch(app.html(), /data-sip-edit|data-sip-delete/);
  assert.match(app.html(), />详情<\/button>/);
  assert.doesNotMatch(app.html(), /未连接|sip-status/);
  assert.doesNotMatch(app.html(), /test-secret|已连接/);
  app.search("1001");
  assert.match(app.html(), /1001/);
  app.search("test-secret");
  assert.match(app.html(), /无匹配结果/);
  for (const query of ["192.0.2.1", "5060", "全部", "固定"]) {
    app.search(query);
    assert.match(app.html(), /无匹配结果/);
  }
  app.search("  1001  ");
  assert.match(app.html(), /1001/);
});
test("edit is scoped and duplicate usernames are rejected without exposing secrets", () => {
  const app = setup();
  app.click("[data-sip-create]");
  app.submit(valid);
  app.click("[data-sip-create]");
  app.submit(valid);
  assert.equal(app.errors.at(-1).field, "username");
  app.click("[data-sip-detail]", { sipDetail: "sip-1" });
  app.submit({ ...valid, username: "1002" });
  assert.match(app.html(), /1002/);
  assert.doesNotMatch(app.html(), />1001</);
});
test("delete waits for confirmation and only removes the selected username", () => {
  const app = setup();
  app.click("[data-sip-create]");
  app.submit(valid);
  app.click("[data-sip-create]");
  app.submit({ ...valid, username: "1002" });
  app.click("[data-sip-delete]", { sipDelete: "sip-1" });
  assert.match(app.html(), />1001</);
  app.confirm();
  assert.doesNotMatch(app.html(), />1001</);
  assert.match(app.html(), />1002</);
});
test("explicit port 80 survives normalization and untrusted username text is escaped", () => {
  const app = setup();
  app.click("[data-sip-create]");
  app.submit({ ...valid, port: "80", username: "<img>" });
  app.click("[data-sip-detail]", { sipDetail: "sip-1" });
  assert.match(app.dialogs.at(-1)[1], /name="port"[^>]+value="80"/);
  assert.match(app.html(), /&lt;img>/);
  assert.doesNotMatch(app.html(), /<img>/);
});

test("IP includes port, supports IPv6 and leaves unresolved domain IP blank", () => {
  for (const [host, expected] of [
    ["192.0.2.1", "192.0.2.1<wbr>:5060</th>"],
    ["[2001:db8::1]", "[2001:db8::1]<wbr>:5060</th>"],
    ["sip.example.com", "—</th>"],
    ["", "—"],
  ]) {
    const app = setup(host);
    app.click("[data-sip-create]");
    app.submit(valid);
    assert.ok(app.html().includes(expected));
  }
});
test("editing port retains module mode and is available in details", () => {
  const app = setup();
  app.click("[data-sip-create]");
  app.submit(valid);
  app.click("[data-sip-detail]", { sipDetail: "sip-1" });
  assert.match(app.dialogs.at(-1)[1], /name="port"[^>]+value="5060"/);
  app.submit({ ...valid, port: "5070" });
  assert.match(app.html(), /192.0.2.1<wbr>:5070<\/th>/);
  app.click("[data-sip-detail]", { sipDetail: "sip-1" });
  assert.match(app.dialogs.at(-1)[1], /name="port"[^>]+value="5070"/);
  assert.equal((app.html().match(/<tr>/g) || []).length, 1);
});

test("details edit the account, assign modules and scope call records", () => {
  const app = setup();
  app.click("[data-sip-create]");
  app.submit(valid);
  app.click("[data-sip-detail]", { sipDetail: "sip-1" });
  assert.equal(app.dialogs.at(-1)[0], "编辑 SIP 账号");
  const html = app.dialogs.at(-1)[1];
  assert.match(html, /分配模块/);
  assert.match(html, /value="all" checked/);
  assert.match(html, /value="fixed"/);
  assert.match(html, /办公室<\/strong><small>\+8613800000002/);
  assert.match(html, /通话记录/);
  assert.doesNotMatch(html, /暂无通话记录|<details/);
  assert.match(html, /data-sip-history/);
});
test("fixed mode requires a known module, saves only on submit and switches back to all", () => {
  const app = setup();
  app.click("[data-sip-create]");
  app.submit(valid);
  app.click("[data-sip-detail]", { sipDetail: "sip-1" });
  app.submit({ ...valid, allocation: "fixed", moduleIds: ["missing"] });
  assert.equal(
    app.sip.validate({ ...valid, allocation: "fixed", moduleIds: ["missing"] })
      .field,
    "moduleIds",
  );
  assert.match(app.html(), />全部<\/td>/);
  app.submit({
    ...valid,
    allocation: "fixed",
    moduleIds: ["module-01", "module-02"],
  });
  assert.match(app.html(), />固定<\/td>/);
  assert.doesNotMatch(app.html(), /办公室/);
  app.click("[data-sip-detail]", { sipDetail: "sip-1" });
  assert.match(app.dialogs.at(-1)[1], /value="module-02" checked/);
  assert.match(app.dialogs.at(-1)[1], /value="module-01" checked/);
  app.submit({ ...valid, allocation: "all" });
  assert.match(app.html(), />全部<\/td>/);
  app.click("[data-sip-detail]", { sipDetail: "sip-1" });
  assert.doesNotMatch(app.dialogs.at(-1)[1], /value="module-02" checked/);
});
test("history includes only explicitly associated SIP calls, never other module or account calls", () => {
  const app = setup("192.0.2.1", [
    {
      sipAccountId: "sip-1",
      number: "10001",
      at: Date.now(),
      kind: "outgoing",
    },
    { sipAccountId: "sip-2", number: "10002", at: Date.now() },
    { lineId: "module-01", number: "10003", at: Date.now() },
    {
      sipAccountId: "sip-1",
      number: "<img>",
      kind: "outgoing",
      at: Date.now(),
    },
    null,
  ]);
  const html = app.sip.historyHTML("sip-1");
  assert.match(html, /10001/);
  assert.match(html, /&lt;img>/);
  assert.doesNotMatch(html, /10002|10003|<img>/);
});

test("history opens a separate view and back restores the unsaved module selection", () => {
  const app = setup();
  app.click("[data-sip-create]");
  app.submit(valid);
  app.click("[data-sip-detail]", { sipDetail: "sip-1" });
  app.node("#sip-form").elements = {
    namedItem: (name) => ({
      value: { ...valid, username: "draft-user", allocation: "fixed" }[name],
    }),
  };
  app.node("#sip-form").querySelectorAll = () => [
    { value: "module-01" },
    { value: "module-02" },
  ];
  app.click("[data-sip-history]");
  assert.equal(app.dialogs.at(-1)[0], "通话记录");
  assert.match(app.dialogs.at(-1)[1], /暂无通话记录/);
  assert.doesNotMatch(app.dialogs.at(-1)[1], /sip-form/);
  app.click("[data-sip-history-back]");
  assert.equal(app.dialogs.at(-1)[0], "编辑 SIP 账号");
  assert.match(app.dialogs.at(-1)[1], /value="draft-user"/);
  assert.match(app.dialogs.at(-1)[1], /value="module-01" checked/);
  assert.match(app.dialogs.at(-1)[1], /value="module-02" checked/);
  assert.doesNotMatch(app.html(), /draft-user/);
});

test("larger SIP dialogs are scoped to editing and history, not creation", () => {
  const app = setup();
  app.click("[data-sip-create]");
  assert.match(app.dialogs.at(-1)[1], /data-sip-editor="create"/);
  app.submit(valid);
  app.click("[data-sip-detail]", { sipDetail: "sip-1" });
  assert.match(app.dialogs.at(-1)[1], /data-sip-editor="edit"/);
  const css = readFileSync(join(__dirname, "..", "sip.css"), "utf8");
  assert.match(css, /#dialog:has\(#sip-form\[data-sip-editor="edit"\]\)/);
  assert.match(css, /width: min\(680px, calc\(100% - 32px\)\)/);
});

test("receive switch defaults off, saves per account and is unaffected by history navigation", () => {
  const app = setup();
  app.click("[data-sip-create]");
  app.submit(valid);
  app.click("[data-sip-detail]", { sipDetail: "sip-1" });
  assert.match(
    app.dialogs.at(-1)[1],
    /name="receiveCalls" aria-label="接电话" >/,
  );
  app.submit({ ...valid, receiveCalls: true });
  app.click("[data-sip-detail]", { sipDetail: "sip-1" });
  assert.match(
    app.dialogs.at(-1)[1],
    /name="receiveCalls" aria-label="接电话" checked/,
  );
  app.node("#sip-form").elements = {
    namedItem: (name) => ({
      value: { ...valid, allocation: "all" }[name],
      checked: name === "receiveCalls",
    }),
  };
  app.node("#sip-form").querySelectorAll = () => [];
  app.click("[data-sip-history]");
  app.click("[data-sip-history-back]");
  assert.match(
    app.dialogs.at(-1)[1],
    /name="receiveCalls" aria-label="接电话" checked/,
  );
  app.click("[data-sip-create]");
  app.submit({ ...valid, username: "1002" });
  app.click("[data-sip-detail]", { sipDetail: "sip-2" });
  assert.doesNotMatch(
    app.dialogs.at(-1)[1],
    /name="receiveCalls" aria-label="接电话" checked/,
  );
  app.click("[data-sip-detail]", { sipDetail: "sip-1" });
  app.submit({ ...valid, receiveCalls: false });
  app.click("[data-sip-detail]", { sipDetail: "sip-1" });
  assert.doesNotMatch(
    app.dialogs.at(-1)[1],
    /name="receiveCalls" aria-label="接电话" checked/,
  );
});

test("username search ignores case and clearing restores the list", () => {
  const app = setup();
  app.click("[data-sip-create]");
  app.submit({ ...valid, username: "Office-A" });
  app.click("[data-sip-create]");
  app.submit({ ...valid, username: "Office-B" });
  app.search("office-a");
  assert.match(app.html(), /Office-A/);
  assert.doesNotMatch(app.html(), /Office-B/);
  app.search("");
  assert.match(app.html(), /Office-A/);
  assert.match(app.html(), /Office-B/);
});
