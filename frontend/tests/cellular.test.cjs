const { test } = require("node:test");
const assert = require("node:assert/strict");
const vm = require("node:vm");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
function setup() {
  const events = {},
    dialogs = [],
    notices = [],
    controls = [{ disabled: false }, { disabled: false }, { disabled: false }],
    nodes = {};
  const context = vm.createContext({
    document: {
      addEventListener: (name, fn) => (events[name] = fn),
      querySelectorAll: () => controls,
      getElementById: (id) =>
        (nodes[id] ||= {
          addEventListener: (name, fn) => (events[name] = fn),
        }),
    },
    UI: {
      read: () => ({}),
      escape: (value) => String(value).replaceAll("<", "&lt;"),
      modal: (...args) => dialogs.push(args),
      toast: (value) => notices.push(value),
    },
    Forms: { report: (_, error) => Boolean(error) },
    Countries: { format: (number) => number },
    fetch() {
      throw Error("Must not transmit activation codes");
    },
    localStorage: {
      setItem() {
        throw Error("Must not store activation codes");
      },
    },
  });
  vm.runInContext(
    readFileSync(join(__dirname, "..", "module-data.js"), "utf8"),
    context,
  );
  vm.runInContext(
    readFileSync(join(__dirname, "..", "cellular.js"), "utf8"),
    context,
  );
  return {
    cellular: vm.runInContext("Cellular", context),
    events,
    dialogs,
    notices,
    controls,
    nodes,
  };
}
const fixture = {
  name: "模块 01",
  number: "+8613800000001",
  status: "online",
  sims: [
    { label: "主号", type: "SIM", enabled: true },
    { label: "副号", type: "eSIM", number: "+8613900000002", enabled: false },
  ],
};
test("cellular overview shows each SIM, count and add eSIM entry", () => {
  const { cellular } = setup();
  const html = cellular.overview(fixture);
  assert.match(html, /2 张/);
  assert.equal((html.match(/data-cellular-sim=/g) || []).length, 2);
  assert.match(html, /添加 eSIM/);
  assert.match(html, /已启用/);
  assert.match(html, /已关闭/);
  assert.doesNotMatch(html, /<small>SIM<\/small>|<small>eSIM<\/small>/);
  assert.match(cellular.overview({ name: "空模块", sims: [] }), /0 张/);
});
test("line settings contain the six requested rows and no type row", () => {
  const { cellular } = setup();
  const html = cellular.detail(fixture, 0);
  for (const label of [
    "号码标签",
    "启用此号码",
    "网络选择",
    "本机号码",
    "Wi-Fi 通话",
    "数据漫游",
  ])
    assert.match(html, new RegExp(label));
  assert.doesNotMatch(html, /类型/);
  assert.equal((html.match(/role="switch"/g) || []).length, 3);
  assert.match(
    cellular.detail(fixture, 1),
    /data-cellular-action="network" disabled/,
  );
});
function clickLine(events, index) {
  events.click({
    target: {
      closest: (selector) =>
        selector === "#dialog-content"
          ? {}
          : selector === "[data-cellular-sim]"
            ? { dataset: { cellularSim: String(index) } }
            : null,
    },
  });
}
test("switches update only the active sample line and disabled lines reject dependent changes", () => {
  const f = setup(),
    item = structuredClone(fixture);
  f.cellular.open(item);
  clickLine(f.events, 0);
  const change = (key, checked) =>
    f.events.change({
      target: {
        dataset: { cellularSetting: key },
        checked,
        closest: () => ({}),
      },
    });
  change("wifiCalling", true);
  change("roaming", true);
  assert.equal(item.sims[0].wifiCalling, true);
  assert.equal(item.sims[1].wifiCalling, undefined);
  change("enabled", false);
  assert.ok(f.controls.every((control) => control.disabled));
  change("roaming", false);
  assert.equal(item.sims[0].roaming, true);
  assert.equal(f.cellular.lines(item)[0].state, "已关闭");
  change("enabled", true);
  assert.ok(f.controls.every((control) => !control.disabled));
});
test("label editing trims, validates and stays scoped to the selected line", () => {
  const f = setup(),
    item = structuredClone(fixture);
  f.cellular.open(item);
  clickLine(f.events, 1);
  const submit = (value) =>
    f.events.submit({
      preventDefault() {},
      target: {
        id: "cellular-label-form",
        elements: { namedItem: () => ({ value }) },
      },
    });
  submit("  工作  ");
  assert.equal(item.sims[1].label, "工作");
  assert.equal(item.sims[0].label, "主号");
  assert.equal(f.dialogs.at(-1)[0], "工作");
  submit("  ");
  assert.equal(item.sims[1].label, "工作");
  assert.equal(f.cellular.validLabel("a".repeat(21)), false);
  assert.equal(f.cellular.validLabel("a\nb"), false);
});
test("manual network selection shows unavailable service without fabricating carriers", () => {
  const f = setup(),
    item = structuredClone(fixture);
  f.cellular.open(item);
  clickLine(f.events, 0);
  f.events.click({
    target: {
      closest: (selector) =>
        selector === "#dialog-content"
          ? {}
          : selector === "[data-cellular-action]"
            ? { dataset: { cellularAction: "network" } }
            : null,
    },
  });
  assert.equal(f.dialogs.at(-1)[0], "网络选择");
  assert.match(f.dialogs.at(-1)[1], /运营商服务尚未接入/);
  f.events.change({
    target: {
      dataset: { cellularSetting: "networkAutomatic" },
      checked: false,
      closest: () => ({}),
    },
  });
  assert.equal(f.nodes["cellular-network-empty"].hidden, false);
  assert.equal(item.sims[0].networkAutomatic, false);
});
test("SIM state reflects module service and disabled lines; details are scoped", () => {
  const { cellular, events, dialogs } = setup();
  assert.equal(
    cellular.lines({ ...fixture, status: "offline" })[0].state,
    "无服务",
  );
  assert.equal(cellular.lines(fixture)[1].number, "+8613900000002");
  cellular.open(fixture);
  events.click({
    target: {
      closest: (selector) =>
        selector === "#dialog-content"
          ? {}
          : selector === "[data-cellular-sim]"
            ? { dataset: { cellularSim: "1" } }
            : null,
    },
  });
  assert.equal(dialogs.at(-1)[0], "副号");
  assert.match(dialogs.at(-1)[1], /13900000002/);
  assert.doesNotMatch(dialogs.at(-1)[1], /13800000001/);
});
test("activation validation and submission never claim installation", () => {
  const { cellular, events, notices } = setup();
  assert.equal(
    cellular.validActivation("LPA:1$smdp.example.com$TEST-ONLY"),
    true,
  );
  for (const value of ["", "hello", "LPA:1$host$", "LPA:1$host$has space"])
    assert.equal(cellular.validActivation(value), false);
  events.submit({
    preventDefault() {},
    target: {
      id: "esim-form",
      elements: {
        namedItem: () => ({ value: "LPA:1$smdp.example.com$TEST-ONLY" }),
      },
    },
  });
  assert.deepEqual(notices, ["eSIM 服务尚未接入，未添加"]);
});
