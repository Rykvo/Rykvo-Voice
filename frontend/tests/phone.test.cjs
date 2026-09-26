const { test } = require("node:test");
const assert = require("node:assert/strict");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
const vm = require("node:vm");

// Minimal DOM and clock for repeatable dialer tests.
function setup() {
  const events = {},
    timers = new Map(),
    nodes = new Map(),
    storage = new Map();
  let clock = 0,
    id = 0;
  function node(selector) {
    if (!nodes.has(selector))
      nodes.set(selector, {
        value: "",
        selectionStart: 0,
        selectionEnd: 0,
        classList: { toggle() {}, add() {}, remove() {} },
        style: { setProperty() {} },
        setAttribute() {},
        setSelectionRange(start, end) {
          this.selectionStart = start;
          this.selectionEnd = end;
        },
      });
    return nodes.get(selector);
  }
  const target = (digit = "0") => ({
    dataset: { digit },
    closest(selector) {
      return [
        "[data-digit]",
        `.phone-workspace,.phone-heading`,
        `[data-digit="${digit}"]`,
      ].includes(selector)
        ? this
        : null;
    },
  });
  const context = vm.createContext({
    crypto: { randomUUID: () => "test-call" },
    document: {
      querySelector: node,
      querySelectorAll: () => [],
      body: {},
      addEventListener(type, fn) {
        (events[type] ??= []).push(fn);
      },
    },
    window: {
      addEventListener(type, fn) {
        (events[type] ??= []).push(fn);
      },
    },
    localStorage: {
      getItem: (key) => storage.get(key) ?? null,
      setItem: (key, value) => storage.set(key, value),
    },
    setTimeout(fn, delay) {
      timers.set(++id, { fn, at: clock + delay });
      return id;
    },
    clearTimeout: (id) => timers.delete(id),
    setInterval: () => 0,
    clearInterval() {},
  });
  for (const file of [
    "http.js",
    "shared.js",
    "countries.js",
    "module-data.js",
    "tests/module-fixture.js",
    "lines.js",
    "phone.js",
  ])
    vm.runInContext(readFileSync(join(__dirname, "..", file), "utf8"), context);
  vm.runInContext("Phone.mount()", context);
  return {
    node,
    target,
    storage,
    html: () => vm.runInContext("Phone.render()", context),
    edit: vm.runInContext("UI.editDial", context),
    emit(type, props = {}) {
      const event = {
        target: target(),
        button: 0,
        preventDefault() {},
        ...props,
      };
      for (const fn of events[type] ?? []) fn(event);
    },
    advance(ms) {
      clock += ms;
      for (const [id, timer] of timers)
        if (timer.at <= clock) {
          timers.delete(id);
          timer.fn();
        }
    },
    value: () => node("#dial-number").value,
  };
}

test("symbols can repeat and insert at any caret position", () => {
  const { edit } = setup();
  assert.equal(edit("88+", "+").value, "88++");
  assert.equal(edit("88", "+", 1).value, "8+8");
  assert.equal(edit("88", "*", 0, 2).value, "*");
  assert.equal(edit("*", "#").value, "*#");
  assert.equal(edit("88++", "delete").value, "88+");
  assert.equal(edit("", "delete").value, "");
});
test("short pointer press inserts one zero", () => {
  const app = setup();
  app.emit("pointerdown");
  app.advance(100);
  app.emit("pointerup");
  app.emit("click");
  assert.equal(app.value(), "0");
});
test("long pointer press inserts plus only, including repeated holds", () => {
  const app = setup();
  for (let i = 0; i < 2; i++) {
    app.emit("pointerdown");
    app.advance(500);
    app.advance(1500);
    app.emit("pointerup");
    app.emit("click");
  }
  assert.equal(app.value(), "++");
});
test("keyboard short and long zero use the same behavior", () => {
  const app = setup();
  app.emit("keydown", { key: "0" });
  app.emit("keyup", { key: "0" });
  app.emit("keydown", { key: "0" });
  app.advance(500);
  app.emit("keydown", { key: "0", repeat: true });
  app.emit("keyup", { key: "0" });
  assert.equal(app.value(), "0+");
});
test("cancelled presses do not enter zero or leak a timer", () => {
  const app = setup();
  app.emit("pointerdown");
  app.emit("pointercancel");
  app.advance(600);
  app.emit("keydown", { key: "0" });
  app.emit("blur");
  app.advance(600);
  assert.equal(app.value(), "");
});
test("history mounts empty without generating sample calls", () => {
  const app = setup();
  assert.equal(app.node("#history-count").textContent, "0 条");
  assert.equal(
    (app.node("#history-list").innerHTML.match(/class="call-record/g) || [])
      .length,
    0,
  );
});
test("backspace and delete edit digits, not display separators", () => {
  const { edit } = setup();
  assert.equal(
    edit("+8613812345678", "backspace", 6, 6).value,
    "+861312345678",
  );
  assert.equal(
    edit("+8613812345678", "forward-delete", 6, 6).value,
    "+861382345678",
  );
  assert.equal(edit("+8613812345678", "backspace", 3, 6).value, "+8612345678");
});
function deleteButton() {
  return {
    dataset: { action: "dial-delete" },
    closest(selector) {
      return [
        '[data-action="dial-delete"]',
        "[data-action]",
        ".phone-workspace,.phone-heading",
      ].includes(selector)
        ? this
        : null;
    },
  };
}
test("short delete removes one digit; long delete clears once", () => {
  const app = setup(),
    target = deleteButton();
  for (const key of "1234") app.emit("keydown", { key });
  app.emit("pointerdown", { target });
  app.advance(100);
  app.emit("pointerup", { target });
  app.emit("click", { target });
  assert.equal(app.value(), "123");
  app.emit("pointerdown", { target });
  app.advance(500);
  assert.equal(app.value(), "");
  app.emit("pointerup", { target });
  app.emit("click", { target });
  app.emit("keydown", { key: "8" });
  app.advance(1000);
  assert.equal(app.value(), "8");
});
test("cancelled delete does not clear numbers", () => {
  const app = setup(),
    target = deleteButton();
  for (const key of "1234") app.emit("keydown", { key });
  app.emit("pointerdown", { target });
  app.emit("pointercancel", { target });
  app.advance(600);
  assert.equal(app.value(), "1234");
});
test("Backspace crosses spacing and deletes a selected range", () => {
  const app = setup(),
    input = app.node("#dial-number");
  input.id = "dial-number";
  input.closest = (selector) =>
    selector === "input,textarea,select" ? input : null;
  input.value = "+85251234567";
  input.setSelectionRange(input.value.length, input.value.length);
  app.emit("input", { target: input });
  assert.equal(app.value(), "+852 5123 4567");
  input.setSelectionRange(10, 10);
  app.emit("keydown", { target: input, key: "Backspace" });
  assert.equal(app.value().replace(/\s/g, ""), "+8525124567");
  input.setSelectionRange(0, input.value.length);
  app.emit("keydown", { target: input, key: "Backspace" });
  assert.equal(app.value(), "");
});

test("dialing preserves line selection but never simulates calls or records", () => {
  const app = setup();
  app.emit("change", { target: { id: "call-line", value: "module-04" } });
  app.emit("click", { target: app.target("8") });
  const call = {
    dataset: { action: "dial-call" },
    closest(selector) {
      return [".phone-workspace,.phone-heading", "[data-action]"].includes(selector) ? this : null;
    },
  };
  for (let i=0;i<2;i++) { app.emit("click", {target:call}); app.advance(5000); }
  assert.equal(app.storage.has("rykvo-voice-calls-v1"), false);
  assert.doesNotMatch(app.html(), /call-duration|通话中/);
  assert.notEqual(app.node("#call-status").textContent, "通话中");
  assert.match(app.html(), /value="module-04"/);
  assert.equal(JSON.parse(app.storage.get("rykvo-voice-call-line-v1")), "module-04");
  app.emit("change", { target: { id: "call-line", value: "random" } });
  assert.match(app.html(), /value="random"/);
});
