const { test } = require("node:test");
const assert = require("node:assert/strict");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
const vm = require("node:vm");
function setup() {
  const events = {},
    notices = [];
  const context = vm.createContext({
    document: {
      addEventListener: (type, handler) => {
        events[type] = handler;
      },
    },
    UI: {
      escape: (value) =>
        String(value).replace(
          /[&<>"']/g,
          (char) =>
            ({
              "&": "&amp;",
              "<": "&lt;",
              ">": "&gt;",
              '"': "&quot;",
              "'": "&#39;",
            })[char],
        ),
      toast: (text) => notices.push(text),
    },
    URL,
    FormData: class {
      constructor(form) {
        return Object.entries(form.values);
      }
    },
    fetch() {
      throw Error("Must not send credentials");
    },
    localStorage: {
      setItem() {
        throw Error("Must not cache credentials");
      },
    },
  });
  for (const file of ["forms.js", "developer.js"])
    vm.runInContext(readFileSync(join(__dirname, "..", file), "utf8"), context);
  return {
    developer: vm.runInContext("Developer", context),
    forms: vm.runInContext("Forms", context),
    events,
    notices,
  };
}
const values = (changes = {}) => ({
  username: "",
  apiKey: "",
  webhook: "",
  botToken: "",
  adminId: "",
  notificationId: "",
  telegramProxy: "",
  ...changes,
});
test("developer page has seven fields, three concealed secrets, and no prefilled IDs", () => {
  const { developer } = setup();
  const html = developer.render();
  assert.equal((html.match(/<input /g) || []).length, 7);
  assert.equal((html.match(/type="password"/g) || []).length, 3);
  assert.doesNotMatch(
    html,
    /751740310|value=|preferences|外观与动画|S5 代理|socks5:\/\//,
  );
  assert.ok(html.includes('placeholder="IP:端口:账号:密码"'));
  for (const name of [
    "Username",
    "API Key",
    "短信 Webhook",
    "Telegram 机器人",
    "机器人令牌",
    "管理员 ID",
    "通知 ID",
    "socks5",
  ])
    assert.ok(html.includes(name));
});
test("webhook accepts web addresses and rejects executable or credential-bearing URLs", () => {
  const { developer } = setup();
  for (const url of [
    "",
    "https://example.test/sms",
    "http://127.0.0.1:5173/hook",
  ])
    assert.equal(developer.validate(values({ webhook: url })), null);
  for (const url of [
    "invalid",
    "javascript:alert(1)",
    "https://user:password@example.test",
  ])
    assert.equal(developer.validate(values({ webhook: url })).field, "webhook");
});
test("Telegram IDs stay strings and support negative notification IDs", () => {
  const { developer } = setup();
  assert.equal(
    developer.validate(
      values({ adminId: "123456789", notificationId: "-1001234567890" }),
    ),
    null,
  );
  assert.equal(developer.validate(values({ adminId: "-1" })).field, "adminId");
  assert.equal(
    developer.validate(values({ notificationId: "1.5" })).field,
    "notificationId",
  );
});
test("developer form does not save or transmit secrets without a backend", () => {
  const { events, notices } = setup();
  events.submit({
    target: {
      id: "developer-form",
      values: values({ apiKey: "test-only", botToken: "test-only" }),
    },
    preventDefault() {},
  });
  assert.deepEqual(notices, ["配置服务尚未连接，修改未保存"]);
});
test("shared form rendering escapes configurable content", () => {
  const { forms } = setup();
  const html = forms.field({
    prefix: "test",
    name: "label",
    label: "<img>",
    placeholder: '" onfocus="test',
    value: "<test>",
  });
  assert.ok(html.includes("&lt;img&gt;"));
  assert.ok(html.includes("&quot; onfocus=&quot;test"));
  assert.ok(html.includes('value="&lt;test&gt;"'));
});

test("socks5 accepts IP:port:username:password and passwords containing colons", () => {
  const { developer } = setup();
  for (const proxy of [
    "",
    "127.0.0.1:1080:test:password",
    "192.0.2.1:65535:test:pass:word@#",
    "127.0.0.1:1080::",
    "[::1]:1080:test:password",
  ]) {
    assert.equal(developer.validate(values({ telegramProxy: proxy })), null);
  }
});
test("socks5 rejects old URLs, invalid IPs, ports and incomplete credentials", () => {
  const { developer } = setup();
  for (const proxy of [
    "socks5://127.0.0.1:1080",
    "proxy.example:1080:test:password",
    "256.1.1.1:1080:test:password",
    "127.0.0.1:0:test:password",
    "127.0.0.1:65536:test:password",
    "127.0.0.1:1080:test",
    "127.0.0.1:1080:test:",
    "127.0.0.1:1080::password",
    "[invalid]:1080:test:password",
    "127.0.0.1:1080:test:pass\nword",
  ]) {
    assert.equal(
      developer.validate(values({ telegramProxy: proxy })).field,
      "telegramProxy",
    );
  }
});
test("proxy field belongs only to Telegram and does not alter webhook", () => {
  const { developer } = setup();
  const sections = developer.render().split('<section class="form-section">');
  assert.ok(sections.at(-1).includes('name="telegramProxy"'));
  assert.ok(
    sections
      .slice(0, -1)
      .every((section) => !section.includes('name="telegramProxy"')),
  );
  const data = values({
    webhook: "https://example.test/sms",
    telegramProxy: "127.0.0.1:1080:test:password",
  });
  const before = JSON.stringify(data);
  assert.equal(developer.validate(data), null);
  assert.equal(JSON.stringify(data), before);
});
