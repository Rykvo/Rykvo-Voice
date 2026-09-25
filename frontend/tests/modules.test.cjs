const { test } = require("node:test");
const assert = require("node:assert/strict");
const vm = require("node:vm");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");

function setup(savedLabels = {}, writeOK = true) {
  const events = {},
    dialogs = [],
    menus = [],
    writes = [],
    errors = [],
    nodes = {
      "module-rows": {},
      "module-result": {},
      "module-label": { select() {} },
      dialog: { close() {} },
    },
    navCount = {};
  const cards = ["all", "online", "offline", "error"].map((status) => ({
    dataset: { moduleFilter: status },
    setAttribute(name, value) {
      this[name] = value;
    },
  }));
  const context = vm.createContext({
    document: {
      addEventListener: (type, fn) => (events[type] = fn),
      querySelector: () => navCount,
      querySelectorAll: () => cards,
      getElementById: (id) => nodes[id],
    },
    ContextMenu: {
      open: (event, entries) => {
        event.preventDefault();
        menus.push(entries);
      },
    },
    Forms: {
      field: () => "",
      report: (form, error) => {
        if (error) errors.push(error);
        return Boolean(error);
      },
    },
    UI: {
      read: () => savedLabels,
      write: (key, value) => {
        writes.push({ key, value });
        return writeOK;
      },
      toast() {},
      escape: (text) =>
        String(text).replaceAll("<", "&lt;").replaceAll('"', "&quot;"),
      modal: (...args) => dialogs.push(args),
    },
    Countries: { format: (value) => value },
    Cellular: {
      open: (item) => dialogs.push(item),
    },
    localStorage: {
      setItem() {
        throw Error("Do not change settings or sessions");
      },
    },
  });
  vm.runInContext(
    readFileSync(join(__dirname, "..", "module-data.js"), "utf8"),
    context,
  );
  vm.runInContext(
    readFileSync(join(__dirname, "module-fixture.js"), "utf8"),
    context,
  );
  vm.runInContext(
    readFileSync(join(__dirname, "..", "modules.js"), "utf8"),
    context,
  );
  return {
    modules: vm.runInContext("Modules", context),
    events,
    dialogs,
    menus,
    writes,
    errors,
    nodes,
    cards,
    navCount,
  };
}
test("four cards and five content columns have no selection controls", () => {
  const { modules } = setup();
  const html = modules.render();
  assert.equal((html.match(/data-module-filter=/g) || []).length, 4);
  assert.equal((html.match(/scope="col"/g) || []).length, 5);
  assert.equal((html.match(/data-module-detail=/g) || []).length, 50);
  assert.doesNotMatch(
    html,
    /iPhone|储存|同步|phone-art|checkbox|module-check|module-selected/,
  );
});
test("network rejection is distinct from a failed modem read", () => {
  const { modules } = setup();
  const item = { id: "test", name: "模块", managed: true, status: "online", signal: "none", hardware: { registration: "denied" } };
  assert.match(modules.rows([item]), /注册被拒绝/);
  Object.assign(item, { status: "error", issue: "QMI_READ_FAILED" });
  assert.match(modules.rows([item]), /读取异常/);
  item.issue = "RECOVERING";
  assert.match(modules.rows([item]), /正在恢复/);
  assert.doesNotMatch(modules.rows([item]), /读取异常/);
});
test("empty slot is distinct from a card without service", () => {
  const { modules } = setup();
  const item = { id: "test", name: "模块", managed: true, status: "online", signal: "none", hardware: { simState: "absent" } };
  assert.match(modules.rows([item]), /无 SIM 卡/);
  assert.doesNotMatch(modules.rows([item]), /无服务/);
  item.hardware.simState = "READY";
  assert.match(modules.rows([item]), /无服务/);
  assert.doesNotMatch(modules.rows([item]), /无 SIM 卡/);
  item.hardware.simState = "unknown";
  item.status = "error";
  item.issue = "READ_TIMEOUT";
  assert.doesNotMatch(modules.rows([item]), /无 SIM 卡/);
});
test("counts and filters derive from records without mutating them", () => {
  const { modules } = setup();
  const records = Object.freeze(
    [
      { status: "online" },
      { status: "online" },
      { status: "offline" },
      { status: "error" },
    ].map(Object.freeze),
  );
  assert.equal(
    JSON.stringify(modules.count(records)),
    JSON.stringify({ all: 4, online: 2, offline: 1, error: 1 }),
  );
  assert.equal(modules.select(records, "online").length, 2);
  assert.deepEqual(
    Array.from(modules.select(records, "all")),
    Array.from(records),
  );
  assert.equal(modules.count([]).all, 0);
  assert.match(modules.rows([]), /colspan="5".*暂无模块/);
});
test("filter click updates only rows and pressed state; repeat clicks are ignored", () => {
  const f = setup();
  f.modules.render();
  const event = {
    target: {
      closest: (selector) =>
        selector === "[data-module-filter]" ? f.cards[1] : null,
    },
  };
  f.events.click(event);
  assert.equal(f.cards[1]["aria-pressed"], "true");
  assert.equal(f.cards[0]["aria-pressed"], "false");
  assert.match(f.nodes["module-rows"].innerHTML, /模块 01/);
  assert.doesNotMatch(f.nodes["module-rows"].innerHTML, /模块 02|模块 03/);
  f.nodes["module-rows"].innerHTML = "unchanged";
  f.events.click(event);
  assert.equal(f.nodes["module-rows"].innerHTML, "unchanged");
});
test("details are scoped to the selected module and sidebar count matches", () => {
  const f = setup();
  assert.equal(f.modules.total, 50);
  f.events.click({
    target: {
      closest: (selector) =>
        selector === "[data-module-detail]"
          ? { dataset: { moduleDetail: "module-03" } }
          : null,
    },
  });
  assert.equal(f.dialogs[0].name, "模块 03");
  assert.equal(f.dialogs[0].status, "error");
  assert.equal(f.dialogs[0].sims.length, 1);
});
test("record fields are escaped", () => {
  const { modules } = setup();
  const html = modules.rows([
    { name: "<script>", id: '"onclick', number: "<img>", status: "online" },
  ]);
  assert.doesNotMatch(html, /<script>|<img>/);
  assert.match(html, /&lt;script>/);
});
test("search combines status, name, formatted numbers and secondary SIM numbers", () => {
  const { modules } = setup();
  const records = [
    {
      name: "模块 01",
      number: "+8613800000001",
      status: "online",
      sims: [{ number: "+8613900000002" }],
    },
    { name: "模块 02", number: "+85296352418", status: "offline" },
  ];
  assert.equal(modules.select(records, "all", "模块 01").length, 1);
  assert.equal(modules.select(records, "all", "+86 138 0000 0001").length, 1);
  assert.equal(modules.select(records, "all", "139 0000 0002").length, 1);
  assert.equal(modules.select(records, "online", "+852").length, 0);
  assert.equal(modules.select(records, "all", "nonexistent").length, 0);
});
test("signal column supports all connection labels and safely defaults to no service", () => {
  const { modules } = setup();
  for (const [signal, label] of Object.entries({
    none: "无服务",
    wifi: "Wi-Fi Calling",
    mobile: "中国移动",
    unicom: "中国联通",
    telecom: "中国电信",
    unknown: "无服务",
    toString: "无服务",
    "<img>": "无服务",
  })) {
    const html = modules.rows([
      { id: "test", name: "模块", number: "123", status: "online", signal },
    ]);
    assert.ok(html.includes(`<td class="module-signal">${label}</td>`));
    assert.doesNotMatch(html, /<img>/);
  }
  const html = modules.render();
  assert.match(
    html,
    /状态<\/th><th scope="col">信号<\/th><th scope="col">操作/,
  );
  assert.match(
    modules.rows([
      { id: "test", name: "模块", number: "123", status: "online" },
    ]),
    /module-signal">无服务/,
  );
});

test("50 preview modules have unique identities, numbers and all five signal labels", () => {
  const { modules, events, dialogs } = setup();
  const html = modules.render();
  const ids = [...html.matchAll(/data-module-detail="([^"]+)"/g)].map(
    (match) => match[1],
  );
  assert.equal(ids.length, 50);
  assert.equal(new Set(ids).size, 50);
  ids.forEach((id) =>
    events.click({
      target: {
        closest: (selector) =>
          selector === "[data-module-detail]"
            ? { dataset: { moduleDetail: id } }
            : null,
      },
    }),
  );
  assert.equal(new Set(dialogs.map((item) => item.number)).size, 50);
  assert.deepEqual(JSON.parse(JSON.stringify(modules.count(dialogs))), {
    all: 50,
    online: 38,
    offline: 6,
    error: 6,
  });
  for (const label of [
    "无服务",
    "Wi-Fi Calling",
    "中国移动",
    "中国联通",
    "中国电信",
  ])
    assert.ok(html.includes(label));
  assert.equal(dialogs[49].name, "模块 50");
  dialogs.forEach((item, index) =>
    assert.equal(item.number, `+86138${String(index + 1).padStart(8, "0")}`),
  );
});

test("module list exposes a keyboard-accessible scroll region", () => {
  const { modules } = setup();
  assert.match(
    modules.render(),
    /class="data-table-wrap" role="region" aria-label="模块列表滚动区域" tabindex="0"/,
  );
});

test("short numeric search matches module number, not incidental phone digits", () => {
  const f = setup();
  for (const value of ["05", "5"]) {
    f.events.input({ target: { id: "module-search", value } });
    assert.match(f.nodes["module-rows"].innerHTML, /模块 05/);
    assert.doesNotMatch(f.nodes["module-rows"].innerHTML, /模块 50/);
  }
  f.events.input({
    target: { id: "module-search", value: "+86 138 0000 0050" },
  });
  assert.match(f.nodes["module-rows"].innerHTML, /模块 50/);
  assert.equal(f.navCount.scrollTop, 0);
});
test("results keep natural module order without mutating the input", () => {
  const { modules } = setup();
  const records = Object.freeze(
    [10, 2, 1].map((n) =>
      Object.freeze({ name: `模块 ${n}`, number: `123${n}`, status: "online" }),
    ),
  );
  assert.deepEqual(
    Array.from(modules.select(records, "all"), (item) => item.name),
    ["模块 1", "模块 2", "模块 10"],
  );
  assert.deepEqual(
    Array.from(modules.select(records, "online", "123"), (item) => item.name),
    ["模块 1", "模块 2", "模块 10"],
  );
  assert.equal(records[0].name, "模块 10");
});

function context(f, id) {
  let prevented = false;
  f.events.contextmenu({
    preventDefault() {
      prevented = true;
    },
    target: { closest: () => (id ? { dataset: { moduleRow: id } } : null) },
  });
  return prevented;
}
function saveLabel(f, id, value) {
  f.events.submit({
    preventDefault() {},
    target: {
      id: "module-label-form",
      dataset: { moduleId: id },
      elements: { namedItem: () => ({ value }) },
    },
  });
}
test("row context menu provides label and details for that module only", () => {
  const f = setup();
  assert.equal(context(f, null), false);
  assert.equal(context(f, "module-05"), true);
  assert.deepEqual(
    Array.from(f.menus[0], (item) => item.label),
    ["标签", "详情"],
  );
  f.menus[0][0].action();
  assert.match(
    f.dialogs[0][1],
    /module-label-form.*data-module-id="module-05"/,
  );
  f.menus[0][1].action();
  assert.equal(f.dialogs[1].id, "module-05");
});
test("labels persist separately, retain numbering, support search and reset", () => {
  const f = setup();
  f.navCount.scrollTop = 200;
  saveLabel(f, "module-05", "  工作卡  ");
  assert.equal(f.writes[0].key, "rykvo-voice-module-labels-v1");
  assert.equal(f.writes[0].value["module-05"], "工作卡");
  assert.equal(f.navCount.scrollTop, 200);
  for (const value of ["工作卡", "05"]) {
    f.events.input({ target: { id: "module-search", value } });
    assert.match(f.nodes["module-rows"].innerHTML, /工作卡/);
    assert.doesNotMatch(
      f.nodes["module-rows"].innerHTML,
      /data-module-row="module-50"/,
    );
  }
  const restored = setup(f.writes[0].value);
  assert.match(restored.modules.render(), /工作卡/);
  saveLabel(restored, "module-05", "");
  assert.equal(restored.writes[0].value["module-05"], undefined);
  assert.match(restored.modules.render(), /scope="row">模块 05/);
});
test("invalid labels and storage failure leave existing labels intact", () => {
  const f = setup({ "module-05": "旧标签" }, false);
  saveLabel(f, "module-05", "a".repeat(21));
  assert.equal(f.errors.length, 1);
  assert.equal(f.writes.length, 0);
  saveLabel(f, "module-05", "新标签");
  assert.match(f.modules.render(), /旧标签/);
  assert.doesNotMatch(f.modules.render(), /新标签/);
  assert.doesNotMatch(
    setup({ "module-05": { bad: true } }).modules.render(),
    /object Object/,
  );
});

test("restart controls are separate and labels stay minimal", () => {
  const { modules } = setup();
  const html = modules.render();
  assert.equal((html.match(/data-module-restart=/g) || []).length, 52);
  assert.match(html, /data-module-restart="all">模块重启/);
  assert.match(html, /data-module-restart="host">主机重启/);
  assert.equal(modules.restartTitle({name:"模块 05"}), "重启模块 05？");
  assert.equal(modules.restartTitle({name:"模块 05",label:"香港卡"}), "重启“香港卡”？");
  const source = readFileSync(join(__dirname, "..", "modules.js"), "utf8");
  assert.doesNotMatch(source, /原设置保留/);
  assert.match(source, /ContextMenu.confirm\(title, \(\) => restart\(scope\), "重启", note\)/);
});
