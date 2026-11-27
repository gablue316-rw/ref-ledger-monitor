import { readFile } from "node:fs/promises";
import { test } from "node:test";
import { strict as assert } from "node:assert";
import vm from "node:vm";

const source = await readFile(new URL("../web/admin.js", import.meta.url), "utf8");
const code = source.slice(source.indexOf('  const announcementBase ='), source.indexOf('  let logsOffset ='));

function fixture(handler, stored = {}) {
  const nodes = new Map(), calls = [], events = {}, windowEvents = {}, intervals = [];
  class Node {
    constructor(tag = "div") {
      this.tag = tag; this.children = []; this.value = ""; this.textContent = "";
      this.hidden = false; this.disabled = false;
    }
    append(...children) { this.children.push(...children); }
    replaceChildren(...children) { this.children = children; }
    focus() {}
    reset() {
      for (const id of ["announcementMessage", "announcementSearch", "announcementStart", "announcementEnd"]) get(id).value = "";
      get("announcementAudience").value = "system";
    }
  }
  function get(id) {
    if (!nodes.has(id)) nodes.set(id, new Node());
    return nodes.get(id);
  }
  const context = {
    $: get,
    el: (tag, text, cls) => Object.assign(new Node(tag), {textContent: text, className: cls}),
    request: async (path, options = {}) => { calls.push({path, options}); return handler(path, options); },
    post: async (path, payload) => { calls.push({path, payload}); return handler(path, {method: "POST", payload}); },
    document: {hidden: false, addEventListener: (name, fn) => { events[name] = fn; }},
    window: {addEventListener: (name, fn) => { windowEvents[name] = fn; }},
    location: {hash: "#announcements"}, URLSearchParams, Intl, Date,
    sessionStorage: {
      getItem: (key) => stored[key] || null,
      setItem: (key, value) => { stored[key] = value; },
      removeItem: (key) => { delete stored[key]; },
    },
    setInterval: (fn, delay) => intervals.push({fn, delay}),
    confirm: () => true,
  };
  vm.runInNewContext(code + `
    globalThis.api = {loadAnnouncements, searchAnnouncementAccounts, loadAnnouncementSessions,
      editAnnouncement, announcementPayload, clearAnnouncement};
  `, context);
  return {get, calls, events, windowEvents, intervals, api: context.api, context};
}

const account = {id: "507f1f77bcf86cd799439011", tenantId: "tenant-a", username: "same@example.test", name: "Alice"};
const identity = {tenantId: account.tenantId, userId: account.id, username: account.username};
const session = {...identity, sessionTargetId: "safe-target", platform: "web", expiresAt: "2099-01-01T00:00:00Z"};

async function selectAccount(f, audience = "session") {
  f.get("announcementAudience").value = audience;
  f.get("announcementAudience").onchange();
  f.get("announcementSearch").value = "same";
  await f.get("announcementSearchButton").onclick();
  f.get("announcementAccount").value = "0";
  f.get("announcementAccount").onchange();
  f.get("announcementMessage").value = "Hello";
}

test("safe account search and verified session target publish without credentials", async () => {
  const f = fixture(async (path) => path.includes("/users?") ? {users: [account], hasMore: false} :
    path.includes("/sessions?") ? {...identity, sessions: [session]} : {announcements: []});
  await selectAccount(f);
  await f.api.loadAnnouncementSessions();
  f.get("announcementSession").value = "0";
  f.get("announcementEnd").value = "2099-12-31";
  f.get("announcementTimezone").value = "America/New_York";
  await f.get("announcementForm").onsubmit({preventDefault() {}});
  assert.match(f.calls[0].path, /^api\/admin\/announcements\/users\?/);
  const write = f.calls.find((c) => c.options.method === "POST");
  assert.deepEqual(JSON.parse(write.options.body), {message: "Hello", audience: "session", ...identity,
    sessionTargetId: "safe-target", endsAt: "2099-12-31", timezone: "America/New_York"});
  assert.equal(write.options.headers["X-Ref-Ledger-Admin"], "1");
});

test("mismatched top-level or individual session identity fails closed", async () => {
  for (const response of [
    {...identity, tenantId: "other", sessions: [session]},
    {...identity, sessions: [{...session, userId: "other"}]},
  ]) {
    const f = fixture(async (path) => path.includes("/users?") ? {users: [account]} : response);
    await selectAccount(f);
    await f.api.loadAnnouncementSessions();
    assert.match(f.get("announcementSessionStatus").textContent, /ambiguous account/);
    assert.equal(f.get("announcementSession").children.length, 1);
    assert.throws(() => f.api.announcementPayload(), /verified active session/);
  }
});

test("editing preserves server timestamps and version; failed update reloads and invalidates edit", async () => {
  const item = {id: "one", message: "old", audience: "system", version: 3,
    startsAt: "2026-11-01T05:30:45.123Z", endsAt: "2026-11-01T06:30:45.123Z", timezone: "America/New_York"};
  const f = fixture(async (path, options) => {
    if (options.method === "PUT") throw Object.assign(Error("stale version"), {status: 409});
    return {announcements: [item]};
  });
  f.api.editAnnouncement(item);
  f.get("announcementMessage").value = "new";
  await f.get("announcementForm").onsubmit({preventDefault() {}});
  const body = JSON.parse(f.calls[0].options.body);
  assert.equal(body.startsAt, item.startsAt);
  assert.equal(body.endsAt, item.endsAt);
  assert.equal(body.version, 3);
  assert.equal(f.calls[1].path, "api/admin/announcements");
  assert.match(f.get("announcementFormStatus").textContent, /select Edit again/);
  assert.throws(() => f.api.announcementPayload(), /Version unavailable/);
});

test("several announcements render as text and session lookup errors remain unknown", async () => {
  const item = {id: "one", audience: "session", ...identity, sessionTargetId: "target", message: "<img src=x onerror=alert(1)>", version: 1};
  const f = fixture(async (path) => {
    if (path.includes("/sessions?")) throw Error("unavailable");
    return {announcements: [item, {...item, id: "two", audience: "system"}]};
  });
  await f.api.loadAnnouncements();
  const cards = f.get("announcementList").children;
  assert.equal(cards.length, 2);
  assert.equal(cards[0].children[1].textContent, item.message);
  await cards[0].children[6].onclick();
  assert.match(cards[0].children[5].textContent, /status unknown/);
  f.events.visibilitychange();
  f.windowEvents["monitor-app-resume"]();
  assert.equal(f.calls.filter((c) => c.path === "api/admin/announcements").length, 3);
});

test("user audience uses stable account identity and schedule validation catches invalid dates", async () => {
  const f = fixture(async () => ({users: [account]}));
  await selectAccount(f, "user");
  f.get("announcementTimezone").value = "America/New_York";
  f.get("announcementStart").value = "2099-02-29";
  assert.throws(() => f.api.announcementPayload(), /Invalid start/);
  f.get("announcementStart").value = "2099-12-31T12:00";
  f.get("announcementEnd").value = "2099-12-30";
  assert.throws(() => f.api.announcementPayload(), /End must be after start/);
  f.get("announcementEnd").value = "2099-12-31";
  const payload = JSON.parse(JSON.stringify(f.api.announcementPayload()));
  assert.deepEqual(payload, {message: "Hello", audience: "user", ...identity,
    startsAt: "2099-12-31T12:00", endsAt: "2099-12-31", timezone: "America/New_York"});
  f.get("announcementTimezone").value = "Invalid/Zone";
  assert.throws(() => f.api.announcementPayload(), /valid IANA timezone/);
});

test("clear posts an empty JSON object and refreshes on failure", async () => {
  const f = fixture(async (path, options) => {
    if (options.method === "POST") throw Error("unavailable");
    return {announcements: []};
  });
  await f.api.clearAnnouncement({id: "a/b"});
  assert.equal(f.calls[0].path, "api/admin/announcements/a%2Fb/clear");
  assert.equal(JSON.stringify(f.calls[0].payload), "{}");
  assert.equal(f.calls[1].path, "api/admin/announcements");
  assert.match(f.get("announcementListStatus").textContent, /Clear failed/);
});

test("uncertain create blocks retry until a successful refresh and explicit review", async () => {
  let failList = true;
  const stored = {};
  const f = fixture(async (path, options) => {
    if (options.method === "POST") throw Error("connection lost");
    if (failList) throw Error("list unavailable");
    return {announcements: [{id: "created", audience: "system", message: "Possible duplicate", version: 1}]};
  }, stored);
  f.get("announcementMessage").value = "Hello";
  await f.get("announcementForm").onsubmit({preventDefault() {}});
  assert.equal(f.get("announcementAcknowledge").disabled, true);
  assert.equal(stored.announcementCreateUncertain, "1");
  const reloaded = fixture(async () => ({announcements: []}), stored);
  assert.equal(reloaded.get("announcementAcknowledge").hidden, false);
  reloaded.get("announcementMessage").value = "Hello";
  await reloaded.get("announcementForm").onsubmit({preventDefault() {}});
  assert.equal(reloaded.calls.length, 0);
  f.get("announcementAcknowledge").onclick();
  await f.get("announcementForm").onsubmit({preventDefault() {}});
  assert.equal(f.calls.filter((c) => c.options.method === "POST").length, 1);
  failList = false;
  await f.api.loadAnnouncements();
  assert.equal(f.get("announcementAcknowledge").disabled, false);
  await f.get("announcementForm").onsubmit({preventDefault() {}});
  assert.equal(f.calls.filter((c) => c.options.method === "POST").length, 1);
  f.get("announcementAcknowledge").onclick();
  await f.get("announcementForm").onsubmit({preventDefault() {}});
  assert.equal(f.calls.filter((c) => c.options.method === "POST").length, 2);
});

test("periodic refresh only while visible, and in-flight stale reads cannot override writes", async () => {
  let resolveOld, resolveWrite;
  let listCount = 0;
  const f = fixture((path, options) => {
    if (options.method === "POST") return new Promise((resolve) => { resolveWrite = resolve; });
    listCount++;
    if (listCount === 1) return new Promise((resolve) => { resolveOld = resolve; });
    return Promise.resolve({announcements: [{id: "new", audience: "system", message: "New", version: 1}]});
  });
  assert.equal(f.intervals[0].delay, 30000);
  f.context.document.hidden = true;
  f.intervals[0].fn();
  assert.equal(f.calls.length, 0);
  f.context.document.hidden = false;
  const old = f.api.loadAnnouncements();
  f.get("announcementMessage").value = "New";
  const save = f.get("announcementForm").onsubmit({preventDefault() {}});
  f.intervals[0].fn();
  f.events.visibilitychange();
  f.windowEvents["monitor-app-resume"]();
  assert.equal(f.calls.length, 2);
  resolveWrite({});
  await save;
  resolveOld({announcements: [{id: "old", audience: "system", message: "Stale", version: 1}]});
  await old;
  assert.equal(f.get("announcementList").children[0].children[1].textContent, "New");
  f.intervals[0].fn();
  assert.equal(f.calls.length, 4);
});
