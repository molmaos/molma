# Brain ↔ host-agent protocol

> The wire-level contract between the malmo brain (in a container) and `host-agent` (running on the host with root). Companion to `CONTROL_PLANE.md`, `AUTH.md`, `SERVICE_PROVISIONING.md`.
>
> Covers transport, wire format, patterns, auth, versioning, and failure semantics. The reconciler pattern (desired-vs-actual state) that goes with this protocol is in `APP_LIFECYCLE.md` # "same reconciler pattern extends to all host-managed state."

## Scope

`host-agent` exists because the brain runs in a container and can't safely touch the host. host-agent does the host-side work the brain asks for:

- **mDNS / Avahi** publishing (per-app hostnames on the LAN).
- **Tier-2 native ops** — `systemctl` toggles, write `/etc/samba/smb.conf`, run `tailscale up`, edit `authorized_keys`, run `passwd`.
- **Disk / LUKS / TPM** — mount, format, smartctl probe, recovery-passphrase operations.
- **System updates** — `apt`.
- **Network configuration** — NetworkManager-backed: list/scan/connect/forget WiFi networks, DHCP vs. static IP per connection, primary-connection pinning, active-interface state. host-agent talks to NetworkManager over DBus; the brain talks to host-agent over this protocol.
- **Power** — shutdown, reboot.
- **Misc host state** — time zone, hostname, system summary.

Things that **don't** cross this protocol:

- **Docker daemon.** Brain talks to Docker directly via `docker-socket-proxy` (a separate sidecar that restricts the API surface). host-agent is not in the Docker path.
- **Caddy.** Runs as a container; brain manages it like any other container.
- **App-facing services** (Postgres, Redis, future background-job runner). Those are Tier-1 services apps consume; orthogonal to host-agent.

If a host capability isn't in the list above, it doesn't live behind host-agent. The boundary is *"touches the host's root filesystem, systemd, or apt."*

## Transport: UNIX socket

host-agent listens on a UNIX socket:

| Path        | `/var/run/malmo/agent.sock`          |
|-------------|--------------------------------------|
| Owner       | `root`                               |
| Group       | `malmo`                              |
| Mode        | `0660`                               |

The brain's container UID is a member of the `malmo` group. The socket is mounted into the brain's container; nothing else on the host can connect. The `malmo` group is **unrelated to `malmo-shared`** (the household-content group) — see `USERS_AND_GROUPS.md` # Group reference.

**Why UNIX socket over loopback TCP:**
- File-permission access control is kernel-enforced and stronger than any app-level token.
- No port allocation, no "what if something else binds to it first," no firewall config.
- Same Go HTTP-server stack works on it — `net.Listen("unix", ...)`.

## Wire format: HTTP/1.1 + JSON

Plain HTTP over the socket, with JSON request and response bodies. Versioned URL prefix: `/v1/...`.

**Why HTTP/JSON, not gRPC or custom binary:**

- **Debuggability is a first-class goal.** From any shell on the host:
  ```
  curl --unix-socket /var/run/malmo/agent.sock http://localhost/v1/system/status
  curl --unix-socket /var/run/malmo/agent.sock -X POST http://localhost/v1/system/reboot
  ```
  This is invaluable during spec/early-implementation iteration, for incident response, and for any future tinkerer-facing tooling.
- Brain already runs an HTTP server (for the dashboard); reusing the HTTP client stack on the brain side is trivial.
- No code-generation step; iterate on schemas at the speed of editing a struct.
- Stream-friendly via SSE (below) and upgradeable to WebSocket where bidirectional is needed (future — see "Web terminal").
- Performance at home-server scale is a non-issue. host-agent is not a hot path.

The tradeoff we accept: no automatic schema enforcement at the wire. We hand-write request/response Go structs and JSON-tagged fields. Acceptable for two binaries shipped together.

## API patterns

The protocol uses **two patterns**, with a clear rule for when each applies.

### Pattern A — Sync request/response

For anything that typically completes in **under ~5 seconds** and doesn't need progress reporting:

```
POST /v1/services/tailscale/enable
→ 200 OK
  { "enabled": true, "running": true }
```

```
GET /v1/system/status
→ 200 OK
  { "hostname": "cindy-zx9", "uptime_s": 84021, "disk_pressure": false,
    "data_disk_free_bytes": 442381180928, "data_disk_total_bytes": 1099511627776,
    "disks": [ { "label": "System", "free_bytes": 19327352832, "total_bytes": 68719476736 },
               { "label": "Data",   "free_bytes": 442381180928, "total_bytes": 1099511627776 } ], ... }
  # disks is the per-volume fullness view for the resources panel's Storage bars
  # (LOCAL_ANALYTICS.md): the OS drive (/) always, the data drive (/srv/malmo)
  # only when it's a distinct mount (a Level-0 box has no data drive, so it's
  # omitted, not zero-filled). data_disk_* are kept for the install-plan
  # footprint (DECISIONS.md 2026-06-13). The brain re-serves disks to the UI at
  # GET /api/v1/system/storage (BRAIN_UI_PROTOCOL.md).

GET /v1/system/resources
→ 200 OK
  {
    "ts_ns": 84021000000000,
    "cpu": { "total_jiffies": 12044910, "idle_jiffies": 9881233 },
    "loadavg": [0.42, 0.51, 0.48],
    "mem": { "total_bytes": 16728338432, "available_bytes": 9214455808, "used_bytes": 7513882624 },
    "net":  [ { "iface": "enp3s0", "rx_bytes": 99201234, "tx_bytes": 41200934 } ],
    "disk": [ { "dev": "sda", "read_bytes": 81002496, "write_bytes": 12300288 } ],
    "uptime_s": 84021
  }
```

**Live system-resources sample (`GET /v1/system/resources`).** Pattern A; the host source for the all-users live-resources view (`LOCAL_ANALYTICS.md` # Real-time system resources). Returns the **raw cumulative counters** from `/proc/stat`, `/proc/meminfo`, `/proc/loadavg`, `/proc/net/dev`, `/proc/diskstats` plus a monotonic `ts_ns`. host-agent is stateless — it reads on request and computes no rates; the brain polls once per second *while a UI is watching*, diffs successive samples (rate denominator = `ts_ns` delta), and fans the derived rates out over its own SSE channel. host-agent applies the interface/device allowlist — physical LAN NICs + mesh, excluding `lo`/`docker0`/`veth*`/`br-*`, whole-disk devices only — so the brain never sees container-bridge noise. Distinct from `GET /v1/health/system`, which is a coarse 60s health poll, not a 1 Hz live feed.

**Update-target read (`GET /v1/system/update-target`, as built #443).** Pattern A, and a **read** — the trigger stays `POST /v1/jobs/system-update`. It answers one question the box asks itself every 15 minutes and used to answer only in the journal (`UPDATES.md` # 8.4): what am I running, what could I be running, and why is the difference not closed yet. Without it the appliance prompt `UPDATES.md` # 3 promises has nothing to read.

```
GET /v1/system/update-target
→ 200 OK
  { "state": "available",
    "running": { "brain": "ghcr.io/malmoos/brain@sha256:…", "ui": "ghcr.io/malmoos/ui@sha256:…" },
    "target":  { "version": "v0.8.0", "brain_image": "…@sha256:…", "ui_image": "…@sha256:…",
                 "published_at": "2026-09-01T10:00:00Z" },
    "checked_at": "2026-09-07T02:45:00Z",
    "from": "seed", "window": "03:00-04:00", "window_from": "answer",
    "auto_apply": true, "profile": "hosted" }
```

`state` is the whole answer in one word, and the seven values stay apart on purpose:

- **`current`** — the target is what the box already runs. The healthy answer.
- **`available`** — a different control plane is on offer. Hosted applies it in the window on its own; appliance waits for an admin (# 8.2).
- **`none`** — the source is up and has nothing to offer. Normal, not a failure: most boxes are here.
- **`unreachable`** — the check could not be completed (the source could not be read, or the box could not read its own running pair). The box keeps running what it runs.
- **`refused`** — the source answered and the box rejected the answer: a tag, an unexpected repository, half an answer. Nothing was pulled. A source stuck on a bad answer is a fleet problem; a source that is down is not, so these two never collapse into one.
- **`disabled`** — this box has no update loop at all, because its configured target is unusable and host-agent refused it rather than falling back (`UPDATES.md` # 8.4). The only state here that needs a human, and the reason it is not folded into `unknown`.
- **`unknown`** — nothing measured yet: the loop has not finished its first tick, or no reporter is wired (the fake binary's default). Never "you are up to date".

Three things about the payload:

- **`running` is read when the request arrives**, from the box's own declaration (`images.json` for the brain, the staged `compose.yml` for the UI). It is not carried from the last tick, because a tick that ended early never read it and host-agent outlives an update.
- **`from` and `window_from` are two different settings** whose values only look alike. `from` is where the box's update-target URL came from (`seed`, `env`, `default`) and is absent on an appliance, which has no such URL. `window_from` is where the update window came from (`answer`, `env`, `default`). A window can come from the source's answer; a URL never can. The URL itself is deliberately not on the wire — `from` carries the fact worth showing, and a hand-edited URL is the one place a credential could turn up in a payload the dashboard renders.
- **`detail`** (omitted above) is the underlying error text for `unreachable`, `refused` and `disabled`. It is a diagnostic, not UI copy: the dashboard writes its sentence from `state`.

**200 always**, like `/v1/health/system`. Every way this can go wrong is a state in the payload, so an HTTP error would tell the brain "ask again later" about facts that are not going to change on their own. The brain re-serves it at `GET /api/v1/system/update-target`, admin-only.

**Health findings report (`GET /v1/health/system`).** The brain can't read host hardware directly (it's containerized behind the socket-proxy), so all *physical* health detection — SMART, `statfs`, mount flags, `systemctl is-active`, memory pressure, the pending-reboot flag (`/var/run/reboot-required`) — is host-agent's job. host-agent samples on its own cadence and the brain polls this one report on the 60s heartbeat, reconciling findings into typed health issues (`HEALTH.md` # Detector catalog, locus B). It returns findings across domains (storage, drives, services, resources, time, system) in one payload — **not** a proliferation of per-domain endpoints — so the brain's `ApplyFindings(category, …)` reconcile can clear-absent / raise-present per category atomically. This supersedes the slice-1 single-purpose storage report (`/run/malmo/health/storage.json` boot reporter stays; the polled endpoint generalizes). See `DECISIONS.md` 2026-05-29.

```
POST /v1/auth/verify-password
  { "user": "cindy", "password": "..." }
→ 200 OK
  { "valid": true }
```

Plain HTTP. The brain blocks on the response. Errors come back as HTTP status + JSON body with `code` and `message`.

**`/v1/auth/verify-password` is hit on every dashboard login** (brain delegates PAM verification rather than storing a password hash itself — see `AUTH.md` # Identity primitive). Implementation: host-agent runs PAM `authenticate()` with the supplied credentials, returns `valid: true|false`. The endpoint never reveals *why* a verification failed (wrong password vs. unknown user vs. locked account) — only the binary result, mirroring PAM's own posture. Rate-limiting lives in the brain; this endpoint just answers truthfully.

**Credential mutation endpoints** — siblings to `verify-password`, used by the dashboard's user-management flows (`/v1/setup`, password reset, remove member). Pattern A, same `user` field naming:

```
POST /v1/auth/set-password
  { "user": "cindy", "password": "..." }
  → 200 OK  {}
```

```
POST /v1/auth/set-role
  { "user": "cindy", "role": "admin" }   # role ∈ {"admin","member"}
  → 200 OK  {}
  → 400 Bad Request  { "code": "bad-request", "message": "role must be admin or member" }
```

```
POST /v1/auth/delete-user
  { "user": "cindy" }
  → 200 OK  {}
```

`set-password` is **upsert**: it creates the user if missing (real impl: `useradd` + `passwd` + Samba sync as one atomic op via `AUTH.md` # Password change) and otherwise just updates the password. The brain stays oblivious to "is this a create or an update" — single endpoint, single round-trip during `/v1/setup`. `set-role` updates Linux group membership in the `sudo` group to match the new role — admin → in `sudo`, member → not in `sudo` (real impl: `gpasswd -a`/`-d`; idempotent on both ends); see `USERS_AND_GROUPS.md` # Roles for the owning policy. The fake host-agent stores the role in memory only. `delete-user` is idempotent: unknown user returns 200 (real impl: `userdel -r -f` — `-r` removes the home dir, `-f` forces removal even with a live session; see `docs/progress/host-agent-delete-user.md` for the session-termination matrix). None of these endpoints return credential material; the brain holds no password material.

**User info endpoints.** The brain runs containerized and has no access to `/etc/passwd`. When installing a personal-scope app, the brain needs the owner's home directory path and POSIX UID/GID to emit a correct bind-mount source and `user:` directive in the compose override. Pattern A:

```
GET /v1/users/{username}/home
→ 200 OK  { "home_path": "/home/alex", "uid": 3001, "gid": 3001 }
→ 404     { "code": "unknown-user", "message": "user not found" }
```

The brain maps `unknown-user` to an installation error (not a 500 retry) — the user was deleted between the install-plan call and the install commit. The fake host-agent returns a deterministic result derived from the username (UID in [3000, 3999]) so the dev loop is coherent without a real `/etc/passwd`. See `APP_ISOLATION.md` # User content for the personal-scope bind-mount contract this feeds.

For a **household-scope** app the owner's UID doesn't apply — the instance runs as a shared service identity (`malmo-app`) and any folder electing a shared source is added to the `malmo-shared` group. The brain learns those fixed identities through a companion Pattern A endpoint:

```
GET /v1/identity/well-known
→ 200 OK  { "malmo_app_uid": 2000, "malmo_app_gid": 2000, "malmo_shared_gid": 2001 }
```

`malmo_app_uid`/`malmo_app_gid` is the shared service identity stamped as the compose `user:` for household instances; `malmo_shared_gid` is the GID added via `group_add` whenever any folder elects the shared source (`/srv/malmo/shared/<Folder>/`), in either scope. The real host-agent resolves these from `/etc/passwd` and `/etc/group` (`os/user.Lookup("malmo-app")`, `os/user.LookupGroup("malmo-shared")`); these accounts are provisioned by the box build, not by host-agent. The fake host-agent returns fixed dev constants (`2000`/`2000`/`2001`) that sit below the per-user `[3000, 3999]` range so service identities never collide with hashed user UIDs. See `APP_ISOLATION.md` # User content and `USERS_AND_GROUPS.md` # Group reference.

**App-service identity allocation.** A folderless app declaring `service_user: true` runs as a dedicated, malmo-allocated non-root identity (`APP_ISOLATION.md` # Runtime identity & data ownership). host-agent owns the reserved **app-service band [2100, 2999]** — below the malmo user floor (`UID_MIN` 3000), above the fixed 2000/2001 well-knowns, with 2002–2099 left as headroom for future fixed identities — and exposes allocation as a sibling of the well-known endpoint:

```
POST /v1/identity/app-service
  { "instance_id": "navidrome-20260610t101500" }
  → 200 OK  { "uid": 2100, "gid": 2100 }

POST /v1/identity/app-service/release
  { "uid": 2100 }
  → 200 OK
```

The brain calls allocate once during install, persists the pair on the instance row, and never re-requests it — the identity is stable for the life of the instance. Release runs at uninstall (and on install rollback); it is idempotent, and a UID outside the band is a 400 — the endpoint must never be usable to delete an arbitrary account. The real host-agent reserves the number by creating a real system account + group named `malmo-svc-<uid>` (`useradd --system`, nologin shell, no home; the instance ID goes in the GECOS comment), so the `/etc/passwd` entry is the durable reservation and the band's state survives restarts with no side state; allocation is idempotent per instance via the GECOS label. The fake host-agent allocates from an in-memory map (not persisted — the brain stores the pair, and the unprivileged dev brain can't chown to it anyway).

**GPU capability query (`GET /v1/system/gpu`).** When a manifest declares `permissions.gpu: true`, the brain needs two host facts before it can install: *is there a usable GPU* (the capacity gate — no GPU is a hard install refusal, `APP_ISOLATION.md` # GPU) and *which render group grants access to it* (so it can `group_add` the container onto `/dev/dri`). One Pattern A probe answers both:

```
GET /v1/system/gpu
→ 200 OK  { "present": true,  "vendor": "intel", "render_gid": 104 }   # iGPU detected
→ 200 OK  { "present": false, "vendor": "",      "render_gid": 0 }     # no usable GPU
```

- `present` — a usable GPU was detected. `false` is what the brain turns into the specced capacity refusal (`APP_ISOLATION.md` # GPU), **before** it generates the compose override — not a late `docker compose up` failure.
- `vendor` — `"intel"` in v1, the only supported runtime (`APP_ISOLATION.md` # GPU scopes the first slice to the Intel iGPU / VA-API path). `"amd"` and `"nvidia"` are reserved for the follow-on runtimes and not emitted yet. Empty when `present` is false.
- `render_gid` — the GID of the host `render` group, which owns the `/dev/dri/renderD*` nodes. The brain `group_add`s it onto the main service so a `cap_drop: [ALL]` container (which lacks `CAP_DAC_OVERRIDE`) can still open the render node. Only meaningful when `present`.

The real host-agent detects the GPU by scanning `/dev/dri/renderD*` and reading each node's PCI vendor (`/sys/class/drm/renderD*/device/vendor`; `0x8086` → `intel`), and resolves the render GID via `os/user.LookupGroup("render")` — the same way `/v1/identity/well-known` resolves `malmo-shared`. The `render` group and the udev rules binding the DRI nodes to it are provisioned by the box build's media stack, not by host-agent (the OS-image half is tracked separately — see issue #125). The fake host-agent returns a synthetic Intel iGPU (`present: true, vendor: "intel"`, a fixed dev `render_gid`) so the override path is exercisable under `make dev`, with a toggle to report `present: false` so the capacity-refusal path is testable without real hardware. This is the brain's only GPU host query — vendor→runtime selection is entirely the brain's job; the manifest stays vendor-agnostic `gpu: true` (`APP_MANIFEST.md` # E).

**System time zone (`POST /v1/system/set-timezone`).** The only write in the "misc host state" group. It is the host op behind the first-run wizard's time-zone step (`FIRST_RUN.md`) and behind Settings → System → Time (`SETTINGS.md`). Pattern A, no response body:

```
POST /v1/system/set-timezone   { "zone": "Europe/Stockholm" }
  → 200 OK
```

- `zone` is an IANA tz database name, like `"Europe/Stockholm"` or `"UTC"`. **The brain checks it before calling.** host-agent does not check it.
- The real host-agent runs `timedatectl set-timezone <zone>`. That is what re-points `/etc/localtime` (`TIME.md` # System TZ). Both build profiles have it, so a hosted box sets its time zone the same way an appliance does.
- Clock **sync** is a different thing, and it is not a write. The `clock-not-synced` detector reads `chronyc tracking` and reports it through `GET /v1/health/system` (# Health). Nothing in this protocol sets the time.

**SSH access (`POST /v1/ssh/set-access`, `GET /v1/ssh/state`).** The host ops behind Settings → My account → Device access (`AUTH.md` # Device access). Pattern A.

```
POST /v1/ssh/set-access  { "user": "alex", "enabled": true,
                           "authorized_keys": ["ssh-ed25519 AAAA... alex@laptop"],
                           "require_password": false }
  → 200 OK

GET  /v1/ssh/state
  → 200 OK { "daemon_running": true,
             "users": [ { "username": "alex", "key_count": 1, "require_password": false } ] }
```

- **The write carries one account's full desired state, not a delta.** `authorized_keys` replaces that account's file; an empty list with `enabled: false` is how an account is removed. Full-state means a retry after a partial failure converges instead of compounding, which matters because the brain commits first and calls host second.
- **host-agent renders the whole drop-in, never line-edits it.** `sshd_config.d/malmo-allowed.conf` is regenerated from the enabled set on every call: a global `AllowUsers`, then one `Match User` block per account carrying that account's `AuthenticationMethods`. Validation happens twice, because the two checks catch different things: the candidate is tested on its own **before** it is installed, and the combined config is tested **after**, since our fragment can only be seen alongside the box's own `sshd_config` once it is in place. A failure in the second check restores the previous file, so a rejected render is never left on disk to fail the next start.
- **The account's keys live in a root-owned file outside the home**, `/etc/ssh/malmo-authorized-keys/<user>`, and the account's `Match` block names it alongside `.ssh/authorized_keys`. host-agent runs as root and `~/.ssh` is user-controlled, so writing keys there as root is a privilege-escalation path — a symlink swapped in between check and use redirects the write, or the `chown`. Owning the file removes the user from the path, and leaves keys they added from their own shell working and untouched.
- **Calls are serialised on the host.** Every write re-renders one drop-in holding the whole enabled set, so two concurrent calls read-modify-writing it would drop whichever account lost the race — silently revoking access, or stopping sshd while someone still has it on.
- **`require_password` adds a factor, it never substitutes for one.** True renders both methods for that account, so sshd demands both. Which method is mandatory is the **brain's** decision and depends on the profile — a key on hosted, the password on the appliance — and the brain refuses a hosted enable with no key before it ever calls here. host-agent does not know the profile and does not second-guess the brain, the same division as `set-timezone`, where the brain validates the zone.
- **The daemon follows the enabled set.** host-agent starts sshd when the call leaves at least one account enabled and stops it when none is left, so :22 is closed on a box nobody uses SSH on (`BUILD.md` # SSH). On hosted this is the only control over that port.
- **`GET /v1/ssh/state` is the reconcile read**, reported alongside the rest of actual state on the 60-second heartbeat. Drift here is asymmetric like everything else (# B): the brain re-applies when it made the last change and surfaces when something else did, so an admin who hand-edited the drop-in over SSH is not fought.
- The fake host-agent keeps the same state in memory and reports a plausible `daemon_running`, so the whole flow is exercisable under `make dev`.

**Network endpoints (NetworkManager-backed).** host-agent exposes Pattern A routes that wrap NetworkManager's DBus surface:

```
GET  /v1/network/state
  → { "primary": "ethernet-1", "ethernet": [...], "wifi": [{ "ssid": "...", "connected": true, "signal": -52, "secured": true, "saved": true }, ...], "ipv4": {...}, "ipv6": {...} }

POST /v1/network/wifi/scan
  → { "networks": [{ "ssid": "...", "signal": -52, "secured": true, "freq_mhz": 5180 }, ...] }

POST /v1/network/wifi/connect
  { "ssid": "...", "password": "...", "hidden": false }
  → { "connection_id": "...", "state": "activated" }    (or error.code = "auth-failed" | "no-signal" | "timeout")

POST /v1/network/wifi/forget               { "connection_id": "..." }
POST /v1/network/connection/set-primary    { "connection_id": "..." }
POST /v1/network/connection/configure-ip
  { "connection_id": "...", "ipv4": { "mode": "dhcp" | "static", "address": "...", "gateway": "...", "dns": [...] } }
```

All network ops are Pattern A (NM DBus calls return promptly). State-change notifications from NM (signal level, connection drops, primary switch) flow as SSE on `GET /v1/network/events` so the dashboard can update live without polling. WiFi credentials are stored by NetworkManager itself (`/etc/NetworkManager/system-connections/`, mode `0600`, root-only) — the brain never sees the password after the `connect` call returns. The primary-connection pin (`connection.required-for-network-online=true`) is owned by `set-primary` — exactly one connection carries it at any time; see `BOOT.md` # NetworkManager.

**Discovery / mDNS endpoints (Avahi-backed).** host-agent owns the publisher; brain registers and unregisters per-app `.local` names. Pattern A:

```
POST /v1/discovery/publish
  { "slug": "photos" }
  → 200 OK  { "name": "photos.local", "state": "established" }
  (or error.code = "hostname-conflict" | "avahi-down" | "timeout")

POST /v1/discovery/unpublish
  { "slug": "photos" }
  → 200 OK

GET  /v1/discovery/state
  → { "publisher": "avahi", "host_name": "malmo", "renamed_to": null,
      "published": [{ "slug": "photos", "name": "photos.local", "state": "established" }, ...],
      "interfaces": ["eth0", "wlan0"] }
```

Implementation: `publish` creates an Avahi DBus entry group and calls `EntryGroup.AddAddress` once per LAN interface — each call scoped to that interface's index with that interface's own IPv4, the set computed from NetworkManager (static service files were verified not to work for bare A records; see `DISCOVERY.md` # Per-app A records). `unpublish` frees the entry group. Both ops are idempotent — duplicate publish is a no-op, unpublish on an unknown slug returns 200. Avahi's RFC 6762 §9 conflict-resolution (host rename to `malmo-2.local`) surfaces in `state.renamed_to` and the brain raises `hostname-conflict` (`HEALTH.md`). The Avahi interface allow-list (`allow-interfaces=` in `/etc/avahi/avahi-daemon.conf`) is computed by host-agent from NetworkManager state at boot and on interface change — eth/wlan in, `tailscale0` / `docker0` / `br-*` out — and host-agent re-publishes every entry group after any network change (committed groups hold literal addresses; an allowlist change additionally restarts avahi-daemon, which destroys them). See `DISCOVERY.md` for the full record model and gotchas.

**`enroll-drive` and `eject-drive` carry credentials inline** because host-agent verifies them via PAM as the first step of the job and uses them to authorize reading `/etc/malmo/secrets/luks-recovery.key`. The brain does not cache or forward the password beyond the single request. On invalid credentials the job fails immediately with `error.code = "auth-failed"`; otherwise host-agent proceeds with format → LUKS → TPM enrollment → mount → mergerfs add (enroll) or stop apps → unmount → marker removal (eject). Declared attributes: `Dangerous: true`, `ResourceClass: "disk"`, `MaxDuration: 10m`. See `STORAGE.md` # Adding a data drive and # Ejecting a data drive for the user-facing flow; `AUTH.md` # Roles for the fresh-password requirement.

**Files endpoints (`/v1/files/*`).** Back the in-dashboard file manager (`FILES.md`). The brain is containerized and cannot touch `/home` or `/srv/malmo`, so every file operation runs here, **with host-agent dropping to the requesting user's Linux UID/GID for the duration of the op** (`setresuid`/`setresgid` to the malmo 3000+ UID, or a forked child). This makes POSIX `0750`/`02770` the kernel-enforced backstop — a member's op cannot read another user's `0750` home even past a brain-side bug — and gives created files correct ownership natively, the same contract the compose `user:` directive gives app instances (`APP_ISOLATION.md` # User content). host-agent owns logical-root resolution: `root` is `home` (→ the user's home, resolved as in `/v1/users/{username}/home`) or `shared` (→ `/srv/malmo/shared/`); it re-validates path containment before acting. The brain passes `user` on every call; there is no "act as a different user" parameter.

Metadata ops are Pattern A:

```
POST /v1/files/list     { "user": "alex", "root": "home", "path": "Photos/2024" }
  → 200 OK  { "entries": [ { "name": "img.jpg", "dir": false, "size_bytes": 81002, "mtime": "...", "hidden": false }, ... ] }
POST /v1/files/mkdir    { "user": "alex", "root": "home", "path": "Photos/New" }   → 200 OK {}
POST /v1/files/move     { "user": "alex", "from": {...}, "to": {...} }             → 200 OK {}
POST /v1/files/copy     { "user": "alex", "from": {...}, "to": {...} }             → 200 OK {}
POST /v1/files/delete   { "user": "alex", "root": "home", "path": "Photos/old.jpg" } → 200 OK {}
```

Errors use the standard `{code, message}` — `permission-denied`, `not-found`, `exists`, `no-space` (`507`-class, ties to `disk-full`, `HEALTH.md`), `blocked-by-health-issue` when `data-drive-missing` is active (`HEALTH.md` # blocks_writes).

**Content transfer is a streamed binary body, not a job.** Download and upload carry file bytes as `application/octet-stream`; host-agent streams as the UID and the brain pipes bytes between the dashboard and host-agent without buffering whole files:

```
GET  /v1/files/content?user=alex&root=home&path=Movies/clip.mp4   → 200 OK, application/octet-stream (streamed)
PUT  /v1/files/content?user=alex&root=home&path=Photos/new.jpg    ← request body streamed; → 200 OK {}
```

This is a **deliberate exception to the ">5s = job" rule** — a transfer can take minutes, but it is pure I/O streaming with transport-native progress (the browser's own upload/download progress) and no server-side job state to poll, the same reasoning that exempts SSE log tails. See `FILES.md` # Transfers and `BRAIN_UI_PROTOCOL.md` # files for the dashboard-facing half.

### Pattern B — Jobs (long-running ops)

For anything that **may exceed 5 seconds** or that needs progress / cancel:

```
POST /v1/jobs/enroll-drive
  { "device": "/dev/sdb", "admin_user": "andrei", "admin_password": "..." }
→ 202 Accepted
  { "job_id": "j_77c1a3", "status": "running", "kind": "enroll-drive" }

POST /v1/jobs/eject-drive
  { "admin_user": "andrei", "admin_password": "..." }
→ 202 Accepted
  { "job_id": "j_88d2b4", "status": "running", "kind": "eject-drive" }

POST /v1/jobs/system-update
→ 202 Accepted
  { "job_id": "j_a4f7b2", "status": "running", "kind": "system-update" }

GET /v1/jobs/j_a4f7b2
→ 200 OK
  { "job_id": "j_a4f7b2", "status": "running", "progress": 0.42,
    "started_at": "...", "kind": "system-update" }

POST /v1/jobs/j_a4f7b2/cancel
→ 200 OK
  { "job_id": "j_a4f7b2", "status": "cancelling" }
```

Status values: `running`, `completed`, `failed`, `cancelled`, `cancelling`.

When `completed`, the response carries a `result` field with the operation's output. When `failed`, an `error` field with code and message.

**The rule:** *if a route can exceed ~5 seconds, it's a job.* Bias toward "make it a job" when uncertain — the job pattern is a strict superset (a caller can poll once with a long-poll if they want sync-ish semantics).

**Why not "everything is a job":** read-only / fast routes don't need the cognitive overhead of "is this done? where's the result?" and the extra JSON nesting. The dividing line is explicit per route and documented in the API contract.

**As built (#381): one kind, and a subset of the machinery.** `system-update` is the only job kind that exists today (`internal/hostagent/jobs.go`). The framework above — a kind registry with typed attributes, resource-class serialization with queue positions, cancel, and the SSE log stream of Pattern C — is **not built**, because it would be an abstraction with a single consumer (`CLAUDE.md` # Go code discipline). What is built is the part one dangerous job needs:

- `POST /v1/jobs/system-update` with `{ "brain_image"?, "ui_image"? }` → `202` `{job_id, status, kind, started_at}`. An empty ref means "leave that component alone"; both empty is a `400`. `501` when this host-agent has no updater wired (the fake binary), the same degrade as `journal_follow`.
- `GET /v1/jobs/{id}` → the record: `status`, `started_at`, `finished_at`, plus `error` `{code, message}` and a `result` `{brain_changed, ui_changed, reverted, failure_mode, revert_error}` once it ends. `404` for an unknown id.
- **`MaxDuration` = 30m**, enforced by cancelling the run's context.
- **One global lock**, which is `Dangerous: true` ("never run two destructive ops concurrently") realized for a single kind. A second update while one runs is **refused with `409`**, not queued — an admin who clicks Update twice wants one update, and there is no second kind to serialize against.

Two deliberate differences from the text above:

- **No `stalled` status.** A run that passes `MaxDuration` is cancelled, and the transaction rolls the box back on a context of its own, so it lands on a known outcome. The job ends `failed` with `error.code = "job-timeout"`, which says both what happened and that it was the bound that fired. `stalled` ("we're not sure") describes a job with no rollback, which this one is not.
- **No cancel route, and no job log stream.** Nothing calls them yet. The dashboard surface for updates is not built either (#381 ships the API only).

Job records live in host-agent memory and are lost on restart — matching "Dangerous: crash mid-flight = no auto-resume". That is also the right side of the socket for them: a control-plane update replaces the **brain** container, so a brain-side record would die halfway through the operation it was tracking.

**What would make us build the rest:** a second job kind. `enroll-drive` is the likely one, and it is the case that needs resource classes (`disk` vs `apt`), the cross-class dangerous lock, and queueing rather than a flat refusal. Generalize then, not before.

### Pattern C — SSE (streaming log/progress output)

For one-way streams from host-agent to brain:

```
GET /v1/jobs/j_a4f7b2/log
→ 200 OK
  Content-Type: text/event-stream

  id: 1
  data: {"ts": "...", "stream": "stdout", "line": "Reading package lists..."}

  id: 2
  data: {"ts": "...", "stream": "stderr", "line": "..."}
```

Three primary uses:

1. **App container logs** (`docker logs -f` equivalents, surfaced in the app-details view in the dashboard).
2. **Long-running job output** — apt upgrade progress, image pull progress, install/update logs.
3. **Tier-2 service logs** — `journalctl -u smbd -f` for the SMB admin page, etc.

Browsers speak SSE natively. When the dashboard surfaces these streams, the browser can subscribe through the brain to host-agent's SSE stream end-to-end with no translation.

**Reconnect resilience.** Each emitted event has a monotonic `id`. host-agent keeps a rolling per-job buffer of the last ~256 KB of log output. On reconnect, the client sends `Last-Event-ID: <n>`; host-agent replays from the buffer starting at `n+1`. If the gap exceeds the buffer, host-agent emits a single `data: {"lost": true}` event and resumes from current. This is standard SSE reconnect — uses spec-defined mechanisms only.

**`journal_follow` (`GET /v1/journal/follow?container=<name>`).** The first of the three journal operations `LOGGING.md` # Mechanisms calls for (`journal_query`, `journal_follow`, `journal_export_range`) — the live tail of one app container's stdout/stderr, surfaced as the dashboard's per-app Logs tab (`LOGGING.md` # Per-app logs, realized by the brain's `GET /api/v1/apps/{id}/log`). host-agent runs `journalctl CONTAINER_NAME=<name> -f -o json -n 100` (relying on Docker's daemon-wide `journald` log driver, `LOGGING.md` # Operational logs) and re-serialises each entry into a `JournalLine` frame — `{"ts","stream","line"}`, with `stream` derived from journald `PRIORITY` (≤3 → `stderr`, else `stdout`). `501` if host-agent has no log source wired, `400` if `container` is missing, otherwise `200` + `text/event-stream`. The brain passes the **main service's container name** (`malmo-<id>-<main_service>`); only that container's logs are exposed, never an arbitrary unit. The operation is read-only — there is no journal write path over the socket.

**Two-tier replay split (deviation from the generic per-job buffer above).** For `journal_follow` the authoritative ~256 KB ring buffer, `Last-Event-ID` replay, and `{"lost":true}`-on-gap live in the **brain's** per-instance log hub, not in host-agent. host-agent is a thin **per-connection streamer**: it stamps its own monotonic `id` per connection and, if a reconnect arrives carrying `Last-Event-ID`, emits one `{"lost":true}` frame and resumes live (it has no cross-connection buffer to replay from). The brain re-stamps every frame with its own monotonic counter, owns the ring shared across all dashboard subscribers of one app, and is the side the browser's `EventSource` reconnects against. A host-side shared-follower buffer (so two brain consumers share one `journalctl`) is deferred until a second consumer exists. The sibling `journal_query` (paginated historical search) and `journal_export_range` (range dump for the diagnostic bundle) are likewise deferred — v1 is live-tail only.

### Pattern D — WebSocket (bidirectional, future)

Not in v1. Will be needed when the web terminal lands: an interactive PTY requires bidirectional I/O.

WebSocket is an HTTP upgrade. It runs over the same UNIX socket, the same Go HTTP server, with no new transport. When we build it:

```
POST /v1/terminal/sessions
→ 201 Created
  { "session_id": "t_abc", "ws_url": "/v1/terminal/sessions/t_abc/io" }

GET /v1/terminal/sessions/t_abc/io
→ HTTP/1.1 101 Switching Protocols
  Upgrade: websocket
  ← bidirectional frames carrying terminal I/O
```

The principle: **HTTP/JSON for ops, SSE for one-way streams, WebSocket for bidirectional.** Additive; no pre-design needed in v1.

A web terminal has independent security implications (root PTY = root on the host) that need their own design — tracked in `NEXT.md` and `AUTH.md`.

## Authentication & authorization

**Authentication = socket file permissions. There is no application-layer token.**

The kernel enforces it: anything not in the `malmo` group can't connect. Anything in the `malmo` group can do everything host-agent exposes. There's no per-caller authorization because the only caller is the brain.

**Test invariant (CI must assert):**

> The `malmo` group on the running system contains exactly one member: the brain's container runtime UID. Any additional member is a configuration error and fails the test.

This is the entire authn/authz model for this boundary. If group membership is wrong, the security boundary is broken; the test is the safety net.

If a future tool ever needs host-agent access (a debug CLI, a recovery tool), we either add it explicitly to the test allowlist *and* the `malmo` group, or it talks through the brain.

## Versioning: lockstep with OS release

Brain and host-agent ship as part of the same OS release. Brain version N talks to host-agent version N. There is **no protocol-version negotiation** at connection.

**Why lockstep:**

- The box is one atomic unit (`UPDATES.md` # What this doc covers — stream A: Debian base + kernel + firmware + host-agent). Both binaries ship in that unit and are upgraded together. Note this covers `host-agent`, not the brain: the brain is a container and rides stream B, so lockstep here is a **compat floor** the brain declares (`minimumAgentVersion`), not a guarantee that the two moved in the same transaction.
- No version-negotiation code to maintain or get wrong.
- A crashed brain ↔ healthy host-agent imbalance is the only transient case; both binaries are tiny and can be upgraded together cheaply.

**Resolves an open item:** `NEXT.md` previously listed "brain ↔ host-agent protocol versioning" as open. Under lockstep, the question dissolves — there is no negotiation surface.

## Failure semantics

Four categories, each with its own mechanism. They're not one problem.

### A. Per-job declared attributes

Every operation host-agent exposes as a job declares static metadata. Not user-visible config — registration-time properties enforced by host-agent uniformly.

```
JobKind {
  Name           "system-update" | "app-install" | "disk-format" | ...
  MaxDuration    e.g. 30m for system-update, 60s for systemctl ops
  Dangerous      bool — crash mid-flight = no auto-resume (see APP_LIFECYCLE)
  ResourceClass  "apt" | "disk" | "systemd" | "network" | "none"
  StallPolicy    optional: "no progress for X = stalled"
}
```

**Timeouts.** host-agent enforces `MaxDuration` uniformly. Exceeded → status flips to `stalled` (distinct from `failed`). Cancellation runs SIGTERM → 10s grace → SIGKILL. Final result wins: if the op completes before SIGKILL, the job ends `completed` regardless of pending cancellation.

**Stalled vs. failed.** Distinct statuses. `stalled` means "we're not sure — it's running too long or producing no progress"; `failed` means "we know it broke." The UI surfaces these with different messaging — important for non-technical users.

**Resource-class serialization.** Two jobs sharing a `ResourceClass` cannot run concurrently. The second queues; job response carries queue position. Two `apt` operations can never race. `ResourceClass: "none"` ops have no serialization.

**Cross-class dangerous lock.** Any job with `Dangerous: true` waits for **all** running jobs (across resource classes) to drain before starting, and blocks any new jobs while it runs. Catches the case where, e.g., a disk format and an apt upgrade are technically different resource classes but you really don't want both at once.

**Registration is required-by-construction.** host-agent's job-kind registration function takes these attributes as required Go-typed parameters. You can't register an op without declaring them.

### B. Reconciler pattern (desired vs. actual state)

The companion mechanism to this protocol, specified in `APP_LIFECYCLE.md` (extends the existing app-lifecycle reconciler to all host-managed state).

Brief shape: brain models desired state in SQLite; host-agent exposes `GET /v1/state/summary` returning actual state; brain reconciles at three triggers — on startup, on a 60-second heartbeat, after every state-changing op. Drift policy: brain auto-reconciles when *it* made the last change (handles crash-mid-step); brain surfaces (doesn't auto-fix) when something *else* changed state (respects manual user changes via SSH).

Dangerous ops are excluded from auto-reconcile — interrupted `mkfs` is not safely retryable.

### C. SSE reconnect

Covered in Pattern C above. Self-contained: monotonic event IDs, ~256 KB per-job rolling buffer, `Last-Event-ID` on reconnect, single `{"lost": true}` event when the gap exceeds buffer. Uses SSE spec mechanisms only.

### D. Orchestration rules

Protocol-shaped rules about *when and how* the protocol is exercised. Not new protocol surface.

**host-agent self-update.** When the OS updater installs a new `malmo-host-agent` package:

1. Brain stops accepting new jobs.
2. Brain waits for running jobs to drain. Hard cap (5 minutes): if a job is still running, the OS update fails with "an operation is still running, retry later."
3. apt installs the new binary; systemd restarts host-agent.
4. Brain reconnects with backoff; resumes.

Brain treats "host-agent unreachable" during this window as expected, not as an error.

**FD limits.** host-agent's systemd unit sets `LimitNOFILE=16384`. Brain enforces ≤16 concurrent SSE streams at a time; host-agent enforces the same as a backstop.

**Concurrent dangerous ops.** Already covered by the cross-class dangerous lock in (A). Spelled out: never run two destructive ops concurrently. Ever. UI shows them as queued.

## Test invariants (CI)

Beyond the malmo-group membership assertion (above), CI asserts:

- Every registered `JobKind` has non-zero `MaxDuration` and an explicit `Dangerous` value (no defaults).
- A round-trip test for SSE reconnect: kill the brain mid-stream, restart, verify resume with the same `Last-Event-ID` recovers continuity (or emits `lost: true` if the buffer was overrun).
- A reconciler test: write a desired state to brain SQLite, simulate brain restart, verify reconciliation converges actual → desired for non-dangerous ops only.

## Locked decisions

- **Transport:** UNIX socket at `/var/run/malmo/agent.sock`, owner `root:malmo`, mode `0660`.
- **Wire format:** HTTP/1.1 + JSON, versioned URL prefix (`/v1/...`).
- **API patterns:** sync request/response (Pattern A) for <5s ops; explicit `Job` objects (Pattern B) for anything that can exceed ~5s or needs progress/cancel; SSE (Pattern C) for one-way streams; WebSocket (Pattern D) reserved for future bidirectional needs (web terminal).
- **Authentication:** socket file permissions only; no app-layer token. CI test asserts `malmo` group has exactly one member (brain's container UID).
- **Versioning:** lockstep with OS release. No protocol-version negotiation.
- **Out of scope for host-agent:** Docker daemon (brain talks to Docker via docker-socket-proxy), Caddy (managed container), Tier-1 app-facing services.
- **Debuggability is a first-class design constraint.** Choices that would make the protocol harder to debug from `curl` need an explicit justification.
- **Per-job declared attributes are mandatory.** Every `JobKind` declares `MaxDuration`, `Dangerous`, `ResourceClass`. Registration-time, type-enforced.
- **Stalled is distinct from failed.** Two job statuses, two UI tones.
- **Cancellation: SIGTERM → 10s grace → SIGKILL. Final result wins.**
- **Cross-class dangerous lock:** any `Dangerous: true` job blocks all other jobs while it runs and waits for all running jobs to drain before starting.
- **SSE reconnect: standard `Last-Event-ID` + ~256 KB rolling per-job buffer. Single `lost: true` event when the gap exceeds buffer.**
- **Reconciler pattern lives in `APP_LIFECYCLE.md`.** Drift policy: brain auto-reconciles when *it* made the last change; surfaces (doesn't auto-fix) when something else did. Dangerous ops excluded from auto-reconcile.
- **Heartbeat: 60 seconds.** Brain polls `GET /v1/state/summary`.
- **host-agent self-update drains all jobs first**; 5-minute hard cap before failing the OS update.
- **Network endpoints wrap NetworkManager over DBus.** host-agent is the only thing on the box that talks to NM. WiFi credentials live in NM's connection store (`/etc/NetworkManager/system-connections/`, root-only); the brain never persists them. See `BOOT.md` # NetworkManager and `DECISIONS.md` 2026-05-18.
- **GPU capability is a host query, not a manifest fact.** `GET /v1/system/gpu` reports presence + vendor + the `render` group GID; the brain uses it for both the install-time capacity gate and the `/dev/dri` `group_add`. v1 detects the Intel iGPU only (`vendor: "intel"`); AMD/NVIDIA runtimes are follow-ons. See `APP_ISOLATION.md` # GPU.

## Knock-ons to other docs

- `CONTROL_PLANE.md` — points to this doc as the authoritative spec for the brain↔host-agent boundary.
- `AUTH.md` — the "Brain ↔ host-agent in the auth path" section is consistent with this protocol (private channel, no app-layer token); the malmo-group test invariant is now documented here.
- `SERVICE_PROVISIONING.md` — Tier-2 ops (systemctl, config edits) flow through host-agent via this protocol's Pattern A and Pattern B.
- `UPDATES.md` — apt operations are Pattern B (jobs with SSE log streams). The "brain ↔ host-agent protocol versioning" open item is resolved (lockstep).
- `NEXT.md` — carries the future web-terminal and app-facing-background-jobs items (failure semantics is now closed).
- `HEALTH.md` — the # Detector catalog owns the per-issue measurement/cadence/threshold contract; this doc owns the `GET /v1/health/system` transport that carries locus-B findings to the brain.
- `APP_ISOLATION.md` — # GPU owns the locked install-refusal-on-no-GPU behaviour and the `/dev/dri` + render-group override stanza; this doc owns the `GET /v1/system/gpu` transport that feeds both. The OS-image media stack and the real `/dev/dri` detection are tracked in issue #125.
