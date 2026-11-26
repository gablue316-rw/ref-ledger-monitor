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
    if (!response.ok) {
      const error = Error(
        data.message || data.error || text.trim() || `HTTP ${response.status}`,
      );
      error.status = response.status;
      throw error;
    }
    return data;
  }
  function post(path, payload, version) {
    return request(path, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "X-Ref-Ledger-Admin": "1",
        ...(version ? {"If-Match": version} : {}),
      },
      body: JSON.stringify(payload),
    });
  }
  const names = {
    maintenance: ["Maintenance", "Pause application changes before export or backup."],
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
    logs: [
      "Ref Ledger logs",
      "Search, export, and manage Ref Ledger structured logs.",
    ],
  };
  let userStarted = false,
    sportsStarted = false,
    logsStarted = false;
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
    if (page === "maintenance") loadMaintenance();
    if (page === "users" && !userStarted) {
      userStarted = true;
      loadUsers();
    }
    if (page === "catalog" && !sportsStarted) {
      sportsStarted = true;
      loadSports();
    }
    if (page === "logs" && !logsStarted) {
      logsStarted = true;
      $("logsTimeZone").value =
        Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
      loadLogThreshold();
      loadLogs(true);
      if (pendingCommand) checkLogCommand();
    }
  }
  document
    .querySelectorAll("[data-page]")
    .forEach((b) => (b.onclick = () => (location.hash = b.dataset.page)));
  window.addEventListener("hashchange", navigate);
  let logsOffset = 0, logsHasMore = false, logsCutoff = "", logsBusy = false;
  let thresholdBusy = false, thresholdEnvironment = "", thresholdLoad = 0;
  let commandMode = false, clearScope = null, commandBusy = false;
  let pendingCommand = null;
  try { pendingCommand = JSON.parse(sessionStorage.getItem("logsPendingCommand") || "null"); }
  catch { sessionStorage.removeItem("logsPendingCommand"); }
  let commandUncertain = !!pendingCommand;
  const logLevels = {1: "ERROR", 2: "WARNING", 3: "INFO", 4: "DEBUG"};
  const logFilterFields = [
    ["dateFrom", "logsDateFrom"], ["dateThrough", "logsDateThrough"],
    ["timeAt", "logsTimeAt"], ["timeFrom", "logsTimeFrom"],
    ["timeThrough", "logsTimeThrough"], ["timeZone", "logsTimeZone"],
    ["level", "logsLevel"], ["collection", "logsCollection"],
    ["tenantId", "logsTenant"], ["sessionId", "logsSession"],
    ["transactionId", "logsTransaction"], ["userId", "logsUser"],
  ];
  function logFilters() {
    const filters = {};
    for (const [key, id] of logFilterFields) {
      const value = $(id).value.trim();
      if (value) filters[key] = key === "level" ? Number(value) : value;
    }
    if (filters.timeAt && (filters.timeFrom || filters.timeThrough))
      throw Error("Choose a single time or a time range, not both.");
    return filters;
  }
  async function loadLogThreshold() {
    const load = ++thresholdLoad;
    $("logsThreshold").disabled = true;
    $("logsThresholdSave").disabled = true;
    thresholdEnvironment = "";
    $("logsThresholdMessage").textContent = "Loading capture threshold…";
    $("logsThreshold").value = "";
    try {
      const data = await request("api/admin/logs/threshold");
      if (load !== thresholdLoad) return false;
      if (![1, 2, 3, 4].includes(data.threshold))
        throw Error("Ref Ledger returned an invalid capture threshold.");
      if (!data.environment) throw Error("Ref Ledger returned an invalid environment.");
      thresholdEnvironment = data.environment;
      commandMode = data.commandMode === true;
      $("logsThreshold").value = String(data.threshold);
      $("logsEnvironment").textContent = data.environment || "Unknown";
      $("logsClearEnvironment").textContent = data.environment || "Unknown";
      $("logsThresholdMessage").textContent =
        `Effective threshold: ${logLevels[data.threshold]}.`;
      $("logsThresholdMessage").style.color = "";
      $("logsThreshold").disabled = !data.updateEnabled || !!pendingCommand;
      $("logsThresholdSave").disabled = !data.updateEnabled || !!pendingCommand;
      return true;
    } catch (error) {
      if (load !== thresholdLoad) return false;
      $("logsThresholdMessage").textContent =
        `Unable to load the capture threshold: ${error.message}`;
      $("logsThresholdMessage").style.color = "#a32020";
      return false;
    }
  }
  async function loadLogs(reset) {
    if (logsBusy) return;
    logsBusy = true;
    $("logsMessage").textContent = "Loading Ref Ledger logs…";
    try {
      if (reset) {
        logsOffset = 0;
        logsCutoff = "";
      }
      const params = new URLSearchParams(logFilters());
      params.set("offset", String(logsOffset));
      params.set("limit", "50");
      if (logsCutoff) params.set("cutoff", logsCutoff);
      const data = await request(`api/admin/logs/records?${params}`);
      logsCutoff = data.cutoff;
      logsHasMore = data.hasMore === true;
      $("logsEnvironment").textContent = data.environment || "Unknown";
      $("logsClearEnvironment").textContent = data.environment || "Unknown";
      $("logsClearOpen").disabled = !data.clearEnabled || !!pendingCommand;
      $("logsClearAvailability").textContent = data.clearEnabled
        ? ""
        : (data.clearUnavailableReason || "Log clearing is unavailable.");
      renderLogs(Array.isArray(data.items) ? data.items : []);
      $("logsPage").textContent = data.items.length
        ? `Showing ${logsOffset + 1}–${logsOffset + data.items.length}${logsHasMore ? " · more available" : ""} · cutoff ${logsCutoff}`
        : "0 matching records";
      $("logsMessage").textContent = data.items.length
        ? `${data.items.length} record(s) · displayed in ${data.timeZone || "UTC"}`
        : "No logs match these filters.";
      $("logsMessage").style.color = "";
    } catch (error) {
      $("logsRows").replaceChildren();
      $("logsClearOpen").disabled = true;
      $("logsClearAvailability").textContent = `Log clearing is unavailable: ${error.message}`;
      $("logsMessage").textContent = `Unable to load logs: ${error.message}`;
      $("logsMessage").style.color = "#a32020";
      logsHasMore = false;
    } finally {
      logsBusy = false;
      $("logsPrev").disabled = logsOffset === 0;
      $("logsNext").disabled = !logsHasMore;
    }
  }
  function renderLogs(items) {
    const rows = $("logsRows");
    rows.replaceChildren();
    if (!items.length) {
      const tr = el("tr", ""), td = el("td", "No logs match these filters.");
      td.colSpan = 7;
      tr.append(td);
      rows.append(tr);
      return;
    }
    for (const [index, item] of items.entries()) {
      const tr = el("tr", "", "log-summary");
      const date = new Date(item.timestamp);
      const time = Number.isNaN(date.getTime())
        ? "Invalid timestamp"
        : date.toLocaleString(undefined, {timeZone: $("logsTimeZone").value || "UTC"});
      const level = logLevels[item.level] || "UNKNOWN";
      for (const value of [
        `${time} [${level}]`, item.event || "—", item.message || "—",
        `${item.operation || "—"} / ${item.outcome || "—"}`,
        `${item.tenantId || "—"} / ${item.userId || "—"}`,
      ]) {
        const cell = el("td", value);
        cell.title = value;
        tr.append(cell);
      }
      const transaction = el("td", "");
      if (item.transactionId) {
        const drill = el("button", item.transactionId, "log-transaction");
        drill.type = "button";
        drill.title = `Show logs for transaction ${item.transactionId}`;
        drill.onclick = () => drillIntoTransaction(item.transactionId);
        transaction.append(drill);
      } else {
        transaction.textContent = "—";
      }
      const detailsCell = el("td", "");
      const toggle = el("button", "Details", "btn log-details-toggle");
      toggle.type = "button";
      toggle.setAttribute("aria-expanded", "false");
      toggle.setAttribute("aria-controls", `log-details-${index}`);
      const detailsRow = el("tr", "", "log-details");
      detailsRow.id = `log-details-${index}`;
      detailsRow.hidden = true;
      const details = el("td", "");
      details.colSpan = 7;
      const attributes = item.attributes || {};
      const http = {};
      for (const key of ["method", "route", "statusCode", "bytesIn", "bytesOut"])
        if (attributes[key] !== undefined && attributes[key] !== null) http[key] = attributes[key];
      for (const [label, value] of [
        ["HTTP metadata", http],
        ["Saved fields", attributes.savedValues],
        ["Changed fields (old / new)", attributes.changedValues],
      ]) {
        details.append(el("strong", label));
        details.append(el("pre", value && Object.keys(value).length ? JSON.stringify(value, null, 2) : ""));
      }
      details.append(el("strong", "Recorded entry"));
      details.append(el("pre", JSON.stringify(item, null, 2)));
      detailsRow.append(details);
      toggle.onclick = () => {
        detailsRow.hidden = !detailsRow.hidden;
        toggle.setAttribute("aria-expanded", String(!detailsRow.hidden));
      };
      detailsCell.append(toggle);
      tr.append(transaction, detailsCell);
      rows.append(tr, detailsRow);
    }
  }
  function drillIntoTransaction(transactionId) {
    for (const [, id] of logFilterFields) $(id).value = "";
    $("logsTimeZone").value =
      Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
    $("logsTransaction").value = transactionId;
    loadLogs(true);
  }
  $("logsFilters").onsubmit = (event) => {
    event.preventDefault();
    loadLogs(true);
  };
  $("logsClearFilters").onclick = () => {
    for (const [, id] of logFilterFields) $(id).value = "";
    $("logsTimeZone").value =
      Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
    loadLogs(true);
  };
  $("logsRefresh").onclick = () => {
    if (!thresholdBusy) loadLogThreshold();
    loadLogs(true);
  };
  $("logsPrev").onclick = () => {
    logsOffset = Math.max(0, logsOffset - 50);
    loadLogs(false);
  };
  $("logsNext").onclick = () => {
    if (logsHasMore) {
      logsOffset += 50;
      loadLogs(false);
    }
  };
  $("logsThresholdForm").onsubmit = async (event) => {
    event.preventDefault();
    if (thresholdBusy || $("logsThresholdSave").disabled) return;
    thresholdBusy = true;
    ++thresholdLoad;
    const threshold = Number($("logsThreshold").value);
    const environment = thresholdEnvironment;
    if (commandMode) {
      thresholdBusy = false;
      await submitLogCommand({type: "set_capture_threshold", threshold, environment});
      return;
    }
    $("logsThreshold").disabled = true;
    $("logsThresholdSave").disabled = true;
    $("logsThresholdMessage").textContent = "Saving capture threshold…";
    let failed = false;
    try {
      await request("api/admin/logs/threshold", {
        method: "PUT",
        headers: {"Content-Type": "application/json", "X-Ref-Ledger-Admin": "1"},
        body: JSON.stringify({threshold, environment}),
      });
    } catch {
      failed = true;
    }
    const refreshed = await loadLogThreshold();
    if (failed || !refreshed || Number($("logsThreshold").value) !== threshold) {
      const current = refreshed ? $("logsThresholdMessage").textContent : "Unable to refresh the effective threshold.";
      $("logsThresholdMessage").textContent =
        `${current} The update could not be confirmed and may have succeeded. Refresh the effective threshold before retrying.`;
      $("logsThresholdMessage").style.color = "#a32020";
    }
    thresholdBusy = false;
  };
  let pendingExportFile = null, exportBusy = false;
  async function exportLogs(all, filters, cutoff = "") {
    if (exportBusy) return;
    const android = window.Capacitor?.getPlatform() === "android";
    pendingExportFile = null;
    $("logsShareExport").hidden = true;
    if (android && (!navigator.share || !navigator.canShare)) {
      $("logsMessage").textContent = "Export is unavailable in this Android WebView. Use the Monitor website in a browser to export.";
      $("logsMessage").style.color = "#a32020";
      return;
    }
    exportBusy = true;
    $("logsExportFiltered").disabled = true;
    $("logsExportAll").disabled = true;
    $("logsMessage").textContent = "Preparing JSON Lines export…";
    $("logsMessage").style.color = "";
    try {
      const response = await fetch(adminURL("api/admin/logs/export"), {
        method: "POST",
        cache: "no-store",
        credentials: "same-origin",
        headers: {
          "Content-Type": "application/json",
          "X-Ref-Ledger-Admin": "1",
        },
        body: JSON.stringify({all, filters, cutoff}),
        signal: AbortSignal.timeout(300000),
      });
      if (!response.ok) {
        const text = await response.text();
        let detail = text;
        try { const data = JSON.parse(text); detail = data.message || data.error || text; } catch {}
        throw Error(detail || `HTTP ${response.status}`);
      }
      const blob = await response.blob();
      const expected = Number(response.headers.get("X-Log-Export-Count"));
      const archive = await blob.text();
      const exported = archive.length ? archive.trimEnd().split(/\r?\n/).length : 0;
      if (!Number.isFinite(expected) || exported !== expected)
        throw Error(`Incomplete export: received ${exported} of ${Number.isFinite(expected) ? expected : "unknown"} records.`);
      if (android) {
        const file = new File([blob], "ref-ledger-logs.jsonl", {type: "application/x-ndjson"});
        if (!navigator.canShare({files: [file]}))
          throw Error("This Android WebView cannot share the export. Use the Monitor website in a browser.");
        pendingExportFile = file;
        $("logsShareExport").hidden = false;
      } else {
        const url = URL.createObjectURL(blob), link = document.createElement("a");
        link.href = url;
        link.download = "ref-ledger-logs.jsonl";
        link.click();
        URL.revokeObjectURL(url);
      }
      $("logsMessage").textContent =
        `${android ? "Export prepared; tap Share prepared export" : "Export complete"}: ${exported} record(s), scope ${response.headers.get("X-Log-Export-Scope") || "environment"}, cutoff ${response.headers.get("X-Log-Export-Cutoff") || "server-defined"}. Final audit status is unavailable here; check Monitor audit records.`;
    } catch (error) {
      $("logsMessage").textContent = `Log export failed: ${error.message}`;
      $("logsMessage").style.color = "#a32020";
    } finally {
      exportBusy = false;
      $("logsExportFiltered").disabled = false;
      $("logsExportAll").disabled = false;
    }
  }
  $("logsShareExport").onclick = async () => {
    if (!pendingExportFile || exportBusy) return;
    exportBusy = true;
    $("logsShareExport").disabled = true;
    try {
      await navigator.share({files: [pendingExportFile], title: "Ref Ledger logs"});
      pendingExportFile = null;
      $("logsShareExport").hidden = true;
      $("logsMessage").textContent = "Export shared. Check Monitor audit records for final audit status.";
    } catch (error) {
      $("logsMessage").textContent = `Sharing failed or was cancelled: ${error.message}`;
      $("logsMessage").style.color = "#a32020";
    } finally {
      exportBusy = false;
      $("logsShareExport").disabled = false;
    }
  };
  window.addEventListener("pagehide", () => { pendingExportFile = null; $("logsShareExport").hidden = true; });
  $("logsExportFiltered").onclick = () => {
    try { exportLogs(false, logFilters()); }
    catch (error) { $("logsMessage").textContent = error.message; }
  };
  $("logsExportAll").onclick = () => exportLogs(true, {});
  function showPendingCommand(message) {
    $("logsCommandMessage").textContent = message;
    $("logsCommandCheck").hidden = !pendingCommand;
    $("logsCommandRetry").hidden = !pendingCommand;
    $("logsCommandCheck").disabled = commandBusy;
    $("logsCommandRetry").disabled = commandBusy;
    if (pendingCommand) {
      $("logsThreshold").disabled = true;
      $("logsThresholdSave").disabled = true;
      $("logsClearOpen").disabled = true;
      $("logsClearConfirm").disabled = true;
    }
  }
  async function submitLogCommand(payload) {
    if (pendingCommand || commandBusy) return;
    pendingCommand = {...payload, requestId: crypto.randomUUID()};
    sessionStorage.setItem("logsPendingCommand", JSON.stringify(pendingCommand));
    await retryLogCommand();
  }
  async function retryLogCommand() {
    if (!pendingCommand || commandBusy) return;
    commandBusy = true;
    showPendingCommand(`Submitting request ${pendingCommand.requestId}…`);
    try {
      await post("api/admin/logs/commands", pendingCommand);
    } catch (error) {
      commandBusy = false;
      if (error.status === 400 && !commandUncertain) {
        pendingCommand = null;
        sessionStorage.removeItem("logsPendingCommand");
        showPendingCommand(`Command not submitted: ${error.message}`);
        await loadLogThreshold();
        $("logsClearConfirm").disabled = false;
        return;
      }
      commandUncertain = true;
      showPendingCommand(`Submission unconfirmed: ${error.message} Check the original request or retry it with the same ID.`);
      return;
    }
    commandBusy = false;
    await checkLogCommand();
  }
  async function checkLogCommand() {
    if (!pendingCommand || commandBusy) return;
    commandBusy = true;
    const id = pendingCommand.requestId;
    showPendingCommand(`Pending request ${id}…`);
    try {
      for (let attempt = 0; attempt < 6; attempt++) {
        const status = await request(`api/admin/logs/commands/${encodeURIComponent(id)}`);
        if (status.state === "completed") {
          pendingCommand = null;
          commandUncertain = false;
          sessionStorage.removeItem("logsPendingCommand");
          commandBusy = false;
          const result = status.outcome === "success"
            ? status.type === "clear_logs" ? `Clear completed: ${status.deletedCount} record(s) deleted.` : "Threshold update completed."
            : `Command rejected: ${status.reason || "unknown reason"}.`;
          showPendingCommand(`${result} Request ${id}.`);
          $("logsClearPanel").hidden = true;
          await loadLogThreshold();
          loadLogs(true);
          return;
        }
        if (attempt < 5) await new Promise((resolve) => setTimeout(resolve, 5000));
      }
      commandBusy = false;
      showPendingCommand(`Request ${id} is still pending. Check the original request before submitting another.`);
    } catch (error) {
      commandBusy = false;
      showPendingCommand(`Unable to confirm request ${id}: ${error.message} Check or retry the original request.`);
    }
  }
  $("logsCommandCheck").onclick = checkLogCommand;
  $("logsCommandRetry").onclick = retryLogCommand;
  $("logsClearOpen").onclick = async () => {
    if (pendingCommand || commandBusy) return;
    try {
      clearScope = await request("api/admin/logs/clear-scope");
      $("logsClearEnvironment").textContent = clearScope.environment;
      $("logsClearScope").textContent = clearScope.allTenants ? "all tenants and server logs" : (clearScope.tenants || []).join(", ");
      $("logsClearCutoff").textContent = clearScope.cutoff;
      $("logsClearTenant").value = "";
      $("logsClearAllOption").hidden = !clearScope.allTenants;
      $("logsClearAll").checked = false;
      $("logsClearByTenant").checked = true;
      $("logsClearConfirmation").value = "";
      $("logsClearConfirmation").placeholder = `CLEAR ${clearScope.environment}`;
      $("logsClearConfirmationEnvironment").textContent = clearScope.environment;
      updateClearSelection();
      $("logsClearPanel").hidden = false;
      $("logsClearConfirm").disabled = false;
    } catch (error) {
      $("logsMessage").textContent = `Unable to prepare clearing: ${error.message}`;
    }
  };
  function updateClearSelection() {
    const all = $("logsClearAll").checked;
    const tenantId = $("logsClearTenant").value;
    $("logsClearTenant").disabled = all;
    $("logsClearSelectedScope").textContent = all
      ? "All logs, including server logs without a tenant ID"
      : tenantId ? `Tenant ID: ${tenantId}` : "By Tenant ID (enter an exact tenant ID)";
    $("logsClearConfirmation").value = "";
  }
  $("logsClearAll").onchange = updateClearSelection;
  $("logsClearByTenant").onchange = updateClearSelection;
  $("logsClearTenant").oninput = updateClearSelection;
  $("logsClearCancel").onclick = () => { $("logsClearPanel").hidden = true; };
  $("logsClearExport").onclick = () => {
    const all = $("logsClearAll").checked;
    const tenantId = $("logsClearTenant").value;
    if (!clearScope || (all && !clearScope.allTenants) || (!all && !tenantId)) {
      $("logsMessage").textContent = "Enter a tenant ID before exporting the clear scope.";
      return;
    }
    exportLogs(all, all ? {} : {tenantId, timeZone: $("logsTimeZone").value || "UTC"}, clearScope.cutoff);
  };
  $("logsClearConfirm").onclick = async () => {
    const all = $("logsClearAll").checked;
    const tenantId = all ? "" : $("logsClearTenant").value;
    const confirmation = $("logsClearConfirmation").value;
    if (!clearScope || (all && !clearScope.allTenants) || (!all && !tenantId) || confirmation !== `CLEAR ${clearScope.environment}`) {
      $("logsMessage").textContent = "Select an authorized scope and enter the exact environment confirmation.";
      return;
    }
    await submitLogCommand({type: "clear_logs", environment: clearScope.environment, scope: all ? "environment" : "tenant", tenantId, confirmation, cutoff: clearScope.cutoff});
  };
  // The About dialog lives outside the Metrics subtree so it is available on
  // every administration page. Resolve it with the shell's document-level $.
  async function openAbout() {
    const dialog = $("rl-about-dialog"), status = $("rl-about-status");
    if (!dialog || !status) {
      notice("The About dialog is unavailable. Reload this page and try again.", true);
      return;
    }
    if (dialog.open) return;
    const buttons = [$("shellAbout"), $("rl-about-open")].filter(Boolean);
    status.textContent = "Loading version…";
    try {
      dialog.showModal();
      buttons.forEach(button => button.disabled = true);
      const data = await request("api/about");
      if (typeof data.version !== "string" || !data.version.trim())
        throw Error("Invalid version response");
      status.textContent = "Version: " + data.version.trim();
    } catch (error) {
      status.textContent = "Unable to load the version (" + error.message + "). Close and reopen to retry.";
    } finally {
      buttons.forEach(button => button.disabled = false);
    }
  }
  $("shellAbout").onclick = openAbout;
  $("rl-about-open").onclick = openAbout;
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
          el("div", session.client || "Unknown", "small"),
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
          "This permanently deletes this Auth0 identity, the Ref Ledger user, every record belonging to this account, and all its sessions. Shared global sports and role definitions remain. This cannot be undone.",
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
      const result = await post("api/admin/users/" + path, payload, a.user.version);
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
    roleVersions = new Map(),
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
        version: s.version ?? "",
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
    sportBusy = true;
    catalogLocks();
    try {
      if (kind === "toggle") {
        await post("api/sport-" + (s.active ? "inactive" : "active"), {
          id: s.id, name: s.name,
        }, s.version);
        notice(s.name + " is now " + (s.active ? "inactive." : "active."));
      } else {
        // Capture role settings before confirmation; never use a later refreshed
        // token to delete settings the administrator did not confirm.
        const roleState = await rolesRequest("api/settings/official-roles?" + new URLSearchParams({sport: s.name}));
        if (!roleState.version) throw Error("Reload role settings before deleting this sport.");
        if (!window.confirm(`Permanently delete ${s.name} and its official role settings? This cannot be undone.`)) return;
        await request("api/sports?" + new URLSearchParams({id: s.id}), {
          method: "DELETE",
          headers: {"If-Match": s.version || "", "X-Ref-Ledger-Roles-Version": roleState.version},
        });
        drafts.delete(s.name);
        roleVersions.delete(s.name);
        if (roleSport === s.name) { roleSport = ""; ++roleRequest; }
        notice("Sport and role settings deleted.");
      }
      await loadSports();
    } catch (error) { notice(error.message, true); }
    finally { sportBusy = false; catalogLocks(); }
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
      if (!Array.isArray(data.roles) || typeof data.version !== "string" || !data.version)
        throw Error("Invalid official roles response.");
      roleVersions.set(sport, data.version);
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
  $("reloadRoles").onclick = async () => {
    if (!roleSport || roleBusy || sportBusy) return;
    if (!window.confirm("Discard this sport's unsaved role changes and load its latest settings?")) return;
    drafts.delete(roleSport);
    roleVersions.delete(roleSport);
    await $("roleSport").onchange();
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
    roleVersions.delete(roleSport);
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
      const result = await rolesRequest("api/settings/official-roles", {
        method: "PUT",
        headers: { "Content-Type": "application/json", "If-Match": roleVersions.get(sport) || "" },
        body: JSON.stringify({ sport, roles }),
      });
      if (!result.version || !Array.isArray(result.roles)) throw Error("Settings were saved but their new version could not be verified. Reload roles before saving again.");
      roleVersions.set(sport, result.version);
      drafts.set(sport, result.roles);
      $("roleMessage").textContent = "Official roles saved for " + sport + ".";
    } catch (error) {
      $("roleMessage").textContent = error.message;
    } finally {
      roleBusy = false;
      renderRoles();
    }
  };
  let maintenanceBusy = false, maintenanceVersion = "";
  function maintenanceUnknown(message) {
    maintenanceVersion = "";
    $("maintenanceStatus").textContent = message;
    $("maintenanceOn").disabled = true;
    $("maintenanceOff").disabled = true;
    $("maintenanceChecked").textContent = "Status unverified. Refresh before starting a backup.";
  }
  function showMaintenance(data) {
    if (typeof data.version !== "string" || !data.version) throw Error("Maintenance version is unavailable. Refresh before changing mode.");
    maintenanceVersion = data.version;
    $("maintenanceStatus").textContent = !data.enabled
      ? "Maintenance OFF — application changes are enabled."
      : data.drained
        ? "Maintenance ACTIVE — existing application writes have finished. You can start your application-data export or backup."
        : `Maintenance STARTING — waiting for ${data.activeWrites} application write(s). Do not start your backup yet.`;
    $("maintenanceDatabase").textContent = "Database: " + data.database;
    $("maintenanceChecked").textContent = "Verified: " + new Date().toLocaleTimeString();
    $("maintenanceOn").disabled = data.enabled;
    $("maintenanceOff").disabled = !data.enabled;
  }
  async function loadMaintenance() {
    if (maintenanceBusy) return;
    maintenanceBusy = true;
    maintenanceUnknown("Verifying maintenance status…");
    try { showMaintenance(await request("api/admin/maintenance")); }
    catch (error) { maintenanceUnknown(error.message); }
    finally { maintenanceBusy = false; }
  }
  async function setMaintenance(enabled) {
    if (maintenanceBusy || !maintenanceVersion) return;
    const version = maintenanceVersion;
    maintenanceBusy = true;
    maintenanceUnknown("Changing maintenance mode…");
    try { showMaintenance(await post("api/admin/maintenance", { enabled }, version)); }
    catch (error) { maintenanceUnknown(error.message); }
    finally { maintenanceBusy = false; }
  }
  $("maintenanceOn").onclick = () => setMaintenance(true);
  $("maintenanceOff").onclick = () => setMaintenance(false);
  $("maintenanceRefresh").onclick = loadMaintenance;
  // Maintenance is checked on page entry and explicit button actions only.
  // Keep the verification timestamp stable until the administrator checks again.
  navigate();
})();
