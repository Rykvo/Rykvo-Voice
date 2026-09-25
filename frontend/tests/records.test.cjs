const { test } = require("node:test");
const assert = require("node:assert/strict");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
const vm = require("node:vm");
const M = "rykvo-voice-messages-v1",
  C = "rykvo-voice-calls-v1",
  S = "rykvo-voice-sms-sender-v1";
function setup(storage = new Map(), mounted = false) {
 let remoteEnabled=false, listener=null, revision=0;const remoteRecords=[], contacts=[], requests=[];
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
    if (selector === ".messages-app" && !mounted) return null;
    if (!nodes.has(selector))
      nodes.set(selector, {
        value: "",
        innerHTML: "",
        style: {},
        dataset: {},
        focus() {}, showModal() {}, close() {}, setAttribute() {},
        querySelector(){return node("note-save")}, isConnected:true,
        insertAdjacentHTML(_, html) {
          this.innerHTML += html;
        },
        classList: { add() {}, remove() {}, toggle() {} },
      });
    return nodes.get(selector);
  };
  const context = vm.createContext({
    MessageData: {
      enabled:()=>remoteEnabled,
      subscribe(fn){listener=fn;fn(remoteRecords,contacts);return ()=>{listener=null}},
      async send(body){requests.push(body);const result={id:"server-"+(++revision),number:body.to,senderId:body.moduleId,lineId:body.lineId,text:body.text,mine:true,kind:"sms",state:"queued",at:Date.now(),revision,remote:true};remoteRecords.push(result);listener?.(remoteRecords,contacts);return result;},
      async saveContact(body){const result={...body,revision:++revision};contacts.push(result);listener?.(remoteRecords,contacts);return result;},
      async remove(ids){for(let i=remoteRecords.length-1;i>=0;i--)if(ids.includes(remoteRecords[i].id))remoteRecords.splice(i,1);listener?.(remoteRecords,contacts)}
    },
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
    "message-identity.js",
    "messages.js",
  ])
    vm.runInContext(readFileSync(join(__dirname, "..", file), "utf8"), context);
  const emit = (type, target) =>
    (events[type] || []).forEach((fn) => fn({ target, preventDefault() {} }));
  return {
    storage, requests,
    publish(items){remoteRecords.push(...items.map((item,i)=>({id:"incoming-"+remoteRecords.length+"-"+i,at:i,text:"message "+i,mine:false,remote:true,senderId:"module-01",lineId:"line-module-01-0",state:"received",revision:++revision,...item})));listener?.(remoteRecords,contacts)},
    async note(name){node("#msg-note-name").value=name;emit("submit",{...node("note-form"),id:"message-note-form"});await new Promise(setImmediate)},
    server(){remoteEnabled=true;vm.runInContext('ModuleData.items.forEach(item=>item.sims.forEach((sim,i)=>sim.id="line-"+item.id+"-"+i));Messages.mount()',context)},
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
      return new Promise(resolve=>setImmediate(resolve));
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
test("new SMS uses server queue and existing chat keeps the concrete SIM",async()=>{
 const app=setup();app.server();assert.doesNotMatch(app.html(),/id="msg-from"/);app.action("compose");assert.match(app.html(),/id="msg-from"/);app.changeSender("module-04");app.node("#msg-to").value="+12025550148";
 const old=app.storage.get(M);await app.send("new line");assert.equal(app.requests[0].moduleId,"module-04");assert.equal(app.requests[0].lineId,"line-module-04-0");assert.equal(app.storage.get(M),old);assert.doesNotMatch(app.html(),/id="msg-from"/);
 await app.send("same SIM");assert.equal(app.requests[1].moduleId,"module-04");assert.equal(app.requests[1].lineId,"line-module-04-0");
});

test("disconnected message backend never adds a fake sent bubble",async()=>{
 const app=setup();const old=app.storage.get(M);app.action("compose");app.changeSender("module-04");app.node("#msg-to").value="+12025550148";await app.send("keep draft");assert.equal(app.requests.length,0);assert.equal(app.storage.get(M),old);assert.equal(app.node("#msg-input").value,"keep draft");
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

test("random new SMS submits one concrete module without changing legacy records",async()=>{
 const app=setup();app.server();const old=app.storage.get(M);app.action("compose");app.node("#msg-to").value="+12025550148";await app.send("first");assert.match(app.requests[0].moduleId,/^module-\d{2}$/);await app.send("reply");assert.equal(app.requests[1].moduleId,app.requests[0].moduleId);assert.equal(app.storage.get(M),old);
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
test("composing the same destination from another SIM does not mix sender bindings",async()=>{
 const app=setup();app.server();app.action("compose");app.node("#msg-to").value="+12025550148";app.changeSender("module-04");await app.send("first");app.action("compose");app.node("#msg-to").value="+12025550148";app.changeSender("module-01");await app.send("second");assert.equal(app.requests[0].moduleId,"module-04");assert.equal(app.requests[1].moduleId,"module-01");assert.notEqual(app.requests[0].lineId,app.requests[1].lineId);
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


test("remote aliases merge both directions with note persistence and full deletion", async () => {
  const app=setup(); app.server();
  app.publish([{number:"+13322500550"},{number:"13322500550"},{number:"3322500550",mine:true,state:"accepted"}]);
  const id="remote:module-01:line-module-01-0:+13322500550";
  app.select(id);
  assert.equal((app.html().match(/data-msg-thread="remote:/g)||[]).length,1);
  app.action("note"); assert.equal(app.copied(),"");
  assert.match(app.node("#dialog-content").innerHTML,/备注名称/);
  await app.note("张先生");
  assert.match(app.html(),/张先生/);
  assert.match(app.node("#msg-transcript").innerHTML,/张先/);
  assert.match(app.node("#msg-transcript").innerHTML,/尚未送达/);
  assert.doesNotMatch(app.node("#msg-transcript").innerHTML,/运营商已接受/);
  app.publish([{number:"3322500550"}]); assert.match(app.html(),/张先生/);
  app.action("note"); await app.note(""); assert.doesNotMatch(app.html(),/张先生/);
  app.open("messages",id)[1].action(); await app.confirm().action();
  assert.doesNotMatch(app.html(),/data-msg-thread="remote:/);
});

test("later international identity keeps selected conversation and draft", async () => {
  const app=setup();app.server();app.publish([{number:"07598999919"}]);
  app.select("remote:module-01:line-module-01-0:07598999919");
  app.node("#msg-input").value="保留草稿";app.emit("input",{id:"msg-input",value:"保留草稿"});
  app.publish([{number:"+447598999919"}]);
  assert.match(app.html(),/data-msg-thread="remote:module-01:line-module-01-0:\+447598999919" aria-pressed="true"/);
  assert.match(app.html(),/保留草稿/);
});


test("empty mounted chat subscribes once and accepts the first remote page",()=>{
  const app=setup(new Map([[M,"[]"]]),true);
  app.server();
  app.publish([{number:"+13322500550"}]);
  assert.match(app.html(),/\+1 332 250 0550/);
  assert.doesNotMatch(app.html(),/msg-empty-state/);
});
