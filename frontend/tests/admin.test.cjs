const { test } = require("node:test");
const assert = require("node:assert/strict");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
const vm = require("node:vm");
function setup() {
  const events = {},
    toasts = [];
  const context = vm.createContext({
    document: {
      addEventListener: (name, fn) => {
        events[name] = fn;
      },
    },
    UI: { toast: (text) => toasts.push(text), escape: String },
    FormData: class {
      constructor(form) {
        return Object.entries(form.values);
      }
    },
    localStorage: {
      setItem() {
        throw Error("Credentials must not be persisted");
      },
    },
    Backend: {
      administrator: {
        update: async () => {
          throw { code: "NOT_CONNECTED" };
        },
      },
    },
    fetch() {
      throw Error("No account API configured");
    },
  });
  vm.runInContext(
    readFileSync(join(__dirname, "..", "forms.js"), "utf8"),
    context,
  );
  vm.runInContext(
    readFileSync(join(__dirname, "..", "admin.js"), "utf8"),
    context,
  );
  return { admin: vm.runInContext("Admin", context), events, toasts };
}
const values = (changes = {}) => ({
  account: "",
  currentPassword: "old-test-password",
  newPassword: "",
  confirmPassword: "",
  ...changes,
});
test("account-only changes need the current password but not a new password", () => {
  const { admin } = setup();
  assert.equal(admin.validate(values({ account: "new-account" })), null);
  assert.equal(
    admin.validate(values({ account: "new-account", currentPassword: "" }))
      .field,
    "currentPassword",
  );
});
test("blank account preserves it during a password change", () => {
  const { admin } = setup();
  assert.equal(
    admin.validate(
      values({
        newPassword: "new-test-password",
        confirmPassword: "new-test-password",
      }),
    ),
    null,
  );
  assert.equal(
    admin.validate(
      values({
        account: "   ",
        newPassword: "new-test-password",
        confirmPassword: "new-test-password",
      }),
    ),
    null,
  );
});
test("unchanged, short, mismatched and reused password values are rejected", () => {
  const { admin } = setup();
  assert.equal(admin.validate(values()).field, "account");
  assert.equal(
    admin.validate(values({ newPassword: "short", confirmPassword: "short" }))
      .field,
    "newPassword",
  );
  assert.equal(
    admin.validate(
      values({
        newPassword: "new-test-password",
        confirmPassword: "different-test",
      }),
    ).field,
    "confirmPassword",
  );
  assert.equal(
    admin.validate(values({ confirmPassword: "test" })).field,
    "confirmPassword",
  );
  assert.equal(
    admin.validate(
      values({
        newPassword: "old-test-password",
        confirmPassword: "old-test-password",
      }),
    ).field,
    "newPassword",
  );
});
test("failed backend save never claims success", async () => {
  const { events, toasts } = setup();
  let prevented = false;
  await events.submit({
    target: {
      dataset: {},
      querySelector: () => ({ disabled: false }),
      id: "admin-form",
      values: values({ account: "new-account" }),
    },
    preventDefault() {
      prevented = true;
    },
  });
  assert.equal(prevented, true);
  assert.deepEqual(toasts, ["保存失败，请重试"]);
});
test("administrator form is a page with four labeled fields and no preset secrets", () => {
  const { admin } = setup();
  const html = admin.render();
  assert.equal((html.match(/<input /g) || []).length, 4);
  assert.equal((html.match(/type="password"/g) || []).length, 3);
  assert.match(html, /data-action="general-back"/);
  assert.doesNotMatch(html, /value=/);
});
