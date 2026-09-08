# Working on malmo

The end-to-end loop for contributing an implementation slice: get oriented, pick a task, branch, build, test, document, open a PR, review it. Read this once; it links out to the docs that own each step rather than repeating them.

This guide is written so a contributor **and their coding agent** can both follow it. If you're driving Claude Code, point it here first — most of what an agent needs to not go off the rails (conventions, where docs live, the definition of done) is one hop from this page.

## Step 0 — Get oriented (~30 min)

Read in this order. Don't skip [`../../CLAUDE.md`](../../CLAUDE.md) — it holds the load-bearing conventions and overrides default agent behavior.

1. **[`../../CLAUDE.md`](../../CLAUDE.md)** — what malmo is, the audience, the locked decisions, and the code/doc discipline you're held to. The annotated map of every spec lives in [`../README.md`](../README.md) # Specs.
2. **[`../specs/SPEC.md`](../specs/SPEC.md)** and **[`../specs/CONTROL_PLANE.md`](../specs/CONTROL_PLANE.md)** — the vision and the control-plane architecture (brain + host-agent + Caddy).
3. **[`../README.md`](../README.md)** — the doc map. You don't read every spec now; you read the one(s) your task touches, end-to-end, when you pick it up.
4. **[`running-locally.md`](running-locally.md)** — get the stack running natively (no VM) before you write a line.
5. **[`testing-brain.md`](testing-brain.md)** + **[`../specs/TESTING.md`](../specs/TESTING.md)** — the test model (brain test pyramid + boot-level lanes).

The spec docs are the source of truth and cross-reference each other heavily. When your task names a spec, read that spec **end-to-end** before changing behavior — a decision in one doc usually constrains others.

## Step 1 — Pick a task

Actionable implementation work lives in **[GitHub Issues](https://github.com/malmoos/malmo/issues)** — the parallel-work board. Find work and claim it:

```bash
gh issue list --label accepted --label P1           # accepted + highest priority; also P2, P3
gh issue list --label accepted --label "area:frontend"  # filter by area: backend / frontend / tooling
gh issue view <N>                                    # full task: spec refs, files to touch, "Done when"
gh issue edit <N> --add-assignee @me                 # claim it — assignment, so no one double-grabs
```

**Only pick issues that carry the `accepted` label and are unassigned.** The `accepted` label means the maintainer has triaged the issue, scoped it, and confirmed it is ready for implementation. Issues without it — however good the idea — have not been reviewed yet; starting on one wastes your time if the scope changes or the issue gets closed. Pick the highest-priority issue (`P1` > `P2` > `P3`) that is **`accepted`**, **unassigned**, and **not labelled `blocked`**, then assign it to yourself before you start.

- Issues are written to be **self-contained** — each names its spec(s), the files it touches, and a crisp *Done when*.
- **Dependencies:** an issue labelled `blocked` has an unmet dependency named in its body (e.g. "Depends on #2"). Don't start it until that issue is closed; drop the `blocked` label when the dep lands.
- The maintainer's critical-path queue is separate — the **Up next** list in [`../progress/README.md`](../progress/README.md). The issue board is the work carved off for parallel contribution; the two are kept from overlapping on purpose.

If a task turns out to need a **design decision that isn't in a spec**, stop — that's a `NEXT.md` item, not implementation. Surface it (comment on the issue + flag the maintainer); don't invent the answer (see [`../specs/NEXT.md`](../specs/NEXT.md) and CLAUDE.md # Working style).

## Step 2 — Branch off `dev`

Always work on a branch; **never commit to `dev` or `main`**. Every change lands via a PR into `dev` — that's the default branch and where feature work integrates. `main` only moves via a dev->main PR the maintainer opens to cut a release (see [Release model](#release-model) below); a contributor never targets `main` directly.

```bash
git checkout dev && git pull             # always start from a fresh dev
git checkout -b <area>/<N>-<short-slug>  # e.g. feat/12-health-banners, fix/8-login-lockout
```

Branch-name shape: `feat/…`, `fix/…`, `test/…`, `docs/…` + the issue number + a short kebab slug (≤4 words, no filler like "add" or "implement"). Including the issue number makes in-flight work visible in `git branch -a` without opening GitHub. One issue per branch — always.

**One PR per issue.** If a task feels too big, split the issue first (file child issues, link them with `Depends on #<N>`, add the `blocked` label to dependents) — then each issue gets its own focused PR. Don't split a single issue across multiple PRs.

**Review feedback goes on the same branch, not a new PR.** When automated or human review flags issues on an open PR, push fixup commits to that branch — never open a second PR for the same issue while one is still open. Opening a second PR splits review history, leaves two competing versions in the queue, and forces the maintainer to decide which to close. To replace a PR entirely (rare), explicitly close the old one first and include "Supersedes #<N>" in the new PR body.

## Step 3 — Build it

The inner dev loop is all native, no VM — see [`running-locally.md`](running-locally.md) (`make dev` runs agent + brain + UI together). Hold to the conventions already in the tree:

- **Go discipline** — CLAUDE.md # Go code discipline (consumer-side interfaces, layer boundaries, `log/slog` only, standard structured field names, typed errors only at boundaries, no premature abstraction). Match the surrounding code.
- **Don't build host-integrated subsystems without the VM outer loop** the specs assume (CLAUDE.md # Repo state). The fake host-agent (`cmd/host-agent`) is the inner-loop stand-in.
- **Keep specs and reality in sync** — if your implementation realizes or *diverges* from a spec, update the matching `docs/specs/` doc in the same change, and add a `DECISIONS.md` entry if a locked decision flips.
- **Scope is exactly the issue, nothing more.** If you encounter something broken or improvable outside the issue scope: search for an existing issue (open or closed) first; if none exists, open one. Never implement it in the current PR. Maintainers decide what gets built and when — that is a product decision, not a developer one.
- **New dependencies must be justified.** Any new `go.mod`/`go.sum` or `package.json`/`package-lock.json` entry must be called out in the PR body with the reason. When in doubt, don't add the dependency.
- **DB schema changes are additive-only.** Never `DROP COLUMN`, `RENAME COLUMN`, or write a destructive `ALTER TABLE`. New columns on existing tables must have a `DEFAULT` or be nullable so the migration runs safely against a live database.
- **Never swallow errors.** `_ = err` or a missing `if err != nil` is almost always wrong. Errors must be returned, logged, or explicitly handled — never silently dropped.
- **User-facing errors must be plain English.** Raw Go error strings must not reach API responses or the dashboard UI. Translate at the API boundary; internal detail goes to `slog` only.

## Step 4 — Test it

Every behavioral change ships with tests. Which layer depends on what you touched — see [`testing-brain.md`](testing-brain.md) for the brain pyramid (unit → store → lifecycle-with-fakes → API → integration → e2e) and [`../specs/TESTING.md`](../specs/TESTING.md) for the boot-level lanes (nspawn fast / QEMU medium / soak).

**Test data is synthetic and self-contained.** Write fixtures with fake data in the shape you need; don't check in a copy of a payload a malmo endpoint serves. This matters most for the app catalog: the artifacts are authored in `malmoos/store` and reach a box only through the published snapshot, so `internal/catalog/testdata/snapshot.json` is hand-written fake apps in the published wire shape. Keep it hand-written — regenerating it from the Go types it is checked against would make `TestNoUnmodeledFields` agree with itself and test nothing. `go test ./internal/catalog -run TestVerifyFixtureSnapshot -update` re-stamps its digest after you edit the shape.

Before you push, run the gate:

```bash
make check          # the pre-PR gate: gofmt-clean + go vet + full test suite
make check-web      # additionally, for any frontend change: web-ui typecheck + build
```

`make check` is the one command that mirrors CI's Go job and the [definition of done](#definition-of-done--checklist) below — formatting, vet, and the full test suite in one pass (fails fast on the cheap checks first). Run `make fmt` to auto-fix formatting if `check` flags it. The full suite needs `libpam0g-dev` (see [`running-locally.md`](running-locally.md)); without the headers, fall back to the narrower targets:

```bash
make fmt-check && make vet    # the non-test gates, no PAM headers needed
make test-nopam               # skip the PAM-cgo target
```

Lane-specific targets exist too (`make test-health`, `make test-caddy`, `make test-usermgr-nspawn`, `make test-boot-chain-nspawn`, `make test-medium-qemu`, …); `make help` lists them. Put unit/store/API tests **in the same package** by default (CLAUDE.md # Go code discipline).

**A green suite is not evidence that your coverage exists.** Both a deleted test and a skipped one report green, so "tests pass" cannot tell you they ran. Two things to check before you push, neither of which the suite will tell you:

- **Did you remove a test?** Look at the deletion count on `*_test.go` in your own diff. Overwriting a test file whole is the usual accident — a directory listing that filters out `_test.go` hides the file you are about to replace.
- **Does your new test run where it matters?** If it is gated on a file, a binary, or an environment, confirm it executes in CI and not just on your machine. CI lists every skipped test in the job summary; if yours is there, it is not coverage. A skip is right when it guards a real environment gap (root, avahi, NetworkManager); it is wrong when it guards something that always exists, like a git-tracked file — that skip will never fire, so the test never runs.

For slices that integrate with a real external system (Docker, PAM, systemd, Caddy, a real VM boot), **unit tests aren't enough** — exercise it against the real thing before you call it done, and say so in the progress entry. Don't rabbit-hole on test scaffolding: if verification keeps failing on the harness rather than the feature, step back and test the feature.

## Step 5 — Document it

A change is not complete until its docs are in the **same change** (CLAUDE.md # Documentation discipline). Concretely:

1. **Write a progress entry.** Add `docs/progress/<slug>.md` (ADR-style, kebab-slug, not numbered) following the template in [`../progress/README.md`](../progress/README.md) # Entry template (status, date, specs touched, what was done, how it maps to specs, known gaps, what's next). Progress entries are **append-only history** — never retro-edit an earlier one; link back to it instead.
   - Be honest in "Known gaps & deviations." Every "handled" claim must be verifiable in the diff — don't assume symmetry between similar code paths.
2. **Update the indexes** in the same change: add your entry to the table + "Latest" list in [`../progress/README.md`](../progress/README.md) and [`../README.md`](../README.md), and re-order the **Up next** queue if you consumed or added a follow-up. A doc not linked from the map is a bug.
3. **Update the spec** you touched if behavior realized or diverged from it. Update the root [`../../README.md`](../../README.md) quickstart if the dev workflow changed.
4. **Markdown style:** no line wrapping (continuous lines, not ~70-char breaks). Never use the `§` symbol — write `#`.

## Step 6 — Open the PR

```bash
git push -u origin <your-branch>
gh pr create --base dev --fill         # then flesh out the body
```

PR body must include **`Closes #<N>`** — do not delete this line. It is the only thing that tells GitHub to auto-close the linked issue when the PR merges; without it the issue stays open and the board goes stale. Replace `<N>` with the issue number. Also include: the spec(s) touched, what you tested (and against what — real Docker? a VM boot?), and any known gaps. If your work unblocks a dependent issue, drop its `blocked` label (`gh issue edit <N> --remove-label blocked`).

**The PR comes before the review, on purpose.** Greptile only runs on an open PR, so a review done before you push cannot include it — and Greptile is not optional (see Step 7). Opening the PR first is what makes the two halves of the review available at all. A PR under review is not a PR asking to be merged; say so in the body if the branch is still moving.

**Don't merge your own PR** unless the maintainer has said to. PRs into `dev` get a review pass first; merging closes the linked issue automatically.

## Step 7 — Review it

A finished self-review has **two halves**, and the PR is not ready for the maintainer until both have been read and acted on.

**Half 1 — a fresh review agent.** It receives only the diff and your progress document — no conversation history — so it has no attachment to the implementation choices you made. In Claude Code, run:

```
/code-review low Read docs/progress/<your-slug>.md first for context, then review the diff per docs/dev/code-review.md.
```

`/code-review low` spawns a fresh subagent with no access to your conversation history, which is what makes it impartial. The `low` effort level limits output to high-confidence findings — fast and cheap. Use `high` or `max` for a particularly complex or risky slice.

**Half 2 — Greptile.** It runs automatically on the open PR and posts its review as a **PR comment a few minutes after the PR opens**, so it is usually not there the moment you look. Read it with `gh pr view <N> --comments` (plus `gh api repos/{owner}/{repo}/pulls/<N>/comments` for line-level comments), then confirm, dismiss, or extend each finding per [`code-review.md`](code-review.md) # Prior review comments.

**An empty comment list means "not posted yet", never "nothing found."** If it has not landed, wait and check again rather than reporting a clean review it might contradict. This is not hypothetical: on #435 the agent review returned no findings and Greptile then caught a real bug in the same diff.

Address every **Block** finding from either half before the PR merges, with **fixup commits on the same branch** — never a second PR (see Step 2). If you disagree with a finding, note it in the progress entry's "Known gaps" section — never silently ignore it.

## Release model

Feature work always branches off `dev` and PRs into `dev` — that's covered above. Releases are a separate, maintainer-only step layered on top, and **the `VERSION` bump is what makes a merge a release** — everything past that point is automatic:

- The maintainer bumps the repo-root `VERSION` file to the new `X.Y.Z` (BUILD.md # Versioning: one repo version for the whole monorepo, not independent per-component SemVer — DECISIONS.md 2026-07-16) as part of preparing the release, and opens a PR from `dev` into `main`. A dev->main PR **is** a release candidate — but only if it bumps `VERSION`; a dev->main merge that doesn't touch `VERSION` ships nothing (see below).
- Both `ci-go.yml` and `ci-web.yml` gate the dev->main PR the same way they gate every feature PR, so a release can't merge with a broken build or a stale OpenAPI/TS client.
- Once the dev->main PR merges, `.github/workflows/release.yml` runs on every push to `main`. It reads `VERSION` and checks whether a `vX.Y.Z` tag for it already exists:
  - **If the tag already exists** (this merge didn't bump `VERSION`), the run is a clean no-op — no tag, no release, no image build. This is the common case for most `main` pushes and is expected to stay green.
  - **If the tag doesn't exist** (this merge bumped `VERSION`), the workflow tags the merge commit `vX.Y.Z`, creates a GitHub Release for it, and then triggers the hosted cloud-image build+publish (`ci-cloud-image.yml`) for that same commit with publishing enabled. The Release notes are the **release PR's own body**, with the generated commit list appended under it; if that body is missing or too short to be a summary, the notes fall back to the generated list alone. So the summary you write in the release PR is what people read on the Release page — write it for someone deciding whether to upgrade, not for someone reading commits.
- The cloud-image build is invoked directly as a reusable workflow (`workflow_call`), not via `ci-cloud-image.yml`'s `push: tags` trigger — a tag pushed with the default `GITHUB_TOKEN` (as `release.yml` does) does not fire another workflow's tag-push trigger, so relying on that event would silently tag a release and never build or publish it. `ci-cloud-image.yml`'s `push: tags: v*` trigger still exists as a manual escape hatch for a human pushing a tag by hand; see that workflow's header comment for the full reasoning.
- A tagged release always ships an image stamped with that same version, runs the full seeded-boot gate, and attaches the image to the GitHub Release as `malmo-vX.Y.Z-amd64.raw.xz` + a `.sha256` sidecar. That Release asset is the only published artifact — the provider-snapshot upload was removed in #352, so a release no longer pushes to any hosting provider. `workflow_dispatch` on `ci-cloud-image.yml` remains available for manual build-only or build+publish runs outside the release flow (see the workflow's header comment).

Contributors never push directly to `main`; the tag and the GitHub Release are created automatically by `release.yml`, not by hand.

### Cutting a release, step by step

The mechanics above say what happens automatically. This is the part a person does, and it is written down because `dev` and `main` drift apart between releases, so a plain `dev` -> `main` PR usually will not merge as-is.

```bash
# 1. Land the version bump on dev, like any other change.
git checkout dev && git pull
git checkout -b release/X.Y.Z
echo "X.Y.Z" > VERSION
# commit, PR into dev, merge.

# 2. Cut the release branch and bring main back into it.
git checkout dev && git pull
git checkout -b release/X.Y.Z            # a fresh branch, after the bump landed
git merge origin/main                    # expect conflicts; see below
```

`VERSION` conflicts on every release, because `main` still holds the previous number. **Always resolve it to the new `X.Y.Z`** — that file is the release trigger, so resolving it the other way ships nothing. Other conflicts are ordinary content: keep both sides unless they genuinely contradict.

Then open the PR from `release/X.Y.Z` into `main`, let `ci-go.yml` and `ci-web.yml` gate it, and merge. `release.yml` does the rest.

**Why the merge-back is needed at all:** anything that landed on `main` without going through `dev` is missing from `dev`, and a `dev` -> `main` PR then conflicts. That should be rare — **every PR targets `dev`, including docs-only and gap-ledger changes**. A PR opened against `main` is the mistake that causes this; if you find one, retarget it to `dev` rather than merging it. After each release, `sync-dev.yml` opens a `main` -> `dev` PR for whatever `main` gained (the merge commit, and anything that slipped in), so the next release starts from a clean `dev`. Merge that sync PR promptly; it is only doing work that grows if left.

## Definition of done — checklist

- [ ] Behavior works in the inner loop (`make dev`), and integration-tested against the real system if it touches one.
- [ ] Tests added at the right layer; `make check` green (frontend changes: `make check-web` too).
- [ ] Progress entry written (`docs/progress/<slug>.md`, kebab-slug, not numbered); indexes in both READMEs updated; Up-next re-ordered if needed.
- [ ] Spec doc updated if behavior realized/diverged; `DECISIONS.md` entry if a locked decision flipped.
- [ ] No `§`, no hard-wrapped markdown, `log/slog` only, conventions per CLAUDE.md.
- [ ] No out-of-scope changes; any out-of-scope finding has an existing or new issue filed, not a fix in this PR.
- [ ] No new dependencies added silently; any new `go.mod`/`package.json` entry called out in the PR body with justification.
- [ ] Schema changes additive-only; no dropped or renamed columns.
- [ ] No swallowed errors (`_ = err`); user-facing error messages are plain English, not raw Go error strings.
- [ ] Commits are logical units with "why" messages; no micro-commit or WIP history.
- [ ] PR opened first, then self-review run — **both halves**: the fresh agent (`/code-review low` with the progress doc as context per [`code-review.md`](code-review.md)) **and** Greptile's PR comment, which lands minutes after the PR opens and is not optional. All Block findings addressed, disagreements noted in Known gaps.
- [ ] Branched off a fresh `dev` (`git checkout dev && git pull` before branching); branch named `<area>/<N>-<short-slug>`.
- [ ] One PR closes exactly one issue; if scope grew, the issue was split first.
- [ ] Review feedback addressed with fixup commits on the existing branch, not a new PR; if the PR was replaced, the old one was closed first with "Supersedes #<N>".
- [ ] PR into `dev` with `Closes #<N>` (do not delete — GitHub won't auto-close the issue otherwise); any dependent issue un-`blocked`.

## When you're stuck or unsure

- **A decision isn't in any spec** → it's an open question. Add/flag it in [`../specs/NEXT.md`](../specs/NEXT.md); don't guess.
- **The spec seems wrong or contradictory** → say so in the PR and tag the maintainer; don't silently work around it.
- **Precision matters** — this is still a spec-driven project. When proposing a change, name the doc and section.
</content>
</invoke>
