# Settings — information architecture

> Working spec for what **Settings** is: the panel tree behind the dock's fourth item, who sees which panel, and where the scattered `Settings → X` destinations named across the other specs actually live. Companion to `DASHBOARD.md` (which owns the dock that reaches Settings and the top bar), `AUTH.md` (the role model and elevation window this doc gates on), and the subsystem docs that own each panel's deep behavior.
>
> This doc owns the **shell and the index** — the bucket split, the panel inventory, role gating, and delegation. It does **not** re-own per-panel UX: `STORAGE.md` owns the add/eject-drive flow, `MALMO_NETWORK.md` the secure-URLs toggle, `UPDATES.md` the rollback affordance, `USERS_AND_GROUPS.md` the user-management flows, and so on. Settings is the frame those panels slot into; each panel's row says "this exists, it's gated like this, its behavior lives there."

## North star for this surface

**Role-scoped and calm, same posture as the home screen** (`DASHBOARD.md` # North star). A member who taps Settings sees a short personal list — their name, their password, their devices — not a wall of system panels they can't touch. An admin sees that same personal list plus the box's system tree. The non-technical owner is the long-term audience (`CLAUDE.md` # Audience); Settings must not read as a control panel to the person who only wants to change their password.

"UI is the path for every privileged operation" (`CLAUDE.md`, `DECISIONS.md` 2026-05-15) makes Settings load-bearing: every box-level knob a tinkerer could reach over SSH has a panel here, so the long-term user never opens a terminal.

---

## Locked: the My-account / Box-settings split is the spine

Settings has exactly two buckets, mirroring `AUTH.md` # Roles:

- **My account** — every signed-in user, scoped to themselves. No admin rights required. This is the whole of what a member sees.
- **Box settings** — admin-gated, scoped to the whole machine. Absent entirely from a member's Settings.

This split is the first decision because it determines the shape of the surface per role: a member's Settings is just the **My account** list; an admin's Settings is **My account** followed by the **Box settings** tree. The two buckets are not styled as one long flat list with greyed-out rows — a member never sees a box panel they can't use, in keeping with the calm posture. This matches `AUTH.md`'s router-level grouping (`/api/me/*` for any authenticated user, `/api/admin/*` for admins): the IA mirrors the auth boundary rather than inventing a second one.

---

## Locked: panel inventory

Every panel below is already implied by an existing spec. The **owning doc** column is where the deep behavior lives; this table is the index, not the design.

| Bucket | Panel | What it holds | Owning doc | Role |
|---|---|---|---|---|
| **My account** | Profile | Display name, change password | `AUTH.md`, `USERS_AND_GROUPS.md` | any user |
| | Device access | Per-account SSH + SMB opt-in toggles, and the account's SSH public keys (upload a file or paste the text). The mandatory auth factor is the profile's — a key on hosted, the password on the appliance | `AUTH.md` # Device access | any user |
| | Sessions | "Sign out everywhere" | `AUTH.md` # Sessions | any user |
| | Notifications | Per-category mute toggles | `NOTIFICATIONS.md` # Configuration | any user (role-filtering the list is a `NEXT.md` open item) |
| **Box settings** | Users | Create / reset password / role change; per-user pending update facts | `USERS_AND_GROUPS.md`, `UPDATES.md` # Admin visibility | admin |
| | Storage | Capacity, add / eject data drive, recovery passphrase | `STORAGE.md` | admin |
| | Network | WiFi, static IP, hostname, multi-NIC priority, mesh enrollment | `MALMO_NETWORK.md`, `FIRST_RUN.md` | admin |
| | Sharing | Samba network shares, media streaming (DLNA) | `SERVICE_PROVISIONING.md` | admin |
| | Outgoing email | SMTP provider registry (add / edit / delete / test-send) apps bind to for sending email. Adding one is a provider picker plus the credential, not seven blank fields; host / port / encryption sit behind Advanced settings | `SERVICE_PROVISIONING.md` # BYO outgoing mail | admin |
| | Remote access | Tailscale / Headscale UI at `/settings/tailscale` | `SERVICE_PROVISIONING.md`, `MALMO_NETWORK.md` | admin |
| | Updates | Aggregate app + OS view, check-for-updates, rollback | `UPDATES.md` | admin |
| | Privacy | Telemetry on/off (single toggle, both streams), last-transmission timestamp | `TELEMETRY.md` # Settings UI | admin |
| | System | Logs viewer + access-log toggle, download diagnostics, time/region/NTP, system health, restart/shutdown, About, factory-reset. (The **Activity**/audit-log view is reachable from here for admins but is a sibling all-users route — see role gating below.) | `LOGGING.md`, `TIME.md`, `HEALTH.md`, `AUTH.md` | admin |

---

## Locked: role gating

A member's Settings is **My account** plus the **Activity** view — their own audit events, scoped server-side (`LOGGING.md` # Visibility rules) — and nothing else. An admin's Settings is **My account**, the **Activity** view (the full box-wide feed), plus the full **Box settings** tree. This is enforced exactly as `AUTH.md` # Roles specifies: role is checked **server-side in the brain** on every request (`/api/admin/*` requires admin; `GET /api/v1/audit` is open to all but row-filtered by role), and the UI hiding box panels from members is defense in depth, not the boundary. A member who hand-crafts a request to an admin panel gets a 403 from the brain regardless of what the UI rendered. (Activity being member-reachable was settled by issue #11 — see `DECISIONS.md` 2026-06-05.)

Two consequences of the calm posture worth stating:

- The **Notifications** panel lists every category to every user today, even the admin-only ones a member can never receive (`storage`, `system`, admin `updates`). A member muting one of those writes a harmless dead row. Trimming the list per-role is deferred (`NEXT.md`).
- **Privacy is an admin-only Box panel**, not a My-account one — the telemetry toggle is box-wide, so a member flipping it would be ambiguous. The panel keeps the name both `FIRST_RUN.md` and `TELEMETRY.md` already use (**Settings → Privacy**); this doc only fixes its bucket (Box, admin-gated) and is the durable home of that placement. This is a deliberate reconciliation with `FIRST_RUN.md`, which frames the telemetry choice as something offered *during setup* (when the person at the keyboard is the founding admin): the *setting* is box-level and therefore admin-gated; the first-run *prompt* is the admin making that box-level choice once. No contradiction — the deep behavior of the toggle stays in `TELEMETRY.md` # Settings UI.

---

## Locked: elevation re-prompts on destructive box operations

On top of the role check, **destructive Settings operations re-prompt for the password** with a 5-minute elevation window per session — the sudo-in-UI pattern (`AUTH.md` # Roles, full mechanics in `USERS_AND_GROUPS.md` # Elevation in the UI). Deleting a user, changing a role, ejecting a drive, factory reset, restart/shutdown — each re-prompts unless the window is fresh.

**Enrollment-class operations bypass the window and re-prompt every time** (`AUTH.md`): add-drive and eject-drive touch the LUKS keyslot set or remove physical media; they're rare, deliberate, and not safely batched. SETTINGS.md does not re-specify the window — it points at `USERS_AND_GROUPS.md` and notes which panels trip it (Users, Storage, and the destructive corners of System).

---

## Locked: Tier-2 service UIs nest under their topical panel

Tier-2 services (Tailscale, Samba, DLNA) have no subdomain of their own; the dashboard surfaces a hand-curated UI for each at `/settings/<service>/*` (`AUTH.md` # Tier-2 admin surface). In the IA those routes nest under the topical panel they belong to, not as siblings of it:

- `/settings/tailscale` lives under **Remote access**.
- `/settings/shares` lives under **Sharing**.

These are same-origin dashboard routes, not separate apps — the `malmo_session` cookie just works, which is the whole reason Tier-2 collapses the auth problem (`AUTH.md`). The panel is malmo's UI talking about the underlying systemd unit and config file via host-agent; the user never sees the upstream admin UI. Deep behavior stays in `SERVICE_PROVISIONING.md`.

---

## Locked: Updates lives in Settings, not the Store

The Store is a discovery / install surface; **Settings → Updates** is the maintenance surface. They answer different questions — "what can I add?" versus "what's stale on my box, and can I roll it back?" — and the decisive fact is that apps are only one part of what goes stale on a box. `UPDATES.md` # What this doc covers splits updates into **stream A (the box:** Debian base, kernel, firmware, host-agent**)** and **stream B (the containers:** brain + UI, apps, managed services**)**. The Store has nothing to say about stream A at all, and within stream B it can speak only for apps. Settings → Updates is the only place that can be the box-wide "everything stale" roll-up, and it is the home of the **rollback affordance** (the kept-for-7-days image + snapshot), which has no conceivable home in a catalog.

Even for apps alone, `UPDATES.md` # 6 locks three surfaces — none of which is the Store catalog:

- **Per-app tile badge** on the dashboard — the contextual "this app moved" nudge, with a "what's new" panel.
- **Settings → Updates** — the aggregate view ("X updated last night, Y waiting on you, Z failed") and the rollback affordance.
- **Settings → Users → `<user>`** — per-user pending permission-accepts, read-only for admins (an admin cannot accept on another user's behalf; Tier-3 per-user instances are that user's to authorize).

SETTINGS.md indexes all three and delegates the behavior to `UPDATES.md`. The permission-expansion modal (`UPDATES.md` # The permission-expansion prompt) is a login-time modal, not a Settings panel, and is out of scope here.

---

## v1 additions this doc claims

The genuinely-new items that no other doc owns yet. SETTINGS.md gives them a home and the gating; deep mechanics live where noted or are deferred.

- **Restart / Shutdown** — under **System**, admin-only, elevation-gated. Buildable in v1: host-agent already exposes the power capability (`BRAIN_HOST_PROTOCOL.md` # Power — shutdown, reboot; `POST /v1/system/reboot`). The UI path is the only missing piece, and "UI is the path" makes it a v1 requirement, not a nicety.
- **System health / diagnostics** — a read-only panel under **System** surfacing the `HEALTH.md` check results (pass / warn / fail), borrowing the `yunohost diagnosis` taxonomy that `HEALTH.md` already adopts. Distinct from the top-bar live-resources chevron (`LOCAL_ANALYTICS.md`), which is real-time gauges; this is the check verdicts. v1.
- **About / Info** — version readout (malmo release, brain, UI, host-agent), box ID, license and links. Under **System**. v1.
- **Factory reset / repurpose** — Settings is its home, but the **mechanics are deferred** and tracked in `NEXT.md` # Factory reset / repurpose: it has a security dimension beyond UX (securely destroying LUKS keyslots so an outgoing drive is unreadable), so it is not purely a dashboard flow. SETTINGS.md reserves the slot under **System**, gated by a fresh password prompt, and points at `NEXT.md` for scope (full wipe vs. reset-config-keep-content) and key-destruction mechanics. v1 reserves the slot; the flow is not yet designed.
- **Backup settings** — the dashboard greeting concept references "last backup," but off-site backup is paid and post-MVP (`NEXT.md` # Backup architecture shape). SETTINGS.md reserves a **Backup** slot under **Box settings** as a stub that points at the deferred architecture; it is not a v1 panel with behavior.

---

## Deferred / out of scope

Recorded so a future PR adding any of these is a deliberate reopening, not drift:

- **Wallpaper / theming.** The calm palette is fixed for v1 (`WEB_UI.md`); no personalization panel.
- **In-settings terminal / web shell.** SSH is rescue-only (`CLAUDE.md`); malmo deliberately ships no web terminal, unlike ZimaOS/CasaOS.
- **Power scheduling / UPS.** Synology-style power schedules and UPS integration are a plausible later add, deferred from v1.
- **API tokens.** Long-lived, user-scoped, revocable tokens would live under **My account** if added; tracked in `NEXT.md` (cookie-only auth in v1).
- **Factory-reset mechanics and backup architecture** — slots reserved above; designs deferred per `NEXT.md`.

---

## Relationship to other docs

- `DASHBOARD.md` — owns the dock that reaches Settings, the top bar (the storage pill and avatar menu both link into Settings), and the single-user simplification that hides multi-user vocabulary in the manage-apps list.
- `AUTH.md` — the role model (My account / Box settings maps to member / admin), the server-side router grouping, the elevation window, and the Tier-2 `/settings/<service>` surface.
- `USERS_AND_GROUPS.md` — the Users panel flows and the full elevation mechanics.
- `UPDATES.md` — the Updates panel, rollback, and the per-user pending-update facts in Users.
- `STORAGE.md`, `MALMO_NETWORK.md`, `SERVICE_PROVISIONING.md`, `TIME.md`, `LOGGING.md`, `HEALTH.md`, `TELEMETRY.md`, `NOTIFICATIONS.md` — own the deep behavior of their respective panels.
- `WEB_UI.md` — the stack the panels are built on.

Open items that touch this surface (role-filtering the Notifications list, the Settings → Storage Level-1 walk-through, the Settings → Network panel UX, the Settings → System deep-view graphs, factory-reset and backup designs, API tokens) live in `NEXT.md`, not here.
