# Ref Ledger logs: Phase 1 rollout

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
The browser does not receive database credentials. No Ref Ledger logging API
or ingress changes are needed.

To roll back, remove all five required logs settings and restart Monitor; the
Logs routes then return a service-unavailable message. Keep the two existing
Ref Ledger transport validation checks unchanged. Threshold changes and
clearing are disabled until Phase 2 command processing and audit verification
are deployed. Do not grant Monitor write or delete access to the log store.
