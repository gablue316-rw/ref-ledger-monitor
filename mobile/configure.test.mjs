import { test } from "node:test";
import { strict as assert } from "node:assert";
import { monitorConfig } from "./configure.mjs";

test("uses a canonical same-origin HTTPS admin URL", () => {
  assert.equal(monitorConfig("https://monitor.example:8443").server.url, "https://monitor.example:8443/admin/");
  assert.equal(monitorConfig("https://monitor.example/admin/").server.url, "https://monitor.example/admin/");
});

test("rejects unsafe and ambiguous backend URLs", () => {
  for (const url of [undefined, "", "http://localhost", "https://" + "user" + "@" + "example.com", "https://example.com/other", "https://example.com/?x=1", "https://example.com/#fragment"]) {
    assert.throws(() => monitorConfig(url));
  }
});
