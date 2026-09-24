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
