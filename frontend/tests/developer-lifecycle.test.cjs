const { test } = require("node:test");
const assert = require("node:assert/strict");
const vm = require("node:vm");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");

function setup() {
  const fields = Object.fromEntries(["apiKey", "webhook", "botToken", "adminId", "notificationId", "telegramProxy"].map(name => [name, { value: "", dataset: {}, focus() {} }]));
  const buttons = new Map(), fieldsets = [{}, {}, {}];
  const node = selector => { if (!buttons.has(selector)) buttons.set(selector, { setAttribute() {} }); return buttons.get(selector); };
  const form = { dataset: {}, elements: { namedItem: name => fields[name] }, isConnected: true, querySelector: node, querySelectorAll: () => fieldsets, closest: () => ({ querySelector: node }) };
  let gets = 0;
  const data = { revision: 2, apiRevision: 3, webhook: "https://example.test/hook", hasApiKey: true, hasBotToken: true, hasTelegramProxy: false };
  const context = vm.createContext({
    document: { addEventListener() {}, getElementById: () => form },
    Backend: { enabled: () => true, developer: { get: async () => { gets++; return data; } } },
    UI: { toast() {}, escape: value => String(value).replaceAll("<", "&lt;").replaceAll(">", "&gt;") },
    localStorage: { setItem() { throw Error("Secrets must remain in memory"); } },
    AbortController, URL,
  });
  for (const file of ["forms.js", "developer.js", "developer-docs.js"]) vm.runInContext(readFileSync(join(__dirname, "..", file), "utf8"), context);
  return { ...vm.runInContext("({Developer, DeveloperDocs})", context), form, fields, gets: () => gets };
}
test("docs navigation preserves unsaved values and explicit clears only in memory", async () => {
  const { Developer, form, fields, gets } = setup();
  await Developer.mount();
  assert.equal(gets(), 1);
  fields.webhook.value = "edited";
  fields.apiKey.value = "unsaved-key-123456789";
  fields.telegramProxy.dataset.clear = "true";
  Developer.unmount("developerDocs");
  fields.webhook.value = ""; fields.apiKey.value = ""; fields.telegramProxy.dataset = {};
  await Developer.mount();
  assert.equal(gets(), 1);
  assert.equal(fields.webhook.value, "edited");
  assert.equal(fields.apiKey.value, "unsaved-key-123456789");
  assert.equal(fields.telegramProxy.dataset.clear, "true");
  assert.equal(form.dataset.apiRevision, 3);
  Developer.unmount("general");
  await Developer.mount();
  assert.equal(gets(), 2);
  assert.equal(fields.apiKey.value, "");
});
test("secret values remain concealed after returning from developer docs", async () => {
  const { Developer, fields } = setup();
  await Developer.mount(); fields.apiKey.type = "text"; fields.apiKey.value = "edited-secret-123456";
  Developer.unmount("developerDocs"); await Developer.mount();
  assert.equal(fields.apiKey.type, "password");
  assert.equal(fields.apiKey.value, "edited-secret-123456");
});
test("developer documentation covers real API semantics without real secrets or dial controls", () => {
  const { DeveloperDocs, Developer } = setup();
  const html = DeveloperDocs.render();
  assert.doesNotMatch(html, /X-API-Username|API_USERNAME/);
  for (const text of ["X-API-Key", "GET /modules", "POST /messages", "cardVersion", "requestId", "carrierAccepted", "deliveryConfirmed", "X-Rykvo-Signature", "message.received", "module.updated"]) assert.ok(html.includes(text), text);
  assert.doesNotMatch(html, /751740310|5433956248|sip\.auokapp/);
  assert.match(Developer.render(), /data-action="developerDocs">开发者<\/button>/);
  assert.doesNotMatch(Developer.render(), /尚未启用/);
});

test("docs separate request budgets from background work and retain retry semantics", () => {
  const { DeveloperDocs } = setup();
  const html = DeveloperDocs.render();
  for (const text of ["三步接入", "docs-limits", "6,000", "1,500", "3,600", "64 路", "16 个请求", "4 个", "API_BUSY", "Retry-After", "unknown", "CURSOR_EXPIRED"]) assert.ok(html.includes(text), text);
  assert.doesNotMatch(html, /每分钟最多 300 个请求|<details open/);
  assert.ok((html.match(/<details>/g) || []).length >= 10);
  assert.match(html, /相同 requestId/);
  assert.match(html, /不代表对方收到/);
});

test("docs distinguish incoming IDs and webhook acknowledgement from business completion", () => {
  const html = setup().DeveloperDocs.render();
  for (const text of ["共 67 位", "1–100 个，不重复", "400 INVALID_BATCH", "hostId + eventId", "更高 version", "不代表上游已展示", "真实空短信", "完整正文入库", "不重新发送短信"]) {
    assert.ok(html.includes(text), text);
  }
  const blocks = [...html.matchAll(/<pre><code>([\s\S]*?)<\/code><\/pre>/g)].map(match => match[1]);
  const lookup = blocks.find(code => code.startsWith('curl "$API_BASE/messages/lookup"'));
  const ids = JSON.parse(lookup.match(/--data '([^']+)'/)[1]).ids;
  assert.equal(ids.length, 2);
  assert.match(ids[0], /^[a-zA-Z0-9_-]{16,64}$/);
  assert.match(ids[1], /^rx-[a-f0-9]{64}$/);
  const event = JSON.parse(blocks.find(code => code.startsWith('{\n  "eventId"')));
  assert.equal(event.type, "message.received");
  assert.equal(event.data.state, "received");
  assert.equal(event.data.mine, false);
  assert.match(event.data.id, /^rx-[a-f0-9]{64}$/);
  assert.ok(event.data.text);
});

test("docs keep reference details collapsed and remove upstream billing policy", () => {
  const html = setup().DeveloperDocs.render();
  const overview = html.replace(/<details>[\s\S]*?<\/details>/g, "");
  assert.doesNotMatch(html, /退额度|恢复扣费|<details open/);
  assert.ok(overview.length < html.length / 2);
  for (const section of ["start", "send", "receive", "webhook", "batch", "limits", "errors"]) {
    assert.equal((html.match(new RegExp(`id="docs-${section}"`, "g")) || []).length, 1);
  }
});
