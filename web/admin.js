"use strict";
(() => {
  const $ = (id) => document.getElementById(id);
  const el = (tag, text, cls) => {
    const node = document.createElement(tag);
    node.textContent = text;
    if (cls) node.className = cls;
    return node;
  };
  function notice(text, error = false) {
    $("adminNotice").hidden = !text;
    $("adminNotice").textContent = text;
    $("adminNotice").style.color = error ? "#a32020" : "";
  }
  async function request(path, options = {}) {
    const { allowMissing = false, ...fetchOptions } = options;
    const response = await fetch(adminURL(path), {
      cache: "no-store",
      credentials: "same-origin",
      ...fetchOptions,
      signal: AbortSignal.timeout(20000),
    });
    if (allowMissing && response.status === 404) return { roles: [] };
    if (response.status === 401 || response.redirected) {
      throw Error(
        "Authentication required. Sign in to the monitor administrator service and reload this page.",
      );
    }
    const text = await response.text();
    let data = {};
    try {
      data = text ? JSON.parse(text) : {};
    } catch {
      if (response.ok) throw Error("Expected JSON from the administrator API.");
    }
    if (!response.ok)
      throw Error(
        data.message || data.error || text.trim() || `HTTP ${response.status}`,
      );
    return data;
  }
  function post(path, payload) {
    return request(path, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "X-Ref-Ledger-Admin": "1",
      },
      body: JSON.stringify(payload),
    });
  }
  const names = {
    metrics: [
      "Metrics overview",
      "Users, HTTP traffic, and collection activity across accounts.",
    ],
    users: [
      "Users & sessions",
      "Manage user accounts, access, and sign-in sessions.",
    ],
    catalog: [
      "Sports & roles",
      "Manage the shared sports catalog and officiating roles.",
    ],
  };
  let userStarted = false,
    sportsStarted = false;
  function navigate() {
    let page = location.hash.slice(1);
    if (!names[page]) page = "metrics";
    document
      .querySelectorAll(".page")
      .forEach((s) => (s.hidden = s.id !== page));
    document.querySelectorAll("[data-page]").forEach((b) => {
      b.classList.toggle("active", b.dataset.page === page);
      if (b.dataset.page === page) b.setAttribute("aria-current", "page");
      else b.removeAttribute("aria-current");
    });
    $("title").textContent = names[page][0];
    $("subtitle").textContent = names[page][1];
    if (page === "users" && !userStarted) {
      userStarted = true;
      loadUsers();
    }
    if (page === "catalog" && !sportsStarted) {
      sportsStarted = true;
      loadSports();
    }
  }
  document
    .querySelectorAll("[data-page]")
    .forEach((b) => (b.onclick = () => (location.hash = b.dataset.page)));
  window.addEventListener("hashchange", navigate);
  $("shellAbout").onclick = () => $("rl-about-open").click();
  let users = [],
    offset = 0,
    hasMore = false,
    usersBusy = false,
    search = { username: "", name: "" },
    actionBusy = false,
    action = null,
    focusReturn = null;
  function userButtons() {
    for (const id of ["prevUsers", "nextUsers", "reloadUsers", "clearUsers"])
      $(id).disabled =
        usersBusy ||
        actionBusy ||
        (id === "prevUsers" && offset === 0) ||
        (id === "nextUsers" && !hasMore);
    $("userSearch")
      .querySelectorAll("button")
      .forEach((b) => (b.disabled = usersBusy || actionBusy));
    $("userRows")
      .querySelectorAll("button")
      .forEach(
        (b) =>
          (b.disabled = usersBusy || actionBusy || b.dataset.locked === "true"),
      );
  }
  function actionButton(label, kind, user, sessionId, locked = false) {
    const b = el("button", label, "btn");
    b.type = "button";
    b.dataset.locked = String(locked);
    b.disabled = locked || usersBusy || actionBusy;
    b.onclick = () => {
      focusReturn = b;
      openAction({ kind, user, sessionId });
    };
    return b;
  }
  function renderUsers() {
    $("userRows").replaceChildren();
    for (const user of users) {
      const tr = document.createElement("tr"),
        actions = el("td", ""),
        group = el("div", "", "rowActions"),
        sessions = Array.isArray(user.sessions) ? user.sessions : [];
      group.append(
        actionButton("Delete", "delete", user, null, user.isCurrentUser),
        actionButton("End all sessions", "all", user, null, !sessions.length),
        actionButton("Role", "role", user),
        actionButton("Password", "password", user),
      );
      actions.append(group);
      tr.append(actions);
      for (const [value, cls] of [
        [user.username, ""],
        [user.name, ""],
        [user.role, ""],
        [user.tenantId, "mono"],
      ])
        tr.append(el("td", value || "—", cls));
      if (user.isCurrentUser)
        tr.children[1].append(el("div", "Your signed-in account", "small"));
      const td = el("td", "");
      if (!sessions.length) td.textContent = "No sessions";
      for (const session of sessions) {
        const div = el("div", "", "session"),
          date = new Date(session.expiresAt);
        div.append(
          el("code", session.sessionId || "Missing session ID"),
          el(
            "div",
            Number.isNaN(date.getTime())
              ? "Expiration unavailable"
              : `${date <= new Date() ? "Expired" : "Expires"}: ${date.toLocaleString()}`,
            "small",
          ),
          actionButton(
            "End session",
            "one",
            user,
            session.sessionId,
            !session.sessionId,
          ),
        );
        td.append(div);
      }
      tr.append(td);
      $("userRows").append(tr);
    }
    if (!users.length) {
      const tr = el("tr", ""),
        td = el("td", "No users to display.");
      td.colSpan = 6;
      tr.append(td);
      $("userRows").append(tr);
    }
    $("userPage").textContent = users.length
      ? `Showing ${offset + 1}–${offset + users.length}${hasMore ? " · more available" : ""}`
      : "0 matching users";
    userButtons();
  }
  async function loadUsers(keepMessage = false) {
    if (usersBusy) return;
    usersBusy = true;
    userButtons();
    if (!keepMessage) $("userMessage").textContent = "Loading users…";
    try {
      let data;
      while (true) {
        data = await request(
          "api/admin/users?" +
            new URLSearchParams({ ...search, offset: String(offset) }),
        );
        if (!Array.isArray(data.users))
          throw Error("Invalid user directory response.");
        if (data.users.length || !offset) break;
        offset = Math.max(0, offset - 50);
      }
      users = data.users;
      hasMore = data.hasMore === true;
      if (!keepMessage)
        $("userMessage").textContent = users.length
          ? "User accounts loaded."
          : "No matching users.";
    } catch (error) {
      users = [];
      hasMore = false;
      $("userMessage").textContent = error.message;
    } finally {
      usersBusy = false;
      renderUsers();
    }
  }
  $("userSearch").onsubmit = (e) => {
    e.preventDefault();
    if (usersBusy || actionBusy) return;
    search = {
      username: $("emailFilter").value.trim(),
      name: $("nameFilter").value.trim(),
    };
    offset = 0;
    loadUsers();
  };
  $("clearUsers").onclick = () => {
    if (usersBusy || actionBusy) return;
    $("userSearch").reset();
    search = { username: "", name: "" };
    offset = 0;
    loadUsers();
  };
  $("prevUsers").onclick = () => {
    if (usersBusy || actionBusy) return;
    offset = Math.max(0, offset - 50);
    loadUsers();
  };
  $("nextUsers").onclick = () => {
    if (usersBusy || actionBusy) return;
    offset += 50;
    loadUsers();
  };
  $("reloadUsers").onclick = () => loadUsers();
  function field(label, type, id) {
    const l = el("label", label),
      input = document.createElement(type === "select" ? "select" : "input");
    input.id = id;
    if (type !== "select") input.type = type;
    input.required = true;
    l.append(input);
    $("dialogFields").append(l);
    return input;
  }
  function updateConfirm() {
    const a = action;
    $("dialogConfirm").disabled =
      actionBusy ||
      !a ||
      (a.kind === "delete" && $("deleteEmail")?.value !== a.user.username) ||
      (a.kind === "role" &&
        (!$("newRole")?.value || $("newRole").value === a.user.role));
  }
  async function openAction(a) {
    if (usersBusy || actionBusy) return;
    action = a;
    $("actionForm").reset();
    $("dialogFields").replaceChildren();
    $("dialogError").textContent = "";
    const titles = {
      delete: "Permanently delete account",
      all: "End all sessions",
      one: "End session",
      role: "Change role",
      password: "Change password",
    };
    $("dialogTitle").textContent = titles[a.kind];
    $("dialogFields").append(
      el("p", `${a.user.username} · ${a.user.name || "—"}`),
      el("p", "tenantId: " + a.user.tenantId, "mono"),
    );
    if (a.kind === "delete") {
      $("dialogFields").append(
        el(
          "p",
          "This permanently deletes this user, every record belonging to this account, and all its sessions. Shared global sports and role definitions remain. This cannot be undone.",
        ),
      );
      field(
        "Type the exact email address to confirm",
        "text",
        "deleteEmail",
      ).oninput = updateConfirm;
    } else if (a.kind === "role") {
      const select = field("Role", "select", "newRole");
      select.disabled = true;
      select.onchange = updateConfirm;
    } else if (a.kind === "password") {
      const p = field("New password", "password", "newPassword"),
        c = field("Confirm password", "password", "confirmPassword");
      p.autocomplete = c.autocomplete = "new-password";
      $("dialogFields").append(
        el("p", "Password requirements are checked by the server."),
      );
    } else {
      $("dialogFields").append(
        el(
          "p",
          `${a.kind === "all" ? "All sessions" : "Only this session"} will be cancelled. Account data is preserved.${a.user.isCurrentUser ? " This may sign you out." : ""}`,
        ),
      );
      if (a.sessionId) $("dialogFields").append(el("code", a.sessionId));
    }
    $("dialogConfirm").textContent =
      a.kind === "delete"
        ? "Delete account"
        : a.kind === "role"
          ? "Save role"
          : a.kind === "password"
            ? "Save password"
            : "End session" + (a.kind === "all" ? "s" : "");
    $("adminDialog").showModal();
    updateConfirm();
    if (a.kind === "role") {
      try {
        const data = await request("api/admin/roles");
        if (action !== a || !$("adminDialog").open) return;
        if (
          !Array.isArray(data.roles) ||
          !data.roles.length ||
          data.roles.some((r) => typeof r !== "string" || !r.trim())
        )
          throw Error("No valid roles returned by the server.");
        const select = $("newRole");
        select.append(new Option("Select a role", ""));
        [...new Set(data.roles)].forEach((r) =>
          select.append(new Option(r, r)),
        );
        select.value = a.user.role;
        select.disabled = false;
        updateConfirm();
      } catch (error) {
        if (action === a) $("dialogError").textContent = error.message;
      }
    }
  }
  $("dialogCancel").onclick = () => {
    if (!actionBusy) $("adminDialog").close();
  };
  $("adminDialog").addEventListener("cancel", (e) => {
    if (actionBusy) e.preventDefault();
  });
  $("adminDialog").addEventListener("close", () => {
    $("actionForm").reset();
    $("dialogFields").replaceChildren();
    action = null;
    if (focusReturn?.isConnected) focusReturn.focus();
  });
  $("actionForm").onsubmit = async (e) => {
    e.preventDefault();
    if (!action || actionBusy || $("dialogConfirm").disabled) return;
    const a = action;
    if (
      a.kind === "password" &&
      $("newPassword").value !== $("confirmPassword").value
    ) {
      $("dialogError").textContent = "Passwords do not match.";
      return;
    }
    let path, payload;
    if (a.kind === "password") {
      path = "password";
      payload = { userId: a.user.id, newPassword: $("newPassword").value };
    } else if (a.kind === "role") {
      path = "role";
      payload = { userId: a.user.id, role: $("newRole").value };
    } else if (a.kind === "delete") {
      path = "delete";
      payload = {
        userId: a.user.id,
        confirmationUsername: $("deleteEmail").value,
      };
    } else {
      path = "sessions/cancel";
      payload = {
        userId: a.user.id,
        allSessions: a.kind === "all",
        sessionId: a.sessionId || "",
      };
    }
    actionBusy = true;
    userButtons();
    $("dialogCancel").disabled = true;
    updateConfirm();
    $("dialogError").textContent = "";
    try {
      const result = await post("api/admin/users/" + path, payload);
      actionBusy = false;
      $("adminDialog").close();
      $("userMessage").textContent = result.message || "Action completed.";
      if (
        result.currentSessionCancelled ||
        (a.kind === "role" && a.user.isCurrentUser)
      ) {
        notice(
          "Your administrator session changed. Sign in again to continue.",
        );
        return;
      }
      await loadUsers(true);
    } catch (error) {
      $("dialogError").textContent = error.message;
      if (a.kind === "password") {
        $("newPassword").value = "";
        $("confirmPassword").value = "";
      }
    } finally {
      actionBusy = false;
      $("dialogCancel").disabled = false;
      updateConfirm();
      userButtons();
    }
  };
  let sports = [],
    sportBusy = false,
    roleBusy = false,
    roleSport = "",
    drafts = new Map(),
    roleRequest = 0;
  function catalogLocks() {
    const locked = sportBusy || roleBusy;
    $("addSportForm")
      .querySelectorAll("button,input")
      .forEach((e) => (e.disabled = locked));
    $("sportRows")
      .querySelectorAll("button")
      .forEach((b) => (b.disabled = locked));
    $("roleSport").disabled = locked;
    $("roleFields").disabled = locked || !roleSport;
  }
  async function loadSports() {
    try {
      const data = await request("api/sports");
      const list = Array.isArray(data) ? data : data.sports || data.Sports;
      if (!Array.isArray(list)) throw Error("Invalid sports catalog response.");
      sports = list.map((s) => ({
        id: s.id ?? s.ID ?? "",
        name: s.name ?? s.Name ?? "",
        active: s.active ?? s.Active ?? false,
      }));
      renderSports();
    } catch (error) {
      notice(error.message, true);
    }
  }
  function renderSports() {
    $("sportRows").replaceChildren();
    $("roleSport").replaceChildren(new Option("Select a sport", ""));
    sports.forEach((s) => {
      const row = el("div", "", "listrow"),
        buttons = el("div", "", "rowActions");
      row.append(el("strong", s.name));
      const toggle = el(
        "button",
        s.active ? "Active · deactivate" : "Inactive · activate",
        "btn",
      );
      toggle.type = "button";
      toggle.setAttribute(
        "aria-label",
        `${s.name}: ${s.active ? "deactivate" : "activate"}`,
      );
      toggle.onclick = () => changeSport(s, "toggle");
      const del = el("button", "Delete", "btn");
      del.type = "button";
      del.onclick = () => changeSport(s, "delete");
      buttons.append(toggle, del);
      row.append(buttons);
      $("sportRows").append(row);
      $("roleSport").append(
        new Option(s.name + (s.active ? "" : " (inactive)"), s.name),
      );
    });
    if (!sports.length) $("sportRows").append(el("p", "No sports configured."));
    if (!sports.some((s) => s.name === roleSport)) {
      roleSport = "";
      ++roleRequest;
    }
    $("roleSport").value = roleSport;
    renderRoles();
    catalogLocks();
  }
  $("addSportForm").onsubmit = async (e) => {
    e.preventDefault();
    if (sportBusy || roleBusy) return;
    const name = $("sportName").value.trim();
    if (!name) return;
    sportBusy = true;
    catalogLocks();
    try {
      await post("api/sports", { id: name, name, active: false });
      $("addSportForm").reset();
      await loadSports();
      notice("Sport added as inactive. Configure roles before activation.");
    } catch (error) {
      notice(error.message, true);
    } finally {
      sportBusy = false;
      catalogLocks();
    }
  };
  async function changeSport(s, kind) {
    if (sportBusy || roleBusy) return;
    if (
      kind === "delete" &&
      !window.confirm(
        `Permanently delete ${s.name} and its official role settings? This cannot be undone.`,
      )
    )
      return;
    sportBusy = true;
    catalogLocks();
    try {
      if (kind === "toggle") {
        await post("api/sport-" + (s.active ? "inactive" : "active"), {
          id: s.id,
          name: s.name,
        });
        notice(s.name + " is now " + (s.active ? "inactive." : "active."));
      } else {
        await request("api/sports?" + new URLSearchParams({ id: s.id }), {
          method: "DELETE",
        });
        let cleanup = "";
        try {
          await rolesRequest(
            "api/settings/official-roles?" +
              new URLSearchParams({ sport: s.name }),
            { method: "DELETE" },
          );
        } catch (error) {
          cleanup = error.message;
        }
        drafts.delete(s.name);
        if (roleSport === s.name) {
          roleSport = "";
          ++roleRequest;
        }
        notice(
          cleanup
            ? `Sport deleted, but role cleanup failed: ${cleanup}`
            : "Sport and role settings deleted.",
          !!cleanup,
        );
      }
      await loadSports();
    } catch (error) {
      notice(error.message, true);
    } finally {
      sportBusy = false;
      catalogLocks();
    }
  }
  async function rolesRequest(path, options = {}) {
    return request(path, {
      ...options,
      allowMissing: !options.method || options.method === "GET",
    });
  }
  function renderRoles() {
    $("roleRows").replaceChildren();
    const roles = drafts.get(roleSport) || [];
    if (!roles.length)
      $("roleRows").append(
        el(
          "p",
          roleSport
            ? "No roles defined. Click Add role."
            : "Select a sport to configure its roles.",
          "small",
        ),
      );
    roles.forEach((role, i) => {
      const row = el("div", "", "roleEdit"),
        input = document.createElement("input");
      input.type = "text";
      input.required = true;
      input.value = role.title;
      input.setAttribute("aria-label", `Role ${i + 1} title`);
      input.oninput = () => (role.title = input.value);
      const label = el("label", "Active"),
        active = document.createElement("input");
      active.type = "checkbox";
      active.checked = role.active ?? true;
      active.onchange = () => {
        role.active = active.checked;
        $("roleMessage").textContent = "Click Save roles to save changes.";
      };
      label.prepend(active);
      row.append(input, label);
      for (const [text, disabled, fn] of [
        [
          "↑",
          i === 0,
          () => {
            [roles[i - 1], roles[i]] = [roles[i], roles[i - 1]];
          },
        ],
        [
          "↓",
          i === roles.length - 1,
          () => {
            [roles[i + 1], roles[i]] = [roles[i], roles[i + 1]];
          },
        ],
        ["Remove", false, () => roles.splice(i, 1)],
      ]) {
        const b = el("button", text, "btn");
        b.type = "button";
        b.disabled = disabled;
        b.setAttribute(
          "aria-label",
          `${text === "↑" ? "Move up" : text === "↓" ? "Move down" : "Remove"} role ${i + 1}`,
        );
        b.onclick = () => {
          fn();
          renderRoles();
        };
        row.append(b);
      }
      $("roleRows").append(row);
    });
    catalogLocks();
  }
  $("roleSport").onchange = async () => {
    roleSport = $("roleSport").value;
    const sport = roleSport,
      seq = ++roleRequest;
    $("roleMessage").textContent = "";
    if (!sport || drafts.has(sport)) {
      renderRoles();
      return;
    }
    roleBusy = true;
    renderRoles();
    try {
      const data = await rolesRequest(
        "api/settings/official-roles?" + new URLSearchParams({ sport }),
      );
      if (seq !== roleRequest) return;
      if (!Array.isArray(data.roles))
        throw Error("Invalid official roles response.");
      drafts.set(
        sport,
        data.roles
          .map((r) => ({ ...r, title: String(r.title ?? "") }))
          .sort((a, b) => (a.displayOrder ?? 0) - (b.displayOrder ?? 0)),
      );
    } catch (error) {
      if (seq === roleRequest) {
        roleSport = "";
        $("roleSport").value = "";
        $("roleMessage").textContent = error.message;
      }
    } finally {
      if (seq === roleRequest) {
        roleBusy = false;
        renderRoles();
      }
    }
  };
  $("addRole").onclick = () => {
    if (!roleSport || roleBusy || sportBusy) return;
    const roles = drafts.get(roleSport);
    if (!roles) return;
    roles.push({ roleId: crypto.randomUUID(), title: "", active: true });
    renderRoles();
    $("roleRows").querySelector(".roleEdit:last-child input")?.focus();
  };
  $("clearRoles").onclick = () => {
    if (roleBusy || sportBusy) return;
    drafts.delete(roleSport);
    roleSport = "";
    ++roleRequest;
    $("roleSport").value = "";
    $("roleMessage").textContent = "";
    renderRoles();
  };
  $("roleForm").onsubmit = async (e) => {
    e.preventDefault();
    if (!roleSport || roleBusy || sportBusy) return;
    const sport = roleSport,
      roles = (drafts.get(sport) || []).map((r, i) => ({
        ...r,
        title: r.title.trim(),
        displayOrder: i + 1,
      }));
    if (
      roles.some((r) => !r.title) ||
      new Set(roles.map((r) => r.title.toLowerCase())).size !== roles.length
    ) {
      $("roleMessage").textContent = "Enter unique, nonempty role titles.";
      return;
    }
    if (
      !roles.length &&
      !window.confirm(`Save ${sport} with no official roles?`)
    )
      return;
    roleBusy = true;
    catalogLocks();
    try {
      await rolesRequest("api/settings/official-roles", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ sport, roles }),
      });
      drafts.set(sport, roles);
      $("roleMessage").textContent = "Official roles saved for " + sport + ".";
    } catch (error) {
      $("roleMessage").textContent = error.message;
    } finally {
      roleBusy = false;
      renderRoles();
    }
  };
  navigate();
})();
