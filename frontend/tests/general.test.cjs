const { test } = require("node:test");
const assert = require("node:assert/strict");
const vm = require("node:vm");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
function setup(hash = "#general", remembered = null) {
  const urls = [],
    storage = new Map(remembered ? [["rykvo-voice-page-v1", remembered]] : []);
  const events = {}, lifecycle = [];
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
  const nav = ["modules", "messages", "sip", "general"].map(page => ({
    dataset: {page}, attributes: {}, active: false,
    classList: { toggle(_name, value) { nav.find(n => n.dataset.page === page).active = value; } },
    setAttribute(name, value) { this.attributes[name] = value; },
    removeAttribute(name) { delete this.attributes[name]; },
  }));
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
    Messages: page,
    Admin: page,
    Developer: page,
    DeveloperDocs: page,
    WebhookDeliveries: page,
    Server: page,
    SIPServer: page,
    SIP: { ...page, preload() { lifecycle.push("preload"); }, mount() { lifecycle.push("mount"); }, unmount() { lifecycle.push("unmount"); } },
    ContextMenu: { close() {} },
    Visibility: { init() {}, apply() {} },
    HostSettings: page,
    NetworkSettings: page,
    Cleanup: {
      start() {},
      open() {
        dialogs.push(["自动清理"]);
      },
    },
    document: {
      body: { classList: { toggle() {} } },
      querySelectorAll: () => nav,
      addEventListener(name, fn) { (events[name] ||= []).push(fn); },
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
  return { context, dialogs, nodes, urls, storage, nav, lifecycle,
    click(page) { for (const handler of events.click || []) handler({target: {closest: selector => selector === ".nav-item[data-page]" ? nav.find(item => item.dataset.page === page) : null}}); },
  };
}

test("authenticated startup preloads SIP once and active navigation never remounts it", () => {
  const app = setup("#sip");
  assert.deepEqual(app.lifecycle, ["preload", "mount"]);
  app.click("sip"); app.click("sip");
  assert.deepEqual(app.lifecycle, ["preload", "mount"]);
  app.click("modules"); app.click("sip");
  assert.deepEqual(app.lifecycle, ["preload", "mount", "unmount", "mount"]);
});
test("general settings present the six entries without duplicate SIP navigation in order", () => {
  const { context } = setup();
  assert.deepEqual(
    Array.from(
      vm.runInContext("generalItems.map(item => item.title)", context),
    ),
    ["管理员", "服务器", "开发者", "网络配置", "主机设置", "软件更新"],
  );
  const html = vm.runInContext("general()", context);
  assert.equal(
    (html.match(/class="setting-row general-entry"/g) || []).length,
    6,
  );
  assert.doesNotMatch(html, /data-action="sip" aria-haspopup="dialog"/);
  assert.doesNotMatch(html, /data-action="cleanup" aria-haspopup="dialog"/);
});
test("host settings opens a remembered general page without dialogs or mutations", () => {
  const { context, dialogs, nodes, nav, storage } = setup();
  vm.runInContext('render("sip"); actions.cleanup();', context);
  assert.equal(nodes.get("#app-window main").dataset.page, "cleanup");
  assert.deepEqual(dialogs, []);
  assert.deepEqual(nav.filter(n => n.active).map(n => n.dataset.page), ["general"]);
  assert.equal(storage.get("rykvo-voice-page-v1"), "cleanup");
  for (const [hash, remembered] of [["#cleanup", null], ["", "cleanup"]]) {
    assert.equal(setup(hash, remembered).nodes.get("#app-window main").dataset.page, "cleanup");
  }
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
    if (icon === "settings.png") {
      assert.ok(readFileSync(join(__dirname, "..", "assets", icon)).length > 0);
      continue;
    }
    assert.ok(icon.endsWith(".svg"));
    const svg = readFileSync(join(__dirname, "..", "assets", icon), "utf8");
    assert.match(svg, /viewBox="0 0 32 32"/);
    assert.match(svg, /rx="7"/);
    assert.match(svg, /stroke="#fff" stroke-width="1.7"/);
    assert.doesNotMatch(svg, /https?:\/\/(?!www.w3.org)/);
  }
});

test("network entry and page share the broadband icon without changing host settings", () => {
  const { context } = setup();
  assert.equal(vm.runInContext('generalItems.find(item => item.id === "networks").icon', context), "ethernet.svg");
  assert.equal(vm.runInContext('generalItems.find(item => item.id === "cleanup").icon', context), "settings.png");
  assert.match(readFileSync(join(__dirname, "..", "network-settings.js"), "utf8"), /Forms\.header\("网络配置", "ethernet\.svg"\)/);
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
  vm.runInContext('render("sip")', f.context);
  assert.equal(f.urls.at(-1), "/");
  assert.equal(f.storage.get("rykvo-voice-page-v1"), "sip");
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

test("SIP server remains an internal route separate from SIP accounts", () => {
  const { context, nodes } = setup();
  vm.runInContext("actions.sipServer()", context);
  assert.equal(nodes.get("#app-window main").dataset.page, "sipServer");
  assert.equal(vm.runInContext("generalPages.has('sipServer')", context), true);
});

test('server navigation groups both forms inside the single server entry',()=>{
 const context=vm.createContext({UI:{escape:String},document:{addEventListener(){}}});
 vm.runInContext(readFileSync(join(__dirname,'..','forms.js'),'utf8'),context);
 vm.runInContext(readFileSync(join(__dirname,'..','server-navigation.js'),'utf8'),context);
 const navigation=vm.runInContext('ServerNavigation',context);
 for (const active of ['server','sipServer']) {
  const html=navigation.header(active);
  assert.match(html,/<h1>服务器<\/h1>/);
  assert.match(html,/class="settings-tabs"/);
  assert.match(html,/主机服务器/);assert.match(html,/SIP 电话服务器/);
  assert.equal((html.match(/aria-pressed="true"/g)||[]).length,1);
  assert.match(html,new RegExp(`data-action="${active}" aria-pressed="true"`));
 }
 const {context:appContext}=setup();const html=vm.runInContext('general()',appContext);
 assert.match(html,/data-action="server"/);assert.doesNotMatch(html,/data-action="sipServer"/);
});

test("SIP is the only phone entry, directly below messages", () => {
  const html = readFileSync(join(__dirname, "..", "index.html"), "utf8");
  const pages = [...html.matchAll(/data-page="([^"]+)"/g)].map(match => match[1]);
  assert.deepEqual(pages, ["modules", "messages", "sip", "general"]);
  const f = setup("#sip");
  assert.deepEqual(f.nav.filter(n => n.active).map(n => n.dataset.page), ["sip"]);
  assert.equal(f.nav[2].attributes["aria-current"], "page");
  assert.equal(vm.runInContext("generalPages.has('sip')", f.context), false);
  assert.doesNotMatch(vm.runInContext("general()", f.context), /data-action="sip"/);
  assert.doesNotMatch(readFileSync(join(__dirname, "..", "sip.js"), "utf8"), /general-back|‹ 通用/);
  vm.runInContext('render("general")', f.context);
  assert.equal(f.nav[2].active, false);
  assert.equal(f.nav[2].attributes["aria-current"], undefined);
});
test("retired phone bookmarks and remembered page open SIP without a dialer", () => {
  for (const [hash, remembered] of [["#phone", null], ["", "phone"]]) {
    const f = setup(hash, remembered);
    assert.equal(f.nodes.get("#app-window main").dataset.page, "sip");
    assert.equal(f.storage.get("rykvo-voice-page-v1"), "sip");
    assert.equal(vm.runInContext("Object.hasOwn(pages, 'phone')", f.context), false);
  }
});
