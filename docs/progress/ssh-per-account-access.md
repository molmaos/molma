# Per-account SSH: the decision and the brain-to-host seam

- **Status:** done
- **Date:** 2026-09-10
- **Specs touched:** docs/specs/DECISIONS.md, docs/specs/AUTH.md, docs/specs/BUILD.md, docs/specs/ENVIRONMENT.md, docs/specs/BRAIN_HOST_PROTOCOL.md, docs/specs/SETTINGS.md, docs/specs/LOGGING.md

Closes #463. The Settings panel and the image packaging are separate follow-ups, named at the bottom.

## What was done

SSH was specified for the appliance in `AUTH.md` and `BUILD.md` and locked **off** for hosted in `ENVIRONMENT.md`, and none of it existed in code — no host-agent operation, no protocol type, no brain endpoint, no UI. This slice makes the decision, writes it down, and builds everything from the brain's API to the sshd config on the host.

**The decision, recorded in `DECISIONS.md` 2026-09-09.** SSH ships on both profiles, per account, off by default, and **the mandatory auth factor is set by the profile**: a public key on hosted, the malmo password on the appliance. Each profile offers the other factor as an optional **second** lock, rendered as `AuthenticationMethods publickey,password`, so sshd demands both. It is never an alternative, because an account's security is set by its weakest accepted method and an alternative password on hosted would discard the reason for requiring a key at all.

The one-password rule (2026-05-15) was a decision about a LAN, not about passwords. It holds on the appliance because nftables keeps :22 to RFC1918 and the mesh. Hosted has neither, so carrying it across would turn a household password into an internet-facing credential — and into a second, unguarded door to the same credential the brain's login throttling carefully guards, since sshd knows nothing of that throttle.

**Port 22 is open only while at least one account has SSH enabled**, replacing `BUILD.md`'s daemon-on-with-an-empty-allowlist posture. Both of that posture's stated reasons were weak: a `systemctl` call through host-agent is no more visible to a user than a config edit, and "rejects at name resolution" defends a weaker position rather than arguing against a stronger one.

**Measured, not assumed.** The cloud repo's Hetzner provider sets no `Firewalls` field in `ServerCreateOpts` and that package has no firewall code, so a hosted box has every port open today. That is what makes stopping sshd a real port control, and it is why no in-guest `nftables` ruleset was needed — one was deferred on purpose in `DECISIONS.md` 2026-06-19.

**The protocol.** Two Pattern A operations, new in `BRAIN_HOST_PROTOCOL.md`, which said nothing about SSH before: `POST /v1/ssh/set-access` carries one account's **full** desired state, and `GET /v1/ssh/state` is the reconcile read. Full-state rather than a delta so a retry after a partial failure converges instead of compounding, which matters because the brain commits first and calls the host second.

**host-agent (`internal/hostagent/sshaccess`).** Renders the drop-in **whole** from the enabled set on every call, never line-editing it: a global `AllowUsers`, then one `Match User` block per account with that account's `AuthenticationMethods`. Three ordering choices carry weight, and each has a test:

- **Keys are written before the config**, so an account never becomes reachable a moment before the key that authenticates it exists.
- **`sshd -t` runs before the reload**, so a bad render cannot take a running daemon down and lock out the accounts that were already working.
- **A running daemon is reloaded, never restarted.** A restart drops live sessions, and the admin fixing something over SSH is exactly who is connected.

Two more that are easy to get wrong. An empty enabled set writes `DenyUsers *`, because omitting `AllowUsers` means *every* account to sshd — the opposite of what an empty set means, and what a hand-started sshd would then honour. And `Match` blocks are written last, because everything after a `Match` line belongs to that block, so a global keyword written below would silently become the final account's policy.

The enabled set is recovered by **reading the rendered file back**; host-agent keeps no state of its own, so a restart re-reads reality rather than trusting a cache. A file without the malmo header is refused rather than overwritten, since destroying a config someone wrote by hand could lock them out of their own box.

**The brain.** `ssh_access` and `ssh_keys` tables, both additive. Self-service routes under `/api/v1/me/ssh` because Device access is a My-account panel for every user, not an admin one. Every write is elevation-class and audits success and failure. The profile's mandatory factor is enforced server-side: hosted refuses to enable an account with no key, and refuses to remove the last key while SSH is on rather than silently switching SSH off. Keys are validated with `ssh.ParseAuthorizedKey` and **re-serialised from the parsed key**, which drops `authorized_keys` options like `command=` and `from=` — accepting those verbatim would let a paste carry behaviour nobody reviewed. A pasted private key gets its own plain-English refusal, because it is the likeliest user error with the worst consequence.

Adding a key to a **disabled** account does not call the host. There is nothing for sshd to admit, and writing an `authorized_keys` for an account that is off would leave a credential live ahead of the switch.

## What the two reviews changed

Three real defects, all in this slice's own new code. Recorded because the shape of each is worth knowing, not to pad the entry.

**A root privilege-escalation path (Greptile).** The first version wrote each account's keys into `~/.ssh/authorized_keys` as root, exactly as `AUTH.md` described. But `~/.ssh` is a path the account controls, and it can be replaced with a symlink between any check and any use. The `chown` could be redirected onto `/etc`, and the read-modify-write could be redirected into copying a root-only file somewhere the user reads it. Hardening the checks would only have narrowed the race.

The fix removes the user from the path. malmo's keys now live in a **root-owned file outside the home**, `/etc/ssh/malmo-authorized-keys/<user>`, and each account's `Match` block names that file **and** the user's own `.ssh/authorized_keys`. Nothing malmo writes is on a path the account can change, keys a user added from their own shell keep working, and malmo never has to parse, preserve or delete that file. `AUTH.md` and `BRAIN_HOST_PROTOCOL.md` are updated: this is a deliberate divergence from the spec's original wording, with the reason recorded.

**A rejected config was left installed (agent review).** `writeDropIn` claimed to test a candidate and then move it into place, and actually wrote straight to the live path and validated afterwards. So a render sshd rejects stayed on disk, and the next start or reload would fail — the lockout the ordering exists to prevent. The test passed because it only asserted the daemon was not touched. There are now two validations: the candidate is tested on its own before installing, and the combined config is tested after, with the previous file restored if that fails.

**Concurrent writes could drop an account (Greptile).** Every call re-renders one drop-in holding the whole enabled set, and the manager had no lock, so two calls could each render from the same starting point and the second would silently revoke the first's account, or stop sshd while someone still had it on. The brain fans out per user, so this was reachable. `Manager` now serialises `SetAccess`, covered by a test that enables four accounts concurrently and runs under `-race`.

Also from the agent review: the elevation-class delete now audits its 404 and its last-key guard rejection. The guard is the same shape as the last-admin guard `CLAUDE.md` names, so it audits rather than passing as a plain validation failure.

## How it maps to the specs

Realizes `AUTH.md` # Device access for SSH, and reverses the hosted half of `ENVIRONMENT.md` # Access & files under a `DECISIONS.md` entry rather than by drift. Follows the established host seams: consumer-side interface in `internal/hostagent` with the concrete provider in its own package, brain commits first with rollback on host failure, elevation-class writes auditing both outcomes, and a `Match`-per-account config the brain owns and the reconciler can compare.

The division of labour with host-agent matches `set-timezone`: the brain validates and decides policy, host-agent applies. host-agent does not know the environment profile and does not second-guess which factor is mandatory.

## Known gaps & deviations

- **Nothing was run against a real sshd or systemd.** `sshaccess` is covered by unit tests with the command runner and the account lookup faked. The daemon lifecycle is the port control, and a unit test cannot show a port closing — the issue's "Done when" asks for that verification and it has **not** been done. It needs the QEMU medium lane or a booted box.
- **No reconcile loop consumes `GET /v1/ssh/state` yet.** The endpoint and the client method exist and are covered, but nothing polls them on the heartbeat, so drift is not surfaced anywhere. The brain re-pushes only when the user acts.
- **No UI.** `web-ui` is untouched, so the feature is unreachable from the dashboard today. The Device access panel is follow-up 1.
- **The image ships no sshd.** The hosted mkosi profile still omits `openssh-server`, and `dev/cloud/expected-packages.txt` still names it as a package that must never appear, so the CI lean check would fail if it were added. Until follow-up 2, a hosted box has nothing for these calls to configure.
- **The appliance nftables drop-in does not exist either.** `BUILD.md` specifies it and no build file writes it, which predates this change and is unchanged by it.
- **`sshd -t` validates the whole config, not the drop-in alone.** On a host whose main `sshd_config` is already broken, every write here fails. That is the safe direction, but the error the user sees will name our call rather than the real cause.
- **Key comments are kept and only stripped of newlines.** A comment is attacker-influenced text that lands in a root-owned file. Newlines are the part that could add a second key line; the rest is preserved because users identify keys by it.
- **The brain does not serialise its own concurrent pushes.** host-agent is now safe against concurrent calls, but two overlapping requests from one user can still reach it in either order, and the store has no revision to reject a stale one. Low risk for a self-service panel one person drives; it would need a revision column to close properly.
- **A failed revocation is not retried.** Removing a key commits in the brain and then pushes; if the push fails the key stays live on the host until something re-pushes, and nothing does today because no reconcile loop consumes `GET /v1/ssh/state`. The failure is logged and audited, so it is visible, but visibility is not recovery. This is the sharpest reason the reconcile follow-up matters.
- **PAM builds need `CGO_CFLAGS=-D_GNU_SOURCE` on this machine.** Pre-existing and unrelated to this change; `make check` fails at `vet` without it here.

## What's next

1. **The Device access panel** in `AccountSection.vue`. Users must be able to **upload a key file or paste the text**, both paths, per the product call on this slice. Build it from the local Tailwind Plus mirror.
2. **Image packaging.** `openssh-server` into the hosted mkosi profile plus the `expected-packages.txt` lockfile and its header comment; the appliance sshd hardening drop-in and the nftables LAN-scoping rule.
3. **Verify the daemon lifecycle on a booted box**, which is what closes the gap this entry names first.
4. **Reconcile `GET /v1/ssh/state`** on the heartbeat so drift is surfaced rather than only re-pushed on user action.
5. **Operator access**, separately: an SSH certificate authority the hosted image trusts, so fleet debug access does not depend on a customer-facing toggle. Control-plane side, and it decides whether the image ships a `TrustedUserCAKeys` line.
