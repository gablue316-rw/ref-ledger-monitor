# Ref Ledger Monitor — real collection metrics

Copy these files into your existing ref-ledger-monitor project, preserving the paths:

- cmd/server/main.go — replaces the earlier server and placeholder metrics handler.
- internal/model/metrics.go — response structures.
- internal/database/metrics.go — read-only MongoDB queries.
- internal/handlers/metrics.go — endpoint, timeout and HTTP errors.
- web/index.html — replaces the earlier page, supports unavailable HTTP counters,
  counts active sessions, and removes the automatic sample-data fallback.
- The *_test.go files are optional for deployment; include them to run the tests.

The copyable-text/ folder contains the same source as .txt files for copying.
Do not put the .txt copies into Go source directories as additional .go files.

## Install and build

Run from the repository root in Git Bash:

```bash
go mod tidy
go test ./...
go build -o ref-ledger-monitor.exe ./cmd/server/.
./ref-ledger-monitor.exe
```

The included go.mod uses `module ref-ledger-monitor`. If your existing module
path differs, use that path in the internal imports, or use the provided go.mod
and go.sum. No additional package beyond the existing v1 MongoDB driver is needed
for the application. The tests use that driver's mock testing package.

Keep your existing environment variables (including PORT=8082 if 8081 is busy).
The same database must be selected in Ref Ledger and Ref Ledger Monitor.
Run from the repository root so ./web/index.html is found.
Your existing monitor username/password protect the page and API.

## Endpoint behavior

GET /api/metrics/last-minute uses the existing requireAuth wrapper.

1. Reads the users collection with a projection containing only tenantId.
2. Reads distinct tenantId values from sessions where expiresAt > the request time.
3. Aggregates metrics where category=collection and bucketStart is in the last
   completed UTC minute, grouped by tenantId and collection. All four counters
   are summed. Missing counter fields sum as zero.
4. Includes all seven collections for every registered user, including zero
   counts when no bucket exists. Rows sort by User ID.

Example at 12:08:25 UTC: counters cover 12:07:00 <= bucketStart < 12:08:00.
The minute currently being written is deliberately excluded. Refreshing every
5 seconds does not turn one-minute buckets into a rolling 60-second window.

Active users now means users with at least one UNEXPIRED SIGN-IN SESSION at the
request time. It does not mean recent HTTP activity. Multiple devices count once.
The top summary remains global when you select an individual user.

The supplied metrics writer only stores collection changes, not HTTP bytes.
HTTP bytes in/out are therefore null in JSON and N/A on the page until Ref Ledger
records that telemetry. The monitor does not invent HTTP values or use its own
traffic as a substitute. Ref Ledger collection operations do not imply an active
sign-in session; a test harness can write metrics without signing in.

User ID on the page continues to display tenantId, as previously requested.
This assumes one users document per tenantId. Missing or duplicate user tenantId
values produce an explicit error instead of double-counting collection activity.
Historical metrics for deleted users are excluded from current-user totals.

This reader creates no indexes and writes no records. Its database identity needs
read access to users, metrics AND sessions. The existing Ref Ledger writer remains
unchanged. For large metrics history, consider a separate category/bucketStart
index in Ref Ledger's startup code; the monitor does not need index-write access.

The page no longer falls back to sample data. Failed requests show an error;
previously received real values remain marked as stale.

## Validation

Go unit tests and MongoDB driver mock-response tests cover UTC boundaries,
zero-activity users, counter decoding, user/session joining, multiple sessions,
unknown/deleted users, unavailable HTTP values, malformed identities, method
handling and query failures. The server was also cross-compiled for Windows amd64.
No connection to your actual database was made during these checks.
