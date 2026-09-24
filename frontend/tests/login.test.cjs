const { test } = require("node:test");
const assert = require("node:assert/strict");
const vm = require("node:vm");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
async function setup(restored = false, home = "/") {
  const events = {},
    nodes = new Map(),
    requests = [],
    redirects = [];
  const node = (key) => {
    if (!nodes.has(key))
      nodes.set(key, {
        value: "",
        hidden: true,
        dataset: {},
        setAttribute() {},
        removeAttribute() {},
        focus() {
          this.focused = true;
        },
        addEventListener(k, f) {
          events[k] = f;
        },
      });
    return nodes.get(key);
  };
  const user = node("username"),
    password = node("password");
  node("#login-form").elements = { namedItem: node };
  let respond = async (method) => {
    if (method === "GET" && !restored) throw { status: 401 };
    return { user: { id: "1" }, csrfToken: "fixture-token" };
  };
  vm.runInNewContext(readFileSync(join(__dirname, "..", "login.js"), "utf8"), {
    document: { querySelector: node, body: { classList: { toggle() {} } } },
    localStorage: {
      getItem: () => null,
      setItem() {
        throw Error("No credential storage");
      },
    },
    location: { replace: (path) => redirects.push(path) },
    Http: {
      home,
      request: async (...args) => {
        requests.push(args);
        return respond(...args);
      },
    },
  });
  await new Promise(setImmediate);
  return {
    user,
    password,
    node,
    requests,
    redirects,
    respond: (fn) => {
      respond = fn;
    },
    submit: () => events.submit({ preventDefault() {} }),
  };
}

test("public login restores its mount without revealing the root app", async () => {
  const f = await setup(true, "/gly");
  assert.deepEqual(f.redirects, ["/gly"]);
  assert.equal(f.node("#login-screen").hidden, true);
});
test("login uses only its own scripts and public shared assets", () => {
  const login = readFileSync(join(__dirname, "..", "login.html"), "utf8");
  const app = readFileSync(join(__dirname, "..", "index.html"), "utf8");
  assert.doesNotMatch(
    login,
    /sidebar|app\.js|visibility\.js|api\.js|admin\.js/,
  );
  assert.doesNotMatch(app, /login-form|login\.js|auth\.css/);
  assert.match(
    readFileSync(join(__dirname, "..", "auth.css"), "utf8"),
    /\.login-status \{\s*text-align: center/,
  );
});
test("missing fields stay on login without sending credentials", async () => {
  const f = await setup();
  await f.submit();
  assert.equal(f.user.focused, true);
  f.user.value = "tester";
  await f.submit();
  assert.equal(f.password.focused, true);
  assert.equal(f.requests.length, 1);
});
test("wrong password has centered generic error and no redirect", async () => {
  const f = await setup();
  f.user.value = "tester";
  f.password.value = "wrong";
  f.respond(async () => {
    throw { status: 401, message: "private" };
  });
  await f.submit();
  assert.equal(f.node("#login-status").textContent, "账号或密码不正确");
  assert.equal(f.redirects.length, 0);
});
test("successful login clears password and requests new document", async () => {
  const f = await setup();
  f.user.value = " tester ";
  f.password.value = " secret ";
  await f.submit();
  assert.deepEqual(JSON.parse(JSON.stringify(f.requests.at(-1))), [
    "POST",
    "/session",
    { body: { username: "tester", password: " secret " } },
  ]);
  assert.equal(f.password.value, "");
  assert.deepEqual(f.redirects, ["/"]);
});
test("duplicate submit is ignored", async () => {
  const f = await setup();
  f.user.value = "tester";
  f.password.value = "secret";
  let resolve;
  f.respond(() => new Promise((r) => (resolve = r)));
  const pending = f.submit();
  await f.submit();
  assert.equal(f.requests.length, 2);
  resolve({ user: { id: "1" }, csrfToken: "test" });
  await pending;
  assert.equal(f.redirects.length, 1);
});

test("session restoration never reveals the login screen", async () => {
  const f = await setup(true);
  assert.equal(f.node("#login-screen").hidden, true);
  assert.deepEqual(f.redirects, ["/"]);
});
test("login screen starts hidden and appears only after session check", async () => {
  const html = readFileSync(join(__dirname, "..", "login.html"), "utf8");
  assert.match(html, /<main[^>]*id="login-screen"[^>]*hidden/s);
  const f = await setup();
  assert.equal(f.node("#login-screen").hidden, false);
});
