const { test } = require("node:test");
const assert = require("node:assert/strict");
const vm = require("node:vm");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
const dir = join(__dirname, "..");
const flush = () => new Promise((resolve) => setImmediate(resolve));
function setup(saved = {}) {
  const events = {},
    win = {},
    close = [],
    dialogs = [],
    writes = [],
    calls = [],
    status = { textContent: "" };
  const nav = ["modules", "phone", "messages", "general"].map((page) => ({
    dataset: { page },
  }));
  const settings = [
    { id: "sip", title: "SIP 电话" },
    { id: "cleanup", title: "自动清理" },
  ];
  const entries = settings.map((item) => ({ dataset: { action: item.id } }));
  const main = { content: "live page" },
    divider = {},
    list = {};
  let stored = saved,
    serverState = saved,
    page = "phone";
  const form = {
    id: "visibility-unlock",
    values: { password: "independent-test-password" },
    querySelector: () => status,
    reset() {
      this.resetCalled = true;
    },
  };
  const dialog = {
    addEventListener: (name, fn) => close.push(fn),
    close() {
      for (const fn of close) fn();
    },
  };
  const backend = {
    challenge: false,
    failSave: false,
    failUnlock: false,
    expired: false,
    async get() {
      calls.push("get");
      return { features: serverState };
    },
    async unlock() {
      calls.push("unlock");
      if (this.failUnlock) throw { code: "INVALID_PASSWORD", status: 403 };
      return { verified: true };
    },
    async access() {
      if (this.expired) throw { code: "VERIFICATION_REQUIRED", status: 403 };
      return { verified: true };
    },
    async lock() {
      calls.push("lock");
    },
    async changePassword() {
      calls.push("changePassword");
    },
    async update({ body }) {
      calls.push("update");
      if (this.expired) throw { code: "VERIFICATION_REQUIRED" };
      if (this.failSave) throw { code: "NETWORK" };
      serverState = { ...serverState, ...body.features };
      return { features: serverState };
    },
  };
  const context = vm.createContext({
    UI: {
      read: () => stored,
      write(k, v) {
        stored = v;
        writes.push([k, v]);
        return true;
      },
      escape: String,
      toast: (text) => calls.push(text),
      modal: (...args) => dialogs.push(args),
      $: (selector) =>
        ({
          "#app-window main": main,
          ".nav-divider": divider,
          ".general-list": list,
          "#dialog": dialog,
          "#dialog-content": {
            replaceChildren() {
              calls.push("clear-fields");
            },
          },
          "#visibility-password": { focus() {} },
        })[selector],
    },
    Backend: {
      visibility: backend,
      ui: {
        async activate({ body }) {
          calls.push(["activate", body]);
          return { challenge: !body.reset && backend.challenge };
        },
      },
    },
    Forms: { field: ({ name }) => `<input name="${name}">` },
    FormData: class {
      constructor(f) {
        return Object.entries(f.values);
      }
    },
    document: {
      querySelectorAll: (s) =>
        s.startsWith(".nav-item")
          ? nav
          : s.startsWith(".general-entry")
            ? entries
            : [],
      addEventListener: (name, fn) => (events[name] ||= []).push(fn),
    },
    window: { addEventListener: (name, fn) => (win[name] ||= []).push(fn) },
  });
  vm.runInContext(readFileSync(join(dir, "visibility.js"), "utf8"), context);
  const v = vm.runInContext("Visibility", context);
  v.init(settings, () => page);
  const f = {
    v,
    backend,
    form,
    status,
    main,
    nav,
    entries,
    dialogs,
    writes,
    calls,
    events,
    win,
    settings,
    click(brand = true) {
      for (const fn of events.click)
        fn({
          target: {
            closest: (s) => (s === ".brand-trigger" && brand ? {} : null),
          },
        });
      return flush();
    },
    async submit() {
      await events.submit[0]({ target: form, preventDefault() {} });
    },
    async unlock() {
      backend.challenge = true;
      await f.click();
      await f.submit();
      backend.challenge = false;
    },
    async change(id, checked) {
      const input = { dataset: { featureVisible: id }, checked };
      await events.change[0]({ target: input });
      return input;
    },
    close: () => dialog.close(),
    setPage: (n) => (page = n),
    setServer: (value) => (serverState = value),
  };
  return f;
}

test("server challenge opens password prompt without revealing switches", async () => {
  const f = setup();
  await f.click();
  assert.equal(f.dialogs.length, 0);
  f.backend.challenge = true;
  await f.click();
  assert.equal(f.dialogs[0][0], "验证密码");
  assert.doesNotMatch(f.dialogs[0][1], /role="switch"/);
  await f.submit();
  assert.equal(f.dialogs.at(-1)[0], "功能显示");
  assert.equal((f.dialogs.at(-1)[1].match(/role="switch"/g) || []).length, 6);
});
test("incorrect independent password and direct changes never bypass verification", async () => {
  const f = setup();
  await flush();
  assert.equal((await f.change("phone", false)).checked, true);
  assert.ok(!f.calls.includes("update"));
  f.backend.challenge = true;
  await f.click();
  f.backend.failUnlock = true;
  await f.submit();
  assert.equal(f.dialogs.at(-1)[0], "验证密码");
  assert.equal(f.status.textContent, "密码不正确");
});
test("switch labels are narrow and independent password form has two fields", async () => {
  const f = setup();
  await f.unlock();
  const html = f.dialogs.at(-1)[1];
  assert.doesNotMatch(html, /<label class="visibility-row"/);
  assert.equal((html.match(/<label class="form-switch">/g) || []).length, 6);
  assert.match(html, /name="currentPassword"/);
  assert.match(html, /name="newPassword"/);
  assert.doesNotMatch(html, /confirmPassword/);
});
test("outside clicks reset the server activation sequence", async () => {
  const f = setup();
  await f.click();
  await f.click(false);
  assert.ok(
    f.calls.some((x) => Array.isArray(x) && x[0] === "activate" && x[1].reset),
  );
  assert.equal(f.dialogs.length, 0);
});
test("authorized updates preserve mounted content and persist only flags", async () => {
  const f = setup();
  await f.unlock();
  await f.change("phone", false);
  assert.equal(f.main.hidden, true);
  assert.equal(f.main.content, "live page");
  assert.equal(f.nav[1].hidden, true);
  assert.equal(f.writes.at(-1)[0], "rykvo-voice-visibility-v1");
  assert.equal(f.writes.at(-1)[1].phone, false);
  await f.change("phone", true);
  assert.equal(f.main.hidden, false);
});
test("hiding general keeps child choices and all hidden pages can be restored", async () => {
  const f = setup();
  await f.unlock();
  await f.change("sip", false);
  await f.change("general", false);
  assert.ok(f.entries.every((x) => x.hidden));
  await f.change("general", true);
  assert.equal(f.entries[0].hidden, true);
  assert.equal(f.entries[1].hidden, false);
  for (const id of ["modules", "phone", "messages", "general"])
    await f.change(id, false);
  assert.ok(f.nav.every((x) => x.hidden));
  f.close();
  await f.unlock();
  await f.change("phone", true);
  assert.equal(f.main.hidden, false);
});
test("server save failure rolls back and expired permission returns to password prompt", async () => {
  const f = setup();
  await f.unlock();
  f.backend.failSave = true;
  assert.equal((await f.change("phone", false)).checked, true);
  assert.equal(f.main.hidden, false);
  f.backend.expired = true;
  await f.change("phone", false);
  assert.equal(f.dialogs.at(-1)[0], "验证密码");
});
test("closing clears secret fields and revokes permission; init binds once", async () => {
  const f = setup();
  await f.unlock();
  f.close();
  await flush();
  assert.ok(f.calls.includes("lock"));
  assert.ok(f.calls.includes("clear-fields"));
  assert.equal((await f.change("phone", false)).checked, true);
  f.v.init(f.settings, () => "phone");
  assert.equal(f.events.click.length, 1);
  assert.equal(f.events.change.length, 1);
});
test("changing independent password requires verified form and closes after save", async () => {
  const f = setup();
  await f.unlock();
  f.form.id = "visibility-password-form";
  f.form.values = {
    currentPassword: "old-test-password",
    newPassword: "new-test-password",
  };
  await f.submit();
  assert.ok(f.calls.includes("changePassword"));
  assert.ok(f.calls.includes("密码已修改"));
  assert.ok(f.form.resetCalled);
  assert.equal((await f.change("phone", false)).checked, true);
});
test("cross-tab changes fetch server preferences instead of trusting modified cache", async () => {
  const f = setup({ phone: false });
  await flush();
  assert.equal(f.main.hidden, true);
  f.setServer({ phone: true });
  f.win.storage[0]({ key: "rykvo-voice-visibility-v1" });
  await flush();
  assert.equal(f.main.hidden, false);
});
test("display dialogs keep outside-click protection and activation threshold is absent from client", () => {
  const css = readFileSync(join(dir, "styles.css"), "utf8"),
    app = readFileSync(join(dir, "app.js"), "utf8"),
    code = readFileSync(join(dir, "visibility.js"), "utf8");
  assert.match(
    css,
    /\.visibility-columns \{[^}]*grid-template-columns: repeat\(2, minmax\(0, 1fr\)\)/,
  );
  assert.match(
    css,
    /dialog:has\(\.visibility-settings\) \{[^}]*user-select: none;/,
  );
  assert.match(
    app,
    /if \(\$\("\.visibility-settings, \.visibility-verify"\)\) return;/,
  );
  assert.doesNotMatch(code, /clicks|=== 10|2000|Qtzydl/);
});
