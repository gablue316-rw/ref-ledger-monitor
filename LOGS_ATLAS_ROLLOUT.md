# Ref Ledger logs rollout

The existing Monitor administrator session grants access to one configured
environment and tenant scope. Monitor does not currently have individual
environment or tenant permissions; do not enable this feature for an
administrator who should have less access than the configured scope. Deploy a
separately scoped Monitor instance where different permissions are required.

Set these server-side deployment secrets and settings together:

- `LOGS_READER_URI`: dedicated Atlas database user, TLS enabled and certificate
  verification required.
- `LOGS_AUDIT_URI`: a **different** Atlas database user for audit inserts.
- `LOGS_DATABASE`: the Ref Ledger database containing `refLedgerLogs`.
- `LOGS_ENVIRONMENT`: the exact value stored in the records' `environment`.
- `LOGS_TENANTS`: comma-separated allowed tenant IDs, or `*` only when the
  administrator is authorized for every tenant and environment-wide events.
- `LOGS_READ_SETTINGS`: set to `true` only when the reader has permission to
  read the dedicated `refLedgerLogSettings` collection. Otherwise the threshold
  is unavailable.
- `LOGS_ADMIN_URL` and `LOGS_ADMIN_TOKEN` (optional, both required for threshold
  updates): the origin of the Ref Ledger service and its
  `REF_LEDGER_LOG_API_TOKEN`. Use HTTPS, except for loopback HTTP in development.
  The token must contain at least 32 non-whitespace ASCII characters. Configure
  the service so every instance behind this origin uses the same environment
  and database as `LOGS_ENVIRONMENT` and `LOGS_DATABASE`. Monitor verifies the
  service's environment before each update; the API does not support an atomic
  environment precondition. Do not mix environments behind this origin.
- `LOGS_COMMAND_MODE`: defaults to `https` (the existing threshold API). Set
  explicitly to `atlas` to submit threshold and clear commands through Atlas.
- `LOGS_COMMAND_URI`: required in Atlas mode. Use a third, dedicated Atlas
  database user with TLS, connected to the same database and cluster as the
  Ref Ledger command worker. Atlas mode ignores, but does not remove, the
  existing `LOGS_ADMIN_URL` and `LOGS_ADMIN_TOKEN` settings. Set the mode back
  to `https` to restore the previous path. `LOGS_READ_SETTINGS=true` is required
  for threshold updates in either mode.

Leaving all five required settings absent disables the feature. Partial
configuration fails startup. In deployed environments the two connection
strings must use `mongodb+srv`, distinct named users, and validated TLS;
connection options that weaken TLS are rejected. Loopback-only non-TLS MongoDB
is allowed solely for development verification.

Create Atlas custom roles scoped to the intended database and collections:

- Reader: `find` on `refLedgerLogs`, and optionally `find` on
  `refLedgerLogSettings`. No log insert, update, or delete, and no unrelated
  collection access.
- Auditor: `insert` on `monitorLogExportAudit` only. This credential is
  separate from the reader and cannot modify or delete existing audit entries.
  Arrange retention, monitoring, and administrative review for this collection.
- Atlas command user: `insert` and `find` on `refLedgerLogCommands` only.
  Do not grant update or delete, or access to logs, settings, or audit history.
  Treat this credential as privileged: the worker executes its valid commands.

Enable the Ref Ledger worker separately using `REF_LEDGER_LOG_COMMANDS_ENABLED`
and its explicit actor and tenant allowlists. Add each Monitor administrator's
actor as `monitor-` followed by the lowercase SHA-256 hex digest of the session
username to `REF_LEDGER_LOG_COMMAND_ACTORS`; put the clearable tenant IDs in
`REF_LEDGER_LOG_COMMAND_TENANTS`. These values and `LOGS_ENVIRONMENT` must
match the worker's environment and policy. A mismatch produces a rejected or
permanently pending command. The worker requires an Atlas deployment supporting
transactions; it applies each effect, command result, and Ref Ledger audit
record in one transaction.

Monitor offers a named tenant clear, or an environment-wide clear only when
`LOGS_TENANTS=*`. It rejects blank tenant IDs for tenant clears and enforces
the configured scope before submission. Review the selected environment,
scope and cutoff and optionally export that exact scope before confirming
`CLEAR <environment>`. The export is a snapshot: late-arriving records with
older timestamps may still be cleared.

**Worker dependency:** Ref Ledger's `development` worker currently accepts
only `clear_logs` with `scope: "tenant"` and an allowlisted `tenantId`. It
rejects Monitor's environment-wide command (`scope: "environment"`, empty
`tenantId`) as `invalid_scope`. Before enabling that choice operationally,
update and deploy the worker to explicitly authorize environment-wide clears
independently of Monitor (for example with an explicit actor allowlist), accept
that scope only with an empty tenant ID, and keep rejecting blank tenant IDs
for tenant scope. Its audit scope must distinguish environment-wide and tenant
clears. The existing deletion helper filters by environment and cutoff and,
for an empty tenant ID, includes tenantless server records; retain its
transactional command result and audit behavior. Verify both scopes, newer
records, other environments and audit history against the updated worker
before rollout.

Pending commands retain their request ID in the browser tab's session storage.
After a timeout, check the original request or retry the same submission; do
not start a new one while its outcome is unknown. A retry uses the same ID and
cannot repeat a completed operation. If the tab is closed, recover the request
ID and status from `refLedgerLogCommands` before submitting another command.
Investigate pending commands and worker logs rather than assuming a timeout
means failure.

The server records a `started` and a final `success` or `failed` audit entry
with a shared export ID. A missing final entry requires investigation; it can
indicate an interrupted process or an audit-store failure. Export requires a
successful initial audit insert. The server logs a sanitized diagnostic if the
initial or final audit insert fails. Correlate its `export_id` with the
`exportId` in the durable `started` entry; an initial insert failure may have
no durable entry. The application log collection and existing 90-day TTL
remain owned by Ref Ledger.

The browser verifies the exported record count, but browser Fetch does not
expose HTTP trailers. Consequently a final audit-insert failure after streaming
starts is visible in server logs and the missing final audit entry, not in the
browser. The page warns that final audit status is unavailable. Monitor those
conditions and resolve this limitation before treating browser export status
as confirmation that the final audit was recorded.

Before enabling, verify the deployed Monitor host's stable allowlisted egress or
private connectivity, TLS, scoped grants, and tenant scope. Confirm a fresh
Ref Ledger record and a historical record in the viewer, filters and both JSONL
exports, and a durable audit record. Test failures using nonproduction data.
The browser does not receive database credentials or the logs API token. When
threshold updates are configured, Monitor needs server-to-server connectivity to
Ref Ledger's authenticated logs admin API. The service records its own audit
entry for threshold updates. Monitor sends a stable SHA-256 representation of
the authenticated session username as the actor; Ref Ledger pseudonymizes it
again for its audit record. An unsuccessful or timed-out request may still
have changed the threshold; refresh the effective value before retrying.

To disable the entire feature, remove all five required logs settings and the
optional `LOGS_ADMIN_URL`, `LOGS_ADMIN_TOKEN`, `LOGS_COMMAND_MODE`, and
`LOGS_COMMAND_URI` settings, then restart Monitor; the Logs routes then return
a service-unavailable message. To disable only Atlas commands, switch to
`https` or unset `LOGS_COMMAND_MODE`. Do not grant Monitor write or delete
access to the log store.

In development, verify a switch from Info to Debug and back to Info, refreshing
after each save. Confirm newly emitted Debug entries are captured only while
Debug is enabled, and that previously captured Debug entries remain searchable.
