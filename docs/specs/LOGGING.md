# malmo Logging & Audit

> Working spec for how malmo logs operational events and audit events, where each lives, who reads them, and what the dashboard surfaces. Companion to `CONTROL_PLANE.md` (the brain owns audit writes), `AUTH.md` (login events feed the audit log), `BRAIN_HOST_PROTOCOL.md` (host-agent is the bridge between brain and journald), `STORAGE.md` (where the journal physically lives).

## Stance

malmo's logging is **appliance-grade**, not minimalist. The Umbrel/CasaOS pattern (Docker container logs in a webview, no system-wide view, no audit log) almost works for tinkerers and fails the "did someone unauthorized access my stuff?" test for non-technical users. The Synology/Windows pattern (separate concepts for system health vs. who-did-what, each with its own retention and UI) is the right reference shape — just built on Linux-native primitives at appliance scale.

Three use cases drive the design:

1. **"This app is misbehaving — what does it say?"** A specific app's logs, live-tailable, filterable. Mostly used by the user; occasionally by us for support.
2. **"Did someone unauthorized access my box?"** Audit feed: logins, admin actions, installs, password changes, sharing grants. Surfaced as an "Activity" view per user, with the full picture visible to admins.
3. **"Something is broken at the system level — what happened?"** Host-agent / brain / Caddy / Tier-2 services in a unified view, admin-only. Used by us and by tinkerers; opaque to non-technical users.

## The split

Two stores, each chosen for its fit:

| Store | Owns | Retention | Backed up? | Read via |
|---|---|---|---|---|
| **journald** | Operational logs: host-agent, brain stdout, Caddy, all containers, kernel, Tier-2 services (smbd, avahi, tailscaled, sshd) | 500MB–1GB rotating ring | No | Dashboard "Logs" tab; `journalctl` from SSH |
| **brain SQLite `audit_events` table** | Things people did: logins, admin actions, installs, password changes, sharing grants, role changes | **Forever** | Yes (rides brain.db backups) | Dashboard "Activity" view |

This is the Synology/Windows pattern with Linux primitives. No new processes, no aggregator daemon. Total memory footprint of "new logging infrastructure" is essentially zero — journald is part of systemd (~5–15 MB resident), brain SQLite is already running.

## Why this split

### 1. Retention semantics differ

- Ops logs: ring buffer, rotates when full. A two-day-old "container restarted" message is worthless.
- Audit log: kept forever (or years). A login from a year ago might matter for "did something get compromised?"

journald has one cap for everything in its store. There's no "keep these messages, rotate those" knob. Sharing the store means either the audit log rotates out (losing its purpose) or the journal stays huge to preserve audit events (wasting disk on ephemeral ops noise).

### 2. Query patterns differ

Audit queries are relational:

- "Everything andrei did in the last 30 days."
- "All failed logins, grouped by user."
- "Every admin action on app `immich`, paginated, newest first."

These are one-line SQLite queries. In journald, they're text/field grep + client-side reassembly. The dashboard "Activity" view is a paginated, sortable, filterable table — the data needs to be in a queryable store regardless. Storing it anywhere else would just be a cache we have to invalidate.

### 3. Backup semantics differ

- Audit log: user-meaningful state. Belongs in backups.
- Ops logs: ephemeral diagnostic noise, large, may contain transient info. Don't belong in backups.

Brain's SQLite is already in the backup set (accounts, settings, app records). Adding the audit table puts it in backups automatically.

### 4. Audit is brain's domain, not the OS's

The events brain records (`andrei logged in`, `admin reset cindy's password`, `app 'immich' installed`) aren't system events brain overhears — they're **things brain does that the user might want to review.** The natural write path is the same code that performs the action, which is already touching SQLite. One more table is zero-friction.

By contrast, "Docker started container X" or "Caddy reloaded config" are system events with no semantic actor; they belong in journald.

### 5. Tamper-evidence is easier in SQLite

A forever-retained audit log eventually wants append-only semantics:

- **Append-only constraint** enforced by SQLite `BEFORE UPDATE` / `BEFORE DELETE` triggers that `RAISE(ABORT)` — defends against buggy migrations and future contributors, not just careful coding.
- **Sequence numbers + optional hash chain** (each entry hashes the previous) for detecting deletion.

Neither is natural in journald — it's designed to overwrite itself. Hash-chain semantics are deferred to v2; the trigger-enforced append-only invariant lands in v1.

## Operational logs (journald)

### Docker daemon uses the `journald` log driver

The single biggest configuration decision: switch Docker's daemon-wide log driver from `json-file` (default) to `journald`. Every container's stdout/stderr becomes a journal entry tagged with `CONTAINER_NAME=`, `CONTAINER_TAG=`, `CONTAINER_ID=`. Side benefit: `docker logs <container>` still works — Docker reads back from journald transparently.

This gives us **one source of truth** for ops: host-agent, brain, Caddy, every app container, every Tier-2 service, the kernel — all in one journal with consistent fields.

### Journal lives on the OS drive

Persistent journal at `/var/log/journal/` on the OS drive root. The same logic that puts the data-drive enrollment marker on the OS drive applies: diagnostics for "the data drive failed" must survive the data drive failing. Diagnostic state belongs on the drive least likely to be the thing that failed.

### Tuning

Two drop-ins.

journald itself, at `/etc/systemd/journald.conf.d/malmo.conf`:

```
[Journal]
Storage=persistent
SystemMaxUse=1G
RuntimeMaxUse=128M
RateLimitIntervalSec=30s
RateLimitBurst=10000
```

- **1 GB persistent cap.** Comfortably large for the household workload (weeks of normal operation; rotates under sustained spam from a misbehaving app). Could be halved without users noticing; pinned at 1 GB for headroom.
- **128 MB volatile cap** for the pre-`/var/log/journal/`-ready early-boot window.
- **Rate limit: 10000 messages / 30s per systemd unit.** Generous burst for system services. Caps `sshd` brute-force spam, runaway daemons, anything attributed to a real systemd unit.

Docker is the deliberate exception, at `/etc/systemd/system/docker.service.d/10-malmo-logging.conf`:

```
[Service]
LogRateLimitIntervalSec=0
LogRateLimitBurst=0
```

journald's per-unit rate limit is enforced against `_SYSTEMD_UNIT_`, and every container's stdout flows through dockerd — so all containers share one bucket attributed to `docker.service`. Under the global rate limit, a single chatty container can cause journald to silently drop messages from every *other* container plus dockerd itself. Disabling rate-limiting on `docker.service` removes that cross-container starvation; `SystemMaxUse=1G` (the ring buffer) becomes the sole backpressure for container output.

Tradeoff worth being honest about: a misbehaving container can age useful recent ops logs out of the journal faster than rate-limiting would. We accept this — silent message drops are a worse debugging experience, and the audit-relevant cross-container events (SSH/SMB auth) go to brain SQLite (`audit_events`), not the journal.

### Brain emits structured logs

Brain's own logs are JSON-structured (via Go's `log/slog` with the JSON handler). journald captures the fields natively; the dashboard renders them as filterable rows. Required fields: `level`, `msg`, `component`. Suggested fields: `app_id`, `user_id`, `request_id`, `error`. No mandate on app containers — they emit whatever they emit, and we render it line-by-line.

### Apps are expected to log to stdout

malmo never rewrites an app's compose file (`APP_MANIFEST.md`), so we can't force the logging driver per-container. Three layers instead:

- **Store apps (Door 1) — manifest review.** Curation criterion: app must log to stdout/stderr; no per-service `logging.driver:` override in compose; no `command:` redirects to files. Folds into the third-party manifest curation criteria entry in `NEXT.md`.
- **Custom compose (Door 2) — detect and warn at install.** Brain parses (never rewrites) the compose. If it sees a `logging.driver` set to anything other than `journald`, or obvious file-redirect patterns in `command:`, the install screen surfaces a yellow banner: *"This app is configured to log to a file. Its logs won't appear in the malmo dashboard — access them via SMB or SSH."* User installs anyway.
- **Runtime, all apps.** If a running container produces zero journal entries over a sliding window, its app card shows: *"No logs received. This app may be logging to a file."* Cheap signal; helps "why is the Logs tab empty?" answer itself.

### Caddy access logs are off by default

Caddy on a busy household box can emit thousands of access-log entries per hour, which would dominate the 1 GB journal cap. malmo ships Caddy with **access logging disabled** — Caddy's error log (warnings, config reloads, cert events) still goes to journald, but per-request access lines do not. An admin can toggle access logging on from Settings → System for short troubleshooting windows. v1 has no UI for permanent-on (a deliberate omission — surface when a real use case appears).

## Audit log (brain SQLite)

### Where

A single table `audit_events` in the brain's existing database (`state/malmo.db`). No new database file, no new connection pool.

### Schema sketch

The exact column set is open (`NEXT.md`), but the shape is:

```
audit_events
  id              INTEGER PRIMARY KEY AUTOINCREMENT  -- sequence, never reused
  ts              INTEGER  NOT NULL                  -- unix epoch ms
  actor_user_id   INTEGER  NULL                      -- who; NULL for system events
  actor_role      TEXT     NOT NULL                  -- 'member' | 'admin' | 'system'
  action          TEXT     NOT NULL                  -- 'login.success', 'app.install', …
  target_kind     TEXT     NULL                      -- 'user' | 'app' | 'share' | 'health_issue' | …
  target_id       TEXT     NULL                      -- slug, user_id, etc.
  source_ip       TEXT     NULL                      -- where the request came from
  success         INTEGER  NOT NULL                  -- 0/1
  metadata        TEXT     NULL                      -- JSON; free-form per-event-type
```

The exact action vocabulary, metadata schema per action type, retention policy (forever? configurable?), and hash-chain decision are deferred to `NEXT.md`. The skeleton above is enough for v1 if we treat `action` as a controlled string and `metadata` as opaque JSON.

### v1 action vocabulary

The following `action` strings are the pinned v1 set. Defined as exported consts in `internal/audit` so call sites never hard-code strings. Additions require a new entry here.

| Action | When |
|--------|------|
| `setup.complete` | First admin bootstrapped via `/v1/setup`. |
| `setup.failure` | A `/v1/setup` call that got past input validation and then failed: the hosted 403 (`/setup` is disabled there — `ENVIRONMENT.md` # Owner sign-in & seed ingestion — as built), a store conflict or error on the admin row, a host-agent 502 with rollback, and a failure to issue the session. Validation 422s do **not** audit, and neither does a recovery-code generation error, so this is not a complete record of refused requests. |
| `sso.success` | Hosted owner signed in through the portal-to-box handshake at `/_malmo/sso`; on the first one it also created the founding admin. |
| `sso.failure` | A portal assertion was refused — bad signature, expired, replayed, wrong box or issuer, or not the box owner. Mirrors `login.failure`. |
| `login.success` | Dashboard password login succeeded. |
| `login.failure` | Dashboard password login failed (bad credentials). |
| `login.lockout` | Per-username failure counter crossed the 15-minute lock threshold (`AUTH.md` # Rate limiting). Emitted once per lock at the crossing failure; `success=false`. |
| `logout` | Session revoked via `/v1/logout`. |
| `app.install` | Catalog (Door-1) app installed. |
| `app.uninstall` | App uninstalled. |
| `app.custom.create` | User-pasted (Door-2) compose installed. |
| `user.create` | Admin created a new user via `POST /api/v1/users`. |
| `user.role.change` | Admin changed a user's role via `PATCH /api/v1/users/:id`. |
| `user.delete` | Admin deleted a user via `DELETE /api/v1/users/:id`. |
| `user.password.reset` | Admin reset another user's password via `POST /api/v1/users/:id/password`. |
| `user.password.change` | User changed their own password via `POST /api/v1/me/password` (success and failure both audited). |
| `ssh.access.set` | User turned SSH on or off for their own account via `PUT /api/v1/me/ssh` (success and failure both audited). Metadata carries `enabled` and `require_password`. |
| `ssh.key.add` | User added an SSH public key via `POST /api/v1/me/ssh/keys`. Metadata carries the fingerprint, never the key. |
| `ssh.key.delete` | User removed one of their SSH public keys via `DELETE /api/v1/me/ssh/keys/{id}`. |
| `health.issue.raised` | A typed health issue transitioned from cleared to active. One record per issue, system actor, `target_kind: health_issue`. |
| `health.issue.cleared` | A typed health issue transitioned from active to cleared. One record per issue, system actor, `target_kind: health_issue`. |

SSH/SMB/sudo ingestion (`ssh.login.success`, `ssh.login.failure`, `smb.login.*`, `sudo.invoke`, `su.invoke`) is deferred — see "What this doc deliberately doesn't pin" and `NEXT.md`.

### Write path

A single function in brain: `audit.Record(ctx, action, target, metadata, success)`. Reads actor (Identity) and client IP from request context; falls back to `actor_role = 'system'` when no identity is present. On INSERT failure, logs at Error level and returns — never propagates to the caller. Called from:

- `api` package — login success/failure, logout, setup.complete.
- `api` package (app handlers) — install, custom-app create, uninstall.
- `cmd/brain` (storage-health poll) — `health.issue.raised` / `health.issue.cleared`, one record per transitioned issue (system actor).
- Future: `users` package — user create / delete / rename / role change.
- Future: `shares` package — grant / revoke, SMB opt-in / opt-out.
- Future: `tier2` package — Tailscale up/down, SMB enable/disable, etc.

Each call is one SQLite `INSERT`. Append-only is enforced at the database, not just by brain code:

```sql
CREATE TRIGGER audit_events_no_update
  BEFORE UPDATE ON audit_events
  BEGIN SELECT RAISE(ABORT, 'audit_events is append-only'); END;

CREATE TRIGGER audit_events_no_delete
  BEFORE DELETE ON audit_events
  BEGIN SELECT RAISE(ABORT, 'audit_events is append-only'); END;
```

A buggy migration or a future contributor's `DELETE` can't accidentally rewrite history. If a retention policy is ever added, the prune routine drops + recreates the triggers inside a transaction that itself emits an `audit.retention.prune` event recording the deletion range. Document that pattern here so it doesn't get reinvented as "let me just disable the trigger temporarily."

### External auth ingestion (SSH, SMB, sudo)

Brain doesn't see SSH or SMB logins directly — they go through `sshd` / `smbd` → PAM → journald, outside brain's code path. But "did someone unauthorized access my box?" is exactly the question audit log answers, so these events have to land in `audit_events`.

Brain opens a long-lived `journal_follow` against host-agent filtered to `_COMM=sshd OR _COMM=smbd OR _COMM=sudo OR _COMM=su`. host-agent streams matching entries; brain parses them through a small `pamparse` package and writes `audit_events` rows via the same `audit.Record()` path. The journald cursor checkpoints in the brain's SQLite (the key-value table is `box_meta`, not a `brain_meta` — `internal/store`) so brain restarts resume cleanly.

Event vocabulary added by this ingestion path:

- `ssh.login.success`, `ssh.login.failure` — actor user resolved from the sshd message, source IP captured.
- `smb.login.success`, `smb.login.failure`.
- `sudo.invoke`, `su.invoke` — privilege escalation outside the dashboard's 5-minute UI elevation window (`USERS_AND_GROUPS.md`).

Format-drift risk is real: if Debian's sshd changes its message wording in a point release, new events stop being parsed until the parser is updated. Mitigation: brain exposes a `parsed_vs_dropped` counter for messages matching the filter; a non-zero "dropped" rate is treated as a release-blocker for the next brain update. Parser patterns are tested against captured real-world messages, not invented.

### Visibility rules

- **Admins** see the full audit feed across all users.
- **Members** see only events where they are the actor or the target. ("My logins, my installs, plus 'admin reset my password' if it happened to me.")
- **System events** (no actor) are admin-only.

Enforced at the brain API layer per `BRAIN_UI_PROTOCOL.md`.

### Audit events that also notify

A small allowlisted subset of audit actions additionally fan out to the dashboard notification center (`NOTIFICATIONS.md`) — login from a new device, repeated failed logins, recovery-code use, role/password change affecting a member, SSH/SMB auth failures, `sudo`/`su` outside the UI elevation window, user create/delete. The `audit_events` row stays the append-only system of record; the notification is a separate, prunable, read-stateful copy in the `notifications` table. Most audit actions do **not** notify — the Activity view is their home.

## Dashboard UI surfaces

Three distinct views, each scoped tight for v1:

### Per-app logs

- Lives on each app's card: "Logs" tab.
- Live tail via SSE (Pattern C, `BRAIN_UI_PROTOCOL.md`); scrollback up to journald's cap.
- Source: brain → host-agent → `journalctl CONTAINER_NAME=<container> --follow`.
- Plaintext lines, monospace, no parsing. App logs are whatever the app emits.
- **Visibility:**
  - Per-user Tier-3 instances: the owning member + admins.
  - Tier-1 shared apps: **admins only.** App stdout commonly leaks per-user behavioral signal (request paths, search queries, accessed file names); even when the app is household-shared, the log stream is not. Per-user filtering at the line level isn't viable — most log formats aren't user-tagged in any consistent way.
  - A manifest opt-in (`logs.member_visible: true`) for apps whose stdout is genuinely uninteresting is tracked in `NEXT.md`.

### System logs (admin-only)

- Settings → System → Logs.
- Filter by service: host-agent, brain, Caddy, Tier-2 services individually.
- Same SSE + scrollback mechanism. Structured fields from brain rendered as filterable; app/Tier-2 lines rendered plain.

### Activity (audit log)

- Settings → Activity (or surfaced more prominently — UX call belongs to dashboard design).
- Table view: timestamp, actor, action, target, source IP, success/failure.
- Filter by user, action type, date range.
- Per-member: their own events. Per-admin: everyone's.
- Export-to-file: CSV or JSON. Useful for "I want to keep my own copy" and for "send us your audit log" support flows.
- Search is **structured** (filter by columns), not free-text. Free-text search across audit events is deferred.

### Diagnostic bundle

- Settings → System → "Download diagnostics" button (admin-only).
- Produces a tarball: last 24h of journal export (`journalctl --since '24h ago' -o export`), brain state snapshot (excluding secrets), audit-events table dump, host-agent's recent state, and a **multicast/discovery probe** (see below).
- The "send us your logs" affordance. Has obvious privacy implications — bundle is generated on demand, never auto-uploaded, never auto-shared. User decides what to do with it.
- **Cross-user privacy note.** The bundle contains every member's app logs, source IPs, and audit events — consistent with admins-see-everything, but the bundle is exportable off-box. The download dialog explicitly surfaces this: "This file includes activity from every member of the household. Only share it with someone you trust to receive it."
- **Multicast / discovery probe.** A small section captured at bundle-generation time: the current Avahi published-name set, the interface allow-list, Avahi daemon state, and a synchronous multicast self-test (broadcast a query for the box's own host record, count responses on each LAN interface). Zero responses with a healthy NIC strongly implies router-level AP/client isolation — the single most common cause of "`.local` doesn't work" support tickets. Captured as JSON in `discovery.json` inside the bundle. See `DISCOVERY.md`.

## Mechanisms — how the dashboard actually reads logs

The dashboard runs in the browser, talks to brain. Brain runs in a container; the host's journal is outside that container's namespace. So:

- Brain → host-agent → journald. host-agent exposes a small set of operations in `BRAIN_HOST_PROTOCOL.md`:
  - `journal_query` (paginated, filtered, returns structured entries).
  - `journal_follow` (SSE-style stream of new entries matching a filter).
  - `journal_export_range` (range dump for the diagnostic bundle).
- Audit log queries skip host-agent entirely — brain reads its own SQLite directly.

This means a new section in `BRAIN_HOST_PROTOCOL.md` for journal operations. The operations are read-only by design — neither brain nor anyone else can *write* to the system journal via host-agent. (If brain wants to log something, it logs to its own stdout, which Docker captures into journald via the daemon log driver.)

## Why not other options

- **`json-file` driver + per-container files** (the Umbrel default): no unified timeline, two stores (host services in journald, containers in files), per-container rotation is fiddly, the dashboard has to know about two query paths forever. Cheaper to set up by ~one line of daemon config; more expensive every other day.
- **Loki / Vector / Fluentd** (centralized aggregator): 50–300 MB resident *idle*. Disqualified for a home appliance where the entire workload doesn't justify it. Rich query and log-shipping features we'd never use.
- **rsyslog / classic syslog**: legacy, weaker structured-data story than journald, no real advantage on a modern Debian box.
- **Audit-in-journald with `MALMO_AUDIT=1` tag**: doable, but defeats every reason for the split — retention, query, backups, tamper-evidence all fight the medium. Considered and rejected.

## Privacy posture

Worth being explicit:

- **Operational logs may contain user-visible filenames, app behavior, request paths.** They're treated as user-private data. Stay on the box, never auto-uploaded, never auto-shared. Visibility follows the rule pinned under "Per-app logs" above — Tier-3 instance owners see their own; Tier-1 shared apps are admin-only; system logs are admin-only.
- **Audit logs contain user actions and source IPs.** Same treatment. Member visibility scoped to their own events.
- **Diagnostic bundles are explicit user action.** Generated on demand, downloaded to the requesting admin's device, user decides what to do with it. No automatic upstream of bundles, ever — even when project telemetry (`TELEMETRY.md`) is enabled, only structured `brain_panic` / `host_agent_panic` events with scrubbed paths leave the box; full bundles never do.

## What this doc deliberately doesn't pin

- **Audit metadata schema per action type.** "Free-form JSON" works for v1; a typed schema per action type is a v2 hardening. Tracked in `NEXT.md`.
- **Hash-chain / tamper-evidence on audit events.** Append-only via brain invariant in v1; cryptographic chain deferred. Tracked in `NEXT.md`.
- **Centralized log shipping** (off-box). Out of scope — no off-box surface at all in v1.
- **Per-user log retention overrides.** Audit log is global, scoped at read-time, not at write-time. No per-user retention.
- **Search across operational logs.** v1 has filtering + scrollback; full-text search across journal is deferred.

## Open questions

Tracked centrally in [`NEXT.md`](NEXT.md).
