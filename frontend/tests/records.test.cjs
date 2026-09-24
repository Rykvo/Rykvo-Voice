const { test } = require("node:test");
const assert = require("node:assert/strict");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
const vm = require("node:vm");
const M = "rykvo-voice-messages-v1",
  C = "rykvo-voice-calls-v1",
  S = "rykvo-voice-sms-sender-v1";
function setup(storage = new Map()) {
  if (!storage.size) {
    storage.set(
      M,
      JSON.stringify(
        ["a", "b"].map((id, i) => ({
          id,
          number: "1380000000" + i,
          unread: false,
          senderId: "module-01",
          messages: [],
        })),
      ),
    );
    storage.set(
      C,
      JSON.stringify(
        ["a", "b"].map((id) => ({
          id,
          number: "13800000001",
          at: Date.now(),
          kind: "incoming",
          duration: 20,
        })),
      ),
    );
    storage.set("rykvo-voice-global-numbers-v1", "true");
    storage.set("rykvo-voice-demo-calls-20-v1", "done");
  }
  const events = {},
    nodes = new Map();
  let menu,
    confirm,
    fail = false,
    copied = "";
  const node = (selector) => {
    if (selector === ".messages-app") return null;
    if (!nodes.has(selector))
      nodes.set(selector, {
        value: "",
        innerHTML: "",
        style: {},
        dataset: {},
        focus() {},
        insertAdjacentHTML(_, html) {
          this.innerHTML += html;
        },
        classList: { add() {}, remove() {} },
      });
    return nodes.get(selector);
  };
  const context = vm.createContext({
    document: {
      querySelector: node,
      querySelectorAll: () => [],
      addEventListener: (name, fn) => (events[name] ??= []).push(fn),
    },
    window: { addEventListener() {} },
    navigator: {
      clipboard: {
        async writeText(text) {
          copied = text;
        },
      },
    },
    localStorage: {
      getItem: (key) => storage.get(key) ?? null,
      setItem: (key, value) => {
        if (fail) throw Error("quota");
        storage.set(key, value);
      },
    },
    ContextMenu: {
      open: (_, items) => {
        menu = items;
      },
      confirm: (title, action) => {
        confirm = { title, action };
      },
    },
    setTimeout: () => 0,
    clearTimeout() {},
    crypto: { randomUUID: () => "new-test-id" },
  });
  for (const file of [
    "http.js",
    "shared.js",
    "countries.js",
    "module-data.js",
    "tests/module-fixture.js",
    "lines.js",
    "phone.js",
    "messages.js",
  ])
    vm.runInContext(readFileSync(join(__dirname, "..", file), "utf8"), context);
  const emit = (type, target) =>
    (events[type] || []).forEach((fn) => fn({ target, preventDefault() {} }));
  return {
    storage,
    copied: () => copied,
    node,
    emit,
    read: (key) => JSON.parse(storage.get(key)),
    html: () => vm.runInContext("Messages.render()", context),
    fail: () => {
      fail = true;
    },
    action(name) {
      emit("click", {
        closest: (selector) =>
          selector === ".messages-app"
            ? {}
            : selector === "[data-msg-action]"
              ? { dataset: { msgAction: name } }
              : null,
      });
    },
    select(id) {
      emit("click", {
        closest: (selector) =>
          selector === ".messages-app"
            ? {}
            : selector === "[data-msg-thread]"
              ? { dataset: { msgThread: id } }
              : null,
      });
    },
    open(kind, id) {
      emit("contextmenu", {
        closest: (selector) =>
          selector ===
          (kind === "messages" ? "#msg-thread-list" : "#history-list")
            ? {}
            : id &&
                selector ===
                  (kind === "messages" ? "[data-msg-thread]" : "[data-call-id]")
              ? {
                  dataset:
                    kind === "messages" ? { msgThread: id } : { callId: id },
                }
              : null,
      });
      return menu;
    },
    confirm: () => confirm,
    changeSender(id) {
      node("#msg-from").value = id;
      emit("change", { id: "msg-from", value: id });
    },
    send(text) {
      node("#msg-input").value = text;
      emit("submit", { id: "ipad-message-form" });
    },
  };
}
test("conversation deletion is confirmed, scoped and persistent", () => {
  const app = setup(),
    calls = app.storage.get(C);
  app.open("messages", "a")[1].action();
  assert.equal(app.read(M).length, 2);
  app.confirm().action();
  assert.deepEqual(
    app.read(M).map((t) => t.id),
    ["b"],
  );
  assert.equal(app.storage.get(C), calls);
  assert.match(app.html(), /data-msg-thread="b" aria-pressed="true"/);
  app.open("messages", "b")[2].action();
  app.confirm().action();
  assert.match(app.html(), /msg-empty-state/);
  assert.doesNotMatch(app.html(), /id="msg-input"/);
  const reload = setup(app.storage);
  assert.deepEqual(reload.read(M), []);
  assert.match(reload.html(), /msg-empty-state/);
  reload.action("compose");
  assert.match(reload.html(), /id="msg-to"/);
  reload.action("cancel");
  assert.match(reload.html(), /msg-empty-state/);
});
test("deleting a different thread preserves the current conversation", () => {
  const app = setup();
  app.open("messages", "b")[1].action();
  app.confirm().action();
  assert.match(app.html(), /data-msg-thread="a" aria-pressed="true"/);
});
test("call deletion leaves conversations and migration flags intact", () => {
  const app = setup(),
    messages = app.storage.get(M);
  app.open("calls", "a")[1].action();
  app.confirm().action();
  assert.deepEqual(
    app.read(C).map((t) => t.id),
    ["b"],
  );
  app.open("calls", "b")[2].action();
  app.confirm().action();
  assert.deepEqual(setup(app.storage).read(C), []);
  assert.equal(app.storage.get(M), messages);
  assert.equal(app.node("#history-count").textContent, "0 条");
});
test("failed deletion keeps records in memory and storage", () => {
  for (const kind of ["messages", "calls"]) {
    const app = setup();
    app.fail();
    app.open(kind, "a")[2].action();
    app.confirm().action();
    assert.equal(app.read(kind === "messages" ? M : C).length, 2);
    assert.equal(app.open(kind, "a")[2].disabled, false);
  }
});
test("empty and background menus disable unavailable actions", () => {
  const app = setup();
  assert.equal(app.open("messages")[1].disabled, true);
  app.open("messages")[2].action();
  app.confirm().action();
  assert.equal(app.open("messages")[2].disabled, true);
});
test("only new messages show the sender picker; existing chats keep their original line", () => {
  const app = setup();
  assert.doesNotMatch(app.html(), /id="msg-from"/);
  app.action("compose");
  assert.match(app.html(), /id="msg-from"/);
  app.changeSender("module-04");
  app.node("#msg-to").value = "+12025550148";
  app.send("new line");
  assert.equal(app.read(M)[0].senderId, "module-04");
  assert.doesNotMatch(app.html(), /id="msg-from"/);
  app.action("compose");
  app.changeSender("module-01");
  app.action("cancel");
  app.send("same original line");
  assert.deepEqual(
    app.read(M)[0].messages.map((m) => m.senderId),
    ["module-04", "module-04"],
  );
  const reload = setup(app.storage);
  reload.send("after reload");
  assert.equal(reload.read(M)[0].messages.at(-1).senderId, "module-04");
  reload.select("a");
  reload.send("existing conversation");
  assert.equal(
    reload
      .read(M)
      .find((t) => t.id === "a")
      .messages.at(-1).senderId,
    "module-01",
  );
});
test("new message uses the selected line and an empty list can receive new threads", () => {
  const app = setup();
  app.open("messages")[2].action();
  app.confirm().action();
  app.action("compose");
  app.changeSender("module-04");
  app.node("#msg-to").value = "+1 202 555 0148";
  app.send("test");
  assert.equal(app.read(M).length, 1);
  assert.equal(app.read(M)[0].messages[0].senderId, "module-04");
});
test("invalid sender and failed preference storage keep a valid selection", () => {
  const app = setup();
  app.action("compose");
  app.changeSender("invalid");
  assert.match(app.html(), /value="random"/);
  app.fail();
  app.changeSender("module-04");
  assert.match(app.html(), /value="random"/);
});

test("menus copy raw numbers without changing records or opening confirmation", async () => {
  for (const kind of ["messages", "calls"]) {
    const app = setup();
    const before = [...app.storage.entries()];
    const entries = app.open(kind, "a");
    assert.deepEqual(
      Array.from(entries, (entry) => entry.label),
      ["复制", "删除", "全部删除"],
    );
    await entries[0].action();
    assert.equal(app.copied(), app.read(kind === "messages" ? M : C)[0].number);
    assert.deepEqual([...app.storage.entries()], before);
    assert.equal(app.confirm(), undefined);
    assert.equal(app.open(kind)[0].disabled, true);
    assert.equal(app.open(kind, "missing")[0].disabled, true);
  }
});

test("random new SMS stores a concrete module and retains it after reload", () => {
  const app = setup();
  app.action("compose");
  app.node("#msg-to").value = "+8613800009000";
  app.send("first");
  const id = app.read(M)[0].senderId;
  assert.match(id, /^module-\d{2}$/);
  const reload = setup(app.storage);
  reload.send("reply");
  assert.equal(reload.read(M)[0].messages.at(-1).senderId, id);
});
test("offline module does not create a conversation or discard the draft", () => {
  const app = setup();
  const before = app.read(M);
  app.action("compose");
  app.node("#msg-to").value = "+8613800009000";
  app.changeSender("module-02");
  app.send("keep draft");
  assert.deepEqual(app.read(M), before);
  assert.equal(app.node("#msg-input").value, "keep draft");
});
test("recomposing an existing number preserves its original sender", () => {
  const app = setup();
  app.action("compose");
  app.node("#msg-to").value = "+8613800009000";
  app.changeSender("module-04");
  app.send("first");
  app.action("compose");
  app.node("#msg-to").value = "+8613800009000";
  app.changeSender("module-01");
  app.send("again");
  assert.equal(app.read(M)[0].senderId, "module-04");
  assert.equal(app.read(M)[0].messages.length, 2);
});

test("fresh delivery ignores old preview records without deleting browser data", () => {
  const oldCalls = JSON.stringify([
    {
      id: "old",
      number: "55523",
      kind: "outgoing",
      duration: 4,
      at: Date.now(),
    },
  ]);
  const oldMessages = JSON.stringify([
    { id: "old", number: "123456", messages: [] },
  ]);
  const storage = new Map([
    ["apple-panel-calls-v1", oldCalls],
    ["apple-panel-messages-v1", oldMessages],
  ]);
  const app = setup(storage);
  assert.equal(app.open("phone")[2].disabled, true);
  assert.match(app.html(), /暂无信息/);
  assert.doesNotMatch(app.html(), /123456/);
  assert.equal(storage.get("apple-panel-calls-v1"), oldCalls);
  assert.equal(storage.get("apple-panel-messages-v1"), oldMessages);
});
