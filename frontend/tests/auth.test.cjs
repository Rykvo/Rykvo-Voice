const { test } = require("node:test");
const assert = require("node:assert/strict");
const vm = require("node:vm");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
const session = {
  user: { id: "1", username: "tester" },
  csrfToken: "test-csrf",
};

function setup(enabled = true, home = "/") {
  const events = {},
    nodes = new Map(),
    calls = [],
    tokens = [],
    redirects = [];
  let ready = 0,
    reloads = 0;
  const node = (selector) => {
    if (!nodes.has(selector))
      nodes.set(selector, {
        value: "",
        type: "password",
        hidden: selector === "#app-window",
        dataset: {},
        attributes: {},
        focus() {
          this.focused = true;
        },
        setAttribute(k, v) {
          this.attributes[k] = v;
        },
        removeAttribute(k) {
          delete this.attributes[k];
        },
        addEventListener(k, fn) {
          assert.equal(events[k], undefined);
          events[k] = fn;
        },
      });
    return nodes.get(selector);
  };
  const username = node("username"),
    password = node("password");
  node("#login-form").elements = { namedItem: node };
  const handlers = {
    get: async () => {
      throw { status: 401 };
    },
    login: async () => session,
    logout: async () => null,
  };
  const context = vm.createContext({
    document: { querySelector: node },
    Http: { home },
    UI: { $: node, toast: (text) => calls.push(text) },
    Forms: { field: ({ name }) => name },
    Backend: {
      enabled: () => enabled,
      setCSRFToken: (token) => tokens.push(token),
      session: Object.fromEntries(
        ["get", "login", "logout"].map((key) => [
          key,
          async (options) => {
            calls.push([key, options]);
            return handlers[key](options);
          },
        ]),
      ),
    },
    location: {
      replace(path) {
        redirects.push(path);
        reloads++;
      },
    },
    localStorage: {
      setItem() {
        throw Error("Do not store credentials");
      },
      clear() {
        throw Error("Do not clear settings");
      },
    },
  });
  vm.runInContext(
    readFileSync(join(__dirname, "..", "auth.js"), "utf8"),
    context,
  );
  const auth = vm.runInContext("Auth", context);
  return {
    auth,
    handlers,
    calls,
    tokens,
    redirects,
    node,
    username,
    password,
    start: () => auth.start(() => ready++),
    submit: () => events.submit({ preventDefault() {} }),
    get ready() {
      return ready;
    },
    get reloads() {
      return reloads;
    },
  };
}

test("unauthenticated session redirects to a separate login document", async () => {
  const f = setup();
  await f.start();
  await f.start();
  assert.equal(f.reloads, 1);
  assert.equal(f.ready, 0);
  assert.equal(f.node("#app-window").hidden, true);
});
test("valid session starts application once", async () => {
  const f = setup();
  f.handlers.get = async () => session;
  await f.start();
  await f.start();
  assert.equal(f.ready, 1);
  assert.equal(f.node("#app-window").hidden, false);
  assert.equal(f.tokens.at(-1), session.csrfToken);
});
test("invalid response keeps the app hidden", async () => {
  const f = setup();
  f.handlers.get = async () => ({});
  await f.start();
  assert.equal(f.ready, 0);
  assert.equal(f.auth.authenticated, false);
});
test("logout clears token without clearing settings", async () => {
  const f = setup();
  const button = { disabled: false };
  await f.auth.logout(button);
  assert.equal(f.reloads, 1);
  assert.equal(f.tokens.at(-1), "");
  f.handlers.logout = async () => {
    throw { status: 503 };
  };
  await f.auth.logout(button);
  assert.equal(f.reloads, 1);
  assert.equal(button.disabled, false);
});

test("expired public sessions and logout return to the prefixed entry", async () => {
  const f = setup(true, "/gly");
  await f.start();
  await f.auth.logout({ disabled: false });
  assert.deepEqual(f.redirects, ["/gly", "/gly"]);
});
