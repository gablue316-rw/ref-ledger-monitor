import { readFile } from "node:fs/promises";
import { test } from "node:test";
import { strict as assert } from "node:assert";
import vm from "node:vm";
import { randomUUID } from "node:crypto";

const source = await readFile(new URL("../web/admin.js", import.meta.url), "utf8");
const code = source.slice(source.indexOf('  const announcementBase ='), source.indexOf('  let logsOffset ='));

function fixture(handler, stored = {}, mode = {mode: "http"}, persistent = {}) {
  const nodes = new Map(), calls = [], events = {}, windowEvents = {}, intervals = [];
  class Node {
    constructor(tag = "div") {
      this.tag = tag; this.children = []; this.value = ""; this.textContent = "";
      this.hidden = false; this.disabled = false;
    }
    append(...children) { this.children.push(...children); }
    replaceChildren(...children) { this.children = children; }
    focus() {}
    setAttribute(name, value) { this[name] = value; }
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
    request: async (path, options = {}) => {
      if (path === "api/admin/announcements/atlas") return mode;
      calls.push({path, options}); return handler(path, options);
    },
    post: async (path, payload) => { calls.push({path, payload}); return handler(path, {method: "POST", payload}); },
    document: {hidden: false, addEventListener: (name, fn) => { events[name] = fn; }},
    window: {addEventListener: (name, fn) => { windowEvents[name] = fn; }},
    location: {hash: "#announcements"}, URLSearchParams, Intl, Date,
    sessionStorage: {
      getItem: (key) => stored[key] || null,
      setItem: (key, value) => { stored[key] = value; },
      removeItem: (key) => { delete stored[key]; },
    },
    localStorage: {
      getItem: (key) => persistent[key] || null,
      setItem: (key, value) => { persistent[key] = value; },
      removeItem: (key) => { delete persistent[key]; },
    },
    crypto: {randomUUID},
    navigator: {locks: {request: async (_name, fn) => fn()}},
    setTimeout,
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
  assert.equal(cards[0].children[2].textContent, item.message);
  await cards[0].children[4].children[1].onclick();
  assert.match(cards[0].children[4].children[0].textContent, /status unknown/);
  f.events.visibilitychange();
  f.windowEvents["monitor-app-resume"]();
  await new Promise(setImmediate);
  assert.equal(f.calls.filter((c) => c.path === "api/admin/announcements").length, 2);
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
  await new Promise(setImmediate);
  f.get("announcementMessage").value = "New";
  const save = f.get("announcementForm").onsubmit({preventDefault() {}});
  await new Promise(setImmediate);
  f.intervals[0].fn();
  f.events.visibilitychange();
  f.windowEvents["monitor-app-resume"]();
  assert.equal(f.calls.length, 2);
  resolveWrite({});
  await save;
  resolveOld({announcements: [{id: "old", audience: "system", message: "Stale", version: 1}]});
  await old;
  assert.equal(f.get("announcementList").children[0].children[2].textContent, "New");
  f.intervals[0].fn();
  await new Promise(setImmediate);
  assert.equal(f.calls.length, 4);
});

const atlasMode = {mode: "atlas", actor: "monitor-actor", environment: "development"};
const pendingKey = "announcementAtlasPendingV1";
const commandPath = "api/admin/announcements/atlas/commands";

test("Atlas paginates lists, accounts and sessions using new command IDs and stable identities", async () => {
  const commands = new Map(), submitted = [];
  const one = {...identity, sessionTargetId: "target-1", platform: "web"};
  const two = {...identity, sessionTargetId: "target-2", platform: "mobile"};
  const item = {id: "announcement-1", audience: "system", message: "first", version: 1};
  const f = fixture(async (path, options) => {
    if (path === commandPath) {
      submitted.push(options.payload);
      commands.set(options.payload.requestId, options.payload);
      return {};
    }
    const command = commands.get(path.slice(commandPath.length + 1));
    let result;
    if (command.type === "search") result = command.after
      ? {announcements: [{...item, id: "announcement-2"}], nextCursor: ""}
      : {announcements: [item], nextCursor: "list-cursor"};
    else if (command.type === "accounts") result = command.after
      ? {accounts: [{...identity, tenantId: "tenant-b"}], nextCursor: ""}
      : {accounts: [identity], nextCursor: "accounts-cursor"};
    else if (command.type === "sessions") result = command.after
      ? {sessions: [two], nextCursor: ""}
      : {sessions: [one], nextCursor: "sessions-cursor"};
    else if (command.type === "detail") result = item;
    else throw Error("Unexpected command");
    return {requestId: command.requestId, state: "completed", outcome: "success", result};
  }, {}, atlasMode);
  await f.api.loadAnnouncements();
  assert.equal(f.get("announcementList").children.length, 2);
  await f.get("announcementList").children[0].children[1].children[0].children[0].onclick();
  assert.equal(f.get("announcementMessage").value, "first");
  await selectAccount(f);
  await f.api.loadAnnouncementSessions();
  assert.equal(f.get("announcementSession").children.length, 3);
  await f.get("announcementNext").onclick();
  assert.match(f.get("announcementAccount").children[1].textContent, /tenant-b/);
  assert.equal(new Set(submitted.map((c) => c.requestId)).size, submitted.length);
  assert.equal(submitted.filter((c) => c.type === "sessions")[1].after, "sessions-cursor");
  assert.equal(submitted.filter((c) => c.type === "sessions")[0].userId, identity.userId);
  assert.equal(submitted.filter((c) => c.type === "accounts")[1].after, "accounts-cursor");
});

test("ambiguous Atlas publish survives reload, blocks rollback and retries original ID and payload on 404", async () => {
  const persistent = {}, posts = [];
  let online = false;
  const handler = async (path, options) => {
    if (path === commandPath) {
      posts.push(JSON.parse(JSON.stringify(options.payload)));
      if (!online) throw Error("connection lost");
      return {};
    }
    if (!online) throw Error("offline");
    if (path.startsWith(commandPath + "/")) {
      if (posts.length === 1) throw Object.assign(Error("not found"), {status: 404});
      return {requestId: posts[0].requestId, state: "processing"};
    }
    throw Error("unexpected");
  };
  const f = fixture(handler, {}, atlasMode, persistent);
  f.get("announcementMessage").value = "private message";
  await f.get("announcementForm").onsubmit({preventDefault() {}});
  assert.match(f.get("announcementCommandStatus").textContent, /Unresolved/);
  const saved = JSON.parse(persistent[pendingKey]);
  assert.equal(saved.command.request.message, "private message");
  assert.equal(posts.length, 1);
  const rollback = fixture(handler, {}, {mode: "http"}, persistent);
  rollback.get("announcementMessage").value = "other";
  await rollback.get("announcementForm").onsubmit({preventDefault() {}});
  await rollback.api.clearAnnouncement({id: "a"});
  assert.equal(posts.length, 1);
  assert.match(rollback.get("announcementFormStatus").textContent, /unresolved/i);
  online = true;
  const reload = fixture(handler, {}, atlasMode, persistent);
  await reload.get("announcementCommandCheck").onclick();
  assert.deepEqual(posts[1], posts[0]);
  assert.match(reload.get("announcementCommandStatus").textContent, /Pending/);
  assert.ok(persistent[pendingKey]);
});

test("Atlas distinguishes terminal outcomes and preserves unresolved commands on bad status", async () => {
  for (const [outcome, label] of [["rejected", "Rejected"], ["failed", "Failed"]]) {
    const pending = {actor: atlasMode.actor, environment: atlasMode.environment,
      command: {requestId: randomUUID(), type: "clear", announcementId: "one"}};
    const persistent = {[pendingKey]: JSON.stringify(pending)};
    const f = fixture(async () => ({requestId: pending.command.requestId, state: "completed", outcome, reason: "conflict"}), {}, atlasMode, persistent);
    await f.get("announcementCommandCheck").onclick();
    assert.match(f.get("announcementCommandStatus").textContent, new RegExp(label));
    assert.equal(persistent[pendingKey], undefined);
  }
  const pending = {actor: atlasMode.actor, environment: atlasMode.environment,
    command: {requestId: randomUUID(), type: "clear", announcementId: "one"}};
  const persistent = {[pendingKey]: JSON.stringify(pending)};
  const f = fixture(async () => ({requestId: pending.command.requestId, state: "completed", outcome: "unknown"}), {}, atlasMode, persistent);
  await f.get("announcementCommandCheck").onclick();
  assert.match(f.get("announcementCommandStatus").textContent, /Unresolved/);
  assert.ok(persistent[pendingKey]);
});

test("Atlas fails closed when storage or exclusive lock is unavailable or identity changes", async () => {
  const f = fixture(async () => { throw Error("must not submit"); }, {}, atlasMode);
  f.context.localStorage.setItem = () => { throw Error("storage disabled"); };
  f.get("announcementMessage").value = "hello";
  await f.get("announcementForm").onsubmit({preventDefault() {}});
  assert.equal(f.calls.length, 0);
  assert.match(f.get("announcementFormStatus").textContent, /storage is unavailable/);
  f.context.navigator.locks = null;
  await f.get("announcementForm").onsubmit({preventDefault() {}});
  assert.match(f.get("announcementFormStatus").textContent, /locking is unavailable/);
  const pending = {actor: "other", environment: "production",
    command: {requestId: randomUUID(), type: "clear", announcementId: "one"}};
  const g = fixture(async () => { throw Error("must not submit"); }, {}, atlasMode, {[pendingKey]: JSON.stringify(pending)});
  await g.get("announcementCommandRetry").onclick();
  assert.equal(g.calls.length, 0);
  assert.match(g.get("announcementCommandStatus").textContent, /operator or environment differs/);
});

test("Atlas mutations are only confirmed after completed success and edits include version", async () => {
  const commands = new Map(), writes = [], persistent = {};
  let state = "pending";
  const f = fixture(async (path, options) => {
    if (path === commandPath) {
      const command = options.payload;
      commands.set(command.requestId, command);
      if (["publish", "edit", "clear"].includes(command.type)) writes.push(command);
      return {};
    }
    const command = commands.get(path.slice(commandPath.length + 1));
    return command.type === "search"
      ? {requestId: command.requestId, state: "completed", outcome: "success", result: {announcements: [], nextCursor: ""}}
      : {requestId: command.requestId, state, outcome: state === "completed" ? "success" : "", result: {}};
  }, {}, atlasMode, persistent);
  f.get("announcementMessage").value = "hello";
  await f.get("announcementForm").onsubmit({preventDefault() {}});
  assert.match(f.get("announcementCommandStatus").textContent, /Pending/);
  assert.ok(persistent[pendingKey]);
  assert.equal(f.get("announcementMessage").value, "hello");
  state = "completed";
  await f.get("announcementCommandCheck").onclick();
  assert.match(f.get("announcementCommandStatus").textContent, /Confirmed/);
  assert.equal(persistent[pendingKey], undefined);
  assert.equal(f.get("announcementMessage").value, "");
  f.api.editAnnouncement({id: "one", audience: "system", message: "old", version: 7});
  f.get("announcementMessage").value = "updated";
  await f.get("announcementForm").onsubmit({preventDefault() {}});
  assert.equal(writes[1].type, "edit");
  assert.equal(writes[1].announcementId, "one");
  assert.equal(writes[1].request.version, 7);
  await f.api.clearAnnouncement({id: "one"});
  assert.equal(writes[2].type, "clear");
  assert.equal(writes[2].announcementId, "one");
});

test("a stale tab cannot clear or resubmit a different persisted command", async () => {
  const pending = {actor: atlasMode.actor, environment: atlasMode.environment,
    command: {requestId: randomUUID(), type: "clear", announcementId: "one"}};
  const persistent = {[pendingKey]: JSON.stringify(pending)};
  const f = fixture(async () => { throw Error("must not submit"); }, {}, atlasMode, persistent);
  const replacement = {...pending, command: {...pending.command, requestId: randomUUID()}};
  persistent[pendingKey] = JSON.stringify(replacement);
  await f.get("announcementCommandRetry").onclick();
  assert.equal(f.calls.length, 0);
  assert.equal(persistent[pendingKey], JSON.stringify(replacement));
  assert.match(f.get("announcementCommandStatus").textContent, /changed in another tab/);
});

test("HTTP 202 is not treated as a confirmed mutation", async () => {
  const requestCode = source.slice(source.indexOf("  async function request("), source.indexOf("  const names ="));
  const context = {
    fetch: async () => ({status: 202, ok: true, text: async () => "{}"}),
    adminURL: (path) => path,
    AbortSignal: {timeout: () => ({})},
  };
  vm.runInNewContext(requestCode + "\nglobalThis.request = request; globalThis.post = post;", context);
  await assert.rejects(context.request("api/admin/announcements", {method: "POST", rejectAccepted: true}), /not confirmed/);
  await assert.rejects(context.post("api/admin/announcements/id/clear", {}, undefined, true), /not confirmed/);
  await context.post(commandPath, {});
});

test("search retains its filter after one bulk clear and only inactive rows can be deleted", async () => {
  const commands = new Map();
  const entries = [
    {id: "active-1", status: "active", audience: "system"},
    {id: "scheduled-2", status: "scheduled", audience: "system"},
    {id: "active-3", status: "active", audience: "system"},
    {id: "expired-4", status: "expired", audience: "system"},
    {id: "cleared-5", status: "cleared", audience: "system"},
  ];
  const f = fixture(async (path, options) => {
    if (path === commandPath) {
      commands.set(options.payload.requestId, options.payload);
      return {};
    }
    const command = commands.get(path.slice(commandPath.length + 1));
    let result;
    if (command.type === "search") {
      result = {announcements: entries.filter((row) => command.status === "all" || command.status === row.status), nextCursor: ""};
    } else if (command.type === "bulk-clear") {
      result = {results: command.announcementIds.map((id) => {
        entries.find((row) => row.id === id).status = "cleared";
        return {id, outcome: "cleared"};
      })};
    } else if (command.type === "delete") {
      entries.splice(entries.findIndex((row) => row.id === command.announcementId), 1);
      result = {id: command.announcementId};
    } else throw Error("Unexpected command");
    return {requestId: command.requestId, state: "completed", outcome: "success", result};
  }, {}, atlasMode);
  for (const status of ["active", "scheduled", "cleared", "expired", "all"]) {
    f.get("announcementType").value = status;
    await f.get("announcementSearchType").onclick();
    assert.equal(f.get("announcementList").children.length, entries.filter((row) => status === "all" || row.status === status).length);
  }
  const before = f.get("announcementList").children;
  for (const row of before.slice(0, 2)) {
    row.children[0].children[0].checked = true;
    row.children[0].children[0].onchange();
  }
  assert.equal(before[3].children[0].children[0].disabled, false);
  assert.equal(before[0].children[1].children[0].children.some((button) => button.title === "Delete permanently"), false);
  await f.get("announcementBulkClear").onclick();
  const bulk = [...commands.values()].filter((command) => command.type === "bulk-clear");
  assert.equal(bulk.length, 1);
  assert.equal(JSON.stringify(bulk[0].announcementIds), JSON.stringify(["active-1", "scheduled-2"]));
  assert.match(f.get("announcementListStatus").textContent, /Cleared 2 of 2/);
  assert.equal(entries.find((row) => row.id === "active-3").status, "active");
  f.get("announcementType").value = "expired";
  await f.get("announcementSearchType").onclick();
  const expired = f.get("announcementList").children[0];
  await expired.children[1].children[0].children[0].onclick();
  f.get("announcementType").value = "cleared";
  await f.get("announcementSearchType").onclick();
  for (let count = 0; count < 3; count++) {
    await f.get("announcementList").children[0].children[1].children[0].children[0].onclick();
  }
  await f.get("announcementRefresh").onclick();
  assert.equal(f.get("announcementList").children.length, 0);
  assert.equal(entries.length, 1);
  assert.equal([...commands.values()].at(-1).status, "cleared");
});

test("bulk delete spans pages, protects active and scheduled banners, and resumes with confirmed counts", async () => {
  const entries = [
    {id: "active", status: "active"},
    {id: "scheduled", status: "scheduled"},
    {id: "cleared", status: "cleared"},
    ...Array.from({length: 23}, (_, i) => ({id: `active-${i}`, status: "active"})),
    {id: "stale-expired", status: "expired"},
    {id: "expired", status: "expired"},
  ];
  const persistent = {};
  const commands = new Map();
  const deletePosts = [];
  let processing = true;
  const handler = async (path, options) => {
    if (path === commandPath) {
      const command = options.payload;
      commands.set(command.requestId, command);
      if (command.type === "delete") deletePosts.push(command.announcementId);
      return {};
    }
    const command = commands.get(path.slice(commandPath.length + 1));
    const {requestId, announcementId} = command;
    if (command.type === "search") {
      return {requestId, state: "completed", outcome: "success",
        result: {announcements: entries, nextCursor: ""}};
    }
    assert.equal(command.type, "delete");
    if (announcementId === "stale-expired") {
      return processing ? {requestId, state: "processing"}
        : {requestId, state: "completed", outcome: "rejected", reason: "announcement_not_found"};
    }
    const index = entries.findIndex((row) => row.id === announcementId);
    assert.ok(index >= 0);
    entries.splice(index, 1);
    return {requestId, state: "completed", outcome: "success", result: {id: announcementId}};
  };

  const f = fixture(handler, {}, atlasMode, persistent);
  await f.api.loadAnnouncements();
  f.get("announcementSelectAll").onclick();
  f.get("announcementPageNext").onclick();
  assert.ok(f.get("announcementList").children.every((row) => row.children[0].children[0].checked));
  f.get("announcementClearSelected").onclick();
  assert.ok(f.get("announcementList").children.every((row) => !row.children[0].children[0].checked));
  assert.deepEqual(deletePosts, []);

  f.get("announcementSelectAll").onclick();
  await f.get("announcementDeleteAll").onclick();
  assert.deepEqual(deletePosts, ["cleared", "stale-expired"]);
  assert.equal(JSON.parse(persistent[pendingKey]).deleteBatch.deleted, 1);
  assert.match(f.get("announcementCommandStatus").textContent, /2 pending/);
  assert.equal(f.get("announcementDeleteAll").disabled, true);

  processing = false;
  const reopened = fixture(handler, {}, atlasMode, persistent);
  await reopened.get("announcementCommandCheck").onclick();
  assert.deepEqual(deletePosts, ["cleared", "stale-expired", "expired"]);
  assert.equal(persistent[pendingKey], undefined);
  assert.match(reopened.get("announcementListStatus").textContent,
    /Deleted 2 announcements\. 26 were not deleted\. 25 were Active or Scheduled\. 1 failed/);
  assert.ok(reopened.get("announcementList").children.every((row) => row.children[0].children[0].checked));
  assert.ok(entries.some((row) => row.id === "scheduled"));
});
