const { test } = require("node:test");
const assert = require("node:assert/strict");
const vm = require("node:vm");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
function setup(hash = "#general", remembered = null) {
  const urls = [],
    storage = new Map(remembered ? [["rykvo-voice-page-v1", remembered]] : []);
  const dialogs = [],
    nodes = new Map();
  const node = (selector) => {
    if (!nodes.has(selector))
      nodes.set(selector, {
        style: {},
        dataset: {},
        offsetTop: 0,
        addEventListener() {},
        classList: { toggle() {} },
      });
    return nodes.get(selector);
  };
  const page = { render: () => "", total: 50 };
  const context = vm.createContext({
    UI: {
      $: node,
      read: () => ({}),
      modal: (...args) => dialogs.push(args),
      escape: String,
    },
    Forms: { icon: (name) => `<img src="assets/${name}">` },
    Auth: { authenticated: true, start: (ready) => ready() },
    Modules: page,
    ModuleData: { start() {} },
    Phone: page,
    Messages: page,
    Admin: page,
    Developer: page,
    Server: page,
    SIP: page,
    ContextMenu: { close() {} },
    Visibility: { init() {}, apply() {} },
    Cleanup: {
      start() {},
      open() {
        dialogs.push(["自动清理"]);
      },
    },
    document: {
      body: { classList: { toggle() {} } },
      querySelectorAll: () => [],
      addEventListener() {},
    },
    window: { addEventListener() {} },
    history: {
      replaceState(_state, _title, url) {
        urls.push(url);
      },
    },
    sessionStorage: {
      getItem: (key) => storage.get(key),
      setItem: (key, value) => storage.set(key, value),
    },
    location: { hash, pathname: "/", search: "" },
  });
  vm.runInContext(
    readFileSync(join(__dirname, "..", "app.js"), "utf8"),
    context,
  );
  return { context, dialogs, nodes, urls, storage };
}
test("general settings present the six requested entries in order", () => {
  const { context } = setup();
  assert.deepEqual(
    Array.from(
      vm.runInContext("generalItems.map(item => item.title)", context),
    ),
    ["管理员", "SIP 电话", "云服务器", "开发者", "自动清理", "软件更新"],
  );
  const html = vm.runInContext("general()", context);
  assert.equal(
    (html.match(/class="setting-row general-entry"/g) || []).length,
    6,
  );
  assert.doesNotMatch(html, /data-action="sip" aria-haspopup="dialog"/);
  assert.match(html, /data-action="cleanup" aria-haspopup="dialog"/);
});
test("new entries open configuration notices without starting services or deleting records", () => {
  const { context, dialogs, nodes } = setup();
  vm.runInContext("actions.sip(); actions.cleanup();", context);
  assert.equal(nodes.get("#app-window main").dataset.page, "sip");
  assert.deepEqual(dialogs, [["自动清理"]]);
});
test("cloud server keeps the existing server route", () => {
  const { context, nodes } = setup();
  vm.runInContext("actions.server()", context);
  assert.equal(nodes.get("#app-window main").dataset.page, "server");
});

test("general entries share the local rounded-square SVG icon system", () => {
  const { context } = setup();
  const icons = Array.from(
    vm.runInContext("generalItems.map(item => item.icon)", context),
  );
  assert.equal(new Set(icons).size, 6);
  for (const icon of icons) {
    assert.ok(icon.endsWith(".svg"));
    const svg = readFileSync(join(__dirname, "..", "assets", icon), "utf8");
    assert.match(svg, /viewBox="0 0 32 32"/);
    assert.match(svg, /rx="7"/);
    assert.match(svg, /stroke="#fff" stroke-width="1.7"/);
    assert.doesNotMatch(svg, /https?:\/\/(?!www.w3.org)/);
  }
});

test("Rykvo Voice branding replaces only the sidebar search", () => {
  const html = readFileSync(join(__dirname, "..", "index.html"), "utf8");
  const app = readFileSync(join(__dirname, "..", "app.js"), "utf8");
  assert.match(html, /<title>Rykvo Voice<\/title>/);
  assert.match(html, /class="sidebar-brand"/);
  assert.match(html, /alt="Rykvo Voice"/);
  assert.equal((html.match(/assets\/rykvo-voice.png/g) || []).length, 2);
  assert.doesNotMatch(html, /搜索设置|id="search"|no-results|⌘ K/);
  assert.doesNotMatch(app, /#search|#no-results/);
  assert.match(
    readFileSync(join(__dirname, "..", "modules.js"), "utf8"),
    /id="module-search"/,
  );
  assert.match(
    readFileSync(join(__dirname, "..", "messages.js"), "utf8"),
    /id="msg-search"/,
  );
});

test("navigation keeps icon and title without count badges", () => {
  setup();
  const html = readFileSync(join(__dirname, "..", "index.html"), "utf8");
  const app = readFileSync(join(__dirname, "..", "app.js"), "utf8");
  const messages = readFileSync(join(__dirname, "..", "messages.js"), "utf8");
  const css = readFileSync(join(__dirname, "..", "styles.css"), "utf8");
  assert.doesNotMatch(
    html + app + messages + css,
    /nav-count|updateBadge|nav-indicator|positionIndicator/,
  );
});

test("module route is canonical while old devices bookmarks still work", () => {
  for (const hash of ["#modules", "#devices", "", "#unknown"]) {
    const { context, nodes } = setup(hash);
    assert.equal(nodes.get("#app-window main").dataset.page, "modules");
    assert.equal(
      vm.runInContext("Object.hasOwn(pages, 'devices')", context),
      false,
    );
  }
});

test("navigation hides fragments and refresh restores this tab's page", () => {
  const f = setup("", "messages");
  assert.equal(f.nodes.get("#app-window main").dataset.page, "messages");
  assert.deepEqual(f.urls, ["/"]);
  vm.runInContext('render("phone")', f.context);
  assert.equal(f.urls.at(-1), "/");
  assert.equal(f.storage.get("rykvo-voice-page-v1"), "phone");
  assert.equal(
    setup("#sip", "phone").nodes.get("#app-window main").dataset.page,
    "sip",
  );
  assert.equal(
    setup("", "invalid").nodes.get("#app-window main").dataset.page,
    "modules",
  );
});

test("unavailable session storage does not block navigation", () => {
  const f = setup("");
  vm.runInContext(
    'sessionStorage.getItem = sessionStorage.setItem = () => { throw Error("denied"); }; render(initialPage());',
    f.context,
  );
  assert.equal(f.nodes.get("#app-window main").dataset.page, "modules");
  assert.equal(f.urls.at(-1), "/");
});
