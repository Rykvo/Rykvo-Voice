const { test } = require("node:test");
const assert = require("node:assert/strict");
const vm = require("node:vm");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
function setup() {
  const context = vm.createContext({
    UI: {
      escape: (value) =>
        String(value)
          .replaceAll("&", "&amp;")
          .replaceAll('"', "&quot;")
          .replaceAll("<", "&lt;"),
    },
    document: { addEventListener() {} },
    window: { addEventListener() {} },
  });
  vm.runInContext(
    readFileSync(join(__dirname, "..", "calendar.js"), "utf8"),
    context,
  );
  return vm.runInContext("Calendar", context);
}
test("calendar validates local dates including leap days and rejects malformed dates", () => {
  const c = setup();
  for (const value of ["2024-02-29", "2026-09-24", "2000-02-29"])
    assert.ok(c.parse(value));
  for (const value of [
    "2026-02-29",
    "1900-02-29",
    "2026-13-01",
    "2026-04-31",
    "2026-1-1",
    "bad",
    "",
  ])
    assert.equal(c.parse(value), null);
  assert.equal(c.dateKey(new Date(2026, 8, 24, 23, 59)), "2026-09-24");
});
test("month navigation clamps the day and supports year boundaries", () => {
  const c = setup();
  for (const [date, step, expected] of [
    ["2026-01-31", 1, "2026-02-28"],
    ["2024-01-31", 1, "2024-02-29"],
    ["2026-03-31", -1, "2026-02-28"],
    ["2026-12-24", 1, "2027-01-24"],
    ["2026-01-24", -1, "2025-12-24"],
  ])
    assert.equal(c.shiftMonth(date, step), expected);
  assert.equal(c.shiftMonth("bad", 1), "");
});
test("calendar builds six complete Monday-first weeks without duplicate days", () => {
  const c = setup();
  for (const month of ["2026-09-24", "2024-02-10", "2026-12-31"]) {
    const days = c.monthDays(month);
    assert.equal(days.length, 42);
    assert.equal(new Set(days).size, 42);
    assert.equal(c.parse(days[0]).getDay(), 1);
    assert.equal(c.parse(days.at(-1)).getDay(), 0);
    for (let i = 1; i < days.length; i++) {
      const d = c.parse(days[i - 1]);
      d.setDate(d.getDate() + 1);
      assert.equal(c.dateKey(d), days[i]);
    }
  }
  assert.equal(c.monthDays("bad").length, 0);
});
test("date trigger uses local custom popover instead of a native date popup", () => {
  const c = setup();
  const html = c.render("test-date", "2026-09-24");
  assert.match(html, /type="hidden" id="test-date" value="2026-09-24"/);
  assert.match(html, /aria-haspopup="dialog" aria-expanded="false"/);
  assert.match(html, /popover="auto" role="dialog"/);
  assert.match(html, /2026\/09\/24/);
  assert.doesNotMatch(html, /type="date"|https?:/);
  assert.match(c.render('"<', "2026-09-24"), /&quot;&lt;/);
});
