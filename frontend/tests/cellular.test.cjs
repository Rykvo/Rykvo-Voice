const { test } = require("node:test");
const assert = require("node:assert/strict");
const vm = require("node:vm");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
function setup() {
  const events = {},
    dialogs = [],
    notices = [],
    confirmations = [],
    controls = [{ disabled: false }, { disabled: false }, { disabled: false }],
    nodes = {};
  const context = vm.createContext({
    document: {
      addEventListener: (name, fn) => (events[name] = fn),
      querySelectorAll: () => controls,
      getElementById: (id) =>
        (nodes[id] ||= {
          reset() {},
          querySelector() { return null; },
          addEventListener: (name, fn) => (events[name] = fn),
        }),
    },
    UI: {
      read: () => ({}),
      escape: (value) => String(value).replaceAll("<", "&lt;"),
      modal: (...args) => dialogs.push(args),
      toast: (value) => notices.push(value),
    },
    ContextMenu: { confirm: (...args) => confirmations.push(args) },
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
    data: vm.runInContext("ModuleData", context),
    confirmations,
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
test("cellular overview shows one module heading, SIMs and add eSIM entry", () => {
  const { cellular } = setup();
  const html = cellular.overview(fixture);
  assert.match(html, /<h3 class="cellular-section-title">模块 01<\/h3>/);
  assert.doesNotMatch(html, /2 张|<h3>SIM<\/h3>|<small><\/small>/);
  assert.equal((html.match(/data-cellular-sim=/g) || []).length, 2);
  assert.match(html, /添加 eSIM/);
  assert.match(html, /已启用/);
  assert.match(html, /已关闭/);
  assert.doesNotMatch(html, /<small>SIM<\/small>|<small>eSIM<\/small>/);
  assert.match(cellular.overview({ name: "空模块", sims: [] }), /无 SIM 卡/);
});
test("line settings omit manual network selection and retain SIM controls", () => {
  const { cellular } = setup();
  const html = cellular.detail(fixture, 0);
  for (const label of [
    "号码标签",
    "启用此号码",
    "本机号码",
    "Wi-Fi 通话",
    "数据漫游",
  ])
    assert.match(html, new RegExp(label));
  assert.doesNotMatch(html, /类型|网络选择|data-network/);
  assert.equal((html.match(/role="switch"/g) || []).length, 2);
  assert.match(html, /data-cellular-action="wifi"/);
  assert.match(
    cellular.detail(fixture, 1),
    /data-cellular-action="wifi" disabled/,
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

test("managed modules hide hardware diagnostics and retain real eSIM controls", () => {
  const { cellular } = setup();
  const item = { ...structuredClone(fixture), managed: true, capabilities: { esim: true, esimDownload: true }, hardware: { model: "EC20", imei: "123456789012345", esim: { eid: "89049032001001234500012345678901" } } };
  item.sims[0] = { ...item.sims[0], number: item.number, id: "line-active", esim: true, iccid: "89123456789012345678", canDisable: true, canDelete: true };
  for (const html of [cellular.overview(item), cellular.detail(item, 0)]) {
    assert.doesNotMatch(html, /设备信息|IMEI|ICCID|EID|EC20|123456789012345|89123456789012345678/);
  }
  assert.match(cellular.overview(item), /data-cellular-add/);
  assert.doesNotMatch(cellular.overview(item), /data-cellular-add disabled/);
  const detail = cellular.detail(item, 0);
  assert.match(detail, /号码标签/);
  assert.match(detail, /启用此号码/);
  for (const label of ["Wi-Fi 通话", "数据漫游", "删除 eSIM"])
    assert.ok(detail.includes(label));
  assert.equal((detail.match(/待接入/g) || []).length, 2);
  assert.doesNotMatch(detail, /网络选择|data-cellular-action="network"/);
  assert.match(detail, /data-cellular-action="wifi" disabled/);
  assert.match(detail, /data-cellular-setting="roaming"[^>]*disabled/);
  assert.match(detail, /data-cellular-delete disabled/);
  assert.doesNotMatch(detail, /confirm-actions/);
  item.capabilities.esim = false;
  assert.match(cellular.overview(item), /data-cellular-add disabled/);
});

test("managed network settings never change device state locally", () => {
  const f = setup();
  const item = { ...structuredClone(fixture), managed: true, capabilities: { esim: true } };
  item.sims[0].esim = true;
  f.cellular.open(item);
  clickLine(f.events, 0);
  for (const key of ["wifiCalling", "roaming"]) {
    const before = item.sims[0][key];
    const input = { dataset: { cellularSetting: key }, checked: true, closest: () => ({}) };
    f.events.change({ target: input });
    assert.equal(item.sims[0][key], before);
    assert.equal(input.checked, Boolean(before));
  }
  f.events.click({ target: { closest: (selector) =>
    selector === "#dialog-content" ? {} : selector === "[data-cellular-action]"
      ? { dataset: { cellularAction: "network" } } : null } });
  assert.notEqual(f.dialogs.at(-1)[0], "网络选择");
});

test("eSIM deletion confirms the stable profile and waits for device results", async () => {
  const f = setup();
  const item = { ...structuredClone(fixture), managed: true, capabilities: { esim: true } };
  item.sims = [
    { id: "profile-a", label: "主号", esim: true, enabled: true, canDelete: true },
    { id: "profile-b", label: "备用", esim: true, enabled: false, canDelete: true },
  ];
  const calls = [];
  f.data.control = async (...args) => { calls.push(args); };
  const remove = () => f.events.click({ target: { closest: (selector) =>
    ["#dialog-content", "[data-cellular-delete]"].includes(selector) ? {} : null } });
  f.cellular.open(item);
  clickLine(f.events, 0);
  remove();
  assert.equal(f.confirmations.length, 0);
  clickLine(f.events, 1);
  item.sims.reverse();
  remove();
  assert.equal(f.confirmations.length, 1);
  assert.equal(f.confirmations[0][0], "删除“备用” eSIM？");
  assert.equal(calls.length, 0);
  f.events.close();
  await f.confirmations[0][1]();
  assert.equal(calls.length, 1);
  assert.equal(calls[0][0], item);
  assert.equal(calls[0][1], "removeLine");
  assert.equal(calls[0][3], "profile-b");
  assert.equal(item.sims.length, 2);
});

test("physical SIMs and protected eSIM profiles cannot be deleted", () => {
  const f = setup();
  const item = { ...structuredClone(fixture), managed: true, capabilities: { esim: true } };
  assert.doesNotMatch(f.cellular.detail(item, 1), /data-cellular-delete/);
  item.sims[1] = { id: "protected", label: "备用", esim: true, enabled: false, canDelete: false };
  assert.match(f.cellular.detail(item, 1), /data-cellular-delete disabled/);
  f.cellular.open(item);
  clickLine(f.events, 1);
  f.events.click({ target: { closest: (selector) =>
    ["#dialog-content", "[data-cellular-delete]"].includes(selector) ? {} : null } });
  assert.equal(f.confirmations.length, 0);
});

test("Wi-Fi calling uses a dedicated settings page and returns to the same line", () => {
  const f = setup();
  f.cellular.open(structuredClone(fixture));
  clickLine(f.events, 0);
  f.events.click({ target: { closest: (selector) =>
    selector === "#dialog-content" ? {} : selector === "[data-cellular-action]"
      ? { dataset: { cellularAction: "wifi" } } : null } });
  assert.equal(f.dialogs.at(-1)[0], "Wi-Fi 通话");
  assert.match(f.dialogs.at(-1)[1], /data-cellular-setting="wifiCalling"/);
  assert.match(f.dialogs.at(-1)[1], /data-cellular-back="detail"/);
});

test("status reporting retry appears only for pending card notifications", () => {
  const { cellular } = setup();
  assert.doesNotMatch(cellular.overview(fixture), /data-cellular-notify/);
  const item = { ...fixture, hardware: { esim: { pending: 1 } } };
  assert.match(cellular.overview(item), /重试状态上报/);
  item.hardware.esim.pending = 0;
  assert.doesNotMatch(cellular.overview(item), /data-cellular-notify/);
});

test("every inactive eSIM profile displays its own ICCID until a number is known", () => {
  const { cellular } = setup();
  const item = { managed: true, name: "模块 02", number: "+12025550101", status: "online",
    sims: Array.from({ length: 37 }, (_, i) => ({ id: `profile-${i}`, label: `卡 ${i + 1}`, esim: true,
      iccid: `8901000000000000${String(i).padStart(4, "0")}`, number: i === 0 ? "+12025550101" : "", enabled: i === 0 })) };
  const html = cellular.overview(item);
  assert.equal((html.match(/data-cellular-sim=/g) || []).length, 37);
  assert.equal((html.match(/ICCID /g) || []).length, 36);
  for (let i = 1; i < item.sims.length; i++) {
    assert.ok(html.includes(item.sims[i].iccid));
    const detail = cellular.detail(item, i);
    assert.match(detail, /<span>ICCID<\/span>/);
    assert.ok(detail.includes(item.sims[i].iccid));
    assert.doesNotMatch(detail, /12025550101/);
  }
  item.sims[1].number = "+447700900123";
  const detail = cellular.detail(item, 1);
  assert.match(detail, /本机号码|447700900123/);
  assert.ok(!detail.includes(item.sims[1].iccid));
});

test("physical SIM without a number uses ICCID without changing its number field", () => {
  const { cellular } = setup();
  const sim = { id: "physical", label: "SIM", enabled: true, number: "", iccid: "89440000000000000001", readOnly: true };
  const item = { managed: true, name: "模块 03", status: "online", sims: [sim] };
  assert.match(cellular.overview(item), /ICCID 89440000000000000001/);
  assert.match(cellular.detail(item, 0), /<span>ICCID<\/span>/);
  assert.equal(sim.number, "");
  assert.doesNotMatch(cellular.detail(item, 0), /data-cellular-delete|IMEI|EID/);
});

test("eSIM progress follows real stages without invented percentages", () => {
  const { cellular } = setup();
  const item = { ...fixture, job: { id: "job-progress", action: "download", state: "running", stage: "installing" } };
  const html = cellular.overview(item);
  assert.match(html, /正在写入/);
  assert.match(html, /<progress aria-label="正在写入"><\/progress>/);
  assert.doesNotMatch(html, /value=|\d+%|已完成|data-cellular-dismiss/);
  item.job.action = "enable";
  item.job.stage = "writing";
  assert.match(cellular.overview(item), /正在切换号码/);
  item.job.stage = "verifying";
  assert.match(cellular.overview(item), /正在确认卡片状态/);
});

test("confirmed jobs leave no success footer or duplicate notification warning", () => {
  const { cellular } = setup();
  const item = { ...fixture, job: { id: "job-result", action: "enable", state: "uncertain", issue: "ESIM_RESULT_UNKNOWN" } };
  assert.doesNotMatch(cellular.overview(item), /号码已切换|<progress/);
  assert.match(cellular.overview(item), /请勿重复操作/);
  item.job = { ...item.job, state: "succeeded", issue: "", warning: "ESIM_NOTIFICATION_PENDING" };
  const html = cellular.overview(item);
  assert.doesNotMatch(html, /号码已切换|已完成|运营商通知待重试|data-cellular-dismiss|class="cellular-job"|<progress/);
  item.hardware = { esim: { pending: 1 } };
  assert.match(cellular.overview(item), /重试状态上报/);
});

test("read errors and pending inventory are not presented as an absent SIM", () => {
  const { cellular } = setup();
  const item = { name: "模块 03", managed: true, sims: [], issue: "READ_TIMEOUT" };
  assert.match(cellular.overview(item), /设备读取超时/);
  assert.doesNotMatch(cellular.overview(item), /无 SIM 卡/);
  item.issue = ""; item.cardReading = true;
  assert.match(cellular.overview(item), /正在读取卡片/);
});

test("add eSIM is visible only for detected download-capable cards", () => {
  const f = setup();
  const item = { name: "模块", managed: true, sims: [], capabilities: { esim: false, esimDownload: false } };
  assert.doesNotMatch(f.cellular.overview(item), /data-cellular-add|添加 eSIM/);
  item.capabilities.esim = true;
  f.cellular.open(item);
  f.events.click({ target: { closest: selector => ["#dialog-content", "[data-cellular-add]"].includes(selector) ? {} : null } });
  assert.equal(f.dialogs.at(-1)[0], "蜂窝网络");
  item.capabilities.esimDownload = true;
  assert.match(f.cellular.overview(item), /data-cellular-add/);
  item.capabilities.esim = false;
  assert.match(f.cellular.overview(item), /data-cellular-add disabled/);
});

test("retired network controls do not issue requests", () => {
  const f = setup(), item = structuredClone(fixture);
  f.data.control = () => { throw Error("retired network control called"); };
  f.cellular.open(item); clickLine(f.events, 0);
  f.events.change({ target: { dataset: { cellularSetting: "networkAutomatic" }, checked: false, closest: () => ({}) } });
  for (const selector of ["[data-network-scan]", "[data-network-index]"]) {
    f.events.click({ target: { closest: s => s === "#dialog-content" ? {} : s === selector ? { dataset: { networkIndex: "0" } } : null } });
  }
  assert.equal(item.sims[0].networkAutomatic, undefined);
  assert.doesNotMatch(f.dialogs.at(-1)[1], /网络选择|搜索网络|data-network/);
});

test("card transport errors replace the pending inventory message", () => {
  const { cellular } = setup();
  const item = { managed: true, name: "模块 04", sims: [], cardReading: true, hardware: { esim: { issue: "READ_TIMEOUT" } } };
  const html = cellular.overview(item);
  assert.match(html, /设备读取超时/);
  assert.doesNotMatch(html, /正在读取卡片|无 SIM 卡|添加 eSIM|<progress/);
  item.hardware.esim.issue = "DEVICE_BUSY";
  assert.match(cellular.overview(item), /设备正在被占用/);
});
