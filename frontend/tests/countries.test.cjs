const { test } = require("node:test");
const assert = require("node:assert/strict");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
const vm = require("node:vm");

function setup(storage = new Map()) {
  const nodes = new Map();
  let copied = "";
  const context = vm.createContext({
    document: {
      addEventListener() {},
      querySelector(selector) {
        if (!nodes.has(selector))
          nodes.set(selector, {
            textContent: "",
            classList: { add() {}, remove() {} },
          });
        return nodes.get(selector);
      },
    },
    navigator: {
      clipboard: {
        async writeText(text) {
          copied = text;
        },
      },
    },
    localStorage: {
      getItem: (key) => storage.get(key) ?? null,
      setItem: (key, value) => storage.set(key, value),
    },
    setTimeout() {},
    clearTimeout() {},
  });
  for (const file of [
    "http.js",
    "shared.js",
    "countries.js",
    "module-data.js",
    "tests/module-fixture.js",
    "lines.js",
    "messages.js",
  ]) {
    vm.runInContext(readFileSync(join(__dirname, "..", file), "utf8"), context);
  }
  return {
    context,
    storage,
    copied: () => copied,
    countries: vm.runInContext("Countries", context),
  };
}

test("245 regions include shared codes and requested prefixes", () => {
  const { countries } = setup();
  assert.equal(countries.regions.length, 245);
  assert.equal(new Set(countries.regions.map((item) => item.id)).size, 245);
  for (const [id, code] of Object.entries({
    US: "1",
    CA: "1",
    CN: "86",
    HK: "852",
    GB: "44",
  })) {
    assert.equal(countries.regions.find((item) => item.id === id).code, code);
  }
});
test("classification uses international prefix, not guessed local country", () => {
  const { countries } = setup();
  for (const [number, code] of [
    ["+852 5123-4567", "852"],
    ["008613123456789", "86"],
    ["+12015550123", "1"],
    ["13800000001", ""],
    ["+999123", ""],
    ["88++", ""],
  ]) {
    assert.equal(countries.code(number), code);
  }
});
test("opening and refreshing never append demonstration conversations", () => {
  const original = {
    id: "existing",
    number: "13800000001",
    unread: true,
    messages: [{ id: "m", text: "保留", at: 1 }],
  };
  const storage = new Map([
    ["rykvo-voice-messages-v1", JSON.stringify([original])],
  ]);
  const snapshot = storage.get("rykvo-voice-messages-v1");
  setup(storage);
  setup(storage);
  assert.equal(storage.get("rykvo-voice-messages-v1"), snapshot);
  const empty = setup();
  assert.match(vm.runInContext("Messages.render()", empty.context), /暂无信息/);
  assert.equal(empty.storage.size, 0);
});

test("click action copies the complete number", async () => {
  const app = setup(
    new Map([
      [
        "rykvo-voice-messages-v1",
        JSON.stringify([{ id: "real", number: "+85251234567", messages: [] }]),
      ],
    ]),
  );
  await vm.runInContext('UI.copy("+85251234567")', app.context);
  assert.equal(app.copied(), "+85251234567");
  assert.match(
    vm.runInContext("Messages.render()", app.context),
    /data-msg-action="copy"/,
  );
});
test("groups sort by calling code and keep unmarked numbers separate", () => {
  const { countries } = setup();
  const groups = countries.group([
    { number: "13800000001" },
    { number: "+85251234567" },
    { number: "+12015550123" },
    { number: "+8613123456789" },
  ]);
  assert.equal(groups.map(([code]) => code).join(","), "1,86,852,");
});
test("phone display follows regional spacing without changing digits", () => {
  const { countries } = setup();
  for (const [raw, formatted] of [
    ["+12684643388", "+1 268 464 3388"],
    ["+8613812345678", "+86 138 1234 5678"],
    ["13812345678", "13812345678"],
    ["+85251234567", "+852 5123 4567"],
    ["+8525123456", "+852 5123 456"],
    ["+447400123456", "+44 7400 123456"],
    ["88++*#", "88++*#"],
  ])
    assert.equal(countries.format(raw), formatted);
  for (const region of countries.regions) {
    assert.equal(
      countries
        .format(`+${region.code}123456789`, region.id)
        .replace(/\s/g, ""),
      `+${region.code}123456789`,
    );
  }
});
test("country labels are available on phone, absent from empty SMS previews", () => {
  const { context, countries } = setup();
  assert.match(countries.country("+85251234567"), /香港/);
  assert.equal(countries.country("13812345678"), "");
  assert.equal(countries.country("+12684643388"), "安提瓜和巴布达");
  const html = vm.runInContext("Messages.render()", context);
  assert.ok(!html.includes("安提瓜和巴布达"));
  assert.ok(!html.includes("香港"));
});
test("caret conversion skips display spaces", () => {
  const { countries } = setup();
  const formatted = "+86 138 1234 5678";
  assert.equal(countries.rawOffset(formatted, 8), 6);
  assert.equal(countries.displayOffset(formatted, 6), 8);
});

test("bare 133 does not imply China", () => {
  const { countries } = setup();
  assert.equal(countries.format("13325550123"), "13325550123");
  assert.equal(countries.country("13325550123"), "");
  assert.equal(countries.format("+13325550123"), "+1 332 555 0123");
  assert.equal(countries.format("13812345678", "CN"), "138 1234 5678");
});
