const { test } = require("node:test");
const assert = require("node:assert/strict");
const vm = require("node:vm");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
const { webcrypto } = require("node:crypto");
const source =
  readFileSync(join(__dirname, "..", "http.js"), "utf8") +
  "\n" +
  readFileSync(join(__dirname, "..", "shared.js"), "utf8");

test("LAN HTTP generates unique UUID v4 without randomUUID", () => {
  const context = vm.createContext({
    crypto: { getRandomValues: webcrypto.getRandomValues.bind(webcrypto) },
  });
  vm.runInContext(source, context);
  const ids = Array.from({ length: 100 }, () =>
    vm.runInContext("UI.id()", context),
  );
  assert.equal(new Set(ids).size, 100);
  for (const id of ids)
    assert.match(
      id,
      /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/,
    );
});

test("secure contexts use native randomUUID", () => {
  const context = vm.createContext({
    crypto: { randomUUID: () => "native-id" },
  });
  vm.runInContext(source, context);
  assert.equal(vm.runInContext("UI.id()", context), "native-id");
});
