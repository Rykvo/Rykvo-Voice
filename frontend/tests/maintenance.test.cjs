const { test } = require("node:test");
const assert = require("node:assert/strict");
const { readFileSync, existsSync } = require("node:fs");
const { join } = require("node:path");
const dir = join(__dirname, "..");
test("clean delivery has no migration loader or legacy storage keys", () => {
  const html = readFileSync(join(dir, "index.html"), "utf8");
  assert.doesNotMatch(html, /data-migration|previews\//);
  assert.equal(existsSync(join(dir, "data-migration.js")), false);
  for (const [, name] of html.matchAll(/<script src="([^"?]+)/g)) {
    const code = readFileSync(join(dir, name), "utf8");
    assert.doesNotMatch(code, /apple-panel-|demo-numbers-|sim-1|sim-2/);
  }
});
test("backend handoff covers every declared endpoint and uses one transport", () => {
  const api = readFileSync(join(dir, "api.js"), "utf8");
  const docs = readFileSync(join(dir, "BACKEND.md"), "utf8");
  for (const [, route] of api.matchAll(
    /\["(?:GET|POST|PUT|PATCH|DELETE)", "([^"]+)"\]/g,
  ))
    assert.ok(docs.includes(route), route);
  const server = readFileSync(join(dir, "server.js"), "utf8");
  assert.doesNotMatch(server, /\bfetch\(/);
  assert.match(server, /Backend\.tunnel/);
});
test("brand trigger keeps the normal cursor without a hover tooltip", () => {
  const css =
    readFileSync(join(dir, "base.css"), "utf8") +
    readFileSync(join(dir, "styles.css"), "utf8");
  const html = readFileSync(join(dir, "index.html"), "utf8");
  assert.match(css, /\.brand-trigger \{[^}]*cursor: default;/);
  const brand = html.match(/<div class="sidebar-brand">[\s\S]*?<\/div>/)[0];
  assert.doesNotMatch(brand, /\btitle=|data-tooltip=/);
  assert.match(brand, /aria-label="Rykvo Voice"/);
});
test("production entry has no sample loader or remote runtime dependencies", () => {
  const html = readFileSync(join(dir, "index.html"), "utf8");
  const app = readFileSync(join(dir, "app.js"), "utf8");
  assert.doesNotMatch(
    app,
    /preview=sip-history|previews\/sip-history|URLSearchParams/,
  );
  for (const [, url] of html.matchAll(/(?:src|href)="([^"]+)"/g)) {
    assert.doesNotMatch(url, /^(?:https?:)?\/\//);
    assert.ok(existsSync(join(dir, url.split("?")[0])), url);
  }
});
test("history uses stored account calls only and retains billing and date controls", () => {
  const js = readFileSync(join(dir, "sip-history.js"), "utf8");
  assert.doesNotMatch(js, /previewRecords|samples|100 条示例/);
  assert.match(js, /UI.read\("rykvo-voice-calls-v1", \[\]\)/);
  assert.match(js, /Math.max\(1, Math.ceil\(duration \/ 60\)\)/);
  assert.match(js, /通话统计/);
  assert.match(js, /Calendar.render/);
  assert.doesNotMatch(js, /localStorage\.(?:clear|removeItem)/);
});
test("shared dialogs support short screens and dark frame has explicit styling", () => {
  const css =
    readFileSync(join(dir, "base.css"), "utf8") +
    readFileSync(join(dir, "styles.css"), "utf8");
  assert.match(
    css,
    /dialog \{[^}]*max-height: calc\(100dvh - 32px\);[^}]*overflow: auto;/,
  );
  assert.match(css, /\.theme-dark \.desktop-glow/);
  assert.match(css, /\.theme-dark \.window/);
});

test("confirmation and cleanup copy remain minimal", () => {
  const confirm = readFileSync(join(dir, "context-menu.js"), "utf8");
  const cleanup = readFileSync(join(dir, "cleanup.js"), "utf8");
  assert.match(confirm, /function confirm\(title, action, label/);
  assert.doesNotMatch(confirm, /description|<p>/);
  assert.match(cleanup, /id="cleanup-note">0 为关闭<\/p>/);
  assert.doesNotMatch(cleanup, /保存后生效|自动删除超过/);
});

test("conversation and call lists share hover scrollbars without layout shifts", () => {
  const css =
    readFileSync(join(dir, "base.css"), "utf8") +
    readFileSync(join(dir, "styles.css"), "utf8");
  assert.match(
    css,
    /\.scroll-area \{[^}]*scrollbar-gutter: stable;[^}]*scrollbar-width: thin;[^}]*scrollbar-color: transparent transparent;/,
  );
  assert.match(
    css,
    /\.scroll-area:hover,[\s\S]*?\.scroll-area:focus-within,[\s\S]*?\.scroll-area:active \{/,
  );
  assert.doesNotMatch(
    css,
    /\.scroll-area::-webkit-scrollbar \{[^}]*display: none/,
  );
  assert.match(css, /@media \(hover: none\), \(forced-colors: active\)/);
  assert.match(
    readFileSync(join(dir, "messages.js"), "utf8"),
    /class="msg-thread-list scroll-area"/,
  );
  assert.match(
    readFileSync(join(dir, "phone.js"), "utf8"),
    /id="history-list" class="scroll-area"/,
  );
});

test("logout stays in sidebar outside display preferences", () => {
  const html = readFileSync(join(dir, "index.html"), "utf8");
  assert.match(
    html,
    /<\/nav>\s*<button[^>]*class="sidebar-exit"[^>]*data-action="logout"[^>]*>[\s\S]*?<span>退出<\/span>\s*<\/button>/,
  );
  assert.doesNotMatch(
    readFileSync(join(dir, "app.js"), "utf8"),
    /data-action="logout"/,
  );
  assert.doesNotMatch(
    readFileSync(join(dir, "visibility.js"), "utf8"),
    /title: "退出|id: "logout"/,
  );
});
test("display password actions share one footer and errors are centered", () => {
  const code = readFileSync(join(dir, "visibility.js"), "utf8");
  assert.doesNotMatch(code, /<h3>修改密码<\/h3>/);
  assert.match(
    code,
    /class="visibility-actions".*form="visibility-password-form".*保存密码.*visibility-done/,
  );
  assert.match(
    readFileSync(join(dir, "styles.css"), "utf8"),
    /\.visibility-error \{\s*text-align: center/,
  );
});


test("wide chat uses bounded side padding rather than a centered fixed-width column",()=>{
 const css=readFileSync(join(dir,"messages.css"),"utf8");
 assert.match(css,/padding: 25px clamp\(20px, 2vw, 36px\) 24px/);
 assert.doesNotMatch(css,/100% - 920px/);
});


test("bubble tail cutout clears the avatar and narrow content reserves the full gutter",()=>{
 const css=readFileSync(join(dir,"messages.css"),"utf8");
 const gap=Number(css.match(/\.msg-line \{[^}]*gap: (\d+)px;/)[1]);
 const tail=Number(css.match(/\.msg-bubble:after \{[^}]*left: -(\d+)px;/)[1]);
 assert.ok(gap>tail);
 assert.match(css,new RegExp("max-width: calc\\(100% - "+(30+gap)+"px\\)"));
});
