# malmo Decisions Log

> Reverse-chronological log of decisions we made, decisions we *changed*, and the reasoning behind each. Not a changelog of code (there is no code). Not a list of locked decisions (those live at the bottom of each doc). This file captures the **evolution of thinking** — what we used to believe, what we believe now, and why we changed our mind.
>
> Read this when you're tempted to relitigate something. Add an entry whenever a load-bearing decision flips or a new one lands with non-obvious reasoning.

## Format

Each entry:

```
## YYYY-MM-DD — Short topic title

**Previously:** what we used to think / had written.
**Now:** what we believe now.
**Why:** the reasoning. Cite the specific friction, evidence, or principle that drove the change.
**Affected docs:** files updated as a result.
```

Keep entries skimmable. The detailed rationale lives in the affected doc; this file is the pointer + the *delta*.

---

## 2026-09-09 — SSH ships on both profiles; the mandatory auth factor is set by the profile (#463)

**Previously:** three separate positions, all written for a box behind a LAN. `AUTH.md` # Device access made SSH per-account opt-in with the **malmo password** as the credential, one password shared with the dashboard and SMB. `BUILD.md` # SSH kept sshd **running from boot** with an empty `AllowUsers`, so the port was always open and always rejecting. `ENVIRONMENT.md` # Access & files turned SSH **off entirely on hosted**, because there is no LAN to scope port 22 to and no mesh. None of it was ever built — there is no SSH code anywhere in the tree.

**Now:** SSH ships on both profiles, per account, off by default, with three things changed.

1. **The mandatory auth factor is set by the profile.** Hosted requires a **public key**. Appliance requires the **password**. Each profile offers the other factor as an optional **second** lock, never as an alternative door: choosing it renders `AuthenticationMethods publickey,password`, so both are demanded and neither alone authenticates.
2. **Port 22 is open only while at least one account has SSH enabled.** sshd is started when the first account opts in and stopped when the last opts out. This replaces daemon-on-but-no-account.
3. **Hosted SSH is no longer off.** The reasoning that put it there still holds and is exactly why hosted's mandatory factor is the key.

**Why:**

- **The one-password decision was a decision about a LAN, not about passwords.** It works on the appliance because nftables blocks port 22 from the public internet, so a household-strength password is only ever offered to devices already in the house or already paired on the mesh. A hosted box has neither. Carrying the rule across would quietly turn that password into an internet-facing credential.
- **A public port 22 routes around the control that already exists for this attack.** `AUTH.md` # Rate limiting gives the login path per-username backoff, a 15-minute lockout, a per-IP bucket, and a careful rule for which hop of `X-Forwarded-For` to trust. sshd knows none of it. Password auth on a public port is a second, unguarded door to the same credential the first door is carefully guarding.
- **The blast radius is other people.** Admins are in `sudo` (2026-05-15), so a guessed admin password is root, and `/home` holds every household member's files. `THREAT_MODEL.md` already refuses privileged containers to admins on exactly this reasoning — a multi-user box means the installer is not the only one who bears the consequence. Refusing an internet-facing guessable credential is the same line.
- **`THREAT_MODEL.md`'s accepted residual was scoped to a LAN.** "A compromised admin SSH session is root" was accepted when the port was unreachable from the internet. Opening it turns that sentence into a much larger claim, so the risk has to be removed rather than re-accepted.
- **Key-only is affordable here, because losing a key is not a lockout.** The dashboard is reached through the portal on hosted and through the login screen on the appliance, never through SSH. A user who loses their key signs in normally and pastes a new one. That self-service recovery is what makes the strict option viable for non-technical owners. It also costs the intended audience nothing: someone who cannot produce a public key has no use for a shell.
- **The daemon is the port control, because the box is its own perimeter.** Measured on the production provider rather than assumed: the cloud repo's Hetzner `ServerCreateOpts` sets no `Firewalls` field and that provider package contains no firewall code, so a hosted box has every port open today. Stopping sshd therefore genuinely closes 22, with no in-guest ruleset — which matters, because a general malmo-owned `nftables` ruleset was deferred on purpose (2026-06-19). The appliance keeps its LAN-scoping drop-in on top; the two are complementary.
- **`BUILD.md`'s two arguments for daemon-on did not survive.** "No visible service restart" is not a real difference, since a `systemctl` call through host-agent is no more visible to the user than a config edit. "Rejects at name resolution before evaluating credentials" defends a weaker posture rather than arguing against a stronger one. A closed port beats a port that answers and refuses.

**Not decided here, and deliberately separate:** operator/fleet debug access. A per-user toggle a tenant can switch off is not fleet access, and the answer is an SSH certificate authority the hosted image trusts, which lives in the control plane. It decides whether the image ships a `TrustedUserCAKeys` line.

**Noticed while measuring, not fixed here:** `ENVIRONMENT.md` # Network filtering states that provisioning every tenant behind a provider security group admitting only 443 and 80 is an explicit operator requirement. The Hetzner provider attaches no firewall, so that requirement is not implemented. Real drift, tracked separately from this work.

**Affected docs:** `AUTH.md` # Device access (rewritten for the two profiles and the AND semantics), `BUILD.md` # SSH (daemon lifecycle; the nftables scoping stays), `ENVIRONMENT.md` # Access & files (the "SSH: off in v1" bullet is replaced), `BRAIN_HOST_PROTOCOL.md` (the new `/v1/ssh/*` operations), `SETTINGS.md` # panel inventory (Device access row). Progress: `ssh-per-account-access.md`.

---

## 2026-09-04 — Catalog: browse and install are two fetches, and the index digest is gone (#434)

**Previously:** the box pulled one bulk snapshot, `GET /catalog/sync`, that carried every published app's verbatim `manifest.yml`, `compose.yml` and resolved images map, whether or not the box would ever install them. The snapshot was stamped with `index_sha256`, a SHA-256 over the app index that the box **recomputed** by re-marshalling what it had parsed, and refused the whole snapshot on a mismatch. Environment visibility was filtered on the box (`visibleIn`).

**Now:** two seams. `GET /catalog?env=<environment>` returns browse data only — display records, the landing page, the category vocabulary, and an opaque `version` token served as the `ETag`. An app's install payload is fetched per app, at install time, from `GET /catalog/apps/{id}/manifest` and `/compose` (`application/yaml`, verbatim), by following the `manifest_url` / `compose_url` the record carries. Every published URL is opaque: the box follows it, never assembles it. The digest is **removed** — `version` is a change token the box stores and echoes and never recomputes — and unknown keys are dropped the way `encoding/json` drops them everywhere else. The schema-version refusal stays. Environment filtering moved to the control plane. An app's manifest and compose are **persisted next to the installation**, and every brain path that needs an installed app's manifest reads that copy.

**Why:**
- **The bulk snapshot was mostly waste.** On a 43-app catalog it was 614KB, of which **77% was install payloads** for apps the box would never install; the browse data the store actually renders was 106KB. Every box paid the full 614KB on first sync and on every catalog change.
- **The digest was doing cache work while wearing an integrity label.** Recomputing it meant the box had to re-marshal to byte-identical JSON, which made field order load-bearing and made **any** new published field a flag day: the digest stopped matching, `verify()` refused the snapshot, and the box showed an empty store. Measured, not theoretical — `external_costs` did exactly this. It closed no threat TLS (origin) and HTTP framing (truncation) do not already close, so it is removed rather than reworked.
- **Routine box operation must not depend on the catalog service.** `Load` was never install-only: the app detail page, the mail picker and the install plan all called it. Turning it into a live fetch would have put a page load behind the network. Writing the manifest and compose next to the installation fixes that and a pre-existing bug with it — an installed app's manifest used to disappear the moment the app was unpublished.

**Trade accepted:** with filtering server-side, an installed app that leaves the box's surface (or the catalog) is no longer in the browse response, so `Entry(id)` cannot resolve its card. The install is unaffected — the manifest lives next to it — but the card loses its catalog-supplied icon and falls back to the instance row's own name. Persisting the display record too was the alternative; it buys an icon and costs a second copy to keep fresh.

**Affected docs:** `docs/specs/APP_STORE.md` (banners, Failure modes, What we run, Landing page, Category labels, What the box models, Locked decisions); `docs/architecture.md` (catalog package row + app-store bullet). Progress: `catalog-split-browse-and-install.md`.

---

## 2026-08-27 — Outgoing-mail provider presets: hardcoded in the brain, and the picked provider is persisted

**Previously:** Adding an email account meant typing seven fields the admin had to look up in their provider's docs. The brain knew nothing about who the provider was — only a host, a port and a credential.

**Now:** The brain ships a table of built-in presets (`internal/mailpreset`), serves it at `GET /api/v1/mail-presets`, and stores which one the admin picked as `mail_providers.provider_type`.

**D1 — presets are hardcoded in the brain, not catalog-served.** The catalog is an app-distribution channel; these constants change roughly never, and the credential broker's per-provider logic (`NEXT.md` # On-box credential broker) has to be Go anyway. Putting them in the catalog would add a fetch, a schema and a failure mode to data that ships fine in the binary.

**D2 — `provider_type` is persisted, not discarded after create.** It re-renders the right edit form and labels the row, but the real reason is the broker: without it, every provider registered before the broker ships is an untyped row someone has to classify by pattern-matching hostnames.

**D3 — the server stores what the client sends and does not re-derive host, port or encryption from the preset.** Re-deriving would silently undo the Advanced override, which exists so a non-standard endpoint is never trapped. The consequence, accepted on purpose: `provider_type` records what the admin picked, not a guarantee that the other fields still match the preset.

**D4 — no change to `MALMO_MAIL_*` injection.** `mailEnvLines` in `internal/lifecycle/mail.go` is untouched. That function is the seam the broker replaces later, and keeping it out of this change is what makes the two features independent.

**Also:** the region mechanism deviates from the issue's `{region}` host template. Each region option names the host it resolves to instead, because Mailgun's EU host is `smtp.eu.mailgun.org` — a prefix, not a region code — so one substitution rule cannot serve both providers.

**Affected docs:** `SERVICE_PROVISIONING.md` # BYO outgoing mail, `SETTINGS.md`, `NEXT.md` # On-box credential broker.

---

## 2026-08-26 — The brain's state lives under `state/`, and the specs follow the code

**Previously:** `STORAGE.md` # mount layout put the brain's database at `/var/lib/malmo/brain/state.db`. It put app instances at `/var/lib/malmo/instances/<id>/`, and managed-service data under `/var/lib/malmo/managed-services/`. `THREAT_MODEL.md`, `USERS_AND_GROUPS.md` and `LOCAL_ANALYTICS.md` copied those paths. `LOCAL_ANALYTICS.md` had a third version of its own, `/var/lib/malmo-state/brain.db`. `TELEMETRY.md` and `BOOT.md` wrote into a `/var/lib/malmo-state/` root.

**Now:** the paths in the code stand, and the docs move to match them. The brain writes everything under one folder: `/var/lib/malmo/state/malmo.db` with its `-wal` and `-shm` files, `state/instances/<id>/`, and `state/services/<kind>-<version>/`. There is no `managed-services/` folder and no `/var/lib/malmo-state/` root. Neither was ever created. **No code change and no migration.**

**Why:** `/var/lib/malmo/` holds files from two owners. `control-plane/` and `seed.json` are host-agent's. Everything under `state/` is the brain's. So `state/` is the line between them.

Drop that folder and we have to do one of two bad things. Either point `MALMO_STATE_DIR` at `/var/lib/malmo`, which puts host-agent's files inside the brain's folder. Or teach the brain about two roots.

One folder also keeps the rest cheap. The dev loop is just `.dev/state/`. The brain container takes one mount. And `internal/hostagent/cpupdate` copies one folder to snapshot the brain around an update.

The flat spelling in the specs reads a little better. That is the only point on the other side, and it is not worth a migration on boxes that already run. The specs were the stale side, so the docs follow the code (`CLAUDE.md` # Documentation discipline).

One cost was not cosmetic. `USERS_AND_GROUPS.md` gives a recovery step for a corrupt database, and it named a path the brain never reads. Anyone following it would restore nothing and see no error. That step now names the right path, and says to bring the `-wal` file too. The brain uses WAL mode, so restoring the main file alone gives a working database that is missing its newest writes.

**Affected docs:** `STORAGE.md` (# mount layout, rewritten to the real tree plus the two silent failures), `THREAT_MODEL.md` (asset table), `USERS_AND_GROUPS.md` (recovery step, plus WAL), `LOCAL_ANALYTICS.md`, `TELEMETRY.md`, `BOOT.md` (note: the first-run marker is `box_meta.first_run_complete`, not a `.bootstrapped` file), `APP_LIFECYCLE.md`, `APP_MANIFEST.md`, `APP_ISOLATION.md`, `FILES.md`, `UPDATES.md`, `NEXT.md`, `CLAUDE.md`. Older entries in this log keep the paths they were written with.

## 2026-08-17 — The box keeps no catalog on disk; only icons and screenshots are cached, for 24 hours

**Previously:** the box was a thin client **with a last-good on-disk cache** (`DECISIONS.md` 2026-07-02). It wrote every verified snapshot to `/var/lib/malmo/catalog-cache/catalog.json` and read it back at boot, so a box that had synced once browsed its last-good catalog forever, online or not. Proxied icons and screenshots were cached next to it with no expiry at all.

**Now:** the brain holds the snapshot **in memory only**. Nothing writes it to disk, nothing reads it back at boot. A box that cannot reach the catalog endpoint shows an **empty store** until a sync lands. Icons and screenshots are still proxied and cached on disk, now with a **24-hour expiry**; an expired asset whose refetch fails is served stale rather than broken. The dev and test lanes that used to pre-seed the cache file now pass a snapshot explicitly as `MALMO_CATALOG_FILE` — an input the brain reads once and never writes.

**Why:**
- **Browsing offline was never installing offline.** The 2026-07-02 entry already said it: "installing an app needs internet to pull images regardless." So the cache bought a catalog you could read but not act on. That is a thin slice of value for a permanent copy of the catalog on every box.
- **A pinned copy goes wrong in a way an empty store does not.** The catalog is a separate distribution with its own release cadence. A box holding an old snapshot can offer a version of a manifest the store no longer publishes, and install it. Empty is a state the user can understand and the box recovers from on the next sync; silently stale is neither.
- **It makes the failure visible.** A box that rejects a snapshot on a digest mismatch used to freeze on its cache and keep looking healthy (`APP_STORE.md` # What the box models). With no cache, a restart shows an empty store — worse cosmetics, better signal.
- **Assets are a different question, so they got a different answer.** Icons are page furniture for a snapshot the box already holds, and re-fetching them per request would be wasteful. But their filenames are stable per app (`icon.png`), so an unexpiring cache meant the first icon a box ever fetched was the icon it served forever, and republished artwork never reached the fleet. A day is short enough that a fixed icon lands on its own and long enough that browsing is not a stream of refetches. A per-asset digest on the wire would be better and is a two-repo change; the TTL is the cheap correct-enough form.

**Affected docs:** `docs/specs/APP_STORE.md` (superseded banner, Failure modes, What the box models, Landing page, Locked decisions); `docs/specs/NEXT.md` (publish-mechanism language); `docs/architecture.md` (catalog package row + app-store note); `CLAUDE.md` + `docs/dev/contributing.md` (where catalog artifacts live, synthetic fixtures); `docs/dev/running-locally.md` (env-var list); `docs/dev/authoring-apps-with-an-agent.md`. Progress: `catalog-no-box-side-copy.md`.

---

## 2026-08-11 — Hosted updates are cloud-decided per box and pushed, not manifest-driven and prompted

**Previously:** `UPDATES.md` and `RELEASE_MANIFEST.md` described one update model for every box: a signed static `stable.json` on a CDN, polled hourly, verified with minisign, surfacing an "Update available" prompt that an admin clicks. That model was written before the `hosted` profile existed and was silently assumed to cover it.

**Now:** hosted and appliance share the **apply and rollback transaction** and differ only in the **trigger**.

- **Appliance** keeps `RELEASE_MANIFEST.md` exactly as specced — signed manifest, hourly poll, admin-prompted, `rollback_to` as the kill switch. That doc is now explicitly scoped to appliance.
- **Hosted** gets a **per-box target version held by the cloud control plane**. No manifest, no signature, no prompt. The box polls outbound, applies in the window, and reports the outcome back.
- **`host-agent` recreates both `malmo-brain` and `malmo-ui`** as one transaction, writing the new image references into the staged control-plane compose before recreating so the brain reconciles to them instead of reverting them.

**Why:**

- **A signed broadcast exists to reach machines you cannot address.** That is the appliance's situation and the manifest is the right answer to it. On hosted we provisioned the box, we hold its `box_id`, and we have a channel to it — so paying for a CDN, a signing key, an offline signing ritual, and a `rollback_to` retraction protocol buys nothing we do not already have.
- **Per-box beats a fleet-wide value, and both beat the manifest.** `RELEASE_MANIFEST.md` # Future work defers phased rollout behind a deterministic cohort hash *because a static file cannot address one box*. A per-box target has staged rollout, single-tenant pinning, and instant halt as properties rather than as features to build. It also narrowed the existing cohort/beta entry in `NEXT.md` down to appliance-only.
- **We operate the box, so prompting is the wrong default.** A hosted box left on a known-bad version because nobody logged in to click is a failure we would be choosing. Pushing is what every hosted product does, and it is the honest posture for a machine whose uptime is our responsibility.
- **But operating a box does not transfer the user's data decisions to us.** The app permission-expansion prompt (`UPDATES.md` # 4) survives unchanged on hosted and still goes to the instance owner. We push infrastructure; we do not push a new grant of access to someone's folders. This asymmetry is the point, not an oversight.
- **One actor for the control-plane transaction.** The brain cannot recreate itself, and splitting the work (host-agent does the brain, brain does the UI) would give a coordinated brain+UI ship two rollback paths that have to agree mid-flight. The cost — host-agent reaching past the brain to containers `CONTROL_PLANE.md` says the brain owns — is paid by making the staged compose file the handoff point rather than by having the two actors negotiate.

**What this doesn't change:** both update streams, the pre-update SQLite snapshot, the per-app `data_volumes` tar, health-check-then-revert, 7-day retention, the 03:00–04:00 window, and the ordering. The expensive half of the machinery is shared and gets built once.

**Known gap this opens:** the hosted trigger has no designed credential. The `enrollment` block in `seed.json` is an acme-dns account, not a control-plane API identity. Filed as a **Tier 1** entry in `NEXT.md` (# Box ↔ cloud API authentication) because it blocks the first implementation slice.

**Affected docs:** `UPDATES.md` (# 8 added, # What this doc covers, # Locked decisions); `RELEASE_MANIFEST.md` (scoped to appliance — header note + locked decisions); `BUILD.md` # 6 (control-plane images published publicly, pulled by digest); `ENVIRONMENT.md` # Updates (hosted); `NEXT.md` (Tier-1 entry added, cohort/beta entry narrowed to appliance).

---

## 2026-08-11 — Two update streams (the box, the containers), not five

**Previously:** `UPDATES.md` opened with "a box has **five independent update streams**, each with its own cadence, risk profile, and delivery mechanism" — Debian base, `host-agent`, control plane (brain + UI), apps, managed services. Each got its own policy section, its own row in the rollback table, and its own place in a four-arrow ordering chain. `SETTINGS.md` and `LOCAL_ANALYTICS.md` both built on the number five.

**Now:** **two streams, split by what the thing runs on.** Stream A is **the box** — Debian base, kernel, firmware, `host-agent` — one atomic unit that may reboot, realized by `apt` today and by an A/B image later. Stream B is **the containers** — brain, UI, apps, managed services — per-image, no reboot, rollback by keeping the previous image. The per-component policies inside each stream are unchanged; what changed is the unit of testing and the unit of rollback.

**Why:**

- **Combinatorics.** Five independent streams means a box in the field is a point in a five-dimensional version space, and we can only ever test the handful of combinations we thought to enumerate. Two streams means a box is described by two numbers, and "what is this box running" has an answer that fits in a sentence.
- **The rollback table was already telling us.** Every stream-B rollback is automatic and mechanical (keep the previous image); neither stream-A rollback is (Debian base has none in v1, `host-agent` is a manual apt revert). That is not five things with five policies, it is two things with two very different physics. The old table's five rows obscured a split that the two-row grouping makes obvious.
- **The comparable products converged here.** umbrelOS ships whole-system A/B images (Rugix + Mender) with apps updating separately from a git-backed store. ZimaOS ships A/B slots via RAUC. Both landed on "the box is one atomic thing, the apps are not." CasaOS is the counter-example — a shell script pulling per-component binaries (Gateway, MessageBus, UserService, LocalStorage, AppManagement, CLI) independently from GitHub releases — and per-component drift is a recurring source of broken installs there. We are not copying the image-based mechanism (our stream B is containers, which are already atomic and already versioned); we are copying the boundary.
- **It resolves a live contradiction.** `BRAIN_HOST_PROTOCOL.md` # Versioning asserted "the OS update is one atomic unit (per `UPDATES.md` v1 — Debian base + brain + host-agent + Caddy + Tier-2 packages)" while `UPDATES.md` asserted five independent streams. Both were half right: the box *is* atomic, the containers are *not* part of that atom. Under two streams each sentence becomes true about its own stream.

**What this doesn't change:** every locked policy inside the streams survives — `unattended-upgrades` security-only for the Debian base, apt for `host-agent`, release-manifest-driven and admin-prompted for the control plane, auto-apply-unless-permissions-expand for apps, invisible-infrastructure for managed services, the 03:00–04:00 window, the pre-update snapshot, and the 7-day retention. A/B images stay deferred to v2.

**Affected docs:** `UPDATES.md` (# What this doc covers rewritten, # Why two and not five added, section headings labelled by stream, # Update ordering, # Rollback summary, # Locked decisions); `BRAIN_HOST_PROTOCOL.md` # Versioning; `SETTINGS.md` # Locked: Updates lives in Settings, not the Store; `LOCAL_ANALYTICS.md` (`stream_update_applied`); `NEXT.md` # OS major-version upgrade commitment.

---

## 2026-07-16 — One repo version, not independent per-component SemVer

**Previously:** `BUILD.md` # Versioning locked "SemVer for `host-agent` and `brain`" — each component carrying its own independently-incrementing `vMAJOR.MINOR.PATCH`, with `malmo-ui` called out explicitly as "versioned independently of the brain" in the same doc's artifact list and locked-decisions section.

**Now:** **one repo version for the whole monorepo.** `host-agent`, `malmo-brain`, and `malmo-ui` all ship from one commit in one repo, so there is exactly one `vX.Y.Z` per release, carried in a `VERSION` file at the repo root — the single source of truth. `VERSION` holds the **last released** version and changes only in the dev->main release PR (`docs/dev/contributing.md` # Release model). Every build additionally stamps the git commit it was built from as a **separate field** — `malmo-brain --version` prints `malmo 0.4.0 (g1a2b3c)` — so a dev build between releases is visibly distinguishable from the tagged release commit without needing a `-dev` suffix or a "next target" counter.

**Why:** independent per-component counters were bookkeeping with no consumer. Nothing in malmo ever installs a brain and a UI from different releases, or a host-agent that predates the repo state it shipped from by more than the normal apt-lag window (`UPDATES.md` # 2) — every release is cut from one commit, so three separately-incrementing numbers were always going to read the same story (a bump on one commit) in three different vocabularies. Tracking one number that means "this commit" is simpler to reason about, simpler to assert in CI (a pushed tag against one file, not three), and removes a whole class of "did I bump the UI's version too" release-checklist item that had no actual failure mode behind it.

**What this doesn't change:** `RELEASE_MANIFEST.md`'s schema still carries separate `brain` and `ui` semver fields (unbuilt, out of scope for this change) — with one repo version those two fields are always equal in practice; a brief note was added there rather than redesigning the manifest.

**Affected docs:** `BUILD.md` # Versioning (rewritten), artifact list, locked decisions; `RELEASE_MANIFEST.md` (note only); `docs/dev/contributing.md` # Release model (VERSION-bump step added); `docs/progress/repo-version-and-compat-range.md`.

---

## 2026-07-16 — The image inherits the repo SemVer; CalVer for the ISO is dropped

**Previously:** `BUILD.md` # Versioning locked "CalVer (`YYYY.MM`) for the ISO itself, since it's a snapshot of host-agent + brain + Debian + apps" — reasoning that avoided "semver-stretching for a thing that isn't a single component," on the assumption that the ISO/cloud-image bundled multiple independently-versioned components that needed reconciling into a different scheme.

**Now:** the image takes the **same `vX.Y.Z`** as the rest of the repo. The premise that motivated CalVer — the image is a bundle of independently-versioned things — no longer holds once host-agent/brain/UI share one repo version (the sibling entry above): the image is a snapshot of *one* version, not several, so there is no reconciliation problem left to solve with a different versioning scheme.

**Why:** one commit shouldn't have two identities. A box builder, a support thread, or a bug report can now say "malmo 0.4.0" and mean the same thing whether they're looking at the brain's `--version` output, the UI's build, or the image filename (`malmo-v0.4.0-amd64.qcow2`) — there's no translation table between a SemVer and a CalVer for the same artifact.

**Affected docs:** `BUILD.md` # Versioning, artifact list (`malmo-vX.Y.Z-amd64.qcow2`/`.raw` — filenames were already the SemVer form and needed no change, only the prose describing the scheme), locked decisions; `docs/progress/repo-version-and-compat-range.md`.

---

## 2026-07-16 — The hosted app strip is per-cookie, not the whole `Cookie` header (#335)

**Previously:** #306 stripped the **whole `Cookie` header** from every hosted app route, on the reasoning (logged below, same-day 2026-07-08 entry) that it was "the simplest form that satisfies the 'no app upstream ever receives a cookie' invariant". That entry stated the tradeoff plainly and named its own escape hatch: an app keeping cookie-based session/CSRF state "won't see it in `restricted` *or* `public` mode", the injected `X-Malmo-User` headers are the session for owner-only apps, "and per-cookie surgical stripping is the follow-up if a real app needs its own cookies."

**Now:** the follow-up lands, on the first real app that needed it. The route strips `malmo_forward_auth` **and only it**, passing every other cookie through byte-for-byte. The invariant it holds is narrowed to the one that was always the point: no app upstream ever receives *malmo's* cookie.

**Why:** "a real app needs its own cookies" turned out to be not an edge case but the common case, and the failure is worse than the tradeoff predicted. The prediction was that an app wouldn't see its own session state; the reality is that a third-party app with a browser login is **unusable**, and unusable in a way that reads as a credential bug rather than a platform one. Nextcloud renders its login page, is handed correct credentials, and rejects them: with no `Cookie` reaching it, it mints a fresh session on every request, so the login POST arrives with no session and no CSRF binding. Verified against a stock `nextcloud:34.0.1-apache` behind a real Caddy carrying the emitted config — two sequential `GET /login` return **different** session ids through the whole-header strip and the **same** id through the per-cookie strip. The `X-Malmo-User` header is not a substitute: a third-party image has no idea what it means. This is most of a real catalog (Nextcloud, Gitea, Memos, Actual Budget, Calibre-web), so the strip's simplicity was buying nothing and costing the app surface.

**What the narrowing costs, accepted:** once apps receive cookies again, an app can set a `Domain=<box-id>.malmo.network` cookie that a sibling app on another subdomain receives (cookie tossing / session fixation between apps). That is inherent to hosting apps as subdomains of one registrable domain, and the whole-header strip was never a deliberate defence against it — only an accidental one, and not a usable one, since it left no app working at all. `__Host-` prefixed cookies are immune; that cannot be forced on third-party images.

**The correctness now lives in a regex, so the test shape had to change.** A `StripCookie bool` on the route config could be asserted true and prove nothing about what the app receives. The strip is `(^|;\s*)<name>=[^;]*` plus a separator cleanup, and the anchor is load-bearing: the obvious unanchored form silently mangles any cookie whose name merely *contains* the forward-auth name (`evil_malmo_forward_auth` arrives as `evil_`). `internal/caddy` now asserts behaviourally against the **emitted** config over an adversarial table (duplicate names, prefixed/suffixed names, token-only, empty value), and `RouteConfig` carries `StripCookieName string` sourced from `auth.ForwardAuthCookieName` so a rename cannot silently stop the strip.

**Affected docs:** `ENVIRONMENT.md` # Public-by-default (the forward-auth cookie bullet + the #306 enforcement paragraph). Progress: `docs/progress/hosted-cookie-strip-per-cookie.md`. Code: `internal/caddy` (`RouteConfig.StripCookieName`, `stripCookieReplacements`), `internal/lifecycle` (`buildRouteConfig`). Narrows the first shape call of the 2026-07-08 (#306) entry below; the second (flip the default to `restricted` now) is untouched.

---

## 2026-07-16 — The catalog-promised digest is the address a Door-1 install pulls, not a promise to check afterwards (#331)

**Previously:** an online box pulled the **tag**, read the resulting registry `RepoDigest`, and compared it to the catalog's promise, refusing the install on any difference (`APP_LIFECYCLE.md` # Locked: image digest pinning; restated as recently as 2026-06-16 (#167) below — "a box *with* a registry is unchanged: it pulls and verifies as before"). `APP_STORE.md` # Trust model had meanwhile always said the opposite — the box "pulls by digest", and an upstream tag move "doesn't affect it". The two specs contradicted each other and the code implemented the `APP_LIFECYCLE` side.

**Now:** the promise is the **address**. A Door-1 install pulls `name@sha256:…` and never consults the tag. The comparison is **deleted rather than moved**: a content-addressed pull cannot return other bytes, so there is nothing left to verify. Door-2 / TOFU still pulls the tag and trusts what it resolves to (no promise exists to pull by). The offline path is behaviorally unchanged, and the online path now *agrees* with it rather than diverging. One narrow failure replaces the old one: a compose pinned `name@sha256:…` that disagrees with the `images`-map promise for the same ref — the catalog contradicting *itself*, a curation bug with no safe pick.

**Why:** a real install failed on `nextcloud:34.0.1-apache` when Docker Hub rebuilt the tag to absorb base-image package updates — same Nextcloud version, same Dockerfile steps, same Debian base layer, 21 of 22 layers new. Nothing was wrong with the catalog or the registry, and the install was refused anyway. This is **routine** for library images, which rebuild a version tag whenever their base is patched, so a version tag is mutable in practice and not just in theory. Pulling it made the bytes a box runs depend on *when* it installed (two boxes, same catalog version, different images — a "works on my box" support problem that is real and undiagnosable), and made every upstream rebuild a hard failure. Pulling the promised digest makes a rebuild a non-event and makes installs of a catalog version byte-identical across boxes.

The deeper point: verifying a promise *after* pulling a tag was never the trust binding it looked like. It could only ever tell us the tag had moved — it could not make the bytes right, and refusing the install was the only thing it could do about it. Deciding whether upstream's new bytes should be promised is **curation's** call, made where the catalog is built, never the box's and never implicitly by the clock. This change also removes the last way unreviewed upstream bytes could reach a box, since a tag pull is what a Door-1 box no longer does.

**Trade accepted:** nothing upstream now reaches a box unless a promise is deliberately updated, so pins are only as fresh as curation makes them, and the registry keeping untagged manifests addressable becomes load-bearing (`APP_STORE.md` # Failure modes already accepts this: a *deleted* digest fails the install with a registry-side error). Both were already true of the offline path and of any un-bumped pin; this makes them true uniformly and on purpose.

**Affected docs:** `APP_LIFECYCLE.md` (# Locked: image digest pinning — rewritten; the offline paragraph's stale "pulls and verifies against it" phrasing corrected), `docs/progress/door1-pull-promised-digest.md`. Supersedes the final clause of 2026-06-16 (#167) below; `APP_STORE.md` needed no change — it described this all along.

## 2026-07-08 — Hosted apps default to owner-only, and every hosted app route strips the whole `Cookie` header (#306)

**Previously:** the hosted profile was "public-by-default, auth-gated" with per-app "public vs require a malmo session" controls **deferred** (`ENVIRONMENT.md` # Deferred), and the eventual flip of the default to owner-only was to be **gated on the mechanism being proven end-to-end** (the #308 e2e), per the #305 progress note. The cookie strip in front of a restricted app was specified narrowly as "so the app never receives the forward-auth cookie."

**Now:** #306 lands the box-side mechanism (per-instance exposure state, the central `forward_auth` + Cookie-strip route builder, the toggle endpoint) **and flips the hosted default to `restricted` in the same change**, rather than waiting for #308. Two shape calls that diverge from the issue's literal text:

- **Whole-`Cookie`-header strip on *every* hosted app route** — both `restricted` and `public`, not just restricted. The forward-auth cookie is `Domain=<box-id>.malmo.network`, so the browser sends it to *every* `<slug>.<box-id>` subdomain, including public apps; stripping only restricted routes would still leak it to a public app that could replay it against the owner's restricted apps. Stripping the whole header (not just the one cookie) is the simplest form that satisfies the "no app upstream ever receives a cookie" invariant the issue asks to make load-bearing.
- **Flip the default now, don't gate on #308.** The mechanism (#305 verify + #306 route) is complete and unit-tested (route shape, strip invariant, appliance-unchanged); #308 becomes the e2e *proof* of an already-shipped default rather than the gate that unlocks it.

**Why:** a per-app gate that leaks its own bearer token to a sibling app is not a gate; making the strip universal closes that at the one central builder the spec already calls the safety boundary. Flipping now keeps the "secure by default" posture (a new hosted app is owner-only until the owner opts it public) instead of shipping the machinery dormant. **Tradeoff, stated plainly:** the whole-header strip means a hosted app never receives *any* browser cookie, so an app that keeps its own cookie-based session/CSRF state won't see it in `restricted` *or* `public` mode; the box identity (injected `X-Malmo-User` headers) is the session for owner-only apps, and per-cookie surgical stripping is the follow-up if a real app needs its own cookies. Appliance is untouched — no exposure concept, no `forward_auth`, byte-for-byte the same plain reverse_proxy.

**Affected docs:** `ENVIRONMENT.md` # Public-by-default (Per-app owner-only access rewritten; # Deferred bullet narrowed to multi-user). Progress: `hosted-forward-auth-route.md`. Code: `internal/store` (`exposure` column), `internal/caddy` (`RouteConfig`/`ForwardAuthConfig` + `AddRoute` rebuild), `internal/lifecycle` (`buildRouteConfig`/`defaultExposure`/`SetExposure`), `internal/api` (`PUT /apps/{id}/exposure`).

---

## 2026-07-04 — The wildcard cert must be named in `certificates.automate`; the apex is served by Caddy's default issuer, not acme-dns

**Previously:** the 2026-07-03 entry below framed the hosted TLS failure as a concurrency bug — two ACME orders (wildcard + apex) racing on the shared `_acme-challenge.<box-id>` acme-dns record — and fixed it by serializing them (`EnsureWildcardTLS` issues the wildcard, waits via a `:443` probe, then adds the apex as a second policy). Both subjects went through acme-dns DNS-01.

**Now:** that was a misdiagnosis. On a real box (`asd-elm`, instrumented via rescue-mode disk logs) the wildcard order was **never placed at all** — zero `*.<box-id>` obtain attempts — so there was no second order to race. A Caddy `automation.policies[]` entry only defines *how* to manage a matching name (the issuer); it does **not** schedule issuance. Caddy obtains a cert only for a name in `apps/tls/certificates/automate` or named by an HTTP route's Host matcher. The wildcard was in neither, so it just sat idle while each installed app's exact host (`<slug>.<box-id>.malmo.network`) forced Caddy to try a per-app cert whose DNS-01 challenge lands at the **undelegated** `_acme-challenge.<slug>.<box-id>` name — failing forever (`could not determine zone` / propagation timeout). The apex succeeded only because it is a real reachable host and Caddy's **default** issuer got it over tls-alpn-01. Fix: `EnsureWildcardTLS` names the wildcard in `certificates.automate` (so Caddy proactively obtains it via the acme-dns policy at the delegated apex `_acme-challenge.<box-id>`); the apex keeps using the default issuer, so **only the wildcard ever touches acme-dns** and there is exactly one order — no race to serialize.

**Why:**
- Ground truth beat reasoning: the base-works-wildcard-fails symptom looked identical to an eviction race, but the box logs showed the wildcard order never existed. The serialize fix (and its `waitForCert`/`dialCertReady`/`certReady` machinery) was solving a problem that never occurred — `waitForCert` polled for a cert Caddy was never told to obtain, so it timed out on every boot by construction.
- With the apex on the default issuer (tls-alpn-01/http-01, no acme-dns write), the shared-record contention the 2026-07-03 entry worried about cannot arise — so the serialization is not just unnecessary, it is unreachable. Removed `addBaseSubjectAfterWildcard`, `waitForCert`, `dialCertReady`, `tlsAddr`, and the `certReady` seam.
- The renewal-race open question from the prior entry is likewise moot: only one name (the wildcard) renews through acme-dns.

**Affected docs:** `internal/caddy/caddy.go` (`EnsureWildcardTLS` rewritten; phase-2 machinery deleted), `internal/profile/appurl.go` (`CertSubjects` doc comment), `cmd/brain/main.go` (call-site comment). Progress: `hosted-wildcard-cert-automate.md`. Supersedes the 2026-07-03 entry below.

---

## 2026-07-03 — Wildcard + apex certs are issued one at a time, not from one automation policy

**Previously:** `caddy.EnsureWildcardTLS` put a single Caddy TLS automation policy whose `subjects` array listed both of the box's certs ("<box-id>.malmo.network" and "*.<box-id>.malmo.network"), read (including in a prior progress entry) as "a single challenge issues the combined cert."

**Now:** there is no combined cert. Caddy/certmagic issues one certificate per SAN, so listing both subjects in one policy silently started two concurrent ACME orders. Both solve DNS-01 at the same name (RFC 8555 puts the wildcard's challenge at the base label, same as the base name's own), against the same enrollment account, whose DNS-01 TXT store keeps only a small fixed number of recent values per account, so a third write from either order (a retry, a propagation recheck) can evict the sibling order's still-unvalidated value and fail it with no visible error. `EnsureWildcardTLS` now issues the wildcard subject alone, waits (bounded, via a local probe, see below) until Caddy has actually obtained it, then adds the base subject as a second policy, so the two orders are never concurrent.

**Why:**
- The obvious fix ("just don't put unrelated subjects in one policy") doesn't apply here: both subjects are for the same box's Caddy and were split intentionally (`profile.CertSubjects`'s doc explains why: a wildcard doesn't cover its own bare parent). The actual hazard is concurrency at the shared DNS-01 record, not the grouping.
- Readiness is checked by dialing the box's own already-listening `:443` with SNI covered by the wildcard and inspecting the returned certificate's SANs, rather than adding any new admin-API surface or externally exposed endpoint. The box already talks to Caddy's admin API for everything else in this package; this stays within "ask Caddy what it's doing," just over the TLS port instead of the admin port.
- The tradeoff is latency, not correctness: first-boot cert acquisition is now roughly two sequential ACME round-trips instead of two overlapping ones, so anything budgeting the box's "serving HTTPS" readiness needs to size for that (`cmd/brain`'s wait around this call got its own, longer, independent timeout rather than sharing the reconcile-and-routes budget).
- Not addressed here: whether the same two-order collision can recur at renewal (~60 days out), since Caddy's background renewal maintenance is internal to certmagic and outside this call's control once both certs are configured. Left as an open question rather than assumed-fixed.

**Affected docs:** `internal/caddy/caddy.go` (`EnsureWildcardTLS` doc comment + implementation), `internal/profile/appurl.go` (`CertSubjects` doc comment), `docs/progress/hosted-wildcard-cert.md` (corrected the "combined cert" claim). Progress: `wildcard-cert-serialize-acme-orders.md`.

---

## 2026-07-02 — The box is a control-plane catalog thin client on every profile; no baked catalog, no signing (cloud #62)

**Previously:** the box's catalog was a disk-backed reader over a baked `catalog/` directory, staged into the image on every profile; `MALMO_CATALOG_DIR` pointed the brain at it. The plan (`APP_STORE.md`) foresaw a *signed* remote catalog fetch replacing it.

**Now:** every box — appliance and hosted alike — is a thin client of the control plane's public-read catalog API. The brain fetches `GET /catalog/sync` over HTTPS, verifies the snapshot's integrity digest + schema version, caches it last-good on disk, and projects the six-method surface locally (`internal/catalog` remote source). No `catalog/` directory is baked into the image; `MALMO_CATALOG_DIR` is retired, replaced by `MALMO_CATALOG_URL` (+ `MALMO_CATALOG_CACHE_DIR`). There is **no Ed25519 signature** — TLS to the control plane plus the integrity digest are the trust story.

**Why:**
- **Unify both profiles.** An earlier cut of this work kept the appliance on the baked directory (fearing the offline appliance would regress). But the last-good cache *is* the offline story: once a box has synced it browses from cache, and a never-synced box shows an empty store — which is fine, because installing an app needs internet to pull images regardless, and the catalog API is public-read precisely so an appliance with no portal account can use it. There is no profile that has, by construction, no network path to the control plane, so a per-profile split earned nothing but a second code path and a baked artifact to keep in sync.
- **Drop signing (don't defer it).** The box only ever fetches the catalog from the malmo control plane over TLS; TLS authenticates that origin, and the integrity digest catches truncation/corruption. An Ed25519 signature would re-authenticate bytes TLS already authenticated, and carry a key-distribution contract for no threat it closes. Not needed — removed from the plan, not parked.

**Affected docs:** `docs/architecture.md` (catalog package row + the app-store deferred-list note); `docs/specs/APP_STORE.md` (superseded banners + rewritten Failure modes / What we run / Locked decisions sections); `docs/specs/NEXT.md` (publish-mechanism language); `docs/specs/APP_MANIFEST.md`, `APP_ISOLATION.md`, `APP_LIFECYCLE.md` ("signed catalog" → "published catalog"); `docs/dev/running-locally.md` (env-var list), `docs/dev/authoring-apps-with-an-agent.md` + `.github/ISSUE_TEMPLATE/catalog-app.md` (removed-`catalog/` notices). Progress: `catalog-remote-thin-client.md`.

---

## 2026-07-01 — OS dashboard adopts cloud's Oatmeal/olive design system; fonts self-hosted, not CDN (#260)

**Previously:** The dashboard used a bespoke "calm launcher" palette — near-white `#f6f6f7` canvas, a blue `#2b6cb0` accent, system-ui fonts — defined ad hoc in `web-ui/src/style.css`. Cloud, meanwhile, shipped the Tailwind Plus **Oatmeal** kit's olive palette + Inter / Instrument Serif.

**Now:** Both malmo surfaces share **one design system**. The OS dashboard's `@theme` block carries the same `--color-olive-50…950` OKLCH ramp and the same two fonts as cloud, lifted verbatim from cloud's `internal/web/tailwind/input.css`. The app's existing shadcn-vue semantic tokens (`background`, `foreground`, `accent`, …) are **repointed onto olive values** rather than rewritten at ~1,000 call sites, so the whole UI recolors from a single file. The accent is monochrome dark olive (no separate accent hue — matching cloud's `bg-olive-950` CTAs). `destructive`/`success` stay semantic red/green (olive is monochrome; cloud keeps red for errors too).

**Divergence — fonts are self-hosted, not loaded from the Google Fonts CDN** (unlike cloud and the Oatmeal README). The dashboard is served over `.local` **HTTP on a LAN that may be offline**; a CDN `<link>` yields broken/unstyled fonts on an air-gapped box. Inter + Instrument Serif ship as bundled `woff2` (`web-ui/src/assets/fonts/`) with their SIL OFL 1.1 notices included.

**Why:**
- Visual consistency between the two malmo surfaces is worth more than palette independence, and Oatmeal is a mature, coherent system we already own the license for.
- The semantic-remap strategy (vs. find-and-replace to literal `bg-olive-*`) avoids ~1,000 edits across 38 files and keeps a clean dark-mode-by-token-swap path. Class names are never literally shared with cloud anyway (OS is Vue, cloud is Go templates), so literal olive classes would buy nothing.
- Self-hosting is a hard requirement of the offline-LAN deployment model, not a preference. Licensing allows it: this copies only palette *values* and font *choices* (config, not Oatmeal component source), inside the Tailwind Plus End-Product allowance; Inter and Instrument Serif are both SIL OFL 1.1.

**Affected docs:** `WEB_UI.md` # Styling, `docs/progress/oatmeal-theme.md`, `docs/dev/web-ui.md`.

---

## 2026-06-28 — Hosted first admin comes from the portal sign-in handshake; the admin-bootstrap secret is gone (#275)

**Previously:** the two entries below (2026-06-20 and 2026-06-26) locked a one-time **admin-bootstrap secret** as the hosted gate: the seed carried `admin_bootstrap_secret`, the brain stored its SHA-256 hash, `POST /setup` took the secret in a `bootstrap_secret` body field and constant-time-compared it, and the operator got the plaintext out-of-band from the cloud console (later prefilled into `/setup` from a link fragment).

**Now:** none of that exists in the code. The seed carries `assertion_verification_key` — the portal's Ed25519 **public** key — and the hosted owner signs in through the **portal-to-box SSO handshake**: the portal mints a short-lived signed ownership assertion, the box verifies it against the seeded key at `GET /_malmo/sso`, and the **first** valid assertion auto-creates the founding PAM admin. `POST /setup` is **disabled on hosted** (403, audited), and there is no hosted setup or login page at all — an unauthenticated visitor is bounced to the portal.

**Why:** the owner already has a `malmo.network` account, so making them copy a second one-time secret into a box form was a step that bought no trust the portal session did not already carry. A signed assertion also removes the shared secret from the seed, the brain's SQLite and the operator's clipboard, and it is what the cloud half was built to send. The flip shipped with the code in #275 (`docs/progress/portal-box-sso.md`), but `ENVIRONMENT.md` and `FIRST_RUN.md` were never updated, so both pages went on describing a login path that could not happen — the same drift as #404 → #407 (#412).

**Affected docs:** `ENVIRONMENT.md` (# Provisioning & first-boot; "Admin bootstrap — as built" renamed to "Owner sign-in & seed ingestion — as built" and rewritten), `FIRST_RUN.md` (# Step 2 hosted note), `NEXT.md` (section cross-reference), `docs/dev/hosted-boot-proof.md` (seed field list). The two entries below stay as written — they record what we believed then. Progress: `docs/progress/portal-box-sso.md`, `docs/progress/hosted-seed-doc-drift.md`.

---

## 2026-06-26 — User-supplied app config injected under the app's own var name, no `MALMO_*` indirection (#264)

**Previously:** malmo had no surface for a value only the user can supply — a third-party API token, an external connection string, a provider selector. The catalog coined this the `operator-env-config` gap (`docs/dev/catalog-import-gaps.md`) and it became the single largest class of rejected/degraded apps (browser-use, hayhooks, dub, betterbox, cube, beeper-bridge-manager, valour, formbricks integrations, …): "no install-form field, no post-install editor, and SSH is rescue-only." Every other injected value rides the `MALMO_*` family (`MALMO_SERVICE_*`, `MALMO_SECRET_*`, `MALMO_FOLDER_*`, `MALMO_MAIL_*`) — a brain-owned stable name the app's compose maps to whatever it expects.

**Now:** a manifest `config:` block (`APP_MANIFEST.md` # D4) declares user-supplied fields (`app_env`, `title`, `description`, `secret?`, `required?`, `type?`, `options?`, `default?`, `service?`); the brain renders a form, collects answers at install (required fields gate the install button) and on the app detail page after, and injects each value **directly under the declared `app_env` name** into the target service's `environment:` in the compose override — **no `MALMO_*` indirection, no mapping line.** Considered and rejected: (a) keeping a `MALMO_CONFIG_*` indirection + a separate logical `name` + author mapping line, and (b) a display-only `app_env` hint over a `MALMO_CONFIG_*` injection.

**Why:** the `MALMO_*` indirection exists *because the brain owns those values* (a generated secret, a provisioned DSN, a resolved folder path) and the app must adapt to receive them — the mapping line is what keeps the app portable. A user-supplied config value is the opposite: it is the app's **own native variable** (`OPENAI_API_KEY`) that the user would set by hand running the app standalone. So the indirection buys no portability — injecting `app_env` directly is *more* portable (same name in and out of malmo) and lets the variable the user reads in the app's upstream docs be exactly the name on the form and exactly the name set in the container. Collapsing to one identifier also drops a field, the per-item mapping line, and the catalog-build lint a separate-hint design would have needed to prevent drift. The two costs are small and bounded: the brain picks the target service (default `main_service`, overridable with `service:`), and config injection is a second path (override `environment:`) distinct from the `.env` interpolation the `MALMO_*` family uses.

**Affected docs:** `APP_MANIFEST.md` (# D4 + locked decision), `SERVICE_PROVISIONING.md` (# Env-var injection), `DASHBOARD.md` (install form + app detail editor), `BRAIN_UI_PROTOCOL.md` (install-plan config schema + install payload + config endpoint). Closes the `operator-env-config` gap in `docs/dev/catalog-import-gaps.md`; the blocked/degraded apps re-screen against it.

## 2026-06-26 — Hosted `/setup` prefills the admin-bootstrap secret from a link fragment; the form validates its shape

**Previously:** The hosted first-run wizard collected the admin-bootstrap secret as a plain pasted field. The operator read the one-time secret from the cloud portal and re-typed/re-pasted it by hand into `/setup`; a mistyped character produced the gate's opaque "invalid admin bootstrap secret" (401), indistinguishable from a genuinely wrong secret.

**Now:** The control plane links to the box's `/setup` with the secret in a URL **fragment** (`<box-url>/setup#secret=<value>`). `AdminStep.vue` reads `location.hash` on mount, prefills the field, and strips the hash via `history.replaceState`. The form also shape-validates the secret (43-char base64url, `/^[A-Za-z0-9_-]{43}$/`) before calling `/setup`, so a truncated paste gets an actionable message instead of the gate's 401. The gate itself (`gateBootstrap`, constant-time SHA-256 compare) and the `bootstrap_secret` wire field are unchanged.

**Why:** A live box rejected a *correct* secret — the failure was the manual hand-off, not the gate. Removing the re-type (the prefill) eliminates the common error; the shape check makes the residual hand-paste path fail loudly and early. A fragment, not a query string, keeps the secret out of the brain's access log and the `Referer` header — it carries no new exposure since the same value is already on screen. This is a **two-side seam** co-owned with the cloud control plane: the `#secret=<value>` link format is now a contract, and the `{43}` shape mirrors the cloud's one-time-secret format (32 bytes, base64url, no padding) — a documented coupling to update on both sides together.

**Affected docs:** `ENVIRONMENT.md` # Admin bootstrap (transport + client-side check; gate unchanged). Progress: `docs/progress/setup-secret-prefill.md`. Cloud half: `malmoos/cloud` `internal/web/static/dashboard.js`.

---

## 2026-06-25 — No managed MongoDB type; Mongo apps bundle their own engine (#253)

**Previously:** MongoDB was listed as a plausible post-v1 managed Tier-1 service (`SERVICE_PROVISIONING.md`), to be built like Postgres/MySQL/Valkey on a license-clean engine — [FerretDB](https://www.ferretdb.com/) v2, the redis→valkey substitution applied to SSPL MongoDB. #253 gated that on a Phase 0 compatibility spike.

**Now:** **malmo will not offer a managed `type: mongodb` service — decided, not deferred.** No FerretDB-backed provider, no alias, and no revisit clock. Apps that need MongoDB **bundle their own engine** in their compose (the Umbrel/CasaOS pattern); curation accepts them as long as the app uses Mongo internally and does not itself serve a database to third parties (`NEXT.md` # Store catalog curation policy).

**Why:**
- **The FerretDB engine fails on two independent measured findings** (`docs/progress/mongodb-compat-spike.md`, 2026-06-25). (1) *Compatibility ceiling:* no change streams, no oplog, no replica set, no multi-doc transactions — Rocket.Chat hard-blocked, Habitica/Appsmith blocked, Wekan degraded, leaving ~1 solid + 1 degraded app, short of the 3+ promotion bar. (2) *Isolation, decisive:* FerretDB v2 enforces authentication but **no** per-database authorization (the proxy has only `--auth`, no RBAC), so a per-app credential has full read/write to every other app's database on a shared instance — it cannot meet the # Per-app isolation contract every other Tier-1 service upholds. The only workaround (one ~2.3 GB instance per app) defeats the shared-instance model that defines Tier 1.
- **Real MongoDB can't be the managed engine either.** SSPL's §13 trigger is *offering the database's functionality as a service to third parties* — exactly what a malmo-operated managed type does. That keeps the engine on the avoid-list *in the managed role* (same reasoning as redis→valkey, 2026-06-13). It does **not** restrict an app bundling Mongo for its own internal data, or a household running such an app — neither offers the database as a service.
- **No `mongodb`→FerretDB alias**, unlike `redis`→`valkey`. That substitution worked because Valkey is a true drop-in (RESP wire, ACL model); FerretDB is not — it fails on both compat and isolation. So `mongodb` is simply not a recognised type (`internal/manifest`'s unknown-type rejection of it is correct-and-intentional and now load-bearing).
- **Bundling is strictly better for Mongo apps here:** it ships *real* MongoDB, so it sidesteps both findings (Rocket.Chat & co. work), at the cost of the bundled-DB tradeoffs (no malmo-managed backups/shared instance, app-state not files-first-class). The managed model bought nothing for Mongo that bundling doesn't already deliver license-clean.
- **Caveat — bundling clears licensing + engine-compat, but not automatically the sandbox.** A bundled DB image still has to boot under the Tier-3 sandbox (`cap_drop: ALL`): the official Mongo image self-chowns its datadir and gosu-drops as root on first init, both of which the dropped caps break (the Gate-B `nonroot-data-ownership` trap, same as bundled `postgres`/`mysql`). The clean shape is to run the engine sidecar at an **elected non-root `user:` over a pre-chowned bind**, so the entrypoint skips its `chown`+`gosu` (official DB images self-chown only when started as root) — the `service_user` path. So "Mongo apps bundle their own engine" presumes that non-root sidecar form; an image that hardcodes a different internal UID or self-chowns unconditionally is still a Gate-B `blocked` pending the deferred user-namespace remap (`NEXT.md`, "User-namespace remap for hardcoded-internal-UID app images"). Curation enforces this at screening (`store:docs/admissibility-gate.md` Gate B/C, `store:docs/curation-checklist.md`).

**Affected docs:** `SERVICE_PROVISIONING.md` (# Post-v1 candidates — MongoDB struck off as declined; # Catalog plausible-additions line). `NEXT.md` (# Store catalog curation policy — bundled-database rule added; the prior "Managed MongoDB — deferred" entry removed, since this is decided not open). `docs/progress/mongodb-compat-spike.md` (the evidence; #253).

---

## 2026-06-25 — Hosted: one in-guest nftables rule blocks app-container egress to the cloud metadata endpoint (#251)

**Previously:** the 2026-06-19 / 2026-06-23 entries fixed malmo's hosted posture as "no malmo-owned firewall ruleset; L3/L4 filtering is the provider's security group," with a general in-guest default-deny backstop deferred (`NEXT.md`). That left the cloud metadata endpoint (`169.254.169.254`) reachable by any local process — including an untrusted app container, since Docker NATs container egress out the host NIC. The first-boot seed (admin-bootstrap secret + acme-dns password) stays retrievable there for the server's life: a classic cloud-metadata SSRF (cf. Capital One 2019), surfaced while implementing the real-cloud seed channel (#246).

**Now:** **hosted ships exactly one malmo-owned in-guest `nftables` rule — a standing forward-hook DROP of egress to `169.254.169.254`, applied every boot by a dedicated oneshot** (`malmo-metadata-firewall.service` → `/etc/malmo/metadata-firewall.nft`). This is **not** the deferred general backstop, which stays deferred: it is a single, static, never-reconciled SSRF control for a malmo-specific secret exposure. The 2026-06-19 substance holds — public-surface L3/L4 filtering is still the provider's security group; this blocks one link-local address from the container forward path only.

**Why:**
- The worst secret at risk is the **acme-dns password** — the credential to rewrite `_acme-challenge.<box-id>` and mint/MITM the box's wildcard cert. Provider security groups do not help: the SSRF source is a local container and the metadata IP is **link-local**, so the packet never traverses the provider's L3/L4 edge. The block has to be in-guest.
- **Forward hook, not output or a blackhole route.** The seed materializer fetches the endpoint as host root in the host netns (OUTPUT path) once at first boot; containers reach it over a Docker bridge (FORWARD path). Dropping only in `forward` blocks every container (apps *and* the brain, which reads the seed from disk) while leaving the host fetch untouched — a standing policy with no "unblock until seeded" timing or state.
- **A static image-baked oneshot, not a host-agent reconciler.** The rule is one fixed IP that never changes, so it does not need the deferred dynamic firewall reconciler; a future host-agent firewall posture can subsume it. Putting it in the seed materializer script was rejected — firewall logic is the wrong owner there and would be clobbered by that future reconciler — so it is its own unit, in its own `inet malmo_metadata` table (loaded without flushing Docker's rules).
- Blocking the metadata endpoint **wholesale after first boot** was considered and rejected as more complex for no gain: the forward-hook block is already always-on and the host (the only legitimate reader) uses the untouched OUTPUT path, so there is no window to gate.

**Affected docs:** `ENVIRONMENT.md` (# How the profile is realized — the "no malmo ruleset" claim now carries this one exception; # Provisioning & first-boot — the #251 residual risk closed + the new "Metadata-endpoint egress block" as-built bullet; # Public-by-default — the metadata block named as the first in-guest rule, general backstop still deferred). `NEXT.md` unchanged (general backstop still deferred). Realized by `docs/progress/hosted-metadata-egress-block.md` (#251).

---

## 2026-06-24 — Hosted box DNS: a static `/etc/resolv.conf`, not systemd-resolved (cloud#6)

**Previously:** unstated. The hosted image enables `systemd-networkd` + DHCP for the single NIC (#242) but nothing wired host name resolution — there was no `/etc/resolv.conf`, and `systemd-resolved` is not in the lean package set, so a provisioned box could route by IP but resolve no name. The gap was invisible until the first real-cloud cert issuance.

**Now:** **the first-boot wiring writes a static `/etc/resolv.conf` with public resolvers.** It is the box's resolver for the host and — via Docker copying the host file into bridge containers — for `malmo-caddy`, which must resolve `acme-v02.api.letsencrypt.org` + `auth.malmo.network` to obtain the `*.<box-id>` wildcard cert (#207/C3b).

**Why:**
- The cloud#6 live on-ramp boots, networks, fetches its seed (all by IP), and serves `:80`, but the wildcard cert never issued because Caddy could not resolve the ACME endpoints. Name resolution was the missing piece — surfaced only on real cloud because nothing before it needed outbound DNS.
- **Rejected: `systemd-resolved`** (the obvious way to consume the DHCP-offered DNS). It is a separate package not in the lean set, so adding it grows the 133-package manifest and trips the lean check — more machinery than a box that only ever resolves public names needs (control-plane images are baked; no registry pulls).
- A static file is acceptable precisely because the need is narrow and public. Revisit only if a hosted box ever needs split-horizon / private resolution, at which point `resolved` + DHCP DNS earns its weight.

**Affected docs:** `ENVIRONMENT.md` (# Networking & discovery — the static-resolver fact). Realized by `docs/progress/cloud-image-live-onramp-fixes.md`; the file is written in `dev/cloud/mkosi.postinst.chroot`.

---

## 2026-06-23 — `nftables` stays in the hosted image: docker-ce hard-Depends on it (#241)

**Previously:** the 2026-06-19 entry (below) and `ENVIRONMENT.md` stated hosted **ships no `nftables`** — the package was on the lean-image cut list, on the reasoning that its only job is LAN-scoping SSH/SMB (both dropped in hosted). The lean check (`dev/cloud/bootstrap.sh`) asserted its absence. #237 separately fixed `nftables` arriving as `iptables`' *recommend* (`WithRecommends=no`), which kept that premise true.

**Now:** **`nftables` is accepted permanently in the hosted image — it is a hard dependency of `docker-ce`, not droppable.** `docker-ce` `5:29.6.0` reads `Depends: ... iptables, nftables` (two separate hard deps); Docker 28 moved its default firewall backend to nftables and 29.x made the package mandatory. Recommends-off can't exclude a hard dep, and the hosted image *must* run docker (the four control-plane containers). `nftables` is dropped from the lean-check cut list for good; the temporary unblock from #243 is replaced with a permanent rationale.

**Why:**
- The 2026-06-19 decision's *substance* is unchanged: malmo still manages **no firewall ruleset of its own** in hosted, and L3/L4 filtering is still the provider's security group. Only the factual premise "no `nftables` package present" flipped — the package now rides in as docker's backend, not as appliance LAN machinery.
- Pinning `docker-ce < 28` to keep `nftables` a recommend would ship an EOL'd, security-frozen Docker in every tenant image. Rejected.
- The image is unchanged and boots; `nftables` present is correct (docker's firewall backend). The deferred in-guest default-deny backstop (`NEXT.md`) is now a ruleset + host-agent seam only — the package is already there.

**Affected docs:** `ENVIRONMENT.md` (# How the profile is realized, # Public-by-default — `nftables` reframed from "cut" to "present as docker's backend, no malmo ruleset"), `NEXT.md` (backstop item: package already present). Realized by `docs/progress/cloud-image-nftables-hard-dep.md` (#241); the lean check's cut list in `dev/cloud/bootstrap.sh` no longer lists `nftables`. See #237 / `docs/progress/cloud-image-recommends-pin.md` for the recommends-path history this supersedes for `nftables`.

## 2026-06-21 — Hosted box runs a custom Caddy build (acme-dns module); appliance keeps stock Caddy (#207)

**Previously:** the control-plane stack ran stock `caddy:2-alpine` for both profiles. C3b's plan assumed acme-dns being "native to Caddy" meant no custom build was needed.

**Now:** **the hosted profile runs a custom Caddy built with `caddy-dns/acmedns` compiled in (xcaddy); appliance keeps stock `caddy:2-alpine`.** The acme-dns DNS provider is a community module that still must be *built into* the binary — stock Caddy ships no DNS-provider modules, so DNS-01 (the only way to issue a wildcard) cannot work on it. `dev/control-plane/caddy-acmedns/Dockerfile` is the recipe; `compose.yml` selects the image via `${MALMO_CADDY_IMAGE:-caddy:2-alpine}`, and only the hosted image sets the var.

**Why:**
- A wildcard `*.<box-id>.malmo.network` can only be issued via ACME DNS-01, which needs a DNS-provider plugin in the Caddy binary. Stock Caddy has none. There is no config-only path.
- Putting the custom image on **both** profiles was rejected: appliance does no ACME today (plain-HTTP `.local`; its secure-URLs toggle is deferred), so it would carry an unused module, drop off upstream's official image (security updates), and grow the offline bundle for nothing.
- A single env var (`MALMO_CADDY_IMAGE`) selecting the image keeps one build recipe and one compose, so when the appliance toggle ships, flipping appliance onto the same image is a one-line change — the "build hosted-only, structure for reuse" shape.

**Affected docs:** `ENVIRONMENT.md` (# Networking & discovery — as built). Realized by `docs/progress/hosted-wildcard-cert.md` (#207/C3b). The cloud side runs joohoi/acme-dns and ships the per-box account in the seed (`malmoos/cloud` CL4).

## 2026-06-21 — The box→acme-dns API endpoint is a box-side constant, not seeded (#207)

**Previously:** undefined. The seed carries the per-box acme-dns *credentials* (`{subdomain, username, password}`); how the box reaches acme-dns to push TXT updates was unspecified on the OS side.

**Now:** **the acme-dns API endpoint is a box-side constant — `MALMO_ACMEDNS_ENDPOINT`, default `https://auth.malmo.network` — not part of the seed.** It is the same for every box, so seeding it per-box would be redundant; only the credentials, which differ per box, are seeded. The brain hands the constant + the seeded creds to Caddy's `acmedns` provider.

**Why:**
- The credential is per-box (each box's acme-dns account can set only its own `_acme-challenge` TXT); the endpoint is shared infrastructure. Seeding only what varies keeps the seed minimal and the wire contract small (cloud `specs/ARCHITECTURE.md` Contract 2 names the endpoint a box-side constant explicitly).
- Overridable via env so a box can be pointed at a staging or self-hosted acme-dns without re-seeding.
- **Cross-repo gap closed (2026-06-23, `malmoos/cloud` #14):** the cloud control-plane VM now fronts acme-dns with Caddy, exposing `/update` + `/health` over real Let's Encrypt TLS for `auth.malmo.network` (`/register` stays loopback-only, 404 on the public face) and answering the authoritative `:53` face publicly. `https://auth.malmo.network` is a **confirmed live endpoint** — the box-side default holds unchanged. Real end-to-end issuance against it is verified jointly at cloud #6 / CL6.

**Affected docs:** `ENVIRONMENT.md` (# Networking & discovery — as built), `NEXT.md`. Realized by `docs/progress/hosted-wildcard-cert.md` (#207/C3b).

## 2026-06-20 — Admin-bootstrap secret is handed off out-of-band, not served by the brain (#206)

**Previously:** C3a (#206) left the hand-off open ("coordinate with C4"). The design review raised the obvious candidate — the brain serves the one-time bootstrap secret to the browser at first boot (à la a provider's one-time root-password page), which would necessarily be an unauthenticated, pre-admin endpoint with an expiry.

**Now:** **the brain never serves the bootstrap secret over any endpoint.** It only ever *consumes* it: `POST /setup` takes the secret in a `bootstrap_secret` body field and constant-time-compares its hash against the seeded one. The secret reaches the operator **out-of-band** — the cloud control plane surfaces it once (the cloud console, the way a VPS hands over an initial root password) at provision time. This is the contract C4's trimmed setup wizard consumes: the wizard adds a `bootstrap_secret` field to the first-admin step and submits it — **it does not invent its own one-time-bootstrap URL**, and the brain grows no secret-serving endpoint.

**Why:**
- A brain-served secret endpoint would have to be unauthenticated and reachable before any admin exists — exactly the public surface a hosted box is trying to close. It would also duplicate, in-guest, a one-time-display the control plane already owns at provision time (cloud-side CL5), splitting the secret's custody across two places.
- The seed already delivers the secret into the box; the operator already gets it from the place that provisioned the box. Routing it back out through the brain to the same operator adds attack surface for no gain.
- Splitting custody cleanly — control plane surfaces the plaintext once (with the expiry), the brain stores only the hash and never re-emits it — keeps the plaintext's lifetime owned by exactly one side.
- The headless/QEMU path consumes the same contract: the test harness reads the secret from the injected seed and POSTs it to `/setup`; no special endpoint.

**Affected docs:** `ENVIRONMENT.md` (# Provisioning & first-boot, "Admin bootstrap — as built" → "Operator hand-off"), `FIRST_RUN.md` (# Step 2, hosted marker). Realized by `docs/progress/hosted-setup-gate.md` (#206/C3a). The cloud-side one-time surfacing with expiry is `malmoos/cloud` CL5.

## 2026-06-19 — Hosted cloud image: firewall is the cloud provider's security groups, not shipped `nftables` (#203)

**Previously:** the appliance ships `nftables` whose sole job is LAN-scoping SSH/SMB (`BUILD.md` # SSH — RFC1918 + mesh only). The hosted cut list drops `nftables` (`ENVIRONMENT.md` # How the profile is realized), which left an implicit gap: with no host firewall, the Docker daemon publishes container ports to `0.0.0.0` by default, so on a public-by-default cloud VM an app's published port could be reachable with nothing in front of it. #203's review flagged that this must be a stated decision, not an omission.

**Now:** **hosted v1 relies solely on the cloud provider's security groups / VPC firewall for L3/L4 network filtering — it ships no host `nftables`.** This is recorded as an **explicit operator requirement**: a hosted tenant VM must be provisioned behind a provider firewall that admits only the intended public surface (443, and 80 for the ACME/redirect path), and the control plane's provisioning is responsible for that posture. malmo's own gate at the edge stays **authentication**, per the public-by-default/auth-gated position (`ENVIRONMENT.md` # Public-by-default, auth-gated) — the security group is the network-reachability boundary, app/dashboard login is the access boundary.

**Why:**
- `nftables` on the appliance exists *only* to LAN-scope SSH/SMB. Hosted ships neither SSH (off in v1 — no LAN to scope to) nor SMB, so the rule set it would carry has no subjects. Re-deriving a public-VM ruleset would be net-new security machinery, not a port of the appliance's.
- A cloud VM already sits behind a provider security-group layer that is the idiomatic, operator-controllable place for L3/L4 filtering — duplicating it in-guest with a second, drift-prone ruleset buys little and can mask a misconfigured security group. `ENVIRONMENT.md` already frames public exposure as "the cloud provider's security-group concern."
- Stating it as an operator requirement keeps the gap from being silent: the Docker-publishes-to-0.0.0.0 behavior is acceptable *only* because the provider firewall is assumed in front. The alternative — shipping a minimal in-guest `nftables` default-deny — is **not** rejected forever; it is deferred as a belt-and-suspenders hardening item (`NEXT.md`) if a provider-firewall assumption ever proves too weak (e.g. a target host with no security-group layer).

**Affected docs:** `ENVIRONMENT.md` (# Public-by-default — the security-group requirement made explicit), `NEXT.md` (deferred in-guest-`nftables` hardening item). Realized by `docs/progress/hosted-cloud-image.md` (#203/C1b); the lean image's omission of `nftables`/`network-manager`/etc. is asserted by `dev/cloud/bootstrap.sh`.

## 2026-06-15 — Managed-service provisioning runs in a one-shot client container, not `docker exec` (#185)

**Previously:** the brain provisioned per-app databases/roles by `docker exec`'ing the service's own client inside the shared service container (`DockerDriver.Exec`, 2026-06-05). The 2026-06-14 call then shipped the socket-proxy with the `EXEC` family **denied**, which breaks that path the instant the containerized brain points `DOCKER_HOST` at the proxy — so managed-DB-in-production was gated on re-architecting provisioning off `docker exec` (two candidate shapes named: engine-over-TCP from the brain, or a one-shot provisioning container).

**Now:** provisioning runs the service's own client (`psql` / `mysql` / `mariadb` / `valkey-cli`) in a **throwaway one-shot container** — `docker run --rm --network malmo-svc-<k>-<v> --env-file <serviceDir>/.env <serviceImage> <client …>` (`DockerDriver.RunOneOff`). The ephemeral container — not the brain — joins the service's internal network and connects over TCP to the `<kind>-<version>.malmo.internal` alias. The service superuser password rides `--env-file` (the same `.env` the service compose uses) and a wrapper `sh -c` remaps it to the client's env var (`PGPASSWORD` / `MYSQL_PWD` / `REDISCLI_AUTH`), so it never reaches host argv; the per-app credential rides argv as before. Readiness moved from an exec'd probe to polling the service's compose-declared healthcheck via `docker inspect` (`ContainerHealth`, a CONTAINERS read the proxy allows). The `EXEC` family stays **denied** — this resolves the 2026-06-14 gate without widening the allowlist. Dev (native raw socket) is unchanged.

**Why:** of the two candidate shapes, the one-shot container preserves *both* reasons the 2026-06-05 decision gave against a Go SQL client — (a) no new Go driver dependency (reuse the engine's own image client) and (b) the brain stays off the app-reachable `malmo-svc-*` network (only the `--rm` container joins it) — while the engine-over-TCP shape would have violated (b). It is also minimal-change: the SQL/ACL command shapes and quoting are identical to the exec path; only the Docker transport differs. Empirically de-risked end-to-end through the M0 proxy allowlist (a foreground `docker run --rm` with output capture survives — `/containers/{id}/attach` is gated by `CONTAINERS=1`, not the denied `EXEC`), and the `dockerlive` suite provisions/connects/drops for Postgres, MySQL, and Valkey against real Docker.

**Affected docs:** `SERVICE_PROVISIONING.md` (# Implementation status, # Locked: provisioning), `CONTROL_PLANE.md` (# Locked: Docker socket exposure — managed-DB gate lifted). Implementation: `internal/lifecycle` (`docker.go` `RunOneOff`/`ContainerHealth` replacing `Exec`; `services.go` `runServiceClient`/`clientWrapper`, health-poll readiness), realized by `docs/progress/managed-db-one-shot-provisioning.md`.

## 2026-06-16 — Hosted environment profile: one OS, lean cloud image, not a fork

**Previously:** the spec set described exactly one environment — a BYO x86 box on the user's LAN. A hosted/cloud offering was acknowledged only as a future "cloud VM image" target in `BUILD.md`, with no design.

**Now:** malmo runs in two **environment profiles** — `appliance` (today's default) and `hosted` (a malmo-operated cloud VM, one per tenant, targeting SMBs running OSS apps publicly). The split is structured, not a fork:
- **Layer 1 (the control plane — brain, UI, Caddy, socket-proxy, protocol, catalog, manifests, lifecycle, auth) is identical across profiles.** It is the product and the migration-portability guarantee.
- **Layer 2 (base image + `host-agent`) diverges** via a lean `mkosi` cloud image profile (no Avahi/Samba/NetworkManager/cryptsetup-TPM/mergerfs) plus a build-tagged slim cloud `host-agent`. Same repo, same Debian/systemd/Docker substrate, same builder.
- **Identity stays PAM-sourced** in hosted even though SSH/Samba are gone, to keep Layer 1 and the migration bundle identical.
- **Hosted v1 networking** is a plain public endpoint, `<slug>.<box-id>.malmo.network` HTTPS, always-on (no `.local`, no toggle, no mesh).
- **Hosted inverts "closed by default"** to public-by-default / auth-gated, and adopts an **honest convenience tier** trust posture: malmo-operated infra is inside the trust boundary; at-rest encryption defends co-tenant/image-theft, not the operator. Not operator-blind.

**Why:** a hosted on-ramp needs a different runtime environment, but the brain is ~95% of the value and the whole migration story depends on it being the same code. Concentrating divergence in Layer 2 — already the small, swappable layer by design — gets the hosted product without forking the control plane. A different base OS and a one-rootfs-disable-at-runtime approach were both considered and rejected (`ENVIRONMENT.md` # Rejected sections). Commercial concerns (metering, billing, fleet ops, the mesh, a central ingress) are explicitly deferred.

**Affected docs:** new `ENVIRONMENT.md` (anchor); pointer sections added to `FIRST_RUN.md`, `STORAGE.md`, `MALMO_NETWORK.md`, `DISCOVERY.md`, `BOOT.md`, `BUILD.md`, `CONTROL_PLANE.md`, `THREAT_MODEL.md`, `AUTH.md`; `docs/README.md`; `NEXT.md`.

## 2026-06-16 — ISO build tooling: `mkosi`, not `live-build` (#197)

**Previously:** `BUILD.md` # 2 recommended `live-build` for v1 (fastest out the door, most Debian-blessed), with a migrate-to-`mkosi`-later note for when A/B-immutable updates arrive. The migration cost was judged smaller than the risk of betting v1 on thinner mkosi-on-Debian recipes.

**Now:** **`mkosi` is the single image builder** — for the install ISO, the cloud VM image, and the QEMU test image. No live-build, no two-builder phase.

**Why:**
- **The test lane is already mkosi, proven up the whole stack.** `dev/test-qemu/` boots the full control plane under `mkosi qemu`, and `mkosi-repart` already produces a LUKS2+ext4 root that TPM-unseals and switch-roots on a real boot (`luks-tpm-enrollment.md`, `qemu-fullstack-app-install.md`). Shipping live-build for the ISO would mean a *second* builder kept byte-identical with the test image to hold the "live fs == installed fs" invariant (`BUILD.md` # 3) — the exact maintenance burden the invariant exists to avoid. One builder makes it trivially true.
- **Systemd-native fits malmo.** We depend heavily on systemd (`systemd-cryptenroll` + TPM, UKI, `systemd-boot`, `cryptsetup-initramfs`); mkosi is the systemd team's own tool, so partitioning/LUKS/TPM/UKI-signing are first-class. The same config also emits the cloud VM image (qcow2/raw) the hosted product needs — live-build has no cloud-image story.
- **A/B-immutable is the stated future** (`SPEC.md` OS update model); live-build has no A/B story, mkosi's disk images A/B-swap natively. v1 stays mutable Debian + a flash-an-ISO install, but mkosi means the A/B future needs no re-tooling.
- This takes the spec's own stated "alternative to push back on" (mkosi-now), now that the test lane has retired most of the Debian-maturity risk that argued for live-build-first.

**Knowingly accepted:** mkosi's Debian track record is thinner than live-build's, and **a live installer ISO that boots a session is live-build's home turf** — the one part of mkosi's fit not yet proven in-repo (the test lane boots a disk image, not a live-session ISO). Validating that is a follow-up, not a reason to keep two builders. The **OTA orchestrator** is left unchosen (`systemd-sysupdate` is presumptive but unproven vs. Mender/RAUC); it waits for the A/B work. The interactive installer (`# 3` / `FIRST_RUN.md` Phase 1) is unchanged — this decision is only how the bootable artifact is assembled.

**Resolved 2026-06-17 (#199):** the flagged follow-up was investigated and turned up a sharper finding — **mkosi has no ISO output format at all** (its `OutputFormat` enum is `{confext,cpio,directory,disk,esp,none,portable,sysext,tar,uki,oci,addon}`; it builds GPT *disk* images). This decision's core — mkosi as the single builder — stands and gets *stronger*: **malmo ships disk images, not an `.iso`.** The maintainer's call (2026-06-17): drop the literal `.iso` entirely. The bootable artifacts are a `qcow2`/`raw` **cloud VM image** and a `raw` image `dd`'d to a **USB stick** for bare metal; CD/DVD/optical boot is out of scope (the original `.iso` requirement was loose terminology inherited from the live-build era, not a product need). The "live fs == installed fs" invariant (`BUILD.md` # 3) survives — a `Format=disk` root is what gets booted and laid down. **Priority order** (per the #196 epic): the cloud VM image lands first — it has no installer/kiosk/LUKS-on-target at all (`ENVIRONMENT.md`: the cloud image *is* the installed system) — and the bare-metal USB + kiosk-installer path follows. Detailed in `progress/iso-mkosi-finding.md`.

**Affected docs:** `BUILD.md` # 2 (recommendation flipped; # 2/# 6/locked-decisions reconciled to disk-image artifacts for the #199 resolution), `NEXT.md` (the "live-build vs mkosi revisit" open item resolved; the transient Tier-1 "ISO packaging path" item resolved and removed).

---

## 2026-06-16 — Offline (air-gapped) installs trust the catalog-promised digest of a locally-loaded image (#167)

**Previously:** every install resolved an image's digest by `docker pull` + inspecting the registry `RepoDigest`, then verifying it against the catalog promise. A pull failure was always fatal, and a `docker save`/`load`ed image (which carries no `RepoDigest`) could never be pinned.

**Now:** the brain has an explicit offline mode (`MALMO_OFFLINE_INSTALL`, off by default). In it, a pull failure is not fatal: if the image is already present locally, the **catalog-promised digest is the pin** — the offline bundle is the trust anchor in place of the absent registry. Two cases stay hard-fails: a genuinely-absent image (incomplete bundle — the missing-image failure the air-gapped lane exists to catch), and a Door-2 install (no catalog promise to trust). A box *with* a registry is unchanged — it pulls and verifies as before.

**Why:** the full-stack QEMU lane (#167) runs the guest air-gapped to prove the offline image bundle is complete, and the production first-boot bootstrap is registry-less by the same offline-first design (`BUILD.md` # First-boot brain bootstrap). With the old unconditional `docker pull`, *any* install on such a box hard-fails — the whole app-install assertion is unreachable. Trusting the catalog digest is sound here because the signed catalog already *is* the version→bytes binding (`APP_STORE.md` # Trust model); offline we simply can't re-derive the manifest digest from a loaded image (it has no `RepoDigest`, and its config `Id` is not the manifest digest), so we trust the promise rather than weaken it. Gating behind an explicit mode — rather than a silent "pull failed, use whatever's local" fallback — keeps an online box from masking a transient registry outage by accepting a stale local image on an update.

**Affected docs:** `APP_LIFECYCLE.md` (# Locked: image digest pinning — the offline paragraph), `docs/progress/offline-install-digest-trust.md`.

## 2026-06-14 — Socket-proxy ships with EXEC denied; managed-DB-in-production is gated on a provisioning re-architecture (#165)

**Previously:** the control-plane stack (Caddy + malmo-ui + socket-proxy) was specced as a single brain-launched compose, and M1b was blocked on an open question the spike (`socket-proxy-compose-validation.md`) escalated: the socket-proxy denies the Docker `EXEC` family by design, but managed-database provisioning (`internal/lifecycle/services.go`) creates per-app roles/databases by `docker exec`'ing the engine's client (`psql`/`mysql`/`valkey-cli`), so the instant the containerized brain points `DOCKER_HOST` at the proxy, managed-DB provisioning breaks.

**Now:** two settled calls. **(1) Defer, don't compromise.** M1b lands with the proxy switch and `EXEC` **denied** — the correct production posture. Managed-DB-in-production is gated on re-architecting provisioning off `docker exec` (connect to the engine over TCP, or run a one-shot provisioning container with only the engine port reachable), tracked as its own follow-up issue. Dev is unaffected (the natively-run dev brain keeps the raw socket and never sets `DOCKER_HOST`), and managed DB is pre-production regardless, so nothing ships broken. Widening to `EXEC=1` was rejected — it would flip the Locked decision the proxy exists to enforce. **(2) host-agent seeds the proxy.** The brain cannot bring up its own sole Docker path (the proxy *is* that path), so host-agent — which holds the raw socket — seeds the `malmo-ingress` network + the `docker-socket-proxy` container before launching the brain, and points the brain at it via `DOCKER_HOST=tcp://docker-proxy:2375`. The proxy is brain *transport infrastructure*, distinct from the Caddy + malmo-ui *services* the brain owns and reconciles; the proxy is therefore not in the brain's control-plane compose.

**Why:** the proxy's whole purpose is to deny `EXEC` and host-bind mounts to the brain — the component with the largest attack surface (LAN-exposed API, third-party manifests). Smuggling `EXEC=1` back in under M1b to keep a pre-production feature working would defeat the mitigation for the entire fleet to serve a path no production box runs yet. Deferring isolates the real, correctly-sized work (provisioning re-architecture, an L/XL spanning `SERVICE_PROVISIONING.md`) instead of letting it block the control-plane bring-up. The host-agent-seeds-proxy split resolves the bootstrap chicken-and-egg cleanly and keeps one chain of custody (host-agent owns every container's launch).

**Affected docs:** `CONTROL_PLANE.md` (# Docker socket exposure — host-agent-seeds-proxy refinement + the managed-DB EXEC limitation), `docs/progress/brain-control-plane-stack.md`, `docs/progress/socket-proxy-compose-validation.md` (the escalating spike).

## 2026-06-13 — Per-disk storage bars get a new `Disks` field; `DataDisk*` stays (#149)

**Previously:** GET /v1/system/status carried only `data_disk_free_bytes` / `data_disk_total_bytes` — a single statfs snapshot of the data drive, backing the install-plan's `free_bytes` warning. No OS-drive space, no per-volume view.

**Now:** `SystemStatus` gains `disks: []DiskSpace` (label, free_bytes, total_bytes) — one entry per mounted volume of interest (OS drive at `/` always, data drive at `/srv/malmo` when present), backing the system-resources panel's new Storage bars. **The existing `DataDisk*` fields are kept untouched** rather than folded into the new slice. The brain exposes the slice to the UI as a one-time poll at GET /api/v1/system/storage (not the live SSE stream — disk fullness doesn't move at the 1 Hz gauge cadence).

**Why:** the install-plan footprint already reads `DataDiskFreeBytes` with its specific semantics (the *data* drive only, Bavail net of the root reserve), and `Disks` is a display superset that includes the OS drive — two different consumers with two different needs. Unifying them would churn the footprint path (`internal/lifecycle/footprint.go`) for no behavioural gain, so the additive field is the surgical choice. The "Data" entry duplicates `DataDisk*` by design; that small redundancy is cheaper than rewiring the install plan. The data drive is included only when its backing filesystem differs from the OS drive's — a Level-0 box has no data drive (`/srv/malmo` is a directory on the OS drive), so a successful statfs there is not enough to call it a separate volume.

**Affected docs:** `LOCAL_ANALYTICS.md` (Real-time system resources — Storage), `BRAIN_HOST_PROTOCOL.md` (GET /v1/system/status).

## 2026-06-13 — Managed Redis always runs Valkey; `valkey` is a first-class type, `redis` is a BSD-3 compatibility alias (#159)

**Previously:** managed Redis ran the upstream `redis` image (`redis:7`), declared in manifests as `type: redis, version: "7"` (the entry immediately below).

**Now:** malmo never runs upstream Redis at any version. `valkey` is a first-class managed-service type (single version line: `8`); `redis` is kept as a pure compatibility alias that **always** provisions Valkey underneath. A `redis: "7"` declaration normalizes once, early, to the engine identity `valkey: "8"` (`normalizeEngine` in `internal/lifecycle/services.go`), so a `redis:7` app and a `valkey:8` app coalesce onto the one shared `malmo-svc-valkey-8` instance. The grant stores the engine identity (`Kind: "valkey", Version: "8"`), so everything downstream — compose, container/network/alias names, the ready probe, ACL provisioning, drop, and the writeEnv DSN — keys off valkey and never sees "redis". The injected DSN keeps the universal `redis://user:pw@valkey-8.malmo.internal:6379` scheme (RESP, every client understands it).

**Why:** the `redis:7` tag now resolves to Redis 7.4+, which is RSALv2 + SSPLv1 (not OSI open source), and Redis 8+ adds AGPLv3 — both license tracks are on malmo's avoid-list (NetBird rejected for AGPL, ZFS avoided for CDDL, Headscale chosen for BSD-3). Valkey is the Linux Foundation BSD-3-Clause fork of Redis 7.2.4 — the same license family as malmo's other picks, and RESP/ACL-compatible, so Valkey 8 is a drop-in for a Redis 7 client. Keeping `redis` as an alias means existing manifests and the broad Redis ecosystem keep working without authors learning a new name, while the engine underneath is unambiguously the BSD-3 one. This is stronger than the mysql/mariadb pairing (two real engines sharing a code path): here there is **one** engine (Valkey) serving two type names. Normalizing redis→valkey once, before anything touches the lifecycle maps, keeps the maps valkey-only — there is deliberately no `redis` key in `servicePort`/`serviceImageRepo`/`provisionedKinds`, so no dead code path could ever silently pull the upstream image. The `valkey/valkey:8` image ships `redis-cli`/`redis-server` as symlinks to `valkey-cli`/`valkey-server` and honors `REDISCLI_AUTH`; the code uses the honest `valkey-*` binary names.

**Affected docs:** `SERVICE_PROVISIONING.md` (# Catalog, # Implementation status, # Per-app isolation), `NEXT.md` (# Managed-service per-app key isolation), `APP_MANIFEST.md` (service types), `docs/dev/catalog-import-gaps.md` (`managed-redis — postiz`). Implementation: `internal/manifest/manifest.go` (`valkey` allowlisted, `redis` retained), `internal/lifecycle/services.go` (`normalizeEngine`, valkey-only maps, `provisionValkeyACL`, `valkeyServiceCompose`); realized by `docs/progress/managed-services-redis.md`. Supersedes the image/engine choice in the entry below (the per-app ACL-user isolation model it describes is unchanged).

---

## 2026-06-13 — Managed Redis is a per-app ACL user with full keyspace, not a logical-DB split (#159)

**Previously:** the manifest schema accepted `services: {…: {type: redis}}`, but the brain didn't provision it — a redis declaration passed `manifest check` then failed at install (a check/install asymmetry). The per-app isolation model was an open question in `NEXT.md`.

**Now:** Redis provisions on the Postgres/MySQL code path. The per-app credential is an **ACL user with the full keyspace** — `ACL SETUSER <app> on >pw ~* &* +@all -@admin -flushall -flushdb -swapdb` — created and dropped via `docker exec redis-cli` and persisted to an external aclfile on the data volume (`ACL SAVE`). The injected DSN is `redis://user:pw@redis-7.malmo.internal:6379` (no database path; clients default to logical DB 0). The shared instance's default (superuser) account lives in the aclfile; its password reaches redis-cli via `REDISCLI_AUTH` in the container env, never host argv.

**Why:** chose the per-app ACL user over the logical-DB-number split because it mirrors the established per-app-role Postgres/MySQL model (one revocable credential per consumer, dropped on uninstall) and gives a real **auth boundary** — every app authenticates as its own user and unauthenticated access is refused — whereas the DB-number split has no auth boundary between apps and a finite DB count. The keyspace stays shared (the "full keyspace" model): partitioning keys by prefix would require apps to cooperate, which they don't, so the credential — not a key namespace — is the boundary, acceptable for a single-tenant home server. `-@admin` keeps a compromised app off the ACL system / `CONFIG` / `SHUTDOWN` / replication so it can't subvert the shared instance or the control plane, and `-flushall -flushdb -swapdb` keeps it from wiping the shared keyspace every other app reads from (subtracted by name rather than the blunt `-@dangerous`, which would also strip `INFO`/`KEYS`/`SORT` that ordinary clients call on connect). The shared keyspace still lets one app *read* another's keys; per-app key confidentiality is deferred (`NEXT.md` # Managed-service per-app key isolation), its clean form being the `isolated: true` dedicated-instance escape hatch. The external aclfile is required because Redis ACLs are server config, not keyspace — they aren't in the RDB/AOF and would vanish on restart; letting Redis persist them to a file on the data volume mirrors how Postgres/MySQL accounts persist in their data dir.

**Affected docs:** `SERVICE_PROVISIONING.md` (# Implementation status, # Per-app isolation in shared instances), `NEXT.md` (# Managed-service lifecycle gaps — Redis item resolved), `docs/dev/catalog-import-gaps.md` (`managed-redis — postiz` → implemented). Implementation: `internal/lifecycle/services.go`, `internal/lifecycle/lifecycle.go` (writeEnv no-dbname DSN); realized by `docs/progress/managed-services-redis.md`.

---

## 2026-06-13 — malmo-brain runtime is `debian:trixie-slim` with the docker/Compose CLI bundled, not distroless (#162)

**Previously:** `BUILD.md` # 5 leaned a **distroless** runtime for `malmo-brain` (`gcr.io/distroless/static-debian12`), and the locked build summary pinned "distroless runtime" — smallest image, least attack surface.

**Now:** the brain runtime stage is **`debian:trixie-slim` with the `docker` CLI + Compose plugin bundled** (`docker-ce-cli` + `docker-compose-plugin` from Docker's official apt repo). The CLI shell-out model is unchanged; no brain code moves.

**Why:** the brain orchestrates apps by shelling out to the `docker` / `docker compose` CLI (~15 call sites in `internal/lifecycle/docker.go`), and a distroless runtime — no shell, no binaries — cannot host them. Three ways out: **(a)** slim base + bundled CLI; **(b)** rewrite the shell-outs onto the Docker Go SDK to keep distroless; **(c)** bind-mount the host's CLI into the container. Chose **(a)**: zero code change, keeps the compose-CLI model the whole codebase + test suite is built on, and `docker-ce-cli` via apt is the blessed install path (same trusted source as the host engine, not a third-party package set). **(b)** rejected as a large refactor — `docker compose up` has no SDK equivalent, so it would mean vendoring `compose-go` and reimplementing the multi-service orchestration the project deliberately delegates to the CLI, busting the size:S budget and the architecture. **(c)** rejected as fragile — distroless lacks the loader/libs the CLI needs, and the brain image would couple to the host's Docker version (glibc / version skew). The distroless **size** win is immaterial: multi-stage already keeps the Go toolchain out of the final image (~30 MB brain binary), and the bundled CLI is a runtime dependency multi-stage can't trim (image ~256 MB, measured in M0 #163) — noise against the multi-GB app images the box pulls. The **attack-surface** win is marginal for a daemon that already holds Docker API access via the socket proxy (`CONTROL_PLANE.md` # Locked: Docker socket exposure mitigated by socket proxy), and slim stays debuggable (it has a shell). Orthogonal to the socket-proxy decision: the bundled CLI reaches the daemon through the same proxy via `DOCKER_HOST`, so the endpoint allowlist still applies. Unblocks `#163`/M0 — the brain image now has an unambiguous base.

**Affected docs:** `BUILD.md` (# 5 build line + locked build summary). Realized by `docs/progress/brain-image-base-slim.md`.

---

## 2026-06-13 — A manifest secret can be owner-visible (`show: true`), so self-auth apps drop the published bootstrap constant (#152)

**Previously:** the brain generated per-app secrets (`MALMO_SECRET_*`) but had no way to *show* one to the owner. An app whose own login is gated by a token rather than malmo's session (Jupyter, #124/#136) therefore had to ship a **published constant** (`malmo-setup`) as its bootstrap token — printed in the app description, disabled by a self-grepping gate in the compose `command:` once the user set a password. If a future image bump renamed the grepped config key, the gate fails *open* and the documented constant becomes a permanent LAN backdoor, silently.

**Now:** a `secrets:` entry may declare `show: true`. Such a secret is owner-visible: the brain serves its value at `GET /apps/{id}/secrets` (owner-or-admin, the same control-authorization gate as stop/start), and the app detail page surfaces a masked **Setup secrets** row the owner reveals to finish first sign-in. Unmarked secrets stay internal (a managed-service password is never listed), so one reveal can't dump every injected credential. The read is a pure read — no audit (only elevation-class mutations audit). The reveal is the **missing molma-side capability**: with it, a self-auth app's bootstrap token can be a *per-instance random* secret, so the fail-open case degrades from "published backdoor" to "a random token nobody knows stays valid" — harmless.

**Why:** the root cause of the Jupyter fragility was molma-side, not Jupyter-side — there was no seam to hand the owner a generated value, so the token had to be public. Surfacing the secret is the smaller, self-contained half of the fix (a first-class "this app authenticates itself" manifest seam is the larger, separate piece — `NEXT.md`). Reusing the existing control-authorization gate (not a new elevation prompt) keeps it consistent with stop/start and with the issue's ask; gating on the manifest `show` flag (not revealing all secrets) keeps internal credentials internal. Migrating the Jupyter manifest off the `molma-setup` constant is a deliberate **follow-up PR**, not this change.

**Affected docs:** `APP_MANIFEST.md` (# D2 — `show` flag + example, locked decisions), `SERVICE_PROVISIONING.md` (# Env-var injection — owner-visible paragraph, locked decisions), `DASHBOARD.md` (# Installed apps — Setup secrets surface). Implementation: `internal/manifest/manifest.go` (`Secret.Show`), `internal/lifecycle/lifecycle.go` (`RevealSecrets`), `internal/api/appsecrets.go` (`GET /apps/{id}/secrets`), `web-ui/src/views/settings/InstalledAppDetailSection.vue`; realized by `docs/progress/owner-visible-secrets.md`.

---

## 2026-06-13 — Start re-asserts the mDNS name, up-front, not only the Caddy route (#153)

**Previously:** `Start` re-registered only the Caddy route; the mDNS name was published at install and re-published by the brain-startup reconcile pass, but never by a user-initiated Start. An app recovered via Start after its `<slug>.local` went dark (a mid-life host-agent restart that dropped the process-local Avahi entry group, or a prior install that failed before publishing) came back reachable by Host-header proxy but unresolvable-by-name until the next brain reboot.
**Now:** `Start` re-asserts mDNS **and** Caddy, lockstep, via the same idempotent `host.Publish` install and the reconcile pass use (extracted as `lifecycle.publishHost`). The publish happens **up-front** — before the "starting" splash, mirroring install's step 9 — not after the app goes healthy as the issue's first sketch suggested.
**Why:** the name-lifecycle model already keeps a name announced for an app's whole life with the *route content* varying (Stop deliberately keeps `<slug>.local` resolving, pointing it at the stopped splash). "Announce only once healthy" would contradict that model; publishing up-front makes the name resolve to the starting splash during recovery — the exact UX the bug is about — and lets one `host` value key the starting splash, the real upstream, and the failed splash without divergence. Idempotent, so re-running on every Start is a host-side no-op. Does **not** close the broader "mid-life host-agent restart while brain runs" gap for apps left untouched (still `DISCOVERY.md` # Restart durability, the `uptime_s`-poll mitigation).
**Affected docs:** `APP_LIFECYCLE.md` (# stop, start, uninstall — Start bullet), `docs/progress/start-reasserts-mdns.md`.

---

## 2026-06-12 — Outgoing email is BYO-SMTP with per-app bindings, not a malmo relay (#122)

**Previously:** apps that send email (Kimai's password resets, Gitea's notifications) had no malmo story at all — the catalog-import ledger parked them as `smtp-relay` gaps, and their descriptions told the user an administrator had to configure a mail server somehow, with no UI path.

**Now:** the admin **brings their own SMTP account**. Settings → Outgoing email holds a box-level provider registry (label, host/port, optional credentials, from address, encryption mode; admin-only, elevation-class, with a synchronous test-send). A mail-capable app declares `mail: {optional: true}` in its manifest; the install dialog then offers a provider picker (None default, sole provider preselected), and the binding is per-instance and rebindable from the app's detail page. The brain direct-injects the bound provider as `MALMO_MAIL_HOST/_PORT/_USER/_PASSWORD/_FROM/_ENCRYPTION` plus a Symfony-style `MALMO_MAIL_DSN`; the app's compose maps them app-defined, with a compose default covering the unbound case. Unbound → nothing injected, app runs with email off.

**Why:** running a relay/smarthost on the box was rejected — residential IPs can't deliver mail (blocklists, missing PTR records), so a malmo relay would just be a queue in front of the user's real provider, plus deliverability support burden malmo can't carry. Direct injection reuses the established `MALMO_*` contract (`MALMO_SERVICE_*`, `MALMO_SECRET_*`) instead of inventing a mail-specific mechanism, and the app dialing the provider itself rides its already-declared `internet` permission. v1 admits only `optional: true` because a box with zero registered providers must still be able to install the app; required-mail (block install until bound) is a possible later loosening, not a v1 shape. Provider edits don't re-stamp bound apps' env until their next rebind/recreate — accepted lag, surfaced in the UI, rather than a fleet-restart side effect hidden inside a settings save.

**Affected docs:** `SERVICE_PROVISIONING.md` (# BYO outgoing mail — new section, injection table, locked decisions), `APP_MANIFEST.md` (# D3 — new section, locked decisions), `SETTINGS.md` (panel inventory), `DASHBOARD.md` (consent dialog), `NEXT.md` (# Outgoing mail), `docs/dev/catalog-import-gaps.md` (kimai/gitea `smtp-relay` entries → implemented). Implementation: `internal/store/mail.go`, `internal/lifecycle/mail.go`, `internal/api/mail.go`, catalog kimai; realized by `docs/progress/byo-outgoing-mail.md`.

---

## 2026-06-10 — App Start uses `docker compose up -d`, not `compose start`

**Previously:** `APP_LIFECYCLE.md` # stop, start, uninstall locked Start as `docker compose -p malmo-<id> start`, the literal inverse of the `stop` it pairs with.

**Now:** Start runs `docker compose up -d` — the same op the reconcile pass already uses to bring a drifted instance back. `stop` is unchanged.

**Why:** `compose start` only restarts the containers that already exist in a stopped state. That has two bites for a real app: a one-shot migration/seed/init job that already exited 0 gets started *again* (re-running the migration), and `start` ignores `depends_on` ordering, so a service can come up before the dependency it gates on. `up -d` reconciles to the desired set instead — it respects `depends_on` ordering and the completion-gate job semantics the override already encodes (`APP_LIFECYCLE.md` # override file contents, #92), and it's idempotent, so it doubles as the crash-recovery path. Using the same op as reconcile means one code path and one set of semantics, not two. The state row is written `running` before the op (brain-commits-first), so a crash mid-start is finished by the reconcile pass exactly as a reboot is.

**Affected docs:** `APP_LIFECYCLE.md` (# stop, start, uninstall). Implementation: `internal/lifecycle/lifecycle.go` (`Manager.Start`/`Stop` + per-instance lock), `internal/api/api.go` (stop/start routes), realized by `docs/progress/stop-start-service.md`.

---

## 2026-06-10 — Folderless apps can opt into a dedicated non-root identity (`service_user`); manifests never name a UID

**Previously:** every Tier-3 instance ran as a resolved managed UID with `data/` chowned to match, but a **folderless** app's only identity was the brain's euid — **root** in production. An image that writes its data as a non-root user (nginx+php-fpm, LinuxServer-style) then couldn't write the root-owned `data/` dir and couldn't chown it (`CAP_CHOWN` stripped), so it failed to start. The catalog-import ledger captured this as `nonroot-data-ownership` (poznote #90; kimai's secondary finding, #89).

**Now:** a manifest may set `service_user: true`. The brain then allocates a **stable per-instance UID/GID from a reserved app-service band** (below the 3000 user floor, distinct from the fixed 2000/2001 well-known identities), persists it on the instance row, reuses it across recreations, pins the container `user:`, and chowns `data/` to it. The app declares **intent only — it can never name a numeric UID.** A manifest-named UID would be read in the host namespace (malmo runs no userns-remap) and could alias a real host principal — a system account, or a malmo user ≥ 3000 — so a numeric `user:` is an admission rejection for both doors. `service_user` is folderless-only; a folder app already runs as a managed non-root identity (the owner, or the malmo-app identity).

**Why:** smallest mechanism that unblocks the broad "runs as a non-root user and owns its own data" class while preserving the model's core invariant — *every running UID is a malmo-managed principal.* A free-form declared UID was rejected on exactly that ground: handing an app a say over its host UID hands a compromised app (a top adversary in `THREAT_MODEL.md`) a say over its host identity. Allocating from a reserved band follows the systemd `DynamicUser=`/`StateDirectory=` and Kubernetes `fsGroup` pattern — the platform owns volume ownership, the app declares the need — and keeping the identity *stable* (not transient like `DynamicUser`) leaves a future cross-app data grant able to reference it. Deliberately **scoped to images that adopt the runtime user**: images that hardcode a *different* internal UID or `setuid`-drop to a fixed service user (poznote, kimai) need user-namespace remap and stay curation-rejects until that lands (`NEXT.md`).

**Affected docs:** `APP_ISOLATION.md` (# Runtime identity & data ownership — new section), `APP_MANIFEST.md` (# B, # Locked decisions), `docs/dev/catalog-import-gaps.md` (`nonroot-data-ownership` status), `NEXT.md` (userns-remap follow-on).

---

## 2026-06-09 — Tier-1 managed services grow MySQL + MariaDB types (#108)

**Previously:** the v1 Tier-1 catalog was Postgres (15, 16) + Redis (7), with MariaDB explicitly parked as a post-v1 candidate ("some apps require it specifically — Nextcloud, WordPress") behind the "3+ store apps actually want them" bar.

**Now:** `mysql` (8.0, 8.4) and `mariadb` (10.11, 11.4) are provisioned Tier-1 types alongside Postgres. Both engines ride the existing managed-services design unchanged — lazy spinup, per-app DB+user, `docker exec`-the-client provisioning (`mysql`/`mariadb` instead of `psql`), `MALMO_SERVICE_*` injection (port 3306, `mysql://` DSN for both — one wire protocol), dedicated `--internal` network, drop-on-uninstall. Per-engine deltas are confined to an image pin, client binary names, the root-password env var, and the readiness probe (`mysqladmin`/`mariadb-admin ping` over TCP only, so the init-time socket-only bootstrap server doesn't read as ready). Version dots fold to dashes in derived names (`malmo-svc-mysql-8-0`, `mysql-8-0.malmo.internal`) because compose project names reject dots.

**Why:** the catalog import sprint hit the same wall twice — Ghost (#85) requires MySQL 8 with no Postgres/SQLite-production path, Kimai (#89) speaks only the MySQL dialect — and bundling a DB into the app compose is structurally impossible under the Tier-3 sandbox (`cap_drop: ALL` strips the CAP_CHOWN/SETUID/SETGID the official `mysql`/`mariadb` entrypoints need; the `managed-mysql` ledger entries document it). A managed service type is the right seam precisely because the brain owns the service container outside the app sandbox. Both engines land as one code path because they share a wire protocol and SQL dialect; supporting both is the product call (some upstreams pin one specifically — Ghost: MySQL 8; Nextcloud: MariaDB). mysql 8.0 stays on the allowlist despite being past Oracle EOL because Ghost pins it. Unlike Postgres, the exec'd client needs the root password — the MySQL images don't trust the local socket — so the exec'd shell expands it from the container's own environment (`$MYSQL_ROOT_PASSWORD`/`$MARIADB_ROOT_PASSWORD`), never host-side argv.

**Affected docs:** `SERVICE_PROVISIONING.md` (# Catalog (v1), # Provisioning protocol, # Post-v1 candidates, locked decisions), `APP_MANIFEST.md` # D, `docs/dev/catalog-import-gaps.md` (Ghost/Kimai `managed-mysql` entries → implemented). Implementation: `internal/manifest`, `internal/lifecycle/services.go`, realized by `docs/progress/managed-services-mysql.md`. Inherits the Postgres path's deferrals (backup/restore, grace-shutdown, at-rest credential encryption — `NEXT.md`).

---

## 2026-06-09 — `estimated_size` is the measured app-state baseline at install, not a usage projection

**Previously:** `storage.estimated_size` was an author's *estimate* of the app's eventual on-disk state — "for warnings on small disks," explicitly a coarse upper bound the catalog footprint rendered as a ceiling (`DECISIONS.md` 2026-06-03). `NEXT.md` parked the residual question of "how to present an app-state estimate that **grows over time**" (a static "~2 GB" understates a photo library).

**Now:** `estimated_size` is the **measured** size of the app's `data/` volumes the moment install completes (main service first healthy), on a clean install — the real cost of *having installed* the app, not a guess at how big it gets with use. Growth from later downloads or user uploads is **not** counted; that's a runtime disk-pressure concern (`HEALTH.md` # `disk-full`). Undercounting (a first-boot download still in flight at the health probe) is acceptable; overcounting by speculating about usage is not. The catalog `footprint` is therefore a measured baseline, not a "coarse upper bound."

**Why:** the old definition produced numbers that varied wildly by use case and tended to alarmism — a speculative "5 GB" on an app that lands at ~200 MB scares a non-technical user off a perfectly cheap install. A concrete "this is what it costs to install" is more honest and actionable, and — unlike the projection — it's **mechanically measurable**: anchoring to "main service healthy" (a signal the brain already owns, and a checkpoint the import smoke-test already hits) makes it reproducible, whereas "size once first-boot setup settles" has no universal observable signal (you'd poll for data-dir quiescence, which is flaky per-app). This resolves the parked `NEXT.md` "grows over time" question by sidestepping it: the manifest figure never represents growth; runtime disk-pressure does.

**Affected docs:** `APP_MANIFEST.md` # Storage, `APP_STORE.md` # Catalog schema, `BRAIN_UI_PROTOCOL.md` # GET /api/v1/catalog/:id/install-plan, `DASHBOARD.md` # Install authorization, `NEXT.md` (residual item resolved), `docs/dev/authoring-apps-with-an-agent.md`. Supersedes the "coarse upper-bound" framing of the 2026-06-03 footprint entry (the footprint shape — image + app-state, user content excluded — otherwise stands).

---

## 2026-06-05 — Override forces `restart: unless-stopped` *except* for terminating jobs; `compose up` is time-bounded

**Previously:** `APP_LIFECYCLE.md` # Locked: override file contents force-stamped `restart: unless-stopped` onto **every** service ("forced, overrides whatever the author wrote"). And `Manager.install` ran `compose up -d` on an unbounded context.

**Now:** the forced restart is preserved for long-running services but **skipped for author-declared terminating jobs**, whose `restart:` is kept verbatim. A job is detected from the union of two signals: (a) the author set `restart: "no"` / `"on-failure"`, or (b) the service is the target of another service's `depends_on: {condition: service_completed_successfully}` (catches an omitted `restart:`, whose Compose default is `no`). `main_service` is **always** forced long-running regardless. Independently, `compose up -d` now runs under a context bounded by the health-wait budget (default 120s).

**Why:** forcing restart on a one-shot init/migrate/seed job is catastrophic — the job exits 0, Docker restarts it, it never reaches the "completed" terminal state, and a completion-gate `depends_on` blocks `compose up -d` forever. This wedged the install transaction (`kan`'s `migrate`-then-`web` shape, a very common Compose pattern, hung past a 600s test timeout). Layer 1 (job detection) fixes the known shape; layer 2 (bounded `compose up`) is a containment backstop that turns any future never-completing gate into a clean install failure + rollback instead of a hung brain. Verified end-to-end: `kan` now installs, its `migrate` job completes, `web` boots against managed Postgres (`TestLiveKanBoot`).

**Affected docs:** `APP_LIFECYCLE.md` (# Locked: override file contents, # Locked: install transaction). Implementation: `internal/lifecycle` (`writeOverride` restart/`depends_on` parsing in `lifecycle.go`, bounded `ComposeUp` context), realized by `docs/progress/one-shot-job-restart.md`.

## 2026-06-05 — Managed-service provisioning runs through `docker exec psql`, not a Go SQL client

**Previously:** `SERVICE_PROVISIONING.md` # Provisioning protocol specced *what* the brain does ("connects to that Postgres-15 instance as superuser and creates a database … a role … grants") but not *how* it connects. The implicit reading was a network SQL connection from the brain to the service.

**Now:** the brain provisions per-app databases and roles by running `docker exec <svc-container> psql` (a new `DockerDriver.Exec`), inside the shared service container, rather than opening a SQL connection of its own. Each service kind+version is a brain-owned compose project under `<stateDir>/services/<kind>-<version>/` with a fixed `container_name` (the exec handle) and an in-network DNS alias `<kind>-<version>.malmo.internal` (what apps put in their DSN). The service network is created `--internal`. Provisioning needs no superuser password because the official postgres image trusts the local socket for the `postgres` superuser.

**Why:** a Go SQL client would (a) add a driver dependency and (b) put the control plane on the `malmo-svc-postgres-*` network — which directly contradicts the 2026-06-02 call to keep the brain off app-reachable networks (the same reason health probes go through Caddy instead of dialing the container). `docker exec` keeps the brain talking only to the Docker socket it already owns, and the network stays app-only. Trade-off accepted: provisioning is shell-string SQL (mitigated — db/role names are sanitized `[a-z0-9_]`, passwords are base64url, both quote-safe), and it's Postgres-shaped (Redis ACL provisioning will be its own path). Scope this slice: **Postgres only** (Redis schema-valid but unprovisioned), **lazy spinup** built, **grace-shutdown / backup-restore / cross-version migration deferred**.

**Affected docs:** `SERVICE_PROVISIONING.md` (# Provisioning protocol, # Network architecture, + locked decisions), `NEXT.md` (Redis provisioning, grace-shutdown, at-rest encryption), `docs/dev/catalog-import-gaps.md` (kan managed-Postgres blocker resolved). Implementation: `internal/manifest`, `internal/store`, `internal/lifecycle` (`services.go`), realized by `docs/progress/managed-services-postgres.md`.

## 2026-06-05 — Apps declare random secrets; the brain generates and injects them as `MALMO_SECRET_*`

**Previously:** there was no mechanism for an app to obtain an app-specific random secret (a JWT/HMAC signing key, `BETTER_AUTH_SECRET`, Rails `SECRET_KEY_BASE`). An app that needed one couldn't boot — the catalog-import ledger captured `kan` as a `blocks-start` gap, and its compose carried a "set this by hand before first start" comment, which is exactly the sysadmin step the long-term audience can't take.

**Now:** a manifest declares `secrets: [{name, bytes?}]`. At install the brain draws each secret from a CSPRNG (`bytes` default 32, floor 16), base64url-encodes it, persists it in SQLite, and injects it as `MALMO_SECRET_<NAME>` — re-emitted verbatim into the instance `.env` on every restart, never re-rolled. The app's compose maps it to whatever variable it expects, the same app-defined wiring as `MALMO_SERVICE_*` and `MALMO_FOLDER_*`.

**Why:** the author can't ship a value (a public-catalog secret signs nothing securely) and the non-technical user can't be asked to generate one — so the platform must. **Generate-once-and-persist** (rather than regenerate per `.env` write) is the load-bearing detail: a token-signing secret that changed on restart would invalidate every live session, so the value is read back from storage, not re-derived. This is the only `MALMO_*` variable the brain *creates* rather than *reflects*. Security hardening (`.env` perms, env-var-vs-`_FILE` delivery, at-rest encryption, backup, rotation) was reviewed and deliberately deferred — parked in `NEXT.md` # App-secret injection hardening rather than folded in, so the mechanism ships correct-but-unhardened under the household trust model.

**Affected docs:** `APP_MANIFEST.md` (# D2 Generated secrets, + locked decision), `SERVICE_PROVISIONING.md` (# Env-var injection — the full family, + locked decisions), `NEXT.md` (# App-secret injection hardening), `docs/dev/catalog-import-gaps.md` (kan `secret-injection` → implemented). Implementation: `internal/manifest`, `internal/store`, `internal/lifecycle`, `catalog/kan`.

## 2026-06-05 — Activity (audit log) is a member-reachable view, not admin-only surface

**Previously:** `DASHBOARD.md` # global navigation and `SETTINGS.md` framed Activity as admin-surface — it nested under the admin-only **System** panel, and `SETTINGS.md` # role gating stated "a member's Settings is **My account** and nothing else."

**Now:** Activity is a Settings route **open to every signed-in user**, scoped server-side: a member sees only events where they are the actor (or target), an admin sees the full box-wide feed. The dashboard link is not admin-gated.

**Why:** `LOGGING.md` # Visibility rules (the authoritative spec for audit visibility) always said "members see only events where they are the actor or the target," and the shipped backend (`GET /api/v1/audit`, `internal/api/auth.go#listAudit`) already returns `200` to members with their own rows — it never 403s them. The IA docs simply lagged that. Issue #11 (accepted) made the call explicit ("not admin-gated, members see their own events"). "Did someone access *my* account?" is a member-facing security question, so a member needing an admin to answer it would be the wrong posture. This reconciles the IA docs with LOGGING.md and the implementation; it narrows nothing a member couldn't already query.

**Affected docs:** `DASHBOARD.md` # global navigation, `SETTINGS.md` # panel inventory / role gating. Realized by `docs/progress/activity-view.md`.

---

## 2026-06-03 — Surface an app's on-disk footprint before install (image + app-state, user content excluded)

**Previously:** the only size signal was `storage.estimated_size` (app state, for small-disk warnings) — never shown to the user. `NEXT.md` parked a narrow `storage.image_size` field for the download alone.

**Now:** the brain shows a **footprint** — container image(s) + app-state estimate — on the store card and in the install consent dialog, so the user sees what an app costs their box before confirming. Three calls fix the shape:
- **App only; user content excluded.** The number is image + app state. The user's Photos/Music/Documents are first-class, unbounded, and survive uninstall, so they're never attributed to the app — counting them would mislead ("this app needs 500 GB"). The dialog calls the exclusion out for reassurance.
- **Image size is CI-derived, not author-declared.** The catalog build resolves `download_bytes` / `disk_bytes` from the registry alongside the digest it already pins. Authors would guess wrong and go stale. This supersedes the rejected `storage.image_size` manifest field.
- **Coarse on the card, sharp in the dialog.** The catalog carries a coarse upper-bound `footprint` (browse grid renders it without fetching the manifest). The install-plan endpoint returns a box-specific number that subtracts already-present images and includes `free_bytes` for a not-enough-space warning.

**Why:** "you're about to use this much of your box" is exactly the kind of implication the non-technical audience needs before committing, and it ties into the existing disk-pressure surfaces (`HEALTH.md` # `disk-full`). Sizes stay strictly advisory — only the digest gates the pull, so a drifted size is cosmetic, never an integrity failure.

**Affected docs:** `APP_MANIFEST.md` # Storage, `APP_STORE.md` # Catalog schema / Trust model, `BRAIN_UI_PROTOCOL.md` # GET /api/v1/catalog/:id/install-plan, `DASHBOARD.md` # Install authorization, `NEXT.md`.

---

## 2026-06-02 — Door-2 form authors the permission block: folders + GPU controls, a form⇄YAML toggle, and an explicit folder `target`

**Previously:** the Door-2 install form (entry below, same day) surfaced **only** the `internet` toggle inline; `folders`, `devices`, `gpu`, and managed `services` were declared "hand-authored in the manifest, out of the paste flow." But with **in-product manifest editing deferred** (uninstall + re-paste is the only change path), "hand-author it later" had no surface — so in practice a Door-2 app could get *no* folder access and *no* GPU at all. That breaks the two most common reasons to paste a compose: a self-hosted app that should see `~/Photos` (directly contradicting the *files-are-first-class* north star, in the door aimed at the people most likely to bring such apps), and a media app that wants the GPU for transcoding.

**Now:** the install form **authors the synthetic manifest's `permissions` block**, not just an internet flag. (a) **Controls for the common cases:** `internet` (default on), `lan` (off), `gpu` (off, single toggle), and **folder grants** as add-a-row pairs — a **Source picker** (over the fixed use-case folders) plus a hand-typed **Destination** in-container path, with read/write. (b) **A form⇄YAML escape hatch:** the form fields are a *friendly projection of the synthetic manifest*; an **Edit as YAML** toggle exposes the same overlay raw for the long tail (`devices`, managed `services`, `health_probe`) so we don't build a control per key. The toggle edits the **manifest overlay only** — the pasted compose stays in its own verbatim textarea; the two are never merged. (c) **Admission still gates every path** (toggled, row-filled, or YAML-edited) via `Synthesize` + `admission.Check` — the escape hatch escapes the form, not the sandbox. (d) **Door-2 folder grants carry an explicit `target`** (below). This is **install-time authoring** of a not-yet-installed app — explicitly *not* the deferred *graduate-in-place* path, which edits a live instance.

**Why folders need an explicit `target` (a real divergence from a locked decision):** `APP_MANIFEST.md` # Locked: "`folders` mount at a fixed path + injected env var" — the brain mounts every store-app folder at `/malmo/<folder>` and injects `MALMO_FOLDER_<NAME>`, relying on the *author* to map that env var to the image's library path. A Door-2 paste has **no author to adapt**: the verbatim third-party compose already hardcodes where it reads data (PhotoPrism → `/photoprism/originals`), and it knows nothing about malmo's env var, so a fixed `/malmo/Photos` mount lands where the app never looks. So a Door-2 folder grant carries an explicit `target` the admin types, and the brain binds the elected source straight there. `target` is **additive and Door-2-only** — store grants omit it and keep the fixed-path + env-var convention, so this evolves the locked decision without flipping it for store apps. The Source side stays a **picker, not free text**, so folder access can't become a back door to "bind any host path" (which admission rejects regardless). Chose surface-folders-and-GPU-but-not-everything because the form should stay calm for the 90% while the YAML toggle absorbs the long tail — the same Door-1/Door-2 logic recursed one level, which also dissolves the "where exactly to draw the permission line" question (common → control, rare → YAML).

**Affected docs:** `DASHBOARD.md` (# The form — step 5 rewritten from a lone internet toggle to the full permission UX; new # Form is a projection of the synthetic manifest and # Folder grants carry an explicit destination path; # Edit-after-install clarified to distinguish install-time YAML authoring from the deferred graduate-in-place), `APP_MANIFEST.md` (# Custom container — synthetic manifest: permission block now admin-elected, example updated, explicit-`target` paragraph, main-port inference extended to the container side of `ports:`; # Locked folders-fixed-path decision annotated with the Door-2 `target` divergence), `NEXT.md` (typed-install-questions item narrowed; graduate-in-place item scoped to post-install). Implementation gaps this opens (not yet built): `manifest.Synthesize` takes only `internet:true` today — it must accept elected `gpu`/`lan`/`folders`; the `Folder` struct needs a Door-2 `target`; the override generator must honor `target`; `main_port` inference must mine the `ports:` container side; the web-ui form needs the permission controls + YAML toggle.

## 2026-06-02 — Door-2 custom-container install flow: admin-only paste form that coaches into the sandbox; edit-after-install deferred

**Previously:** Door 2's *admission* and *manifest* were locked (synthetic manifest with permissive defaults, door-symmetric sandbox, admin-only — entry below; `POST /api/v1/apps/custom` exists and pre-checks `Synthesize` + `admission.Check`), but the **install UX** was an open `NEXT.md` Tier-2 item: where the paste-compose flow lives, what we ask vs. autodetect, main-port inference, name collisions, and the edit-after-install path were all unspecced. `APP_MANIFEST.md` also carried an aspirational "edit the synthetic manifest later to graduate the app" line that no built path backed.

**Now:** locked in `DASHBOARD.md` # Door-2 custom container install flow. (a) **Where:** an admin-only "Install a custom container" affordance at the **bottom of the Store** (not a dock item, not in the browse grid; members never see it), opening a **dedicated full-screen form**, not the catalog consent dialog — a store install *elects sources* off a known manifest, a custom install *authors* one from a paste. (b) **Ask vs. autodetect:** `main_service` autodetected when the compose has one service, asked (dropdown) otherwise; `main_port` is the *container-internal* port, **best-effort inferred from the main service's `expose:`** and asked when absent (malmo can't read the image's `EXPOSE` without pulling), always editable; published `ports:` are an admission rejection, never the routed port. The only permission surfaced inline is `internet` (default on); `folders`/`devices`/`gpu`/managed `services` are hand-authored, out of the paste flow. (c) **Validation coaches into the sandbox:** two-stage (client YAML parse + synchronous brain `422` pre-check before any job), with door-symmetric admission rejections (`ports:`, host bind paths, `privileged`, `cap_add`, `build:`, host namespaces) shown **inline with their built-in remedy**, not as opaque toasts — Door 2's job is to turn a copy-pasted forum snippet into a sandboxed install. TOFU image pinning and "custom apps don't auto-update" are surfaced honestly. (d) **Collisions:** custom installs never hit the duplicate-install warning (fresh manifest id per paste); slug collisions resolve via the existing first-come bare → `--<user>`/`-2` rule, with a live `<slug>.local` URL preview. (e) **Edit-after-install is deferred** — v1 is **install-only**; changing a custom app is uninstall + re-paste. The "graduate in place" path is parked in `NEXT.md`.

**Why:** the load-bearing call was framing Door 2 as a **coaching** surface rather than an accept/reject gate. The realistic Door-2 input is a forum/README compose that almost always trips the (deliberately door-symmetric) admission rules; if those rejections read as opaque failures, the admin can't self-serve and Door 2 fails its "bridge to the tinkerer" purpose. Surfacing each rejection inline with the remedy that admission already names turns the bright lines into a guided rail without relaxing them. Tucking the entry into the Store bottom (vs. a dock item) keeps the calm-launcher posture for the non-technical primary audience while leaving it findable for power users. Main-port inference is *best-effort from `expose:` only* because reading the image's real `EXPOSE` would require a pull (work, and before the user has committed) — prefill-and-confirm is the honest middle. Edit-after-install was deferred for v1-simplicity symmetry with the already-deferred store-app permission *revocation*: an in-product manifest editor is a larger surface (validation, re-render, restart, audit) than the install flow needs, and uninstall + re-paste is a working floor. Reconciled the stale `APP_MANIFEST.md` "edit later" line to read as the intended-future shape, explicitly not-v1, so the spec stops claiming an unbuilt editor.

**Affected docs:** `DASHBOARD.md` (new # Door-2 custom container install flow — IA, form fields, validation-as-coaching, collisions, deferred edit), `APP_MANIFEST.md` (# Custom container — synthetic manifest: infer-vs-ask paragraph + graduation/edit reconciled to deferred; # one model, two doors line corrected), `NEXT.md` (Tier-2 "Custom container (Door 2) install flow" removed; Tier-4 "Door-2 synthetic-manifest edit / graduate-in-place path" added as the deferred remainder), `docs/README.md` (DASHBOARD map line extended). Contributor issue filed (`accepted`) — scope is the admin-only paste-compose install screen wired to the existing `POST /api/v1/apps/custom`, including best-effort `expose:`-derived main-port inference and inline admission-error coaching.

## 2026-06-02 — Door-2 admission holds the line: same sandbox as store, admin-only, multi-user is the reason

**Previously:** `APP_ISOLATION.md` # Trust tiers contradicted itself — "Door-2 = the user can do anything" / "the user wrote it, owns the consequences" (host ports `:71`, arbitrary bind mounts `:120`, `privileged`/socket `:199`), yet the same table's enforcement row and the implemented admission policy (`internal/admission`, run for *both* doors) said enforcement is identical and rejected all of those uniformly. The set of primitives Door-2 may relax was an open `NEXT.md` Tier-4 item.
**Now:** **hold the line.** Admission is deliberately **door-symmetric** — `privileged`, the Docker socket, host ports, absolute/host bind mounts, `cap_add`, and host namespaces are refused for store *and* custom apps alike. Door 2 differs only in its **manifest** (permissive defaults, synthesized, TOFU digest pinning), never in its **sandbox**, which is byte-for-byte the store envelope (`cap_drop: [ALL]`, no host access, Caddy-routed, runs as a UID). **Door 2 is admin-only** (`POST /api/v1/apps/custom` requires admin; members install store apps, personal scope only). Genuinely-unsandboxable containers (Portainer/Watchtower → socket; Tailscale/WireGuard → `NET_ADMIN` + host net) route to Tier-2 curation or the admin's SSH access, not a custom paste.
**Why:** the spec's "the user owns the consequences" reasoning quietly assumed a **single-user** box. malmo is multi-user and the threat model (`THREAT_MODEL.md` # B2) names "a compromised app" as a top adversary — a Door-2 app that roots the host exposes *every other household member's* data, none of whom consented. So host-rooting primitives aren't the installer's consequence alone; the bright lines stay up in the UI path. The determined-admin counter ("they can SSH and `docker run --privileged` anyway") is real but doesn't defeat it: SSH requires intent and knowledge, a UI paste is frictionless — holding the line prevents *accidental* host compromise from a copy-pasted forum snippet, which is the actual threat. Chose hold-the-line over a graduated relax (relax app-bounded primitives, hold host-equivalent ones) for v1 simplicity — one admission policy, no door-conditional branching, matches the code that already exists — with relaxation explicitly available later (`NEXT.md`). This also ratifies the long-standing `APP_ISOLATION.md` principle "same enforcement everywhere; only the defaults and catalog rules differ."
**Affected docs:** `APP_ISOLATION.md` (# Principles, # Trust-tiers table rewritten + manifest-not-sandbox note, # Inter-app/ports, # bind-mounts, # Capabilities & privilege, # Forbidden-for-both-doors), `APP_LIFECYCLE.md` (# admission policy — door-symmetry + admin-only note), `NEXT.md` (Tier-4 "Door-2 vs. Door-1 admission asymmetry" removed). Contributor issue filed (`accepted`) — its scope is the one real code gap: `installCustomApp` lacks `requireAdmin` today (members can install custom), plus a door-symmetry regression test.

## 2026-06-02 — Per-app HTTP health-probe: opt-in manifest field, probed through Caddy (not brain-on-app-net)

**Previously:** `app-unresponsive` was a registered-but-deferred health issue (`HEALTH.md`) with no detector and no manifest field — `NEXT.md` Tier 4 ("per-app HTTP health-probe"). Docker `HEALTHCHECK` was the only liveness signal, and since malmo holds the compose file verbatim it cannot add one for the author.
**Now:** locked. A new optional `health_probe` manifest field (`APP_MANIFEST.md` # B) declares an HTTP `path` (+ optional `healthy_status`, `start_period`); the brain probes it on the 60s health-poll tick and reconciles the per-instance `app-unresponsive` warning (non-blocking, Tier 2). Opt-in: no field → no probe, issue never raised for that app. Default healthy = **any status < 500** ("the server answered coherently"; `401`/`403`/`404` still count as responding). The probe goes **through Caddy via the app's own route** (`Host: <slug>`), not by the brain dialing the container.
**Why:** the load-bearing call was *how the brain reaches the app to probe it*. Dialing the container directly would require the brain to join an app-facing Docker network (the shared `malmo-ingress` today) — and Docker bridges are bidirectional, so that same membership would hand every app container (an **assumed-compromised** principal, `THREAT_MODEL.md` # B2) L3 reach to the brain's listening sockets, i.e. the control plane (`brain compromise = host compromise`). Probing through Caddy keeps the trusted control plane off every app-reachable network — the brain only ever talks to Caddy, which it already does for routing and which is the one component designed to sit on that boundary — and it measures the user-visible truth ("does a request through the front door get a coherent answer"; a dead upstream surfaces as Caddy's own `502`). It is also robust to the latent shared-ingress-vs-per-app-bridge divergence (`APP_ISOLATION.md` describes per-app bridges with no inter-app traffic; the impl uses a shared ingress) — a probe that only talks to Caddy doesn't care how the app-facing topology evolves. Detector mechanics mirror the existing `container-restart-loop` loop: cross-cutting debounce (2 bad → raise, 1 good → clear), a start-period grace so warming-up apps don't flap, and probing only steady-running containers so crash-loopers surface as `container-restart-loop`, not `app-unresponsive`.
**Affected docs:** `HEALTH.md` (`app-unresponsive` un-deferred; detector added at locus C with measurement/cadence; locus label corrected from D; probe-through-Caddy + healthy-default + anti-flap prose), `APP_MANIFEST.md` (new `health_probe` field in # B + locked-decision bullet), `NEXT.md` (Tier-4 line removed). Contributor issue filed (`accepted`).

## 2026-06-02 — Public-API rate-limit posture: throttle-not-ban, three orthogonal planes, LAN-scoped

**Previously:** `BRAIN_UI_PROTOCOL.md` punted rate-limiting to a `NEXT.md` Tier-2 follow-up with four open questions (per-session limits, per-IP for unauthenticated routes, SSE-stream-count vs. request-rate budgets, 429 messaging). Only the two narrowest facets were specced and ticketed — the login throttle (`AUTH.md` # Rate limiting, issue #8) and the ≤16 SSE-stream cap (issue #47). The general posture was undecided, so it couldn't be handed to a contributor without inventing design answers.
**Now:** locked in `BRAIN_UI_PROTOCOL.md` # Rate limiting & abuse. Three orthogonal planes plus the existing login throttle: (1) **per-session request-rate** token bucket (120/min, burst 60) over authenticated short requests; (2) **per-IP request-rate** bucket (30/min) over the *unauthenticated* allowlist only, sitting above login's own stricter throttle; (3) **SSE-stream concurrency** (the ≤16 cap) kept as a *separate budget* from request rate — opening a stream doesn't draw from plane 1. `429` carries a stable `rate-limited` code + a `Retry-After` header so external callers back off correctly. Throttle-not-ban, in-memory, resets on restart.
**Why:** the framing question was "what threat?" malmo is closed-by-default, LAN + mesh only (`THREAT_MODEL.md` # B1), and DoS is an explicit non-goal (# Out of scope) — so this is **not** internet-scale DoS defense. The real risk is a runaway/buggy client or a compromised LAN device grinding a modest single-node box into resource pressure. That justifies cheap in-memory buckets over distributed/persistent machinery, and "log-don't-ban" over IP blacklisting (boxes mostly see LAN IPs). Keeping SSE concurrency and request rate as separate budgets answers the sharpest open question — a tight EventSource reconnect loop is bounded by the slot cap, not throttled as request volume. Deferred (kept as a `NEXT.md` line, not built): a per-session file-transfer concurrency cap for the streaming `/files/content` endpoints — real but not yet biting, and adding it now would be the one genuinely-new surface this posture introduces. This unblocks the general rate-limiting contributor issue (the design is now spec, not invention).
**Affected docs:** `BRAIN_UI_PROTOCOL.md` (new # Rate limiting & abuse section + locked-decision bullet + public-API-posture and knock-on updates), `AUTH.md` (# Rate limiting scoped to the login path + cross-reference), `NEXT.md` (Tier-2 item removed; Tier-4 file-transfer-cap line added). `THREAT_MODEL.md` deliberately untouched — it owns no mitigations and names DoS as an accepted residual risk; this throttling posture is consistent with that residual-risk acceptance, not a retraction of it.

## 2026-05-31 — In-dashboard file manager: ops execute as the user's UID in host-agent; own + Shared only

**Previously:** the in-dashboard file manager was an unwritten `NEXT.md` Tier-1 gap. "Files are first-class" was true on disk but had no in-product browse surface — the only specced access to a user's own content was SMB + a desktop file manager.
**Now:** specced as `FILES.md`. Three load-bearing calls: (a) **file operations execute inside host-agent, dropping to the requesting user's Linux UID/GID per operation** — the brain is policy + a transparent byte-pipe, never touching user content itself; (b) **scope is own `/home/<user>/` + the `Shared` tree for everyone, admins included** — no admin "browse all homes" view; (c) **thumbnails/preview deferred** (generic icons + download-to-view in v1).
**Why:** (a) the brain is containerized behind the docker-socket-proxy and cannot touch `/home` (`BRAIN_HOST_PROTOCOL.md` # Scope) — same constraint that puts physical health and `/proc` reads in host-agent. Running ops as the user's UID makes POSIX `0750`/`02770` the kernel-enforced backstop (authz bugs degrade to "denied," not "leaked"), gives created files correct ownership natively (same contract as the compose `user:` directive), contains symlink attacks for free, and is the exact shape fscrypt wants later. (b) Admin-over-all-homes would violate the already-locked fscrypt-forward discipline — `STORAGE.md` # Future: per-user encryption ("no admin-keyed cross-user search or indexing") and `APP_ISOLATION.md` # Privacy ceiling ("design as if fscrypt were already on"). Admin reach into other homes stays where it is: SSH/`sudo` rescue. (c) The rich Photos grid is an app's job (Immich) — "apps are windows." Transfers introduce one new wire shape (a streamed binary body), a deliberate exception to the ">5s = job" rule justified like SSE log tails (transport-native progress, no server-side job state).
**Affected docs:** `FILES.md` (new), `docs/README.md` (specs map), `NEXT.md` (Tier-1 item removed, Tier-4 data-import folded), `DASHBOARD.md` (Files dock destination de-qualified), `BRAIN_UI_PROTOCOL.md` (`/api/v1/files/*`), `BRAIN_HOST_PROTOCOL.md` (`/v1/files/*` + streaming body).

---

## 2026-05-31 — Live system-resources: host-agent owns the `/proc` read; chevron is a fourth locked top-bar element

**Previously:** `LOCAL_ANALYTICS.md` said the brain "reads `/proc` once per second" for the real-time view, and its top-bar dropdown was referenced only in passing — `DASHBOARD.md` # the top bar was **Locked: three elements** (storage pill, avatar, bell) and didn't list it.
**Now:** (a) the read is **host-agent's**, exposed as Pattern A `GET /v1/system/resources` (raw cumulative counters + monotonic `ts_ns`); the brain polls it once per second *while a UI is watching*, diffs to rates, and fans out over SSE `GET /api/v1/system/live`. (b) The live-resources chevron is a **fourth locked top-bar element**, available to every user. (c) The admin Settings → System deep-view is deferred (`NEXT.md`) so the all-users dropdown ships first.
**Why:** the brain runs in a container and cannot read host `/proc`/`/sys` — the same constraint that puts physical health detection in locus B (`BRAIN_HOST_PROTOCOL.md` # Health findings report); the old "brain reads /proc" wording was wrong. Pattern A polling (over Pattern C streaming) makes the "only while watching / zero idle cost" promise fall out for free: no subscribers → brain stops polling → stateless host-agent does nothing, with no start/stop lifecycle to manage. The stream is also exempt from `Last-Event-ID` replay (stale gauge samples are worse than useless) and counts against the ≤16-per-session SSE cap. Promoting the chevron to a locked element resolves a direct `LOCAL_ANALYTICS.md` ↔ `DASHBOARD.md` contradiction (chevron "next to the user menu" vs. a locked three-element list); recorded here so it reads as a deliberate reopening, not drift.
**Affected docs:** `LOCAL_ANALYTICS.md` (# Source, # Mechanism, # UI surfaces, # Cross-references), `BRAIN_HOST_PROTOCOL.md` (new `GET /v1/system/resources`), `BRAIN_UI_PROTOCOL.md` (new `GET /api/v1/system/live` SSE channel + no-replay/cap notes), `DASHBOARD.md` (# the top bar — fourth element), `NEXT.md` (Settings → System deep-view deferred).

## 2026-05-31 — `reboot-required` is locus B (host-agent reads the Debian flag), not C/A

**Previously:** `HEALTH.md` # Detector catalog placed `reboot-required` in the locus-A/D table as "C/A — `/var/run/reboot-required` present (post-update)."
**Now:** it's **locus B** — host-agent reads `/var/run/reboot-required` (+ `/var/run/reboot-required.pkgs` for the package list) on a relaxed cadence (1h) and reports it in `/v1/health/system`; the brain reconciles. Clears when the file is absent (self-clears on reboot — `/run` is tmpfs).
**Why:** the brain runs in a container and cannot read host `/run` (same constraint that moved clock-not-synced and the host half of service-down to locus B). And the flag is **Debian's**, not the brain's — `update-notifier-common` creates it when kernel/libc-class packages upgrade via apt/unattended-upgrades, not the brain's Docker app-update path — so the "brain tracks an update it applied" (locus C) framing the old label implied doesn't map. Host-agent reading the file is the only coherent locus. Notably this has **no unbuilt prerequisite** beyond the `/v1/health/system` transport (#34): Debian sets the file regardless of whether malmo's update mechanism exists, so a test just `touch`es it.
**Affected docs:** `HEALTH.md` (# Detector catalog — moved `reboot-required` from locus-A/D to locus-B; the # State-severity summary row is unchanged and locus-agnostic).

## 2026-05-31 — Caddy liveness is a self-heal trigger, not a banner (deferred on brain-owned Caddy lifecycle)

**Previously:** the `service-down`(caddy) locus-C check was framed like every other detector — detect, raise a health issue, surface a banner.
**Now:** because Caddy fronts `malmo.local`, a fully-down Caddy means there is **no dashboard to show the banner**. So Caddy liveness pairs detection with **bounded self-heal**: the brain restarts the Caddy container on failure, capped like host-agent's `StartLimitBurst` (≈5/60s), and raises `service-down`(caddy) only when the restart budget is exhausted (logged incident + post-recovery surface). The detector is **deferred** until its prerequisite exists.
**Why:** a passive banner is near-worthless for the one service whose death removes the surface that shows banners — the value is the box healing itself. But the brain does not yet own Caddy's **container** lifecycle (it manages Caddy's *routes* via the admin API — `EnsureServer`/`EnsureCatchAll` — not container start/stop/restart); in prod "brain-managed Caddy" is currently intent, not implementation. Building the self-heal detector before that infra exists would violate the "don't build host-integrated subsystems ahead of their infra" rule, so it's parked in `NEXT.md` with the dependency named. Detect-only-now was rejected as low-value; the prerequisite is the real next chunk.
**Affected docs:** `HEALTH.md` (# Detector catalog locus-C Caddy row + self-heal/deferral note), `NEXT.md` (# Caddy liveness self-heal — new Tier-3 entry naming the brain-owned-Caddy-lifecycle prerequisite).

## 2026-05-31 — `service-down` detection splits by locus: host units (B) vs Caddy container (C)

**Previously:** `HEALTH.md` # Detector catalog had one `service-down` row — "`systemctl is-active` over the core-unit allowlist" — whose summary listed Docker, **Caddy**, Avahi, chrony, Samba, host-agent.
**Now:** the host units (`docker`, `avahi-daemon`, `chrony`, `smbd`, `host-agent`) are checked by host-agent via `systemctl is-active` (locus B); **Caddy is checked by the brain via the Docker API + Caddy admin-API reachability (locus C)**. Both raise the same `service-down` issue with a per-unit `instance_key`; each reporter is authoritative only over its own keys (reconcile rule added to `HEALTH.md`).
**Why:** `CONTROL_PLANE.md` locks Caddy as a **brain-started container, not a systemd unit** — there is no `caddy.service` for `systemctl is-active` to query, so the original row was unbuildable for the Caddy entry. Wrapping Caddy in a systemd unit was rejected: it would flip the locked "Caddy runs as a container, updates on the brain+UI stream" decision and create dual ownership of Caddy's lifecycle (systemd vs brain). The brain already owns Docker access and Caddy's admin API, and a brain-side check is *strictly better* — it verifies Caddy is **serving** (admin API answers / catch-all route present), catching a wedged-but-not-exited Caddy that process-liveness would miss. The socket-proxy is not separately monitored; its failure surfaces as the brain losing all Docker access at once.
**Affected docs:** `HEALTH.md` (# State summary row, # Detector catalog locus-B row scoped to host units + new locus-C Caddy row + per-reporter reconcile rule).

## 2026-05-31 — `clock-not-synced` is detected by host-agent (locus B), not the brain

**Previously:** `TIME.md` # Drift monitoring read "Brain polls `chronyc tracking` once a minute." Written before the health-detector locus model landed.
**Now:** host-agent samples `chronyc tracking` every 5 minutes and reports findings; the brain reconciles. Detection is **locus B** per `HEALTH.md` # Detector catalog, and findings ride the generalized `GET /v1/health/system` report. The "Force sync now" action (`chronyc -a makestep`) likewise runs in host-agent, driven by the brain.
**Why:** the 2026-05-29 detector catalog established that the brain runs in a container behind the Docker socket-proxy and **cannot exec host tooling** — "no `smartctl`, no `systemctl is-active`, no `chronyc`." The old `TIME.md` line contradicted that fact directly; a contributor following it would build the poll in the brain and hit the socket-proxy wall. Cadence also reconciled: `HEALTH.md`'s 5 min wins over `TIME.md`'s 1 min — clock drift is slow and the lighter poll is sufficient. Thresholds (6h-since-sync OR offset >10s) already agreed across both docs and are unchanged.
**Affected docs:** `TIME.md` (# Drift monitoring rewritten to host-agent/5 min), `HEALTH.md` (already correct — the source of truth here).

## 2026-05-31 — App LAN URLs go single-label `<slug>.local` (was `<slug>.malmo.local`)

**Previously:** every app instance was reachable on the LAN at `<slug>.malmo.local` — e.g. `photos.malmo.local`, `immich--alex.malmo.local`. `DISCOVERY.md` justified the double-dash slug shape as "single-label … resolves on every mDNS client (multi-label `.local` does not)" and asserted Linux desktops resolve it with `nss-mdns` and need "nothing to do."

**Now:** apps are reachable at single-label `<slug>.local` — `photos.local`, `immich--alex.local`. The box/dashboard stays `malmo.local`; the `<slug>.<box-id>.malmo.network` HTTPS scheme is unchanged. On an Avahi name collision the publisher retries once with a box-qualified fallback `<slug>-<box>.local` (e.g. `photos-malmo.local`) and the brain uses the *returned* name for the URL it shows and the Caddy route it writes.

**Why:** the old shape never resolved on Linux. `<slug>.malmo.local` is multi-label relative to `.local`, and `nss-mdns` rejects any name with a dot before `.local` outright (verified empirically on a dev box: instant NXDOMAIN in ~23ms, no network query; a single-label name published by the same Avahi resolved fine via the same `getaddrinfo` path). `systemd-resolved`'s mDNS rejected it too. So the foundational, no-cloud LAN URL — the thing `.local` exists to provide — was broken on Linux and the dev loop, exactly the early-adopter (tinkerer) base. The `.malmo` infix bought nothing in return: mDNS (RFC 6762) has no zones, delegation, or wildcards, so `<slug>.malmo.local` is not a subdomain of `malmo.local` — it's a flat name that merely *contains* dots, published individually like any other. Dropping the infix is the smallest change that makes the friendly-name default actually resolve, while preserving browser origin-isolation (each app keeps a distinct host) and the `<slug>` prefix symmetry with the `.malmo.network` scheme. Competitor research backs the difficulty: Umbrel, ZimaOS, and Synology all avoid per-app `.local` subdomains entirely (ports on one single-label host for LAN; real DNS + reverse proxy for named external access) — per-app `.local` names are a genuine differentiator, but only if single-label. Ports remain the spec's opt-in fallback (`SPEC.md` # Optional port-based routing) for LANs where even single-label mDNS is flaky. The flat-namespace collision risk (`photos.local` vs a printer) is handled by the `<slug>-<box>.local` fallback plus Avahi's RFC 6762 conflict-resolution.

**Affected docs:** `DISCOVERY.md` (# Per-app A records rewritten to single-label + collision fallback; client-compat matrix corrected — Linux was the casualty, not "nothing to do"). `MALMO_NETWORK.md` (# URL-scheme table). `SPEC.md` (# LAN routing examples; the multi-name-mDNS cost note is now categorical-on-Linux, not edge-case). `DASHBOARD.md`, `APP_LIFECYCLE.md` (URL examples; reconciler binds Caddy to the published name). `AUTH.md` + `THREAT_MODEL.md` (cookie-`Domain` warnings restated for the single-label scheme). Mechanical example updates in `APP_ISOLATION.md`, `BRAIN_HOST_PROTOCOL.md`, `SERVICE_PROVISIONING.md`, `CONTROL_PLANE.md`, `BUILD.md`. Code: `internal/protocol` (`AppHostSuffix` constant), `internal/hostagent/avahipublisher` (collision fallback), `internal/lifecycle` + `internal/api` (trust the published name). Progress: `single-label-app-local.md`.

---

## 2026-05-31 — App hostname encodes uniqueness, not ownership

**Previously:** `DASHBOARD.md` # instance naming specified that the hostname encodes scope — bare `<slug>` for household, `<slug>--<user>` for personal. "Ownership is legible in the URL itself" was a stated pro. Every personal instance was always suffixed, even on a single-user box where no disambiguation was necessary.

**Now:** The bare `<slug>` is **first-come, any scope.** The first instance of any app installed wins the clean name (`immich.local`) regardless of whether it's household or personal. Collisions use the `--<user>` suffix for personal instances (no owner name otherwise available) and a numeric suffix (`-2`, `-3`) for household instances. The hostname encodes uniqueness, not ownership; scope and owner are surfaced in the dashboard grid (Household / Yours grouping + owner label on the tile).

**Why:**
- **Single-user boxes got noisy hostnames for no reason.** With the old rule, a single-admin household installing Immich as a personal instance got `immich--admin.local` — a suffix signaling a disambiguation that never happened. The natural name is `immich.local`.
- **First-come is the global DNS model.** DNS registrations are first-come regardless of who registered them; routing correctness doesn't require the hostname to encode the registrant's identity.
- **Ownership belongs in the dashboard, not the URL.** The dashboard already groups apps into Household / Yours and labels the owner — that's the right surface for ownership legibility. The URL needs to be unique and stable; it doesn't need to be inferrable.
- **Reduces the mDNS username leak.** Under the old rule every personal instance leaked `username ↔ app` to the LAN via its mDNS record. Under the new rule bare names (the common case on a lightly loaded box) reveal nothing about scope or ownership; the `--<user>` suffix appears only when a second instance of the same app actually exists and disambiguation is necessary.

**Note:** The `<slug>--<user>` separator and flat-label constraints from the 2026-05-29 decision still hold — the separator rationale (kebab ambiguity), the catalog slug and username `--` prohibition, and the `xn--` guard are all unchanged. What changes is which cases actually trigger the suffix.

**Affected docs:** `DASHBOARD.md` (# instance naming table + bullets rewritten; pros/cons updated; privacy leak note revised); `DISCOVERY.md` (# Per-app A records — slug description updated); `APP_LIFECYCLE.md` (# slug derivation note); `APP_ISOLATION.md` (# Routing per instance). Code: `internal/lifecycle/lifecycle.go` (`allocateSlug` now tries bare first for any scope, appends `--<user>` on personal collision, numeric on household collision). Progress: `hostname-uniqueness-not-ownership.md`.

---

## 2026-05-31 — Single-user simplification: suppress household/personal UI when only one user exists

**Previously:** the dashboard always rendered Household/Yours section groupings, app tiles always showed a "Shared"/"Personal" label, and the install consent dialog contained a scope radio picker (admins saw "For the whole household" / "Just for me"; members saw a fixed "Installing as a personal app" message). The picker was inside the dialog regardless of how many users the box had.

**Now:** when `single_user_mode` is true (one registered user), the household/personal distinction is suppressed everywhere: section headers hidden, tile scope label hidden, Settings owner label hidden, install button is a plain button (no split-button/chevron). The scope radio is removed from the dialog entirely for everyone; scope is pre-decided by which button variant the user clicked before the dialog opens. For admins on a multi-user box the store row shows a split-button: primary = personal (just for me), dropdown = household. The shared folder source label is relabeled from "The household's shared X" to "Shared X (accessible from your other devices)" when `single_user_mode`, since "household" is confusing without a second user even though the Samba use-case (cross-device file access) is valid solo.

**Why:**
- A single-user box has no audience for the household/personal distinction. Showing the grouping, the label, and the picker is noise that adds cognitive load and implies a multi-user model the user hasn't entered yet.
- Moving scope selection out of the dialog and onto the Install button (split-button pattern) is cleaner for multi-user too: the user decides who the app is for before entering the consent flow, rather than being asked mid-flow.
- The "just for me" action is the dominant action (personal scope is the silent default) so it gets the primary button slot; "for the whole household" is secondary and gets the dropdown.

**Affected docs:** `DASHBOARD.md` (new # Single-user simplification section; install-flow description updated); `BRAIN_UI_PROTOCOL.md` (`single_user_mode` on session-bearing responses; scope-picker note on install-plan). Code: `internal/store` (`UserCount`), `internal/api/auth` (`fullUserDTO`, `single_user_mode` on `/me`+`/login`+`/setup`), `web-ui` (`SplitButton.vue`, `InstallDialog` scope prop, `singleUserMode` in `useAuth`, home grid headers, tile label, settings label, shared folder relabel). Progress: `single-user-simplification.md`.

---

## 2026-05-30 — Folder source is installer-elected; `user_folders` / `shared_folders` collapse into one `folders` declaration

**Previously:** the permission schema reconciled earlier the same day (entry below) declared content access through two keys that *encoded the source in the key name*: `user_folders` (bound the owner's `~/<Folder>/`) and `shared_folders` (bound `/srv/malmo/shared/<Folder>/` + `malmo-shared` group). The author chose the source by choosing the key; a personal instance could not read household-shared content (an explicit `APP_ISOLATION.md` MVP carve-out, deferred in `NEXT.md`). The consent model was specced as "inputs = scope + pick-subfolder + acknowledgements only."

**Now:** the two keys collapse into a single `permissions.folders: [{folder, mode, scope, default}]` declaration. The author declares *what* content the app touches (folder + mode + subfolder granularity); **the host source — personal `~/<Folder>/` vs household `/srv/malmo/shared/<Folder>/` — is the installer's per-folder choice at install time:**
- A **personal** instance offers, per folder, the owner's folder (default) or the household Shared folder. Choosing shared adds the container to `malmo-shared` (compose `group_add`) — exactly what the owner can already reach as a member, no new privilege.
- A **household** instance is always the Shared folder (no single owner whose `~/` it could bind); no per-folder toggle.

This makes source a fourth consent input (alongside scope, pick-subfolder, acknowledgements) — a natural sibling of pick-subfolder, since both answer "where does this folder point." It also **resolves** the per-user/shared boundary carve-out: a personal instance reading the shared tree is now a supported election. The boundary still holds where it matters — a shared instance never binds one member's private `~/`.

**Why:** the author genuinely cannot know a given household's intent. "I want *my own* Jellyfin on *my* movies" and "I want it on the *family* library" are both valid, and the app's code is identical either way — source is a deployment choice, not a property of the app. Encoding it in the manifest key forces the author to guess and forecloses the other valid use. Crucially this does **not** violate the non-technical-user north star: "your movies or the shared family movies?" is a human question a Plex/Synology-grade user understands, not the infra-flavored "which mount path" question we avoid. The collapsed `folders` key is also strictly more expressive — an app can declare two folders and the installer points each at an independent source (shared movies in, personal downloads out). Schema churn is cheap now: `internal/manifest` parsed only `internet`/`lan`, so nothing downstream depended on the split yet.

**Affected docs:** `APP_MANIFEST.md` (# E permissions block, # `folders` subsection replacing the `user_folders` + `shared_folders` subsections, both sample manifests, Locked decisions). `APP_ISOLATION.md` (# User content rewritten to personal-vs-shared source election + the boundary-carve-out resolution; high-level toggles table). `internal/manifest` (`Permissions.Folders`/`Devices`/`GPU` + taxonomy/mode/scope validation; tests). Implementation of the install-time source election (install-plan endpoint, `writeOverride` `group_add`/`user:`, consent UI) is the follow-up slice.

---

## 2026-05-30 — App permission schema reconciled across `APP_MANIFEST` / `APP_ISOLATION`; `user_folders` mount convention locked

**Previously:** the permission-bearing manifest fields were specced inconsistently across three docs and barely implemented (`internal/manifest` parses only `internet`/`lan`). The drifts: (a) GPU was `devices: [/dev/dri]` in `APP_MANIFEST.md` but a separate `gpu: true` field in `APP_ISOLATION.md`; (b) `APP_ISOLATION.md` offered a reviewed `permissions.capabilities` escape hatch while `APP_MANIFEST.md` + `APP_LIFECYCLE.md` admission forbid any `cap_add` for store apps; (c) `APP_MANIFEST.md` # G declared a `multi_user: shared | per_user` field while `APP_ISOLATION.md` said "per-user by tier, no field" and `DASHBOARD.md` (2026-05-29) said scope is installer-elected; (d) `APP_ISOLATION.md`'s multi-user runtime described clean per-session `<app>.malmo.local` routing and `malmo-<app>-<user-slug>` container names, both superseded by the owner-scoped `<slug>--<user>` / `malmo-<instance-id>` model; (e) `user_folders` declared *which* folder but nothing said *where inside the container* it mounts.

**Now:** one reconciled schema, with `APP_MANIFEST.md` as canonical and `APP_ISOLATION.md` as its enforcement companion:
- **`gpu: true` is its own field**, separate from `devices` (explicit `/dev/...` passthrough). No-GPU box fails at the capacity check.
- **No added Linux capabilities for store apps**, period — `cap_drop: [ALL]`, admission rejects `cap_add`. Capability/`privileged`/Docker-socket needs go through Door-2 or Tier 2. A reviewed store `permissions.capabilities` list is *deferred*, not in the v1 schema.
- **Scope is installer-elected, not a manifest field** — the `multi_user.mode` field is removed. Admins choose household or personal; members install personal only. This confirms `DASHBOARD.md` (2026-05-29) into the manifest schema and the isolation model. Guest-sharing and per-app household visibility are deferred and are not manifest fields.
- **`user_folders` mounts at a fixed path + injected env var:** the brain bind-mounts each declared folder at `/malmo/<folder>` and injects `MALMO_FOLDER_<NAME>`; the app's compose maps that to its own library path. Same injection convention as `MALMO_SERVICE_*` / `MALMO_DATA_DIR`. The manifest stays declarative (folder + `mode` + `scope`, `mode` default `read`); the brain resolves the source side (the owner's `~/<Folder>/`, plus UID/home from host-agent) at install. This closes the hole that made the bind un-generatable.

**Why:** the next implementation slice is the install consent/config flow, which renders directly off these fields and whose `writeOverride` must emit the bind mounts — it can't be built on a schema that three docs describe three ways. `APP_MANIFEST.md` is the published, versioned contract, so it wins as canonical; `APP_LIFECYCLE.md` admission is already implemented and emphatic on capabilities, so that position holds and `APP_ISOLATION.md`'s escape hatch yields. The fixed-path + env-var mount mirrors the injection pattern authors already follow for services and data dirs — least new surface, keeps apps portable. Consent model is **all-or-nothing** (the install screen shows declared permissions; accept or cancel) — granular per-permission control is the separate post-install revocation feature (`NEXT.md`), not install-time toggles.

**Affected docs:** `APP_MANIFEST.md` (# E permissions block + `gpu`/capabilities prose; # `user_folders` mount convention + `mode` default; # G rewritten to installer-elected; both sample manifests; Locked decisions). `APP_ISOLATION.md` (# Multi-user runtime → Owner-scoped instances, routing, no-`multi_user`-field; capabilities escape-hatch reframed as deferred; `user_folders` mount + env var; app-state path corrected to `/var/lib/malmo/instances/<id>/data/`). `NEXT.md` (two new open items: deferred store `capabilities` escape hatch; Door-2 vs. Door-1 admission asymmetry). No code yet — implementation (parse the fields, render the install plan, enforce in `writeOverride`) is the follow-up slice.

---

## 2026-05-29 — App instances are owner-scoped; shared is an admin-elected mode (+ the `<slug>--<user>` naming scheme)

**Previously:** `SPEC.md` # Accounts & users said *"single shared app instance is the default for shared use cases"* — one Immich, one grocery list, every household member logs into the same instance. Per-user instances were a deferred Tier-3 concept (`APP_LIFECYCLE.md:12`), and `NEXT.md` carried the dashboard apps model and the per-user-instance hostname scheme as open items.

**Now:** **Every app instance has an owner.** A *household* instance is admin-owned and shared (one instance, app-internal multi-user separates people inside it). A *personal* instance is owned by one user, with its own data dir, route, managed-service DB, and folder bindings (it binds the owner's `~/` folders only). Admins choose Household or personal at install; members can only create personal instances. Duplicate installs **warn, don't block** ("Jellyfin is already installed as a household app — install your own copy?"). This is now the v1 model, owned by the new `DASHBOARD.md`, not deferred.

The per-user instance **naming scheme is locked**: bare `<slug>` for household, `<slug>--<user>` (double-dash) for personal — flat, single-label.

**Why:**
- **Files-first-class forces it.** A single shared Immich would have to read every user's `~/Photos`, violating the per-user `0750` isolation in `STORAGE.md`. A personal Immich binds only its owner's `~/Photos`. Owner-scoping *resolves* the tension instead of creating it. The user's own example — "my photo backup ≠ my partner's" — is the canonical case.
- **The plumbing already exists.** Per-user instances are exactly `APP_LIFECYCLE.md:12`'s N-compose-projects shape, and managed services are already per-instance (`SERVICE_PROVISIONING.md`: a DB + role per instance inside the shared Postgres server). The marginal cost over the shared-only floor is an owner column + an install guard + a naming decision + a two-group grid — not a new subsystem. Building shared-only first would bake single-tenant assumptions into the grid, install flow, and storage bindings, making this a painful retrofit.
- **It's the differentiator.** A May-2026 scan (Umbrel single-user; ZimaOS apps owner-bound; TrueNAS multi-instance is manual admin charts; Synology multi-instance is manual Docker) shows *nobody* ships identity-driven per-user app instances — everyone punts on the routing/cert/DB plumbing malmo already has. Squarely on "app ecosystem is the strongest pillar."

**Why `<slug>--<user>` and not the prettier `<user>.<slug>`:** the cert architecture decides it. `MALMO_NETWORK.md` (27, 138–139) issues **one** wildcard cert `*.<box-id>.malmo.network`, and a TLS wildcard spans exactly one label. `immich--alex.<box-id>…` is one label (covered); `alex.immich.<box-id>…` is two (not covered → a per-app wildcard issued on every install, killing the "one cert, renew quietly" model). mDNS agrees — multi-label `.local` names resolve inconsistently on Android/Windows. Double-dash because kebab-case slugs make a single `-` ambiguous. Constraint: slugs/usernames may not contain `--` or produce an `xn--` prefix.

**Deferred (unchanged):** per-user resource quotas (surfaced as a tightness *warning*, not a cap), cross-user shared-folder access for personal instances (`APP_ISOLATION.md` carve-out), and SSO (which would later make shared the *encouraged* path for multi-user-capable apps).

**Affected docs:** `DASHBOARD.md` (new — owns the apps model, naming scheme, and the logged-in IA); `SPEC.md` # Accounts & users (flipped from "shared is the default" to "instances are owner-scoped"); `docs/README.md` (Frontend list); `NEXT.md` (Tier-1 dashboard-shell item removed; Tier-2 first-arrival folded; control-plane per-user-instance-hostname open item resolved; per-user-instance work re-tiered from deferred to v1); `APP_LIFECYCLE.md` + `DISCOVERY.md` (slug-derivation note).

---

## 2026-05-29 — Health detector catalog: who measures what, and the SMART-don't-block call

**Previously:** `HEALTH.md` locked the issue *model* and listed ~15 issue *types* in a taxonomy table, but never said how any of them is detected — it deferred debounce/retry policy to "inside each detector" and named no thresholds, cadences, or data sources. The concrete check set was a `NEXT.md` Tier-4 open item.

**Now:** `HEALTH.md` gains a **Detector catalog** that binds every issue to one detector with an execution locus (A boot-reporter / B host-agent-periodic / C brain-periodic / D reactive), a measurement, cadence, threshold, and clear condition. Six new issue types land from the exercise: `disk-smart-failing`, `service-down`, `container-restart-loop`, `ram-pressure`, `reboot-required`, `journal-disk-pressure` (plus `auto-unlock-degraded` from the TPM work and a registered-but-deferred `app-unresponsive`). Two cross-cutting defaults are pinned: **raise on 2 consecutive bad samples / clear on 1 good** (authoritative boot+reactive signals exempt), and **hysteresis** (raise and clear thresholds differ) on all percentage-threshold issues.

**Two non-obvious sub-decisions:**

1. **A failing drive (`disk-smart-failing`) warns loudly but does NOT block writes.** The instinct is to treat a dying drive like `data-drive-readonly` and refuse writes. That's backwards: a SMART-failing drive is usually still *readable*, and blocking writes traps the user's data on the drive they most need to copy *off*. So: `error` severity, loud banner, notification — zero block flags.
2. **Host system health is one report, not many endpoints.** The brain can't read host hardware (it's containerized behind the socket-proxy), so all physical measurement is host-agent's. The existing single-purpose `GET /v1/health/storage` generalizes to one `GET /v1/health/system` carrying findings across domains; `ApplyStorageFindings` generalizes to `ApplyFindings(category, …)` with per-category reconcile so a storage poll can't clear a service finding.

We also wrote down the **non-goals** — email deliverability, public-port scans, public-DNS/IPv6 checks, fail2ban status, kernel-panic capture — because they're core to public-facing neighbors (Yunohost's `diagnosis`) and a "parity" PR would otherwise import them into a closed-by-default box where they make no sense.

**Why:** the implementation reached the point where notifications are derived from health-issue transitions (`docs/progress/README.md` # Up next) — the emitter has nothing to emit until the detectors exist, so the enumeration stopped being deferrable. Yunohost's `diagnosis` taxonomy was the prior-art reference; the locus split is malmo-specific (driven by the containerized brain).

**Affected docs:** `HEALTH.md` (new # Detector catalog section; taxonomy tables extended to ~22 issues; capacity section tabularized); `NEXT.md` (Tier-4 enumeration item removed; folded-in children re-filed); `BRAIN_HOST_PROTOCOL.md` (knock-on note for the generalized `/v1/health/system` report). Implementation follows as brain `internal/health` registry additions + host-agent reporters.

---

## 2026-05-24 — Per-app A records via Avahi DBus, not static service files

**Previously:** `DISCOVERY.md` and `docs/progress/0012-host-agent-avahi-files.md` specified that per-app A records are published by writing `/etc/avahi/services/app-<slug>.service` XML files. The rationale was that static files survive daemon restarts without replay, avoiding the need for host-agent to track groups across restarts.

**Now:** Per-app A records are published via `org.freedesktop.Avahi.EntryGroup.AddAddress` (DBus). Static service files are withdrawn as the mechanism for this use case.

**Why:** Tested against a real Avahi 2026-05-24. Static service files announce *services*, not bare A-record aliases. File loaded without error (`avahi-daemon` logged "Service ... successfully established") but `avahi-resolve -n <slug>.malmo.local` timed out — Avahi does not synthesize an A record for a `<host-name>` inside a service-group file without first owning that hostname through its own probing/announcing cycle. The XML schema reference allows the field but the runtime does not act on it. `EntryGroup.AddAddress` is the only documented programmatic path for publishing a raw A record on behalf of an arbitrary hostname.

The restart-durability argument for static files was correct in principle but moot in practice: static files cannot do the job at all.

**New tradeoff:** DBus entry groups are process-local. They are lost on host-agent restart. The brain re-publishes all running instances via the startup reconcile (`lifecycle.Reconcile`), which already calls `host.Publish` per running instance. Mid-life host-agent restart while the brain is running is a known gap — tracked in `docs/progress/avahi-dbus-publisher.md`.

**Forward consequences:** brain owns more replay logic; the nspawn CI lane (future slice) is the place to verify "Avahi accepted our AddAddress" end-to-end.

**Affected docs:** `DISCOVERY.md` (§ "Per-app A records" rewritten; install-transaction list updated); `docs/progress/avahi-dbus-publisher.md` (new, replaces `0012`); `docs/progress/README.md` (index updated).

---

## 2026-05-18 — Avahi + per-app mDNS records as the LAN discovery model; `.local` is a desktop story

**Previously:** `MALMO_NETWORK.md` committed to subdomain URLs (`photos.malmo.local`) and a two-scheme model (`.local` HTTP default, opt-in `<box-id>.malmo.network` HTTPS) but never specified *how* those `.local` names resolve on the LAN. No publisher daemon chosen, no record-publication mechanism, no handling of multi-app subdomains, no acknowledgement that `.local` doesn't work on Android. Implicit assumption: "we'll figure out mDNS later."

**Now:**

1. **Avahi is the publisher** — `avahi-daemon` enabled at image-build; systemd-resolved's mDNS responder disabled. Avahi publishes service records (the things Finder/Explorer key off), Samba already integrates with it for SMB/TimeMachine advertisement, and it's what every neighbor (Umbrel, Synology, TrueNAS) runs.
2. **Per-app A records are the only viable subdomain mechanism.** mDNS has no wildcard support and CNAME-following is too inconsistent across resolvers (Android's NSD especially) to rely on. Each installed app gets a static service file at `/etc/avahi/services/app-<slug>.service` written by the install reconciler — the *same* transaction that writes the Caddy site block. Install/uninstall keeps the two in lockstep. The dashboard does not mark an app `ready` until Avahi reports `EntryGroup.StateChanged → ESTABLISHED`.
3. **Avahi is scoped to LAN interfaces only.** host-agent computes the allow-list from NetworkManager state (eth/wlan in, `tailscale0` / `docker0` / `br-*` out) and writes the Avahi config fragment at boot and on interface change.
4. **`.local` is a desktop URL scheme.** Works on macOS, iOS, Windows (with Bonjour installed), and Linux desktop. **Does not work on Android browsers** — NSD is an app-level API, not a system resolver, and `getaddrinfo("photos.malmo.local")` returns NXDOMAIN. This is by Google's design (battery, multicast-on-WiFi cost, cloud-mediated discovery preference) and there is no workaround at malmo's layer. The `<box-id>.malmo.network` HTTPS path from `MALMO_NETWORK.md` is therefore not a "premium" feature — it is the **compatibility path** for an entire OS family, and first-run nudges Android households toward it explicitly.

New doc: `DISCOVERY.md`.

**Why each landed where it did:**

- **Avahi over systemd-resolved's mDNS:** systemd-resolved is hostname-only — no service records. Finder / Explorer / TimeMachine all need service records (`_smb._tcp`, `_device-info._tcp`, etc.); Samba already integrates with Avahi. Running both would mean two responders multicasting on the same socket. One responder, full feature set.
- **Per-app A records, not wildcards or CNAMEs:** mDNS (RFC 6762) has no central server and no zone — wildcards are undefined and unimplemented. CNAME-following works on Apple, mostly on Bonjour, inconsistently on systemd-resolved, and unreliably on Android's NSD; same reconciler work as A records for strictly worse compatibility. Host-header routing at Caddy is moot because the name has to resolve before the HTTP request is built.
- **Static service files, not `avahi-publish` processes:** Avahi watches `/etc/avahi/services/` and handles re-announcement on link-up and IP change automatically. One file write per app, durable across daemon restarts. `avahi-publish` would need a supervised process per name with no upside.
- **LAN-only scoping:** announcing on the Headscale mesh interface fights MagicDNS (which is the mesh's naming model); announcing on Docker bridges leaks per-container names back into the host's mDNS namespace. Both are wrong; both are easy to prevent with a config allow-list.
- **`.local` as HTTP-only by definition:** Let's Encrypt requires public DNS; `.local` has none. Shipping a private CA and installing it on every client device is the wrong shape for our audience. State the constraint instead of fighting it — HTTPS goes via `<box-id>.malmo.network`.
- **Android compatibility framed honestly:** the "Use secure URLs" toggle was previously described as a security/convenience choice. After looking at it carefully, the load-bearing reason it exists is "your partner has an Android phone." Surface that in first-run; don't bury it.

**Sharp edges:**

- **AP isolation / client isolation on consumer routers silently breaks mDNS.** Common on guest networks, occasional on default home configs. Diagnostic bundle includes a multicast probe so support tickets can be triaged. No fix at our layer — affected users have to disable client isolation on the router or switch to secure URLs.
- **`.local` collision with Active Directory.** Some SOHO networks use `.local` as an internal AD domain and the unicast resolver swallows queries before Avahi gets asked. Rare for our audience but real; documented, with secure URLs as the workaround.
- **Hostname conflicts.** RFC 6762 §9 conflict-resolution renames a duplicate to `malmo-2.local`. The dashboard surfaces a new typed health issue `hostname-conflict` and prompts the admin to pick a new hostname.
- **Two URL schemes, two access models in users' heads.** We are explicitly accepting this complexity — single-scheme proposals (always secure, or always `.local`) were briefly considered and both impose worse failure modes than the toggle. Tracked in `NEXT.md` # the "URL scheme unification" topic.

**Affected docs:** new `DISCOVERY.md`; `MALMO_NETWORK.md` (Android-compatibility framing for the secure-URLs toggle, cross-ref); `APP_LIFECYCLE.md` (install transaction adds Avahi service-file step alongside Caddy; readiness gate on `ESTABLISHED`); `FIRST_RUN.md` (Android-household nudge in the wizard, Windows-Bonjour link); `HEALTH.md` (new `hostname-conflict` typed issue); `BOOT.md` (`avahi-daemon.service` ordering, off the critical path); `BRAIN_HOST_PROTOCOL.md` (host-agent computes Avahi interface allow-list from NM state); `LOGGING.md` (multicast probe in the diagnostic bundle); `STORAGE.md` (Samba/Avahi cross-ref); `NEXT.md` (discovery open items added); `CLAUDE.md` (`DISCOVERY.md` in document list, new load-bearing decision).

---

## 2026-05-18 — NetworkManager for the whole network stack; WiFi is first-class in first-run

**Previously:** `BOOT.md` cited `systemd-networkd-wait-online` and a "primary ethernet interface" with `RequiredForOnline=routable`. `FIRST_RUN.md` Step 1 was a few bullets ("DHCP by default, Ethernet recommended, WiFi allowed with a warning"). No mechanism for picking an SSID, entering a password, or switching networks later. `BRAIN_HOST_PROTOCOL.md` had a single bullet — *"Network configuration — DHCP vs. static IP, primary interface detection"* — with no endpoints sketched. `NEXT.md` Tier 1 carried "Wifi + first-run network setup" as an open item.

**Now (one decision, three doc impacts):**

1. **NetworkManager owns every interface on the box** — ethernet, WiFi, future bridge/VPN. Not systemd-networkd. NM's internal `dnsmasq` plugin is off; `unmanaged-devices` excludes Docker's bridges/macvlan parents; WiFi MAC randomization is off (DHCP reservations need stable MACs).
2. **The "primary interface" concept becomes the primary *connection*** — exactly one NM connection profile carries `connection.required-for-network-online=true` at a time. host-agent owns the pin; the user can flip it from the dashboard.
3. **First-run Step 1 branches on link state.** Ethernet + DHCP = silent confirm. No carrier or WiFi-only = SSID picker with password entry, hidden-network affordance, inline retry on bad password / DHCP fail. Ethernet recommended but not required — the warning surfaces honestly ("discovery-based smart-home apps work much better on Ethernet"), not as `.local`-doesn't-work FUD.

**Why each landed where it did:**

- **NetworkManager over systemd-networkd:** WiFi is a first-class supported case (laptop-in-the-pantry is the canonical install). networkd doesn't do WiFi — it does IP-layer config after a separately-managed `wpa_supplicant` brings the link up. That's two daemons and our own integration code for scan/connect/state, with no DBus surface for SSID listing. NM absorbs the whole stack (supplicant integration, credential storage, roaming, hidden SSIDs, captive-portal detection) and exposes a clean DBus API the host-agent can drive in Go. Footprint argument (~30 MB) is irrelevant at malmo's hardware target.
- **Split-brain rejected:** "NM for WiFi, networkd for ethernet" was briefly on the table. Two daemons fight over the routing table, DNS resolution, and `network-online.target`. Standard practice and the right call: one daemon owns the network stack.
- **Ethernet is recommended, not required:** the failure mode for WiFi is `lan: true` smart-home discovery (macvlan + multicast unreliability on consumer APs), *not* mDNS — `.local` works fine on WiFi (the Apple ecosystem depends on it). The wizard's WiFi warning text reflects the actual cause; "WiFi breaks `.local`" would have been wrong and confused users whose phones happily resolve `malmo.local` over WiFi.

**Sharp edges:**

- **WiFi MAC stability matters for DHCP reservations.** NM's default would randomize per-SSID; we override. Tinkerers who want randomization can flip it per-connection from the network panel.
- **`lan: true` apps on a WiFi-primary box install but are flagged `discovery-on-wifi-degraded`** — per-app warning, not a global block. Users with no ethernet jack still get Home Assistant; they just see honest framing about the limitation.
- **Captive portals are explicitly out of scope.** The first-run wizard can't navigate one; the failure mode is "stuck on DHCP" with a hint. Acceptable for the target install context (home network), not for hotels/dorms.
- **Enterprise WPA (802.1X) deferred.** Out of scope for v1; comes back when there's a credible university-pilot use case.

**Affected docs:** `BOOT.md` (# `network-online.target` section rewritten; new # NetworkManager-owns-the-network-stack subsection; NM unmanaged-devices list, dnsmasq-off, MAC-stability notes); `FIRST_RUN.md` (Step 1 rewritten with link-state branching, SSID picker UX, honest ethernet-recommendation framing); `BRAIN_HOST_PROTOCOL.md` (Network configuration bullet expanded; new Pattern A endpoint sketch for `/v1/network/*` over NM DBus; SSE on `/v1/network/events`; WiFi credentials live in NM's store, not the brain; new locked decision); `NEXT.md` (Tier 1 "Wifi + first-run network setup" item closed); `CLAUDE.md` (load-bearing decision added).

---

## 2026-05-17 — App store as a signed static catalog; app update UX; pre-update snapshot as the v1 migration safety net

**Previously:** Three connected gaps — (1) no spec for how the app catalog reaches the box or what the box trusts; mentions of "third-party stores" in `APP_MANIFEST.md` without an infra shape; (2) `UPDATES.md` had the update *mechanics* for apps but not the *interface* (Tier 2 in `NEXT.md`); (3) app-driven schema migrations had no rollback story — image-only revert leaves old code running against migrated data, and hooks (the intended fix) were deferred.

**Now (three coupled decisions):**

1. **App store is a signed static JSON catalog**, sibling to `RELEASE_MANIFEST.md` in shape. Git-repo source of truth (`malmo/store`), CDN serving (`store.malmo.network`), minisign/Ed25519 signature, pubkey baked into the brain image (verifier accepts a list for forward-compat rotation), separate signing key from the release manifest. Verification lives in the **brain**, not host-agent — the catalog is about app lifecycle, which is the brain's domain. Authors declare `image: foo/bar:1.2.3` in compose; CI resolves digests at catalog-build time and writes them into the signed catalog. Per-app manifest + compose files are bound to the catalog by content hash (one signed root, hash-chained leaves). We don't host container images — local image cache delivers the "app keeps working if the developer disappears" property; mirroring is deferred. v1 catalog is hand-curated by malmo; the brain's data model supports multiple catalogs from day one, but UI ships for one. New doc: `APP_STORE.md`.

2. **App update UX:** per-app tile badges + Settings → Updates aggregate view + auto-dismissing post-batch toast. The permission-expansion prompt is the only modal — surfaces on **next login of the instance owner**, blocks the app until they decide, accepted updates apply **immediately** (not at 03:00) since the user just initiated. Admins get read-only visibility into other users' pending updates in Settings → Users; they cannot accept on another user's behalf. New `changelog_url` field in the manifest powers the "What's new" panel. Landed in `UPDATES.md` # 6.

3. **Pre-update snapshot** of `data_volumes` (+ `pg_dump` of any managed-service DB) is taken before every app update, restored on health-check failure alongside the image revert. Brute force on purpose: the brain doesn't know the app's schema, so it captures the bytes. `cache_volumes` are excluded — that's literally what the data/cache split was designed for. Retained 7 days alongside the kept image. When hooks return, an author-provided `pre_update` replaces the tar for that app; the brain's snapshot stays the default for apps without one. A `post_update_rollback` hook is sketched but deferred.

**Why each landed where it did:**

- **Catalog signed-by-malmo with digest pinning in the catalog (not the manifest):** the meaningful trust binding is "the malmo store promised this version → these specific bytes." Putting that in the catalog rather than asking authors to manage SHAs in their manifest keeps the author surface ergonomic (versions, not 64-char digests) while making the bytes cryptographically pinned. Tag mutation on Docker Hub can't ship malicious code to malmo boxes because the box pulls by digest the signed catalog committed to.
- **Two signing keys (release + store):** different blast radius. A compromised store key publishes a bad app manifest; a compromised release key ships a bad brain. Don't merge the two.
- **Verification in brain, not host-agent:** the release manifest controls the brain itself, so host-agent verifies it (avoids brain-verifying-its-own-upgrade). The store catalog is about apps — the brain's domain. Two verifiers, two scopes, clean layering.
- **Permission-expansion prompt on next login (not push notification, not banner):** push channels don't exist yet (email-on-file is still in `NEXT.md`); a banner makes "decline" mean "I'll never see this again." A modal-on-next-login matches the user's actual attention pattern with the dashboard — the box waits until the user is present.
- **Accept applies immediately, not at 03:00:** the user just deliberately initiated the action; making them wait until tomorrow morning is confusing. The minute of unavailability is the cost of the choice they made.
- **Pre-update snapshot in v1 (vs. waiting for hooks):** the rollback UX promise is unconditional ("Update failed → previous version restored"). Image-only rollback violates that promise the moment an app's migration touches its own data volume. The brain doesn't know the app's schema, so the safety net has to be byte-level (tar of declared `data_volumes`). Cost is bounded — declared data is typically small, snapshot runs in the 03:00 window. Hooks will refine this when they return; the snapshot stays the default.
- **App-side rollback hook (`post_update_rollback`) deferred:** would push complexity onto every author for a case the brain's snapshot handles. Right shape long-term for apps with destructive migrations that can't be tar-restored cleanly, but not the right v1 first move.

**Sharp edges:**

- **Image hosting is upstream's, not ours.** If an upstream registry pulls a version we've published in our catalog, new installs of that app fail until we publish a manifest update pointing at a different version or image source. Existing installs keep working (image is in the box's local cache). Acceptable for v1; mirroring is a known Tier-3 future.
- **Two users running different versions of the same Tier-3 per-user app is normal**, not a bug. Per-user instances are isolated by design; coordination would be a new constraint to add, not an old one we lost.
- **The snapshot doesn't protect Door-2 custom apps from their own migrations the same way** — Door-2 apps may not have declared `data_volumes` honestly. The brain still snapshots whatever is declared; if the user pasted a compose with everything in one volume, that gets tar'd in full. Acceptable: Door-2 is the tinkerer door, not the long-term-audience door.

**Affected docs:** new `APP_STORE.md`; `UPDATES.md` (# 4 mechanics renumbered with snapshot, new "Pre-update snapshot" subsection, new # 6 "Dashboard update UX" section, rollback summary table updated, locked decisions extended); `APP_LIFECYCLE.md` (on-disk layout adds `snapshots/`, digest-pinning section split into Door-1 catalog-authoritative vs. Door-2 TOFU, update transaction renumbered to include snapshot step + restore on failure, deferred-hooks section points at snapshot); `APP_MANIFEST.md` (runtime section notes catalog/digest binding, `changelog_url` field added, `post_update_rollback` sketched in hooks future, two new locked decisions); `CLAUDE.md` (`APP_STORE.md` added to documents list); `NEXT.md` ("App update UX" Tier-2 item removed, "Third-party manifest curation criteria" reframed as "Store catalog curation policy").

---

## 2026-05-16 — LUKS recovery passphrase hidden by default; one passphrase covers all drives; admin password gates add/eject

**Previously:** `STORAGE.md` # Encryption posture had the recovery passphrase *"generated at install, shown to the user once with 'write this down or save it somewhere.'"* `FIRST_RUN.md` Phase 1 had a dedicated "Recovery passphrase shown once" installer step (full-screen, copy-to-clipboard, "I have saved this" checkbox). Each drive enrollment (initial install + add-drive-later) had its own passphrase display. The user was on the hook for capturing and storing the secret.

**Now (three coupled decisions):**

1. **One recovery passphrase covers every drive on the box**, present and future. Generated once at install; enrolled as a LUKS keyslot on the OS drive, the data drive (if present), and every drive added later. The user's mental model is "one box, one recovery secret."
2. **The passphrase is not shown to the user.** At install, it's silently stored at `/etc/malmo/secrets/luks-recovery.key` (mode `0400`, root-owned, on the LUKS-encrypted OS drive). The installer no longer has a "show passphrase" step. The dashboard recovery code (`AUTH.md` # The recovery code) becomes the only "save this" moment in first-run. Tinkerers who need the passphrase (drive moves, BIOS reset, motherboard swap) find it under Settings → Storage → Advanced.
3. **Add-drive and eject-drive are host-agent jobs gated by a fresh admin password.** host-agent verifies the password via PAM, then reads the passphrase file and extends/removes the keyslot. The fresh-password requirement bypasses the 5-minute UI elevation window — add/eject are enrollment-class actions, not batch-work. A user with the dashboard recovery code but no admin password redeems the code first (forced password reset, `AUTH.md`), then proceeds normally — add-drive never accepts the recovery code directly.

**Why each flipped:**

- **One passphrase per box (vs. per-drive):** the "drives are independent LUKS volumes" framing led naturally to per-drive passphrases, but it's a leaky implementation detail. Users care about *the box*, not about how many LUKS headers it contains. A single keyslot value enrolled on multiple headers is no security loss (drives are co-located) and a meaningful UX simplification.
- **Hide the passphrase entirely (vs. show-once at install):** the passphrase only matters in scenarios where the box can't auto-unlock at boot — TPM wiped, Secure Boot policy change, drive moved to another box. All three are tinkerer scenarios. For non-technical users, the v1 answer to hardware death is already *restore from off-box backup* (`STORAGE.md` # What we don't do in v1), not *type the passphrase at the console*. Asking every user to capture a 32-character secret they'll likely never use is friction without payoff. The doubly-lost case (box can't boot, no backup, never fetched the passphrase) is honest and accepted.
- **Admin-password-gated host-agent op (vs. just-do-it / TPM-sealed):** add-drive needs *some* gate, since it extends the recovery keyslot. Three options were on the table: (a) re-prompt for the existing passphrase, forcing the user to know the secret we just decided to hide; (b) generate a new passphrase per drive, breaking the "one passphrase forever" promise; (c) the file-on-disk + admin-password approach taken here. (c) is the only option consistent with hiding the passphrase. The marginal trust handed to admins (they can implicitly authorize key extension) is already present — admins mutate the host via host-agent on every settings change.

**Sharp edges:**

- **Doubly-lost scenario is real and accepted.** Box can't boot *and* no off-box backup *and* never opened Settings → Advanced: data is gone. Surfaced in `STORAGE.md` # Threat model. Mitigation lives in the eventual backup-nagging UX, not here.
- **Recovery-code → drive operation chain is intentionally indirect.** Recovery code redeems to a fresh admin password (`AUTH.md` # Using the recovery code) which then unlocks add-drive. No carve-out for "logged in via recovery code, doing add-drive in the same session" — the redemption flow terminates in a real password by construction, so the carve-out is unnecessary.

**Affected docs:** `STORAGE.md` (# Encryption posture rewritten; # Adding a data drive expanded with host-agent flow; new # Ejecting a data drive section; # Threat model gains the doubly-lost line; # First-run flow drops the show-passphrase step), `FIRST_RUN.md` (# Phase 1 drops the dedicated passphrase step; # Step 2a clarifies redemption flow), `AUTH.md` (# Using the recovery code expanded with forced-new-password + fresh-recovery-code; # Roles gains the enrollment-class bypass note), `BRAIN_HOST_PROTOCOL.md` (`enroll-drive` and `eject-drive` jobs added to Pattern B examples; declared as `Dangerous: true, ResourceClass: "disk"`), `HEALTH.md` (new `disk-full` issue with `blocks_writes`/`blocks_apps` at 95%), `NEXT.md` (Storage Level 1 walk-through closed; Tier-3 entry for the remaining UX design pass).

---

## 2026-05-16 — Degraded mode in the brain; boot is best-effort, not strict-gate

**Previously:** `BOOT.md` had `malmo-storage-ready.target` as a **strict** gate (`Requires=` on every malmo userspace service, `BindsTo=` floated for the catastrophic-failure-mode units). Storage anomalies — data drive missing, UUID mismatch, canary mismatch — routed to `malmo-recovery.target`, which served a static page on port 80. The model optimized for "never silently corrupt data, even at the cost of refusing to boot."

**Now:** The boot chain is **best-effort, not strict-gate.** `malmo-storage-ready.target` uses `Wants=` (not `Requires=`); `malmo-storage-verify.service` is a **reporter** that writes findings to `/run/malmo/health/storage.json` rather than a gatekeeper that fails the boot. host-agent always starts (assuming Docker is up); the brain always starts (assuming host-agent is up); the dashboard is always reachable if the brain can run.

Anomalies become **health issues** the brain raises in *degraded mode*. The brain holds a typed set of active issues, each with `blocks_writes` / `blocks_apps` / `blocks_users` flags that gate operations uniformly. The dashboard surfaces issues as banners + inline cards + disabled action affordances. Every issue carries a primary remediation action, tiered: **Tier 1** (physical action by the user, UI detects completion), **Tier 2** (UI-driven one-click remediation), **Tier 3** (genuinely needs console/SSH — reserved for the cases where the brain can't run).

`malmo-recovery.target` shrinks to **two triggers only**: TPM2 unseal failure on the root drive (box can't boot at all unattended; LUKS recovery passphrase at console), and host-agent crashloop past `StartLimitBurst` (host-agent itself broken; static rescue page on port 80 with a one-click "roll back host-agent" button). Everything else — drive missing, drive wrong, canary mismatch, brain DB corrupt, version mismatch — flows through the degraded-mode mechanism.

**Why the flip:**

- The strict-gate model optimized for malmo's *least likely catastrophic failure* (silent wrong-tree writes) at the cost of malmo's *most likely catastrophic failure* (a non-technical user faced with a dark, unbootable box and no SSH knowledge). For the target audience — home users, not sysadmins — a brick is a worse outcome than the rare corruption case it prevents. Most corruption risks are also recoverable if caught early: the user's files are usually still *there*, just inaccessible or in the wrong place.
- The competitive landscape supports this. **Synology** is the gold standard: UI always reachable, drive issues surface as banners + guided walkthroughs, box never bricks. **TrueNAS / HexOS** keep the main UI up in degraded states too (just with a more prosumer-friendly tone). **Umbrel / ZimaOS / CasaOS** punt to SSH-rescue when things go wrong — which is the failure mode malmo explicitly wants to avoid. Copying Synology's model is the right shape for malmo's audience.
- The "silent wrong-tree writes" risk is still mitigated, just at the application layer: the brain consults the health-issue set on every write and refuses writes when `blocks_writes` is active. The protection is *application-level soft-refusal*, not *systemd-level hard-refusal*. The brain can also tell the user *why* the write was refused, which a systemd hard-stop cannot.
- The recovery-target surface stays *honestly* small. Two triggers, both unavoidable. Everything pulled out of that target becomes a Tier-1 or Tier-2 issue the user can self-recover from with no shell access.

**No-override stance preserved.** Critical blocks (data drive missing, brain DB corrupt) cannot be "clicked through" with a confirm dialog. The block is the protection; an override is an option the user will use, blame malmo for, and remember. Degraded mode is friendlier than recovery mode, not weaker.

**Tier-3 stays tiny on purpose.** A new health issue landing in Tier 3 is a design red flag — the question is always "can we pull this back to Tier 2 with better tooling?" The two existing Tier-3 cases (TPM unseal, host-agent crashloop) are the irreducible set: the brain can't run, so there's no UI to surface anything from.

**Affected docs:** `HEALTH.md` (new — the model in full), `BOOT.md` (rewritten — best-effort assembly, recovery-target shrunk to two cases, storage-verify reframed as reporter, `Wants=` throughout), `STORAGE.md` (canary section reframed: device-backing check added, "reporter not gatekeeper" noted, marker-mismatch behaviors point at `HEALTH.md`), `CONTROL_PLANE.md` (host-agent ordering uses `Wants=malmo-storage-ready.target` not `Requires=`; brain gains a Health manager package), `BRAIN_UI_PROTOCOL.md` (new `/api/v1/health/issues` endpoints, new `health.issue_raised` / `_cleared` / `_updated` event kinds, `blocked-by-health-issue` 409 response shape), `WEB_UI.md` (new Health & degraded mode surfacing section: `useHealth()`, global banner, inline cards, `<HealthGated>` wrapper), `CLAUDE.md` (HEALTH.md added to the doc list), `NEXT.md` (RECOVERY.md scope narrowed to the two true rescue-page cases).

---

## 2026-05-15 — Admins get sudo; UI is the path, SSH is rescue

**Previously:** No written posture on Linux-side privileges. The implicit working assumption (carried over from the Tier 1 OS-structure sketch) was "no sudo for anyone by default, opt-in admin-sudo toggle deferred to Tier 2." The thinking: keep dashboard-admin and Unix-root as distinct trust boundaries, so a compromised admin session can't escalate to the host.

**Now:** **Dashboard role maps to Linux group membership.** Members are unprivileged Linux users (own group + `malmo-shared` only). Admins are additionally in the `sudo` group and can `sudo` over SSH. Promote/demote in the dashboard flips group membership via host-agent.

On top of the role check, **destructive Settings operations re-prompt for the password** with a **5-minute elevation window** per session — sudo-in-UI pattern, scoped to the brain's session (not a real `sudo -v`).

The principle: **every privileged operation on a malmo box has a UI path; SSH is a rescue tool, not a workflow.** If a feature can only be configured from the shell, that's a bug in the spec.

**Why the flip from "no sudo for anyone":**

- The strict-separation posture left no recovery path when the brain itself is broken (corrupt SQLite, failed migration, host-agent unreachable). An admin couldn't restart services, read logs, or fix the box without a reinstall. That's unacceptable papercut for the tinkerer audience that v1 is actually shipping to.
- The marginal blast radius of "admin SSH session = root" is small: a dashboard admin already mutates the host through host-agent on every settings change. The trust boundary that matters is "is this user an admin," not "do they have a root shell."
- SSH is off-by-account-by-default (`AUTH.md` # Device access). An admin who doesn't opt in to SSH gets no shell access at all — the strict posture lives on, just opt-in instead of mandatory.
- Members get no opt-in. The "tinkerer who wants member + sudo" case is rejected; if you want shell-root, use an admin account.

**5-minute window over alternatives:**
- *Always re-prompt* on every destructive op is too annoying for admins doing batch work (setting up multiple users, configuring a new household).
- *Elevate-for-session* leaves a forgotten browser tab as a standing admin shell.
- 5 minutes matches the macOS System Settings pattern users already know.

**Affected docs:** `USERS_AND_GROUPS.md` (new), `CLAUDE.md` (doc list entry + load-bearing decision), `AUTH.md` (Roles section adds elevation + Linux-group pointer), `STORAGE.md` (malmo-shared mention pointers to group reference), `FIRST_RUN.md` (Step 2 notes admin = sudo group), `BRAIN_HOST_PROTOCOL.md` (clarifies `malmo` group vs. `malmo-shared`), `NEXT.md` (TPM-fail rescue, demotion-doesn't-kill-sudo, `malmo-shared` management UI).

---

## 2026-05-15 — One password for dashboard + SSH + SMB; PAM is the credential store

**Previously:** `AUTH.md` had a separate "Device access password" — one credential for SSH+SMB, independent from the dashboard password. Stored separately (`/etc/shadow` for device, brain SQLite for dashboard). Rationale was "different blast radii" and "brain runs in a container, can't access /etc/shadow."

**Now:** **One malmo password per user.** Same string authenticates the dashboard, SSH, and SMB. **PAM (`/etc/shadow`) is the single source of truth**; the brain has no password hash in its SQLite. Dashboard login calls host-agent's `verify_password` endpoint, which runs PAM `authenticate()` and returns yes/no.

Per-protocol access stays per-protocol — SSH and SMB are still off-by-account-by-default. What's off is *which services accept the password for each account*, controlled via sshd's `AllowUsers` and Samba's `valid users`. The password exists in PAM from account creation; opt-in just adds the user to the relevant allowlist.

**Why the flip:**

- The "different blast radii" argument was theoretical — users would reuse the same string for both anyway. We'd be enforcing a separation that existed only on paper while paying the UX cost of explaining it.
- "Brain runs in a container, can't access `/etc/shadow`" was an implementation-convenience reason, not a security reason. Easily solved by routing dashboard verification through host-agent (which already has root). Per-login PAM check is sub-millisecond.
- The recovery code mechanic is unchanged — it's a brain-SQLite artifact, validated by the brain, then triggers a `passwd` through host-agent. No coupling to the password-storage flip.
- Net UX win: one password to remember; SMB on a phone uses the password the user already knows; first device-access opt-in has no new-credential friction.

**Affected docs:** `AUTH.md` (rewrite of "Device access password" → "Device access (SSH + SMB)"; password storage moves to PAM; brain↔host-agent path adds verify_password; locked decisions updated), `BRAIN_HOST_PROTOCOL.md` (Pattern A example adds `/v1/auth/verify-password`), `STORAGE.md` (SMB share auth references "malmo password" not "Device access password"), `FIRST_RUN.md` (Step 2 reframed), `CLAUDE.md` (load-bearing decision updated).

**Supersedes** the "unified device-access password" decision from earlier the same day — that was an interim position that kept the *device* credential separate from the *dashboard* credential. We've now collapsed all three.

---

## 2026-05-15 — Filesystem layout, mergerfs from day 1, SMB shares, SSH scoped to LAN+mesh

**Previously:** `STORAGE.md` had user content at `/mnt/data/users/<slug>/` and explicitly excluded mergerfs from v1 ("No pooling without parity. Level 2 by itself is a footgun by default"). `AUTH.md` had a separate "SSH password" with no cross-protocol scope. `BUILD.md` was contradictory about whether sshd was enabled at boot.

**Now (six locked decisions, one session):**

1. **User content lives at `/home/<user>/`** with macOS / XDG-style capitalized use-case folders: `Photos/`, `Music/`, `Movies/`, `Documents/`, `Notes/`, `Downloads/`. The data drive mounts at `/srv/malmo/` with bind mounts to `/home/` and `/var/lib/malmo/`. Standard Linux paths; no invented vocabulary.

2. **Mergerfs ships from day 1.** Always-pool when a data drive is present (pool of one with one drive). Adding drive #N is `mergerfs add` with zero downtime. Placement: `epmfs` ("existing path, most-free-space"). SnapRAID parity stays deferred as Level 2.

3. **Files are first-class, apps are windows.** User content (photos, music, notes) lives in `/home/<user>/<use-case>/`. App state (indexes, caches, DBs) lives in `/var/lib/malmo/instances/<id>/data/`. Uninstalling an app deletes the index, never the photos. Manifests bind-mount use-case folders by declaration. Apps that can't comply opt in via `storage.app_managed_user_content: true` with a user-visible warning; curation discourages.

4. **SMB shares** via Samba: `\\malmo\<user>` (per-user home) and `\\malmo\shared` (household). mDNS-advertised so devices auto-discover. TimeMachine-compatible. The household-shared tree lives at `/srv/malmo/shared/` with setgid + `malmo-shared` group.

5. **Unified Device access password** for SSH and SMB. Same threat model (non-browser LAN/mesh access); one credential, not two. Off by default per user, opt-in via Settings. Separate from the dashboard password.

6. **SSH (and SMB) scoped to LAN + mesh via nftables.** RFC1918 + the mesh interface (`tailscale0`/`headscale0`). SSH from the public internet is structurally blocked, not just per-account opt-in. Pair the device on the mesh to access the box remotely — same trust model as the dashboard.

**Why each flipped:**

- **`/mnt/data/users/` → `/home/<user>/`**: pure FHS pedantry on the old layout. Every Linux tool, SSH session, SMB client expects `/home/<user>/`. macOS does `/Users/`, Windows does `C:\Users\`. Inventing a different path was friction without benefit. The "user content lives on the data drive" concern is solved by *mounting* the data drive such that `/home` lives on it — standard Linux multi-disk pattern.
- **Mergerfs day 1 instead of Level 2-deferred**: the audience explicitly includes tinkerers who add drives. Without always-pool, expanding storage is a "stop services, swap mounts, restart" operation; with it, it's `mergerfs add` with zero downtime. The FUSE overhead (single-digit % on most workloads) is a small permanent tax for a meaningful UX property. Trading correctness-friction for expansion-friction is the right call for this audience.
- **Files-first-class principle**: the differentiator vs. Nextcloud/Photoprism is "your files are real files, accessible from any device via SMB, surviving any app swap." This is a value statement, not a layout detail.
- **SMB instead of WebDAV/NFS/AFP**: only protocol with first-class clients on Windows, macOS, iOS, Android, Linux. Zero-config discovery via mDNS gets the box into every device's file browser without instruction.
- **Unified device password (SSH+SMB)**: three passwords (dashboard + SSH + SMB) is too many; users would reuse them. Two passwords (dashboard for browser, device for everything else) maps to the user's actual mental model. Same threat model = same credential.
- **SSH+SMB on :22 firewalled to LAN+mesh**: a closed port to the public internet is structurally safer than "open port relying on per-account opt-in." The trust model already exists for remote access (mesh pairing); SSH inherits it.

**Affected docs:** `STORAGE.md` (rewrite of disk roles, mount layout, permissions, SMB; mergerfs always-on; reframed levels table), `AUTH.md` (SSH section → Device access password section, SMB notes), `APP_MANIFEST.md` (`permissions.user_folders` / `shared_folders` with `scope: pick-subfolder`, `storage.app_managed_user_content`, external-storage convention), `BUILD.md` (SSH daemon-on-but-no-auth, nftables :22 scoping, samba/mergerfs preinstalled), `FIRST_RUN.md` (device access password reference).

---

## 2026-05-15 — Brain web library: huma; CI enforces additive-minor via generated OpenAPI + oasdiff

**Previously:** `BRAIN_UI_PROTOCOL.md` # Codegen said "deferred — v1 ships with hand-rolled Go structs ↔ TS types; OpenAPI 3 spec + generated TS client lands as a follow-up build step before the public-API surface goes external." `NEXT.md` Tier 2 had "CI enforcement of additive-minor API discipline" as an open item, sketched as "diff `openapi.yaml` between commits" — but no `openapi.yaml` was specified to exist.

**Now:** The brain is written using [`huma`](https://huma.rocks), a Go web library that emits OpenAPI 3 from typed handler registrations. The OpenAPI artifact is a byproduct of the code, not a hand-maintained file. CI runs `oasdiff breaking` between base and PR on every commit; non-zero exit fails the build. Bypass is moving the change to `/api/v2`, not a skip flag.

The "Codegen deferred" line splits: **server-side OpenAPI emission lands from day one** (it's the CI substrate); **client-side TS generation stays deferred** until the schema is stable enough that hand-maintenance cost > codegen cost.

**Why huma over alternatives:**

- **`huma`** wins on "OpenAPI is the byproduct of writing handlers, not a separate doc." Typed I/O structs per route; library reflects to emit OpenAPI 3. Active 2026 project, designed around this exact pattern.
- **`chi` + `swaggest/rest` or `go-swagger` annotations** can also produce OpenAPI but treat it as a side-channel — handler signature and schema live in different places, so drift returns through the back door.
- **`gin` / `echo`** are popular but don't put schema first; OpenAPI is a third-party plugin afterthought.
- **Hand-written `openapi.yaml` + standard router** was the alternative that asks for trouble — second source of truth, drift caught only when something breaks for a real caller.

**Why oasdiff over snapshot tests:** snapshot tests verify what they call. A snapshot suite has coverage gaps (untested endpoint, unobserved enum value, omitted-empty optional field, error-response shape). Removing a field that no snapshot happened to capture is a silent breaking change. `oasdiff` operates on the schema — the set of *possible* responses — so a field declared in a type is checked whether or not a test exercises it. Structural, not heuristic.

**The forcing function this protects:** in-flight UI tabs across a malmo update, and external callers from day one (third-party stores, CLI, future tooling) per the public-API posture. Both groups can't be coordinated with at change-time; the discipline is the only thing that keeps their code working.

**Headline calls:**

- **Generated OpenAPI, not hand-written.** Drift is structurally impossible.
- **Schema-diff, not snapshot-diff.** Contract changes are detected, not just observed ones.
- **Event `kind` and error `code` are first-class Go enum types** so they appear in OpenAPI as named enums; oasdiff catches removals.
- **No bypass flag.** Breaking a v1 contract requires moving to v2.

**Resolves:** `NEXT.md` Tier 2 "CI enforcement of additive-minor API discipline."

**Affected docs:** `BRAIN_UI_PROTOCOL.md` (# Codegen rewritten; new # CI enforcement; locked-decisions bullets updated), `NEXT.md` (Tier 2 entry removed).

---

## 2026-05-15 — Release manifest: no phased rollout, no beta channel in v1

**Previously:** `UPDATES.md` # 3 specced a time-based phased rollout (`rollout: [{after, percent}, ...]` in the manifest, deterministic `hash(machine_id + version)` cohort bucket) and a beta channel with a 7–14 day bake before promoting to stable. Both treated as v1 mechanics.

**Now:** v1 ships **one channel (`stable`) with no phased rollout**. The manifest is a small static JSON file (`brain`, `ui`, `minimum_host_agent`, `released_at`, `rollback_to`, plus `manifest_version` and `channel` for forward-compat), signed with minisign (Ed25519). `rollback_to` is the load-bearing protection. Every box that polls sees the new manifest immediately. Phased rollout + beta channel are deferred with explicit triggers (see `RELEASE_MANIFEST.md` # Future work, # Channels).

**Why:**

- **Admin-prompted updates already provide natural pacing at v1 scale.** The first admins to click are self-selected eager adopters — functionally equivalent to a beta cohort. A separate beta cohort on a fleet in the hundreds would be effectively empty.
- **Detection of bad releases at v1 scale is GitHub issues + forum + direct reports**, not telemetry-gated cohort advancement. Channels and cohorts mitigate a problem (auto-apply at scale) we don't have yet.
- **`rollback_to` is independent of pacing.** It works whether the rollout is staggered or instantaneous; it's the protection that actually matters in v1.
- **Both deferrals are additive.** `manifest_version: 1` is present from day one; host-agent ignores unknown fields. A future `rollout` field or `beta.json` lands without a flag day.
- **Trigger to revisit:** A/B immutable images landing (auto-apply becomes safe, admin-prompting no longer paces) or fleet growth past the point where direct reports are timely. Either signal flips both decisions.

**One forward-compat nudge kept from the start:** the signing verifier accepts a **list** of pubkeys, not a single constant. This is the only thing that has to be right now to keep key rotation cheap forever — without it, a lost or compromised key forces a synchronized fleet update.

**Cohort hash refinement (when phased rollout activates):** `UPDATES.md` previously specified `hash(machine_id + version)`. `RELEASE_MANIFEST.md` # Future work narrows this to `hash(machine_id || canonical(brain, ui))` — hashing over the version pair (not the full manifest bytes) keeps cohort identity stable across mid-rollout schedule edits. Canonical form pinned when the feature lands.

**Headline calls:**

- One static JSON manifest per channel, git repo as source of truth, CDN as delivery cache. Promotion is a PR.
- Signed with minisign (Ed25519); pubkey list baked into host-agent.
- `rollback_to` is the kill switch and the only fleet-coordination mechanism in v1.
- No phased rollout, no beta, no cohorts in v1 — additive when triggers fire.

**Resolves:** `NEXT.md` Tier 2 "Release-manifest schema + publishing pipeline."

**Affected docs:** `RELEASE_MANIFEST.md` (new), `UPDATES.md` (# 3 compressed; rollout array, cohort hashing, and beta-bake paragraphs removed and deferred), `BUILD.md` (channels line; pipeline diagram points at RELEASE_MANIFEST.md), `CLAUDE.md` (Documents list), `NEXT.md` (Tier 2 #1 removed, Tier 3 + Tier 4 follow-ups added).

---

## 2026-05-14 — Web UI deploy: dedicated container, release-manifest, API-versioned (not lockstep)

**Previously:** Two threads pointed in tension:
- `WEB_UI.md` left the deploy model open with three options: separate UI container; UI baked into brain image and served by Caddy from a shared volume; brain serves UI via `embed.FS`.
- `BRAIN_UI_PROTOCOL.md` had locked **lockstep versioning** — UI and brain ship as one unit with the OS release. The `426 Upgrade Required` path was framed as a transient-during-update artifact.

The lockstep posture made `embed.FS` look attractive (one binary, one update). But it also meant every UI tweak forced a brain release — wrong cost shape for the UI's natural iteration cadence, and inconsistent with malmo's "everything is a container" architecture.

**Now:** Three coupled decisions, locked together:

1. **Deploy model: dedicated `malmo-ui` container.** `caddy:alpine` base + UI bundle baked in at CI build time. Read-only FS, no secrets, no host privileges, no Docker socket. The simplest container in the stack. The existing LAN Caddy routes `/api/v1/*` → `malmo-brain`, everything else → `malmo-ui`.
2. **Versioning posture: API-versioned + additive-minor** (not lockstep). `/api/v1` minors only add fields; breaking changes go to `/api/v2`. UI declares its required minor in `version.json`; brain returns `426` only when it genuinely can't serve that minor. The `426` path is the in-tab safety net for "user had a tab open while the UI container updated," not a transient-during-OS-update artifact.
3. **Update mechanism: single release manifest, two artifacts.** One channel, one user-facing "auto-update malmo" toggle. The manifest names both `brain` and `ui` versions. Updater pulls + recreates only what changed: UI-only ship → recreate UI; brain-only → recreate brain; coordinated change → recreate both as one transaction with paired rollback.

**Why these vs. the alternatives:**

- **`embed.FS` (Option 3)** would have been the simplest engineering choice but breaks the "everything is a container" pattern we've been deliberate about, and forces every UI ship through a brain release — exactly the iteration-speed trap that drove the "separate codebase" lock in the first place.
- **UI baked into brain image + served by Caddy from shared volume (Option 2)** kept "one artifact" but introduced a shared-volume dance between brain and Caddy that's awkward semantics for marginal benefit.
- **Two separate update toggles (one for brain, one for UI)** was the obvious-but-wrong UX answer for Option 1. Rejected: the user has one mental model ("malmo updates"); the fact that internally there are two containers is implementation detail.
- **Lockstep versioning** was rejected because the UI ↔ brain relationship is the same shape as any web app ↔ backend: a versioned API with additive minors is the standard, well-understood discipline. Lockstep was over-coupling masquerading as conservatism.
- **`nginx:alpine` and `scratch + tiny Go static server`** were both viable UI-server bases. `caddy:alpine` won on consistency — we already run Caddy as the LAN reverse proxy; one HTTP server toolkit across the appliance.

**Headline calls:**

- **One channel, one toggle, two artifacts.** The release manifest is the primitive. The user never sees the brain/UI distinction at update time.
- **Most weeks, only the UI moves.** Brain stays running; no API gap. This is the iteration-speed payoff.
- **Additive-minor discipline is now load-bearing.** `/api/v1` fields can be added, never removed or repurposed. Event `kind` values are added, never removed (deprecation = stop emitting). CI enforces. This is the cost we pay for independent UI iteration.
- **Paired rollback on failed updates.** Brain + UI version pairs are tested together; a failed deploy reverts both, restoring the previous tested pair.

**Resolves:** the last open piece of `NEXT.md` Tier 1 "Web UI framework + deploy model."

**Open follow-ups (parked in `NEXT.md`):**

- Concrete release-manifest schema and publishing pipeline (the JSON shape is sketched in `UPDATES.md`; the build + signing infrastructure isn't).
- CI enforcement for additive-minor discipline (regression test that compares `openapi.yaml` between commits and fails on field removal / repurpose).

**Affected docs:** `WEB_UI.md` (Option 1 locked, deploy section rewritten), `BRAIN_UI_PROTOCOL.md` (lockstep replaced with additive-minor versioning, `426` reframed as in-tab safety net), `UPDATES.md` (#3 covers brain + UI as one stream with two artifacts; release manifest carries both versions), `NEXT.md`.

---

## 2026-05-14 — Web UI stack: Vue 3 + Tailwind + shadcn-vue + TanStack Query

**Previously:** `NEXT.md` Tier 1 "Web UI framework + deploy model" was open. The "UI is a separate codebase" / Vite / `/api/v1` proxy / version-handshake were locked, but the framework and the libraries on top were unspecified.

**Now:** Framework + library stack locked in `WEB_UI.md` # "Locked: stack." Deploy model (bundled in brain image vs. separate static-files container vs. served by the brain directly) remains open — narrowed Tier 1 item in `NEXT.md`.

**The lock list:**

| Layer | Pick |
|---|---|
| Language | TypeScript, `strict` |
| Framework | Vue 3, Composition API + `<script setup>` only (no Options API) |
| Build | Vite 5+ |
| Routing | Vue Router 4, history mode |
| State (client) | Pinia |
| State (server / cache) | `@tanstack/vue-query` v5 |
| HTTP client | Native `fetch` + ~30 LOC wrapper, designed to be swapped for `openapi-fetch` when codegen lands |
| SSE | Native `EventSource`, wrapped in a `useEvents()` composable that dispatches Query invalidations |
| Jobs polling | `useJob(jobId)` composable wrapping `useQuery` with terminal-status `enabled` |
| Styling | Tailwind CSS 4 (CSS-based config, no `tailwind.config.js`) |
| Components | shadcn-vue (copy-paste, owned in repo) on **reka-ui** (headless primitives — Vue port of Radix) |
| Icons | lucide-vue |
| Package manager | pnpm |
| Lint / format | ESLint (`eslint-plugin-vue` + `@typescript-eslint`) + Prettier |
| Node | Latest LTS, pinned via `.nvmrc` |

**Why these vs. the alternatives:**

- **Framework:** Vue, React, Svelte 5, and HTMX were all live candidates.
  - **React** was the safest ecosystem bet — most libraries, biggest hiring pool, best AI-assist code quality. Rejected because the velocity/identity wins it offers are smaller for a single-team admin SPA than its verbosity costs over a multi-year codebase.
  - **Svelte 5** was the runtime-feel and DX winner. Rejected because (a) the ecosystem is materially thinner — particularly for OpenAPI ↔ TanStack-Query integration that lands in the next codegen step — and (b) AI-assist code quality is materially worse in 2026 (less training data, Svelte 4 patterns leak in).
  - **HTMX + Go templates** was a serious wildcard for an admin dashboard. Rejected because the future web terminal needs a true SPA, and HTMX would force a hybrid model later.
- **Component library:** PrimeVue (fast MVP, hard to escape its look), Naive UI (best defaults, but its own theming system fights Tailwind), Vuetify (Material lock-in, wrong aesthetic), Element Plus (enterprise CRUD aesthetic), Headless UI Vue (too thin) were all weighed. **shadcn-vue on reka-ui** won because malmo's "polished, non-technical user, distinctive brand" target argues against off-the-shelf-looking libraries, and the shadcn ecosystem is where modern admin-app design language is being formed.
- **Data-fetching:** rolling our own thin composables (~200 LOC) was the alternative. Rejected for a 30+ screen dashboard: cache, dedup, retries, and optimistic-update plumbing get rewritten per call site otherwise. TanStack Query is the library specifically built for the cache-heavy, mutation-heavy, push-invalidated shape we just specced.
- **HTTP client:** axios is heavy and irrelevant in 2026. ofetch is nicer but pulls Nuxt-adjacent deps. Native `fetch` + tiny wrapper is right-sized and forward-compatible with `openapi-fetch`.
- **Tailwind 4 vs. 3:** v4 is current; v3 is legacy. Cost: occasional internet examples written for v3 config (now obsolete). Worth it.
- **pnpm vs. npm/yarn:** smaller `node_modules`, faster installs, friendlier if the repo ever grows a second package.

**Headline calls:**

- **One Composition style.** `<script setup>` everywhere. No Options API. Discipline at code-review time.
- **Server state lives in Query, not Pinia.** Pinia is for genuinely client-side state (UI mode, in-progress form drafts, the toggle for "show advanced settings"). API data goes through TanStack Query — single source of truth, one cache.
- **The SSE event stream is the cache-invalidation channel.** `useEvents()` subscribes once at app mount; `event.kind` switches drive `queryClient.invalidateQueries(...)`. Components consume `useQuery` and stay fresh without knowing about events.
- **Forms, dark mode, i18n, testing stack** are downstream decisions, not part of this lock. Tracked separately.

**Resolves:** the framework portion of `NEXT.md` Tier 1 "Web UI framework + deploy model."

**Open follow-up:** deploy model (UI bundled into the brain image vs. separate `nginx:alpine`/scratch container vs. brain serves the static files itself). Tracked as a narrowed Tier 1 item in `NEXT.md`.

**Affected docs:** `WEB_UI.md` (new), `NEXT.md`.

---

## 2026-05-14 — Brain ↔ UI API: REST + SSE, jobs pattern mirrors host-agent

**Previously:** `NEXT.md` Tier 1 listed "Brain HTTP/RPC API style" — REST+WS vs. gRPC+WS vs. all-WebSocket RPC framing — as the blocking item gating UI work, manifest tooling, and the future malmo-store API.

**Now:** Locked in `BRAIN_UI_PROTOCOL.md`. Mirrors `BRAIN_HOST_PROTOCOL.md` deliberately — one API discipline across the whole stack.

- **Transport:** HTTPS via Caddy → brain. Browser-native fetch / EventSource / WebSocket. No bespoke client library required.
- **Wire:** HTTP/1.1 + JSON. Versioned URL prefix `/api/v1/...`. UI sends `X-Malmo-API-Version`; brain returns **426 Upgrade Required** on mismatch (per existing UI deploy-model lock).
- **Four patterns:**
  - **A — Sync request/response** for ops under ~5s (list apps, get user, update setting).
  - **B — Jobs** for anything that can exceed ~5s (app install/update, mkfs, Tailscale enrollment). Same shape as host-agent jobs: `POST` returns `{job_id, status}`; `GET /api/v1/jobs/:id` polls; `POST /api/v1/jobs/:id/cancel` cancels. `status ∈ {running, completed, failed, cancelled, cancelling, stalled}`. `result` on completion; `error` on failure.
  - **C — SSE for streams.** Two distinct stream types: **per-resource log/job tails** (`/api/v1/jobs/:id/log`, `/api/v1/instances/:id/log`) and a **global event stream** (`/api/v1/events`) for dashboard liveness — app state changes, update-available, drift surfaces, peer-online. Each event carries a typed `kind` and resource ID. Same reconnect resilience as host-agent SSE: monotonic `id`, rolling buffer, `Last-Event-ID` replay.
  - **D — WebSocket reserved** for the future web terminal. No v1 pre-design.
- **Auth:** opaque `malmo_session` cookie (per `AUTH.md`). Same cookie carries the SSE/WS upgrade. No bearer tokens, no JWTs.
- **Errors:** HTTP status + `{code, message, details?}` body. Codes are stable strings; messages are human-readable but not contractual.
- **Codegen: deferred.** Hand-rolled Go structs ↔ TS types in v1. OpenAPI 3 spec + generated TS client as a follow-up build step before the public-API surface ships.
- **Public-API posture from day one.** The API the dashboard uses is the same API a future CLI, third-party app store, or external tool will hit. No "internal-only" carve-outs. Concretely: stable URLs, stable error codes, stable event `kind` values, no hidden auth shortcuts the dashboard uses but external callers can't.

**Why these vs. the alternatives:**

- **tRPC end-to-end (Umbrel's path)** is the most ergonomic option *if both ends speak TypeScript*. The brain is Go (`DECISIONS.md` foundational), so the central tRPC advantage — zero codegen, types flow through TS project references — is unavailable. Adopting tRPC would require either rewriting the brain in TS (rejected; orchestration daemons are Go's niche) or a TS shim daemon in front of the Go brain (pure overhead).
- **gRPC + grpc-web (CasaOS-adjacent)** gives schema enforcement and native streaming, but loses `curl`-debuggability, requires a codegen step from day one, and is awkward in browser devtools. The performance argument doesn't apply at home-server scale.
- **All-WebSocket with RPC framing** collapses transports but loses HTTP's free toolchain — proxies, caches, devtools, `curl`. Streams that are naturally one-way (95% of our streaming surface — logs, events) don't benefit from full-duplex. Reserved for the terminal, not the default.
- **Separate MessageBus service (CasaOS's pattern)** is good for microservices but malmo-brain is one binary (`CONTROL_PLANE.md`). A single SSE endpoint on the brain delivers the same affordance without a second daemon.
- **Reconstructing job progress from polling state** (Umbrel + CasaOS both do this) was rejected — a typed `Job` resource with progress + terminal result is a strict improvement, and we get it free by mirroring host-agent's existing pattern.
- **Codegen from day one** was tempting (compile-time TS types catch drift). Rejected for v1 because hand-rolled types let us iterate on the schema at editing speed; the OpenAPI step lands when the surface is stable enough that drift cost > codegen cost.

**Headline calls:**

- **One API discipline across the stack.** Brain↔UI and brain↔host-agent use the same four patterns. Engineers learn one model. Jobs, errors, SSE reconnect — all identical shapes.
- **Debuggability is a first-class design constraint** (inherited from `BRAIN_HOST_PROTOCOL.md`). Anything callable from the dashboard is callable from `curl` over the LAN with a session cookie. Bug reports include curl commands.
- **Streams are first-class, not afterthoughts.** Every event `kind` is enumerated in the schema. No untyped `{type, data}` blobs.
- **Third-party stores are a v1 design input.** The store-API surface is the dashboard's API; there is no second public API to maintain.

**Resolves:** `NEXT.md` Tier 1 item #1 "Brain HTTP/RPC API style."

**Open follow-ups (parked in `NEXT.md`):**

- Timing of the OpenAPI codegen build step.
- Concrete event `kind` enumeration (post-MVP UI surfaces will pull this in).
- Rate-limit / abuse posture for third-party API consumers.

**Affected docs:** `BRAIN_UI_PROTOCOL.md` (new), `NEXT.md` (Tier 1 #1 resolved, follow-ups added).

---

## 2026-05-14 — Brain ↔ host-agent failure semantics: four categories, four mechanisms

**Previously:** Failure modes were deliberately deferred when the happy-path protocol landed. `NEXT.md` Tier 1 carried "Brain ↔ host-agent failure semantics" as the explicit follow-up.

**Now:** Locked in `BRAIN_HOST_PROTOCOL.md` # Failure semantics and `APP_LIFECYCLE.md` # "same reconciler pattern extends to all host-managed state." Treated as four distinct problems with four distinct mechanisms — not one unified framework.

| # | Problem                                                            | Mechanism                                                                                  |
|---|--------------------------------------------------------------------|--------------------------------------------------------------------------------------------|
| A | Hung ops, dangerous ops, concurrent destructive ops                | Per-job declared attributes (`MaxDuration`, `Dangerous`, `ResourceClass`), enforced uniformly by host-agent |
| B | Multi-step crash recovery + drift from manual host changes         | Reconciler pattern — desired in brain SQLite, actual via host-agent summary, 60s heartbeat |
| C | SSE stream resilience across brain restarts                        | `Last-Event-ID` + ~256 KB per-job rolling buffer; standard SSE replay                      |
| D | host-agent self-update, FD limits, cross-class destructive locks   | Orchestration rules at the lifecycle level — not protocol surface                          |

**Headline calls:**

- **Stalled and failed are distinct job statuses.** "We're not sure" vs. "we know it broke" — different UI tones.
- **Cancellation: SIGTERM → 10s grace → SIGKILL. Final result wins** — if the op completes before kill, the job ends `completed` regardless of pending cancellation.
- **Cross-class dangerous lock:** any `Dangerous: true` job blocks all other jobs while it runs and waits for everything to drain before starting. Catches "disk format and apt upgrade are technically different resource classes but you really don't want both at once."
- **Drift policy is asymmetric.** Brain auto-reconciles when *it* made the last change (handles crash-mid-step). Brain surfaces — doesn't auto-fix — when *something else* changed state. Respects users who deliberately SSHed in to fix something; avoids fight-loops.
- **Dangerous ops are excluded from auto-reconcile.** An interrupted `mkfs` is not safely retryable. UI surfaces; user decides.
- **Heartbeat is 60 seconds.** Brain polls host-agent's `GET /v1/state/summary`. One tiny request per minute. Quieter than 30s, more responsive than 120s.
- **host-agent self-update drains all jobs first**, with a 5-minute hard cap. If a job is still running, the OS update fails with "retry later" rather than risking corruption.
- **CI tests guard the discipline.** Every `JobKind` must declare its attributes (type-enforced at registration). SSE reconnect, reconciler convergence, and malmo-group membership all have round-trip tests.

**Why these vs. the alternatives:**

- **Auto-fix drift always** (the box does what its settings say) was simpler but lost — it would fight users who deliberately changed something via SSH and create reset-loop bugs. Surface-don't-auto-fix is the right respect-the-user posture.
- **Unified failure-handling framework** (one mechanism for all the problems) was tempting but conflates concerns. The four problems have genuinely different shapes; one mechanism per shape is simpler than a generic one.
- **No heartbeat** (only reconcile on startup and after ops) would miss drift entirely until the next user-initiated action. 60s heartbeat catches it within a minute for no real cost.
- **Auto-resume dangerous ops** (try to be helpful after a crash) is exactly the kind of thing that takes a small problem and turns it into a corrupted disk. Surface and let the user decide.
- **Treat SSE reconnect as application-layer** (custom protocol, ack-based) was rejected because spec-defined `Last-Event-ID` + a ring buffer does everything we need with zero new protocol surface.

**Resolves:** `NEXT.md` Tier 1 item "Brain ↔ host-agent failure semantics."

**Affected docs:** `BRAIN_HOST_PROTOCOL.md` (# Failure semantics replaces the deferred-stub), `APP_LIFECYCLE.md` (reconciler pattern extended from apps to all host-managed state), `NEXT.md`.

---

## 2026-05-14 — Brain ↔ host-agent: HTTP/JSON over UNIX socket, two API patterns, lockstep versioning

**Previously:** `NEXT.md` Tier 1 listed the brain↔host-agent protocol as open. `CONTROL_PLANE.md` mentioned the boundary informally.

**Now:** Full happy-path protocol locked in `BRAIN_HOST_PROTOCOL.md`. Headline calls:

- **Transport:** UNIX socket at `/var/run/malmo/agent.sock`, `root:malmo` `0660`. Brain's container UID is in the `malmo` group. Kernel-enforced authn, no app-layer token.
- **Wire:** HTTP/1.1 + JSON, versioned URLs (`/v1/...`). Debuggable with `curl --unix-socket` from any shell on the host.
- **Two API patterns, clear rule:** plain HTTP request/response for ops under ~5 seconds; explicit `Job` objects with poll + cancel for anything longer or anything needing progress reporting. Bias toward "make it a job" when uncertain.
- **SSE for one-way streams** (app container logs, job log tail, journalctl). Browser-native, end-to-end through the brain.
- **WebSocket reserved** as the future-shape for bidirectional needs (web terminal). Additive via HTTP upgrade; no v1 pre-design.
- **Versioning: lockstep with OS release.** Brain N talks to host-agent N. No protocol negotiation. Resolves the `UPDATES.md` "brain↔host-agent protocol versioning" open item.

**Why these vs. the alternatives:**

- **gRPC over UNIX socket** was the main contender. Schema enforcement and native streaming are real wins, but the code-gen step and lost `curl`-debuggability outweigh them for two binaries we ship together. We can switch later if hot paths ever warrant it; nothing about HTTP/JSON forecloses gRPC.
- **Loopback TCP** would be slightly easier to call from outside the brain's container but exposes port-allocation problems and weakens authn (anyone who can reach the port can talk to host-agent, vs. anyone in the kernel-checked group).
- **Direct shared-state via SQLite** (brain writes "desired" rows, host-agent polls) gives free persistence but adds latency and is clunky for streaming. Rejected.
- **Everything is a job** (uniform model, one pattern) was considered. Rejected because read-only / fast routes don't need the "is this done? where's the result?" overhead. The dividing line is explicit per route.
- **Protocol-version negotiation** is unnecessary given lockstep release; would only matter if we ever upgraded brain independently of host-agent, which the OS-update model doesn't do.

**Notes:**

- Failure semantics (timeouts, drift detection, reconciler discipline, SSE reconnect, dangerous-op resume) are **deliberately deferred** to a separate design pass. Tracked as a Tier-1 item in `NEXT.md`. The protocol is not implementation-ready without those.
- The "debuggability is a first-class design constraint" line in `BRAIN_HOST_PROTOCOL.md` is load-bearing: future changes to this protocol that would make it harder to inspect from `curl` need an explicit justification.

**Affected docs:** `BRAIN_HOST_PROTOCOL.md` (new), `CONTROL_PLANE.md` (host-agent reference points here), `AUTH.md` (malmo-group test invariant), `UPDATES.md` (protocol-versioning question resolved), `NEXT.md`.

---

## 2026-05-14 — Tier-2 apps: native Debian + systemd, UI lives in the dashboard

**Previously:** Implicit assumption that all malmo-managed software runs as Docker containers. `SERVICE_PROVISIONING.md` left Tier-2 deployment open: *"a privileged container, a host service managed by host-agent, or a combination."* The malmo session ↔ Tier-2 admin UI auth story was complicated by the prospect of forward-auth across per-app subdomains.

**Now:** **Tier-2 apps install as Debian packages, run under systemd. No upstream admin UI is exposed at its own subdomain.** The malmo dashboard surfaces a curated UI for each Tier-2 service at `/settings/<service>/*`. The brain edits config files and toggles systemd units via host-agent. Tier-1 (managed data services) stays containerized; Tier-3 (regular apps) stays containerized.

**Why:**
- Native is what upstream docs recommend for these services. Tailscale's Docker path needs `--privileged` and `/dev/net/tun` gymnastics; native Samba sidesteps uid-mapping container ugliness.
- Auth collapses: Tier-2 routes are same-origin as the dashboard, so the `malmo_session` cookie just works. No forward-auth, no Authelia-style redirect dance, no embedded iframes.
- We control the UX completely. Curated set means we choose which knobs to expose; the user never has to learn a third-party admin UI per Tier-2 service.

**Knock-ons:**
- Limits Tier-2 to "what's in Debian (or what we package as .deb)." Acceptable for a small curated set.
- Host-agent gains real responsibility — writing config files, calling `systemctl`. Correct: host-agent is the thing that should hold host-level privilege.
- Tier-2 updates ride apt (aligned with `UPDATES.md` v1).
- The "Tier-2 update model — rides OS channel vs. independent" item in `NEXT.md` is now answered: rides OS channel via apt.

**Affected docs:** `AUTH.md` (new), `SERVICE_PROVISIONING.md`, `CONTROL_PLANE.md` (host-agent scope), `NEXT.md`.

---

## 2026-05-14 — Auth & session model: password-only, opaque cookies, recovery code

**Previously:** Auth was an undocumented "Tier 1" topic in `NEXT.md`. We had only the no-SSO-into-apps decision from `SPEC.md`.

**Now:** The full auth model is locked in `AUTH.md`. Headline calls:
- **Password is the only identity primitive in v1.** No passkeys (origin-bound, would break across the toggle), no TOTP, no email-based recovery (no email on file).
- **Sessions are server-side opaque cookies** in a SQLite `sessions` table, 30-day rolling, 90-day hard cap. Not JWTs.
- **Cookie is host-scoped** (no `Domain` attribute). Critical for preserving subdomain isolation between Tier-3 apps and the dashboard.
- **Login UX is a user list** (macOS / Plex style), not a username field. Household device, small known user set.
- **Admin recovery code: opt-in toggle, default on.** Shown once at admin creation, hashed in SQLite, single-use. Phone-photo backup encouraged.
- **No physical-access reset.** Box gets stolen → TPM auto-unlocks LUKS → physical-access reset would hand the thief admin. Rejected.
- **Dashboard password and SSH password are separate.** SSH is off by default; users opt in from Settings.

**Why these vs. the alternatives:**
- **Passkeys** were the main "interesting" idea ruled out. The toggle flips origins (`.local` ↔ `.malmo.network`); passkeys are origin-bound by design, which means re-enrollment per scheme. Bad UX. Password works on either origin.
- **JWTs** offer non-revocable tokens — useless for a single backend with logout / "sign out everywhere" requirements. Opaque cookies + DB lookup is right at this scale.
- **Wildcard `.malmo.local` cookie** would have been the "easy" way to share sessions with Tier-2 subdomains; would have defeated Tier-3 subdomain isolation that `SPEC.md` paid for. Hard no.
- **Same password for dashboard + SSH** would have forced the brain to round-trip every login through host-agent → PAM, doubling failure modes for non-technical users who never use SSH.
- **Cross-origin session handoff token** (drafted earlier) is dropped under the global-toggle model — toggle flips are the only routine origin transitions, re-auth there is accepted.

**Affected docs:** `AUTH.md` (new), `FIRST_RUN.md` (Step 2 + recovery code sub-step), `MALMO_NETWORK.md` (toggle-flip sharp edge points to AUTH), `CONTROL_PLANE.md` (brain session store, host-agent credential mutations), `NEXT.md` (Tier-1 item resolved).

---

## 2026-05-14 — URL access model: collapse to one scheme at a time

**Previously:** "Two URLs always exist per app — `.local` and `.malmo.network`. `.local` is canonical and user-facing; `.malmo.network` is HTTPS plumbing that the brain transparently routes to for apps with `requires_https: true`. A power-user toggle in Settings could flip the dashboard to surface `.malmo.network` URLs everywhere."

**Now:** **One URL scheme at a time, governed by a single global toggle.** Default off → all app URLs are `.local`. Toggle on (which implies enrollment) → all app URLs become `.malmo.network`. There is no per-app routing override and no transparent mixed-scheme tile behavior. Apps that need HTTPS-gated browser APIs (cameras, mic, PWAs, secure cookies) declare `needs_secure_context: true` in the manifest — this triggers a **warning at install time** on a toggle-off box, not a routing override or install block. The user can install anyway and choose whether to flip the toggle.

**Why:**
- The previous model had the brain silently send the user to a *different origin* for some apps. That meant a `requires_https` app would have a different session, different cookies, and different sharing semantics from neighboring apps on the same dashboard, with no UI cue. Inconsistent mental model.
- The "transparent routing" idea also required the brain to know the user's intent (LAN vs. remote, casual vs. secure) — which it doesn't have.
- A global toggle frames the choice honestly: *"Use secure HTTPS URLs for my apps"* is one decision the user makes once, applies to everything, and matches how they actually think about the box ("is my whole house on HTTPS or not?").
- Cross-origin session friction (cookies don't carry across `.local` ↔ `.malmo.network`) only happens once per toggle-flip rather than every time the user opens a `needs_secure_context` app. Re-auth on a deliberate mode switch is acceptable; re-auth on a tile click is not.
- Hard-blocking install of `needs_secure_context` apps on un-enrolled boxes was paternalistic. A warning + user agency is the right shape — some apps degrade gracefully on HTTP, the user can judge.

**Knock-ons:**
- The "brain-mediated session handoff token" idea (drafted but not written into the docs) is **dropped**. Under the one-scheme-at-a-time model, the only origin transition is the deliberate toggle flip, where re-auth is acceptable.
- Naming: `requires_https` → `needs_secure_context`. The new name describes the *cause* (browser secure-context requirement), not the *mechanism* (HTTPS). Better hint for app authors deciding whether to set the flag.
- The dashboard always shows one URL per app. No "two URLs to share with my partner" problem.

**Affected docs:** `MALMO_NETWORK.md`, `APP_MANIFEST.md`, `FIRST_RUN.md`, `APP_LIFECYCLE.md` (Caddy registration).

---

## 2026-05-14 — Auth: no SSO into apps, no session handoff

**Previously:** Open question whether malmo would mediate sessions across `.local` and `.malmo.network` via a one-time handoff token, so users wouldn't re-authenticate when crossing origins.

**Now:** **No SSO, no handoff.** Each app keeps its own auth (already locked in `SPEC.md:64`). The malmo dashboard has its own session. Cross-origin re-auth happens only when the user flips the global URL-scheme toggle, which is a rare deliberate action.

**Why:**
- The one-scheme-at-a-time model eliminates the day-to-day cross-origin case the handoff was solving.
- SSO into apps would require every app to implement an OIDC-style flow against malmo — a real ask we'd be making of app authors, for v1, with no concrete user demand.
- Keeping app auth fully independent preserves the "apps work as upstream authors designed them" principle that drove subdomain routing (`SPEC.md` # "Why subdomain").

**Affected docs:** none yet — this is a confirmation of the existing position, captured here because we considered changing it.

---

## 2026-05-13 — Hooks: deferred from MVP

**Previously:** The manifest included a `hooks:` block for `pre_install`, `post_install`, `pre_update`, `post_update`, etc., running as shell scripts inside the app container.

**Now:** **Hooks are out of v1.** Every concrete use case we could name was tied to managed services or backups — both deferred. When hooks return, they'll be **one-shot container images**, not in-container scripts.

**Why:**
- In-container scripts force app authors to ship a shell + the malmo-specific glue inside their image. That's a real ask for commercial / closed-source apps that don't want shell-execution paths in their distribution images (IP concerns).
- One-shot container images let the app vendor publish a separate migrator image (`photoprism/migrator:2.4.1`) that the brain runs as a transient container with the app's volumes mounted. Clean integration boundary, no in-image patching.
- Deferring entirely (rather than shipping a half-formed in-container version) avoids locking ourselves into the wrong shape.

**Affected docs:** `APP_MANIFEST.md` # F, `APP_LIFECYCLE.md` (lifecycle hooks).

---

## 2026-05-13 — Tier-3 apps cannot use `cap_add`

**Previously:** Apps could declare needed Linux capabilities in the manifest; the brain would enforce.

**Now:** **No `cap_add` for Tier-3 (store) apps, period.** Apps that genuinely need Linux capabilities (VPN clients like Tailscale, FUSE mounts, raw sockets) belong in **Tier 2** — OS integrations curated by malmo with a separate install path.

**Why:**
- Tier-3 is the "anyone can submit, third-party stores allowed" path. Granting Linux capabilities to apps from that channel expands the attack surface beyond what we want to underwrite by default.
- The apps that legitimately need caps (VPN, SMB, DLNA, mount tooling) are a small, identifiable set — they fit Tier 2's "curated by malmo" model naturally.
- Splitting cleanly at the tier boundary is simpler than per-capability allowlists and matches how Umbrel/Synology handle the same problem (system services vs. user apps).

**Affected docs:** `APP_MANIFEST.md` # E, `SERVICE_PROVISIONING.md`, `APP_ISOLATION.md`.

---

## 2026-05-13 — Compose + manifest stay two files

**Previously:** Considered collapsing `docker-compose.yml` and `manifest.yml` into a single file to reduce author cognitive load.

**Now:** **Two files. The compose file is held verbatim by the brain.** The manifest holds malmo-specific metadata only.

**Why:**
- Authors already know `docker-compose.yml`. Keeping it unchanged means "test locally with `docker compose up`, then publish" works without translation steps.
- Verbatim compose means the brain never has to round-trip user-authored YAML through a parser/emitter, which would mangle comments, formatting, and edge-case syntax.
- The override-file pattern (`docker compose -f compose.yml -f override.yml`) gives the brain a clean place to inject env vars, drop capabilities, and bind networks without touching the author's file.
- Two files also gives us a clean schema-versioning story for the manifest without affecting the compose contract.

**Affected docs:** `APP_MANIFEST.md`, `APP_LIFECYCLE.md`.

---

## How to add an entry

When you write one, ask:
1. Did this *flip* a previous position, or is it net-new? (Flips are the highest-value entries.)
2. Will future-me wonder "why did we do this instead of the obvious thing?" If yes, write the entry — capture the obvious-thing-we-rejected explicitly.
3. Is the reasoning derivable from reading the affected doc today? If yes, you may not need an entry — the doc itself is sufficient.

If in doubt, write the entry. The cost of an extra paragraph here is much lower than the cost of relitigating six months later.
