import { readFile } from "node:fs/promises";
import { test } from "node:test";
import { strict as assert } from "node:assert";
import vm from "node:vm";

const script = await readFile(new URL("../web/admin-logout.js", import.meta.url), "utf8");

function fixture(status) {
  const redirects = [];
  const events = {};
  const location = {
    href: "https://monitor.example/admin/",
    replace: (target) => redirects.push(target),
  };
  const window = {
    fetch: async () => ({status}),
    addEventListener: (name, callback) => { events[name] = callback; },
  };
  const document = {getElementById: () => null};
  const adminURL = (path) => `https://monitor.example/admin/${path}`;
  vm.runInNewContext(script, {window, document, location, adminURL, URL});
  return {window, redirects, events};
}

test("expired same-origin API sessions return to login", async () => {
  const f = fixture(401);
  await f.window.fetch("https://monitor.example/admin/api/metrics/last-minute");
  assert.deepEqual(f.redirects, ["https://monitor.example/admin/login"]);
});

test("unrelated origins and successful requests do not redirect", async () => {
  const unauthorized = fixture(401);
  await unauthorized.window.fetch("https://other.example/admin/api/metrics");
  assert.deepEqual(unauthorized.redirects, []);
  const authorized = fixture(200);
  await authorized.window.fetch("https://monitor.example/admin/api/metrics");
  assert.deepEqual(authorized.redirects, []);
});
