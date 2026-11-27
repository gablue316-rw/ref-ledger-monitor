# Ref Ledger Monitor for Android

The Android app displays the existing Monitor website from a configured HTTPS
origin. Sign-in, sessions, metrics, logs, Details and administration are served
by that Monitor instance; the app does not contain database credentials or
administrative service tokens. Deploy the matching changes from this branch to
the Monitor backend before testing the app.

Monitor has one configured administrator identity and an instance-wide log
environment/tenant scope, not per-administrator tenant permissions. It has no
Subscriptions screen on this branch. See `../LOGS_ATLAS_ROLLOUT.md` for log
scope and command-worker configuration. A disabled Clear Logs control shows
the reason reported by the server; a permitted clear retains the server's
scope, confirmation, command and audit checks.
The dashboard tenant selector filters metrics already returned to the browser;
it is not an authorization boundary.

## Announcement backend deployment

Monitor proxies announcement management to the Ref Ledger contract pinned at
[`d7782e0942d68ff2a15dc8b173d270201cf80ca9`](https://github.com/gablue316-rw/ref-ledger-v2.0/blob/d7782e0942d68ff2a15dc8b173d270201cf80ca9/docs/announcements-api.md).
Ref Ledger owns announcement state, validation, scheduling, and delivery.

Set both `ANNOUNCEMENTS_ADMIN_URL` to the Ref Ledger origin (no path, query,
fragment, or userinfo) and `ANNOUNCEMENTS_ADMIN_TOKEN` to the same secret as
Ref Ledger's `REF_LEDGER_LOG_API_TOKEN`. The token must contain at least 32
printable non-space ASCII characters and is server-only. Configure neither to
disable the proxy; incomplete or invalid configuration prevents startup.
HTTPS is required, except HTTP to localhost or a loopback IP in
`APP_ENV=development`. The upstream must not redirect. Monitor sends a hashed
administrator identity for auditing, not customer credentials.

Authenticated Monitor endpoints, also available under `/admin`, are:

* `GET /api/admin/announcements` and `POST /api/admin/announcements`
* `PUT /api/admin/announcements/{id}`
* `POST /api/admin/announcements/{id}/clear` with JSON body `{}`
* `GET /api/admin/announcements/sessions?username=...`
* `GET /api/admin/announcements/users?username=...&name=...&offset=...`

The users search is paginated in pages of 50 and returns `users` with only
`id`, `username`, `name`, and `tenantId`, plus `hasMore`, `offset`, and
`pageSize`. Use the selected user's stable `id` as `userId` and `tenantId`
for user targeting; usernames alone are not unique. Do not use the unrelated
`/api/admin/users` session IDs for announcements. For session targeting, use
only `sessionTargetId` from the announcement sessions endpoint, not a session
cookie, token, or `sessionId`.

The upstream sessions endpoint resolves **only username**, not a selected
account. The caller must compare the response's `tenantId` and `userId` and
each session's identity against the selected account before presenting or
submitting a target, and fail closed on any mismatch. Monitor checks that each
returned session matches the upstream response identity, but cannot select the
intended account from the username alone.

In the pinned Ref Ledger implementation, `mongoAnnouncementDirectory.userID`
performs `FindOne` using username alone and then checks tenant. With duplicate
usernames, session resolution and customer delivery for one account can fail
even when publishing a user-targeted announcement succeeds. Monitor cannot
repair that upstream ambiguity; do not infer successful delivery from publish.

Writes require `Content-Type: application/json`, `X-Ref-Ledger-Admin: 1`,
and the existing Monitor origin and CSRF protections. Monitor preserves
upstream 400, 404, 409, and 422 statuses with safe, status-specific `message`
responses. Other upstream errors, invalid or oversized responses, redirects,
and timeouts return 502 without upstream details; disabled integration returns
503. A failed or timed-out write may have succeeded upstream: refresh the
announcement list before retrying. Monitor never forwards browser authorization
or cookies, and never forwards upstream response headers or raw error bodies.
The pinned upstream list is not paginated; Monitor bounds its response to 16 MiB
and other announcement responses to 1 MiB. Lists exceeding the bound cannot be
managed through Monitor until the upstream result shrinks or supports pagination.

## Requirements

* Windows 10/11, Git, Node.js 20 or 22 LTS and npm.
* Android Studio, JDK 21, Android SDK
  Platform 35, SDK Build-Tools 35.0.0, and Android SDK Platform-Tools.
* The checked-in Gradle wrapper downloads Gradle 8.11.1; Android Studio also
  needs access to Google Maven and Maven Central. An HTTPS Monitor origin with
  a publicly trusted certificate, reachable from the phone or emulator.
* Android 6.0 (API 23) or later. USB debugging must be enabled for a USB phone.

The build URL is public configuration, not a secret. Each APK is bound to
one Monitor origin. Do not use a production URL for a development build. The
backend must serve the administration UI at `/admin/`; HTTP, credentials,
paths other than `/admin/`, query strings and fragments are rejected. Install
another build to change origins. Do not embed credentials in the APK.

## Windows PowerShell, clean checkout

```powershell
git clone https://github.com/gablue316-rw/ref-ledger-monitor.git
cd ref-ledger-monitor
git checkout copilot/feature-add-android-app
cd mobile
$env:JAVA_HOME = "C:\Program Files\Eclipse Adoptium\jdk-21"
$env:ANDROID_HOME = "$env:LOCALAPPDATA\Android\Sdk"
$env:Path = "$env:JAVA_HOME\bin;$env:ANDROID_HOME\platform-tools;$env:Path"
npm ci
$env:MONITOR_URL = "https://monitor-development.example.com"
npm run sync
npm test
cd android
.\gradlew.bat assembleDebug
```

Replace the example URL and the JDK and SDK paths with your installed paths.
In Android Studio, also set **Gradle JDK** to JDK 21.
The APK is
`mobile\android\app\build\outputs\apk\debug\app-debug.apk`. To install to a
connected phone after authorizing USB debugging:

```powershell
adb devices
adb install -r .\app\build\outputs\apk\debug\app-debug.apk
```

Alternatively, after `npm run sync`, open **`mobile\android`** (not the
repository root or `mobile`) in Android Studio. Let Gradle sync, select the
connected Samsung phone or an emulator, and press **Run**. Re-run
`npm run sync` from `mobile` whenever the URL changes, then rebuild/reinstall.
`MONITOR_URL` is evaluated at sync time, not at runtime. Configure the
development HTTPS certificate and backend before installing; an emulator's
`localhost` is not the host computer.

## Acceptance checks still required

Use an authorized administrator and disposable data to verify sign-in,
invalid credentials, expiry, logout and restart; tenant selection and log
details; permissions and tenant scope; administrative writes and confirmation;
background/foreground polling, Back, rotation, and loss of connectivity. Check
that a failed or uncertain write is reconciled with server state before retry.
Run the same server-side authorization tests independently of the app.

JSONL export requires Android WebView support for sharing files via the Web
Share API. After preparing an export, tap **Share prepared export**. If sharing
is unavailable, the app explicitly reports that limitation; use the
Monitor website in a browser to export. The share target chosen by the user
may persist sensitive logs. Verify export and server audit records with
disposable data before relying on it. Google Play distribution is out of scope.
