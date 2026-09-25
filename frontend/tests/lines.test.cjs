const { test } = require("node:test");
const assert = require("node:assert/strict");
const vm = require("node:vm");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
function setup(labels = {}) {
  const context = vm.createContext({
    localStorage: { getItem: () => JSON.stringify(labels) },
  });
  for (const file of [
    "http.js",
    "shared.js",
    "countries.js",
    "module-data.js",
    "tests/module-fixture.js",
    "lines.js",
  ])
    vm.runInContext(readFileSync(join(__dirname, "..", file), "utf8"), context);
  return vm.runInContext("({ Lines, ModuleData })", context);
}
test("both pickers show random first then all 50 modules in number order", () => {
  const { Lines } = setup();
  for (const id of ["call-line", "msg-from"]) {
    const html = Lines.select(id, "线路", "random");
    const ids = Array.from(Lines.options(), (item) => item.id);
    assert.deepEqual(ids, [
      "random",
      ...Array.from(
        { length: 50 },
        (_, i) => `module-${String(i + 1).padStart(2, "0")}`,
      ),
    ]);
    assert.match(html, /value="random"/);
    assert.match(html, /<span>随机<\/span>/);
    assert.match(html, /popover="auto" role="dialog"/);
    assert.match(html, /aria-haspopup="dialog" aria-expanded="false"/);
    assert.doesNotMatch(html, /<select|<option/);
    assert.doesNotMatch(html, /卡 1|卡 2/);
  }
});
test("labels share live module data, escape content and keep the phone number", () => {
  const { Lines, ModuleData } = setup({ "module-01": "办公室" });
  assert.match(
    Lines.select("call", "线路", "module-01"),
    /办公室 · \+86 138 0000 0001/,
  );
  ModuleData.items[0].label = "<测试>";
  assert.match(
    Lines.select("sms", "线路", "module-01"),
    /&lt;测试&gt; · \+86 138 0000 0001/,
  );
});

test("search matches labels, module names and formatted numbers in original order", () => {
  const { Lines } = setup({ "module-01": "Office" });
  assert.equal(Lines.options(" OFFICE ")[0].id, "module-01");
  assert.equal(Lines.options("模块01")[0].id, "module-01");
  assert.equal(Lines.options("+86 138 0000 0001")[0].id, "module-01");
  assert.deepEqual(
    Array.from(Lines.options("模块 0"), (item) => item.id),
    Array.from({ length: 9 }, (_, i) => `module-0${i + 1}`),
  );
  assert.equal(Lines.options("不存在").length, 0);
  assert.equal(Lines.caption("module-01"), "Office · +86 138 0000 0001");
  assert.equal(Lines.caption("invalid"), "随机");
});
test("random uses only available enabled modules and handles zero candidates", () => {
  const { Lines, ModuleData } = setup();
  ModuleData.items.forEach((item) => (item.status = "offline"));
  assert.equal(Lines.resolve("random"), undefined);
  ModuleData.items[3].status = "online";
  assert.equal(Lines.resolve("random"), "module-04");
  assert.equal(Lines.resolve("module-04"), "module-04");
  assert.equal(Lines.resolve("module-02"), undefined);
  ModuleData.items[3].sims[0].enabled = false;
  assert.equal(Lines.resolve("random"), undefined);
  assert.equal(Lines.resolve("module-04"), undefined);
  assert.equal(Lines.valid("sim-2"), false);
  assert.equal(Lines.recorded("sim-2"), false);
  assert.equal(Lines.recorded("random"), false);
  assert.equal(Lines.resolve("sim-2"), undefined);
});

test("SMS selection is independent from call capability and supports the active second SIM",()=>{
 const {Lines,ModuleData}=setup();const item=ModuleData.items[0];item.managed=true;item.capabilities={calls:false,sms:true};item.sims=[{enabled:false},{enabled:true}];assert.equal(Lines.resolve(item.id,"sms"),item.id);assert.equal(Lines.resolve(item.id),undefined);item.capabilities.sms=false;assert.equal(Lines.resolve(item.id,"sms"),undefined);
});
