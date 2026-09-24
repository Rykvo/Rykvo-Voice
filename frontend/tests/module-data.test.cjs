const { test } = require("node:test");
const assert = require("node:assert/strict");
const vm = require("node:vm");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");

function setup() {
  const context = vm.createContext({
    UI: { read: () => ({ "module-01": "原标签" }), escape: String },
    Countries: { format: String },
    document: { addEventListener() {} },
  });
  for (const file of ["module-data.js", "modules.js", "lines.js"])
    vm.runInContext(readFileSync(join(__dirname, "..", file), "utf8"), context);
  return vm.runInContext("({ModuleData, Modules, Lines})", context);
}

test("production has no sample modules and renders zero-count empty state", () => {
  const { ModuleData, Modules } = setup();
  assert.equal(ModuleData.items.length, 0);
  const html = Modules.render();
  assert.match(html, /暂无模块/);
  assert.equal((html.match(/<strong>0<\/strong>/g) || []).length, 4);
  assert.doesNotMatch(html, /data-module-detail=|1380000|模块 01/);
  assert.equal(ModuleData.labels["module-01"], "原标签");
});

test("empty pickers retain random but never resolve a fabricated outgoing module", () => {
  const { Lines } = setup();
  assert.equal(Lines.options().length, 1);
  assert.equal(Lines.options()[0].id, "random");
  assert.equal(Lines.resolve("random"), undefined);
  assert.equal(Lines.resolve("module-01"), undefined);
  assert.equal(Lines.valid("module-01"), false);
  assert.equal(Lines.recorded("module-01"), true);
});

test("ICCID is a display fallback and search key, never a dialable phone number", () => {
  const { ModuleData, Modules, Lines } = setup();
  const card = "89440000000000000001";
  const item = { id: "module-03", name: "模块 03", label: "英国", managed: true, number: "", status: "online",
    hardware: { iccid: card }, sims: [{ number: "", iccid: card, enabled: true }, { iccid: "89010000000000000002", enabled: false }] };
  ModuleData.merge([item]);
  assert.match(Modules.render(), /ICCID 89440000000000000001/);
  assert.equal(Modules.select([item], "all", "000000000001").length, 1);
  assert.equal(Modules.select([item], "all", "89010000000000000002").length, 1);
  assert.equal(Lines.options()[1].number, "");
  assert.equal(item.number, "");
  item.number = "+447700900123";
  ModuleData.merge([item]);
  assert.match(Modules.render(), /447700900123/);
  assert.doesNotMatch(Modules.render(), /ICCID 8944/);
  assert.equal(ModuleData.identity("", "").value, "—");
  assert.equal(ModuleData.identity("  ", card).label, "ICCID");
});

test("nearby LTE reception is not displayed as a registered cellular service", () => {
  const { Modules } = setup();
  const item = { id: "module-02", name: "模块 02", managed: true, status: "online", kind: "usb", signal: "none", hardware: { technology: "LTE", operator: "" } };
  assert.match(Modules.rows([item]), /无服务/);
  assert.doesNotMatch(Modules.rows([item]), /LTE/);
  item.signal = "cellular";
  item.hardware.operator = "CHINA MOBILE";
  assert.match(Modules.rows([item]), /CHINA MOBILE · LTE/);
});
