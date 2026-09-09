# Architecture (as built)

What exists in this repo today and how the pieces are wired. This doc is the
**current state** — design intent lives in [`specs/`](specs/), per-change history
lives in [`progress/`](progress/). When the code changes, this doc changes in
the same PR.

## Components

Five real processes/artifacts make up a running malmo right now. Three are Go,
one is JavaScript, one is a container we don't write.

| Component | Lives in | What it is | Status |
|---|---|---|---|
| **`malmo-brain`** | `cmd/brain/`, `internal/` | The control-plane daemon. Owns SQLite state, the REST+SSE API, the app lifecycle, and the Caddy config. One Go binary. | Real |
| **`host-agent` (fake)** | `cmd/host-agent/` | Privileged side used in the inner dev loop. Speaks the real `BRAIN_HOST_PROTOCOL.md` wire format over a UNIX socket; the host operations themselves (Avahi, LUKS, PAM, apt) are stubbed in memory. | **Fake** (real wire, canned ops) |
| **`host-agent-real`** | `cmd/host-agent-real/`, `internal/hostagent/` | The real privileged binary. Seam-injected reporters: PAM password verify (`pamverifier`), `/proc` system sampling (`procsource`), disk usage, RAM pressure, journal streaming, service health, reboot-required flag, user manager, system time-zone setter (`timezone`, `timedatectl set-timezone` — the first-run wizard's Step 3, wired in both build profiles), per-account SSH access (`sshaccess`, #463 — renders `sshd_config.d/malmo-allowed.conf` whole from the enabled set, validates with `sshd -t`, and starts or stops sshd so :22 is open only while someone has SSH on; wired in both build profiles). Discovery is real: per-LAN-interface Avahi announcements (`avahipublisher`) driven by the NetworkManager LAN set (`netstate`), with an avahi-daemon.conf allowlist sync and IP-change replay. Seeds the brain's Docker transport then launches the brain container on startup (`brainlaunch`: `EnsureTransport` creates `malmo-ingress` + runs the `docker-socket-proxy`; `Launch` docker-loads the bundled image if absent, lockstep `malmo.protocol.major` OCI-label check, `docker run --restart unless-stopped` on the ingress net with `DOCKER_HOST` at the proxy). Host ops not yet wired: LUKS/TPM, apt, NM configuration (WiFi setup, `/v1/network/*`). A build-tagged slim **`hosted`** profile (`go build -tags hosted`, #204/C1c) compiles the discovery/NetworkManager stack out for the cloud image — `avahipublisher`/`netstate` unwired, no-op publisher, nil `Net` — keeping the same PAM/user-mgmt/health-system/brain-launch seams (`cmd/host-agent-real/wiring_appliance.go` vs `wiring_hosted.go`). | Partial — see "What is not built yet" |
| **Caddy** | `dev/caddy.json`, `dev/docker-compose.yml` | Reverse proxy. Terminates `*.local` (appliance) or `*.<box-id>.malmo.network` over real Let's Encrypt HTTPS (hosted, via a custom acme-dns build) and routes to app containers + the brain. Configured live by the brain via Caddy's admin API. | Real (container) |
| **`web-ui`** | `web-ui/` | Vue 3 + Vite + TanStack Query dashboard. Talks only to the brain. Tailwind 4 with the Oatmeal `@theme` tokens; `reka-ui` + `cn()` are present as shadcn-vue scaffolding, but the owned components in `components/ui/` (`Button`, `Heading`) are hand-written from the Oatmeal patterns, not pulled through the shadcn CLI (#261). Internal code architecture: [`dev/web-ui.md`](dev/web-ui.md). | Real |
| **SQLite** | `$STATE_DIR/malmo.db` | The brain's only persistent store. Schema + queries in `internal/store/`. | Real |

Plus the **Docker daemon** on the host, which the brain drives with the
`docker compose` CLI (`internal/lifecycle/docker.go`). App containers run on the
`malmo-ingress` Docker network so Caddy can reach them by service name.

## How the wires connect

```
                           ┌─────────────────────┐
        browser ─────────► │      web-ui         │
                           │ (Vue, TanStack Q.)  │
                           └──────────┬──────────┘
                                      │ HTTP + SSE
                                      ▼
                           ┌─────────────────────┐    docker compose CLI
                           │     malmo-brain     │ ──────────────────────► Docker daemon
                           │                     │                                │
                           │  api / lifecycle    │ ─── Caddy admin API ──► Caddy ─┘
                           │  store / catalog    │                          │
                           │  auth / audit       │                          ▼
                           │  caddy / events     │                       app containers
                           └──────────┬──────────┘                       (malmo-ingress net)
                                      │ HTTP/JSON over UNIX socket
                                      ▼
                           ┌─────────────────────┐
                           │  host-agent (fake)  │
                           │  in-memory state    │
                           └─────────────────────┘
```

**Each arrow, in one line:**

- **browser → web-ui:** Vite dev server in dev; the brain serves the built
  bundle in prod (planned). The UI is plain SPA, no SSR.
- **web-ui → brain:** REST under `/v1/*` (OpenAPI generated by huma) for
  reads/mutations; SSE under `/v1/events` for install/lifecycle progress. Auth
  is an opaque cookie minted by `internal/auth`; the middleware in
  `internal/api` gates every mutation. See `specs/BRAIN_UI_PROTOCOL.md`.
- **brain → Docker:** the brain shells out to `docker compose` per
  instance. The compose file is held verbatim from the manifest — the brain
  never rewrites it. Driver interface lives in `internal/lifecycle/` (the
  consumer), implementation in the same package.
- **brain → Caddy:** the brain POSTs JSON to Caddy's admin API to add/remove
  site blocks per app. A splash route covers `<slug>.local` until the
  container's health check passes, then flips to the real upstream.
- **brain → host-agent:** HTTP/JSON over `MALMO_AGENT_SOCK`. Two patterns,
  sync request/response and SSE-streamed jobs (`internal/protocol/host.go`
  defines the types; `internal/hostclient/` is the brain-side client). The routes
  the brain calls today, from `internal/hostclient/`:
  `/v1/auth/{verify-password,set-password,set-role,delete-user}`,
  `/v1/users/{username}/home`,
  `/v1/identity/{well-known,app-service,app-service/release}`,
  `/v1/discovery/{publish,unpublish}`,
  `/v1/system/{status,resources,gpu,set-timezone}`,
  `/v1/health/system`, `/v1/journal/follow`, and the job routes
  `/v1/jobs/system-update` + `/v1/jobs/{id}`. `specs/BRAIN_HOST_PROTOCOL.md`
  owns the wire and also lists the routes that are specced but not built yet
  (files, network, terminal, drive jobs).

## Inside the brain

Packages under `internal/` and what each owns. Layer rules come from
[`../CLAUDE.md`](../CLAUDE.md) # Go code discipline; only the directional rules
are stated below. The table is the whole of `internal/`'s top level; the
host-side implementation packages under `internal/hostagent/…` are covered by
the `host-agent-real` row in # Components and by # What is not built yet.

| Package | Owns | Imported by |
|---|---|---|
| `api` | HTTP handlers (huma), auth middleware, request/response shapes. The only package that knows about HTTP. | `cmd/brain` |
| `lifecycle` | The install transaction: door-1 (catalog) and door-2 (paste-a-compose), digest pinning, reconcile pass, health-wait, Caddy timing, uninstall. Owns the **one central route builder** (`buildRouteConfig`) that resolves each app's `caddy.RouteConfig` from profile + per-instance `exposure` (hosted owner-only default, `SetExposure` toggle, #306) + the manifest's `access.public_paths` carve-out and the always-on identity-header scrub (#415). Defines `DockerDriver` consumer-side. | `api`, `cmd/brain` |
| `store` | SQLite schema + queries. Sole persistence boundary. `ErrNotFound` is the only typed error. | `api`, `lifecycle`, `auth`, `audit`, `cmd/brain` |
| `catalog` | Door-1 source behind a fixed six-method facade. Production (every profile) uses the control-plane thin client (`NewRemote`, `MALMO_CATALOG_URL`): fetches `GET /catalog?env=<profile>` for browse data, checks its schema version, holds it in memory only (no catalog on disk), proxies+caches assets with a 24h expiry. An app's install payload is a separate per-app fetch at install time (`Load(ctx, id)` follows the record's `manifest_url` / `compose_url`); every published URL is opaque. Environment filtering is the control plane's (the `?env=` on the fetch), and there is no index digest — `version` is an opaque ETag token the box never recomputes (#434). The payload also carries the store's authored landing page (a spotlight app + category groups, authored in `home.yml`) and the authored category vocabulary (id + display `label`, in authored order) verbatim; `Home()` projects both (`docs/specs/APP_STORE.md` # Landing page, # Category labels). Category display text is always the authored label, never derived from the id. The disk reader (`New`) is retained only as a test constructor; no catalog is baked into the image. | `lifecycle`, `api`, `cmd/brain` |
| `manifest` | `manifest.yml` schema (parse + validate), and the synthesizer that wraps a pasted compose into a door-2 manifest. | `catalog`, `lifecycle`, `api` |
| `admission` | The single compose admission policy applied to both doors (image pinning rules, forbidden constructs, etc.). | `lifecycle` |
| `caddy` | Client for Caddy's admin API. Site-block JSON generation lives here (per-app route via `AddRoute(RouteConfig)` — optional hosted `forward_auth` gate, a strip of the single `RouteConfig.StripCookieName` cookie from the `Cookie` header, never the whole header, #306/#335, an unconditional `RouteConfig.ScrubHeaders` delete, and a `subroute` that carves `RouteConfig.PublicPaths` out of the gate, #415), plus the hosted wildcard-TLS automation policy (`EnsureWildcardTLS`: ACME DNS-01 via the `acmedns` provider for `*.<box-id>.malmo.network`). Profile-agnostic: the strip/gate policy is resolved by the caller. | `lifecycle`, `cmd/brain` |
| `profile` | The environment-profile marker (`appliance`\|`hosted`) + the first-boot seed reader, and the hosted URL-shape helpers (`HostedAppHost`/`HostedAppURL`/`HostedDashboardHost`/`CertSubjects` — the single place `<slug>.<box-id>.malmo.network` is named). Leaf package. | `api`, `lifecycle`, `cmd/brain` |
| `mailpreset` | The built-in outgoing-mail provider presets (host, port, encryption and the username rule per provider) plus `List`/`Get`/`Valid`/`LabelFor`. Hardcoded rather than catalog-served: the catalog is an app-distribution channel, these constants change roughly never, and the credential broker's per-provider logic has to be Go (`DECISIONS.md` 2026-08-27). Leaf package, no malmo imports. | `api`, `store` |
| `hostclient` | Brain-side client for `host-agent`. Mirrors the routes in `protocol`. | `lifecycle`, `api`, `auth`, `cmd/brain` |
| `protocol` | Wire types shared with `cmd/host-agent`. Source of truth for the host protocol. | `hostclient`, `cmd/host-agent` |
| `auth` | First-admin bootstrap, password verification (delegates to host-agent), opaque cookie sessions, plus the hosted per-app forward-auth credential (a second, lower-privilege `Domain`-scoped cookie on the session row that the box Caddy's `forward_auth` verifies against the brain, #305). Owns `ForwardAuthCookieName`, the one source of truth for the cookie name the route builder strips (#335). No password hashes on the brain side. | `api`, `lifecycle`, `cmd/brain` |
| `assertion` | Verifies the portal's short-lived Ed25519 ownership assertion for the hosted portal-to-box SSO handshake (`Verify`: signature + expiry; box-id/issuer/replay are the handler's policy). Minimal signed token, not a JWT. Mirrors the cloud signer's wire format. Leaf package. | `api` |
| `audit` | Append-only `audit_events` table writes. Every elevation-class mutation calls `audit.Record` on success **and** failure. | `api` |
| `events` | In-memory pub-sub bus for SSE. Lifecycle stages publish; the SSE handler subscribes. | `lifecycle`, `api`, `cmd/brain` |
| `health` | The brain's typed-issue registry (`HEALTH.md`). The taxonomy is registered in code — a stable string ID binds severity / category / tier / `blocks_*` at registration, never redeclared per raise. Writes through to SQLite on every raise/clear so issues survive a brain restart. | `api`, `notify`, `store`, `cmd/brain` |
| `notify` | Routing + derivation for the dashboard notification center (`NOTIFICATIONS.md`). Notifications are *derived* from events that already exist — today, health raise/clear transitions through a code-registered allowlist — never a parallel taxonomy. Coalesces by `dedup_key`, and emits the member-transparency variant for box-blocking storage issues. | `api`, `store`, `cmd/brain` |
| `applog` | Per-app log fan-out (`BRAIN_UI_PROTOCOL.md` Pattern C, `LOGGING.md` # Per-app logs). Sits between host-agent's single upstream follow per instance and the dashboard's many SSE readers, and owns the reconnect contract host-agent deliberately does not: a ~256 KiB ring buffer, replay from `Last-Event-ID`, one `{"lost":true}` marker when a position was evicted, and a linger so a quick reconnect reuses the warm buffer. One ref-counted `Hub` per instance — zero idle cost when nobody is watching. | `api`, `cmd/brain` |
| `systemlive` | The live system-resources stream (`BRAIN_UI_PROTOCOL.md` Pattern C stream 3, `LOCAL_ANALYTICS.md`). Ref-counted upstream poller: the first SSE subscriber starts a 1 Hz poll of host-agent's raw cumulative counters, each poll is diffed into rates and fanned out, the last unsubscribe stops it. Same zero-idle-cost shape as `applog`. | `api`, `cmd/brain` |
| `storageverify` | The canary + enrollment-marker check behind the `malmo-storage-verify` reporter (`BOOT.md` # The storage-ready target, `STORAGE.md` # Storage canary). Split out of `cmd/` only so the check is unit-testable against a tempdir root; the binary is a thin shell that writes findings to `/run/malmo/health/storage.json`. **Not a brain package** — it is imported by `cmd/malmo-storage-verify` alone. | `cmd/malmo-storage-verify` |
| `version` | The malmo build identity: `Version` (repo `VERSION` file) and `Commit` (git sha), stamped at build time via `-ldflags -X` (`Makefile`, `BUILD.md` # Versioning). Dumb — vars + a `String()`, no logic. | `api`, `hostagent`, `cmd/brain`, `cmd/host-agent`, `cmd/host-agent-real` |

**Cross-cutting invariants:**

- **Brain commits first, host is reconstructible.** Mutations that span SQLite
  + host-agent commit to the brain first, then call the host. On host failure,
  the brain row is rolled back. Established by `/setup`, `createUser`,
  `updateUserRole`, `deleteUser`. See [`../CLAUDE.md`](../CLAUDE.md).
- **Single logger.** `slog.Default()` everywhere; no `*slog.Logger` threading,
  no `log` package, no `fmt.Println` for diagnostics. Standard field names are
  listed in CLAUDE.md.
- **Audit on success and failure.** Elevation-class handlers emit
  `audit.Record(..., success=false)` on every observable failure path (host
  502, store 500, conflict 409, guard rejection), mirroring `login.failure`.

## On-disk layout (dev)

```
.dev/
  agent.sock          UNIX socket the brain dials the fake host-agent on
  state/
    malmo.db          brain's SQLite (schema in internal/store)
    instances/        per-app state (compose file, .env, digests)
    services/         managed-service data (postgres-<v>/, valkey-<v>/, …)
  catalog-cache/      proxied catalog icons + screenshots (24h expiry)
  host-agent          built binary
  brain               built binary
dev/
  caddy.json          Caddy bootstrap config (replaced live via admin API)
  docker-compose.yml  brings up the dev Caddy + malmo-ingress network
```

`MALMO_STATE_DIR` and `MALMO_AGENT_SOCK` are set by the Makefile so the brain
and host-agent agree on paths.

## Dev orchestration

`make dev` runs the four foreground processes — Caddy (container), host-agent,
brain, Vite — in one terminal. `make help` lists the per-process targets for
the four-terminal layout. See [`dev/running-locally.md`](dev/running-locally.md)
for the full inner loop. The VM-based outer loop for host-integrated parts
(boot, LUKS, systemd) is not wired into the native dev loop — it lives in the
QEMU lanes (`specs/TESTING.md`).

## What is **not** built yet

So this doc isn't read as a claim about the finished product:

- **Full real host-agent.** `cmd/host-agent-real` is partially real: PAM password verify, `/proc` system sampling, disk usage, RAM pressure, journal streaming, service health, reboot-required, discovery (per-LAN-interface Avahi announcements from the NetworkManager LAN set, allowlist sync, IP-change replay), and the first-boot brain launch (#164: load-if-absent, lockstep label check, `docker run --restart unless-stopped`) are wired. LUKS/TPM, apt, and the NM configuration surface (WiFi setup, `/v1/network/*`) are not yet wired — those ops are still no-ops or stubs.
- **Control-plane stack bring-up — built (M1b, #165), VM-boot acceptance pending.** host-agent seeds the brain's Docker transport (the `malmo-ingress` network + the `docker-socket-proxy`, raw socket `:ro`, `EXEC` denied) before launching the brain, and points it at `DOCKER_HOST=tcp://docker-proxy:2375`; the brain then reconciles Caddy + `malmo-ui` from the staged control-plane compose (`lifecycle.EnsureControlPlane`) and installs the dashboard route (`/api/v1/* → brain`, else → `malmo-ui`). All of it is **production-gated** on `MALMO_CONTROL_PLANE_DIR`/`MALMO_DASHBOARD_UI_UPSTREAM`, so the natively-run dev brain is unchanged (standalone dev Caddy, Vite UI, raw socket). Managed DB in production is **no longer blocked**. #185 moved provisioning off `docker exec` onto a one-shot `--rm` client container. So `EXEC` stays denied, and the brain stays off the app-reachable `malmo-svc-*` network (`DECISIONS.md` 2026-06-15, which lifts the 2026-06-14 gate). The bring-up is proven on a booted box by the **hosted** cloud lane, which runs in CI. The **appliance** medium-lane run (`sudo make test-medium-qemu`) is still outstanding.
- **Storage subsystem — the boot half exists, the pooling half does not.** The
  userspace boot chain is real and shipped in `dist/systemd/`
  (`malmo-storage-ready.target`, `malmo-storage-verify.service`,
  `malmo-recovery.target`) with the reporter in `cmd/malmo-storage-verify` and
  the health wiring in `internal/health`
  ([`progress/boot-pipeline-units.md`](progress/boot-pipeline-units.md)), and
  **LUKS root + first-boot TPM enrollment + unseal is proven end to end** in the
  QEMU medium lane against a real kernel and a software TPM
  ([`progress/luks-tpm-enrollment.md`](progress/luks-tpm-enrollment.md)). What
  does **not** exist: the data-drive half — no mergerfs assembly, no `/srv/malmo`
  pool (the path appears only as the storage-verify canary), no UI or host-agent
  surface for adding a drive or unlocking one. On a dev box apps still write to
  wherever Docker puts volumes.
- **Boot, install ISO, updates.** The `mkosi` image build (`BUILD.md` # 2;
  proven in the test lane, not yet the production ISO) and stream A
  (`unattended-upgrades` + the apt repo) are spec-only. **Stream B — the
  control-plane update — is half built.** A box declares its brain/UI pair in
  two files (`internal/hostagent/controlplane`: the staged compose plus an
  `images.json` ledger), the apply/health-check/revert transaction exists
  (`internal/hostagent/cpupdate`), and an admin can start one:
  `POST /api/v1/system/update` with two explicit image refs → the
  `system-update` job on the host socket (`internal/hostagent/jobs.go`), polled
  via `GET /api/v1/system/update/{job_id}`. That path **is proven on a booted
  box**: the hosted cloud lane's `update` boot drives a real update and a real
  failed-update-then-revert against a real Docker daemon and a registry inside
  the guest (#382/#388), and it runs on PRs that touch the updater (#389).
  **What picks the target is one seam** (`internal/hostagent/updatetarget`,
  #401), consumed once by host-agent: it asks a source, validates the answer,
  compares it with the pair the box declares, and starts the same job an admin
  would. On **hosted** the source is the control plane's public update-target
  URL, asked with the box's own `box_id` (#408) so the answer can be per box,
  and the box applies without a prompt, inside the window the answer names or
  the 03:00–04:00 default; on
  **appliance** the source is the signed release manifest and the box only
  learns its target, because the control plane there is admin-prompted. Every
  answer must name both images pinned to a digest in an expected repository, or
  it is refused with nothing pulled. The hosted half is proven on a booted box
  (the `update` boot also drives the loop and its refusal path).
  The appliance trigger is **half built**: `internal/hostagent/relmanifest`
  verifies, parses and caches the signed release manifest and polls it hourly
  (#395/#397). What is still missing is the rest of picking a target: **no
  signing key and no `releases.malmo.network`** (so every build refuses every
  manifest and does not poll — the deliberate inert state), **no digests in the
  manifest** (it names versions, so the appliance source can only refuse —
  `NEXT.md`), no per-box targeting or report-back on hosted (blocked on the
  box↔cloud credential, `NEXT.md` Tier 1), no update notification, and no
  dashboard surface beyond Settings → About reporting the running versions
  (#393). **What the seam decided is now readable** (#443):
  `GET /v1/system/update-target` on the host socket, re-served admin-only as
  `GET /api/v1/system/update-target`, answering what the box runs, what it could
  run, where the target and the window came from, when it last checked and why
  the last check produced nothing. One `state` field carries the whole answer,
  and it keeps "nothing on offer", "could not ask", "answer refused" and "this
  box has no update loop at all" apart. No UI reads it yet — it is what the
  dashboard prompt will consume.
- **File manager.** `FILES.md` is written, and `/files` is a real top-level
  route. Nothing behind it is built. The brain registers no `/api/v1/files/*`
  handlers, and host-agent implements none of the `/v1/files/*` ops the protocol
  reserves. `FilesView.vue` is a "coming soon" stub.
- **Telemetry — we record consent, and send nothing.** The first-run consent
  choice is stored (`POST /api/v1/system/telemetry` → `box_meta`). Nothing goes
  anywhere after that: there is no telemetry client, and no endpoint to send to
  (`TELEMETRY.md`). Health, notifications, time and discovery used to sit in
  this bullet too. They are built now: `internal/health` raises typed issues
  that show on `GET /api/v1/health` and in the dashboard's `HealthBanner`; the
  notification centre and its bell work (`internal/notify`,
  `NotificationBell.vue`, mute per category); the `clock-not-synced` detector
  reads real `chronyc tracking` (`internal/hostagent/clockhealth`); the time
  zone can be set through host-agent; and Avahi discovery is listed as real in
  # Components above.
- **Off-box notification transports.** The bell is dashboard-only. No email and
  no push, so a box that needs attention while nobody is looking at the
  dashboard cannot say so (`NOTIFICATIONS.md`, `specs/NEXT.md` Tier 2).
- **Login UI — it renders, but it is not a route.** `Login.vue` is real and
  users reach it. `App.vue` chooses between `Setup`, `Login` and the dashboard
  based on auth state, so a logged-out appliance box shows the login screen with
  no route change. There is no `/login` route: the logged-out screens are app
  state, not router entries. `/recover` is the one exception. Cookie sessions
  and the auth pipeline behind the screen are real.
- **App store.** Every box syncs browse data from the control plane (`GET /catalog?env=`) and fetches an app's manifest + compose per install, then persists them next to the installation; TLS is the trust story, with no integrity digest and no Ed25519 signature (`DECISIONS.md` 2026-07-02, 2026-09-04). No catalog is baked into the image and the box keeps no browse copy on disk (`DECISIONS.md` 2026-08-17). What remains is cloud-side: the store is the authoring surface. The door-1 app-authoring how-to (`docs/dev/authoring-apps-with-an-agent.md`) is reconciled with that — it authors into `store:apps/<id>/` and keeps the schema, tooling, and gap ledger here.

For where each of these is planned, see the matching `specs/` doc.

## Reading order for a new contributor

1. This file.
2. [`specs/SPEC.md`](specs/SPEC.md) and [`specs/CONTROL_PLANE.md`](specs/CONTROL_PLANE.md)
   — the design vocabulary the code uses.
3. [`dev/running-locally.md`](dev/running-locally.md) — get the stack up.
4. [`progress/walking-skeleton.md`](progress/walking-skeleton.md)
   through the latest entry — the order things were built, with the why.
5. `cmd/brain/main.go` — 100 lines, names every package and how they wire.
