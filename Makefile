# malmo dev orchestration. The fast inner loop runs everything natively on the
# host (no VM): host-agent + brain as Go processes, Caddy as a container, the
# UI on Vite. The VM is the outer loop for host-integrated parts (boot, LUKS,
# systemd) and is not wired here yet.

GO ?= $(shell command -v go || echo $(HOME)/.local/go/bin/go)
# gofmt from the same toolchain as $(GO), so `make check` matches CI exactly
# regardless of what's on PATH.
GOFMT ?= $(shell $(GO) env GOROOT)/bin/gofmt
DEV_DIR := .dev
STATE_DIR := $(DEV_DIR)/state
AGENT_SOCK := $(abspath $(DEV_DIR)/agent.sock)

# Build identity (BUILD.md # Versioning): one repo VERSION for the whole
# monorepo, plus the git commit a build was cut from — two stamped fields, no
# "-dev" suffix logic (DECISIONS.md 2026-07-16). VERSION is read from the repo
# root; the commit falls back to "unknown" outside a git checkout (e.g. a
# container build context with no .git) rather than failing the build.
MALMO_VERSION := $(shell cat $(CURDIR)/VERSION)
MALMO_COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
# The minisign public keys a host-agent build accepts for the appliance release
# manifest (RELEASE_MANIFEST.md # Signing). Comma-separated base64 key lines.
# EMPTY BY DEFAULT, and that is the safe state: a build with no key refuses every
# manifest and does not poll at all, rather than trusting one. There is no
# runtime override on purpose — changing which releases a box accepts should take
# a new, apt-signed binary, not an edit to a unit file.
MALMO_RELEASE_KEYS ?=
LDFLAGS := -X github.com/malmoos/malmo/internal/version.Version=$(MALMO_VERSION) \
           -X github.com/malmoos/malmo/internal/version.Commit=$(MALMO_COMMIT) \
           -X github.com/malmoos/malmo/internal/hostagent/relmanifest.BakedKeys=$(MALMO_RELEASE_KEYS)

export MALMO_AGENT_SOCK := $(AGENT_SOCK)
export MALMO_STATE_DIR := $(STATE_DIR)
# The brain syncs the catalog from the control plane (MALMO_CATALOG_URL, default
# the public apex) and holds it in memory; only proxied icons and screenshots are
# cached, here. Point that at a writable dev path so `make dev` (native, non-root)
# can write it; set MALMO_CATALOG_URL to a local control plane to develop the
# store offline. To boot against a local snapshot instead, see `make dev-app`.
export MALMO_CATALOG_CACHE_DIR := ./.dev/catalog-cache

.PHONY: build host-agent brain host-agent-real host-agent-real-hosted brain-image ui-image control-plane-images caddy-acmedns-image build-cloud-image check check-web fmt fmt-check vet test test-nopam test-caddy test-avahi test-netstate test-health test-usermgr test-usermgr-nspawn test-boot-chain-nspawn test-medium-qemu test-cloud-qemu run-agent run-brain net caddy caddy-down ui dev dev-app seed-catalog stop openapi openapi-check clean check-state-owner help

# msteinert/pam v2.1.0 uses RTLD_NEXT, a GNU extension that requires
# _GNU_SOURCE at C compile time. Apply globally; harmless to non-cgo builds.
export CGO_CFLAGS := -D_GNU_SOURCE

help:
	@echo "make build       - compile brain + host-agent"
	@echo "make build-cloud-image - build the self-bootstrapping hosted cloud VM image via mkosi (needs sudo; #203/#242)"
	@echo "make caddy       - start the dev Caddy reverse proxy (container)"
	@echo "make caddy-down  - stop the dev Caddy"
	@echo "make check       - pre-PR gate: gofmt + vet + full test suite (Go). Run before every PR."
	@echo "make check-web   - pre-PR gate for frontend changes: web-ui typecheck + build"
	@echo "make clean       - stop apps, remove dev state"
	@echo "make control-plane-images - build malmo-brain + malmo-ui images and docker-save the control-plane bundle to .dev/"
	@echo "make caddy-acmedns-image  - build the hosted Caddy (stock Caddy + the caddy-dns/acmedns module)"
	@echo "make dev         - all three foreground procs in one terminal (recommended); Go edits rebuild + restart the brain"
	@echo "make dev-app APP=<id> [STORE=../store] - boot ONE store app under curation: seed its catalog snapshot, then make dev with an inert catalog URL"
	@echo "make seed-catalog APPS=\"<id> <id> ...\" [HOMEFILE=<path/to/home.yml>] - seed several store apps (+ optionally the curated landing) into a local snapshot file, without starting dev"
	@echo "make fmt         - rewrite Go sources into gofmt-canonical form (autofix)"
	@echo "make host-agent-real-hosted - build the slim hosted-cloud host-agent (-tags hosted; #204/C1c)"
	@echo "make net         - create the malmo-ingress docker network"
	@echo "make openapi     - regenerate api/openapi.{json,yaml} from the brain (no server)"
	@echo "make run-agent   - run the fake host-agent (foreground)"
	@echo "make run-brain   - run the brain (foreground)"
	@echo "make stop        - stop the native dev stack (brain/host-agent/vite)"
	@echo "make test        - run the full Go test suite (needs libpam0g-dev)"
	@echo "make test-avahi  - Avahi DBus publisher integration test (needs avahi-daemon)"
	@echo "make test-boot-chain-nspawn - boot dist/systemd units in nspawn + assert shape (needs sudo)"
	@echo "make test-caddy  - end-to-end Caddy routing test (requires make dev)"
	@echo "make test-cloud-qemu - QEMU boot of the hosted cloud image; control plane up (needs sudo; no swtpm/LUKS)"
	@echo "make test-health - end-to-end storage-health pipeline (self-contained, ~3s)"
	@echo "make test-medium-qemu - QEMU+swtpm boot with real kernel + TPM (needs sudo; first run ~5 min)"
	@echo "make test-netstate - NetworkManager LAN-interface integration test"
	@echo "make test-nopam  - full test suite minus pamverifier (no libpam0g-dev needed)"
	@echo "make test-usermgr - LinuxUserManager integration test (needs sudo; nspawn lane recommended instead)"
	@echo "make test-usermgr-nspawn - run usermgrtest in systemd-nspawn (needs sudo)"
	@echo "make ui          - run the Vite dev server (web-ui/)"
	@echo ""
	@echo "One-terminal: make dev   (Caddy started detached; Ctrl-C stops the rest)"
	@echo "Four terminals: make caddy ; make run-agent ; make run-brain ; make ui"

# ---- Quality gate -------------------------------------------------------
# `make check` is the pre-PR gate. It mirrors CI's Go job and the
# definition-of-done in docs/dev/contributing.md: gofmt-clean, vet-clean, and
# the full test suite green. Cheapest checks run first so it fails fast.
# Frontend changes additionally need `make check-web`. The full test suite
# needs libpam0g-dev (see docs/dev/running-locally.md); use the individual
# targets if you don't have the headers.
check: fmt-check vet openapi-check test

# Web typecheck + production build (mirrors CI's web job). Needs node/npm.
# Regenerates the OpenAPI TS client from the committed spec and fails if the
# checked-in copy (web-ui/src/generated/openapi.ts) is stale — keeps the
# generated client honest the way openapi-check keeps the spec honest.
check-web:
	cd web-ui && npm ci && npm run gen:api
	@git diff --quiet web-ui/src/generated/openapi.ts || { \
	  echo "web-ui/src/generated/openapi.ts is stale — regenerate with: (cd web-ui && npm run gen:api)"; exit 1; }
	cd web-ui && npm run build

# Rewrite Go sources into gofmt-canonical form (autofix).
fmt:
	$(GOFMT) -w $$(git ls-files '*.go')

# Fail (listing offenders) if any Go source isn't gofmt-clean. Pure check —
# never mutates the tree; run `make fmt` to fix.
fmt-check:
	@out=$$($(GOFMT) -l $$(git ls-files '*.go')); \
	  if [ -n "$$out" ]; then \
	    echo "These files are not gofmt-clean:"; echo "$$out"; \
	    echo "Fix with: make fmt"; exit 1; \
	  fi

# `go vet ./...` does NOT compile a test file behind a build tag — it is excluded
# from the build, so a signature change can leave it uncompilable and the gate
# stays green. That is not hypothetical: the #434 lifecycle.Install signature
# change left dockerlive_test.go broken through a full green `make check`, and a
# PR reviewer caught it. Vet the tagged variants too. These need no hardware and
# run no test — vet only type-checks — so they are cheap and belong in the gate;
# actually RUNNING them still needs the real system each tag names (TESTING.md).
VET_TAGS := dockerlive usermgrtest avahitest nmtest pamtest

# The packages the gate covers: every directory holding a TRACKED .go file.
#
# Not `./...`, which walks the working tree and therefore also walks whatever a
# local build left behind. `dev/cloud/mkosi.tools/` is the real case — it is
# gitignored, it holds vendored third-party sample code, and `go vet ./...`
# reports that code and fails. CI never sees it (it is not in the repo), so the
# local gate went red for something CI is structurally incapable of catching,
# which is the fastest way to teach people to ignore a gate.
#
# `git ls-files` is the same idiom `fmt-check` already uses for exactly this
# reason. It also keeps covering any package added later, which a hardcoded
# list of top-level directories would not.
GOPKGS = $(shell git ls-files '*.go' | xargs -n1 dirname | sort -u | sed 's|^|./|')

# An empty GOPKGS would make `go test` fall back to testing the CURRENT
# directory and exit 0 — a gate that passes having covered nothing. That is the
# #375 failure exactly (a guard pointed at a path it never matched, green
# forever), so it fails loudly instead. It can only happen outside a git
# checkout, e.g. an unpacked tarball.
require-gopkgs:
	@if [ -z "$(GOPKGS)" ]; then \
	  echo "GOPKGS is empty — no tracked .go files found."; \
	  echo "This target derives its package list from git; run it inside a git checkout."; \
	  exit 1; \
	fi

vet: require-gopkgs
	$(GO) vet $(GOPKGS)
	@for tag in $(VET_TAGS); do 	  echo "$(GO) vet -tags $$tag <tracked packages>"; 	  $(GO) vet -tags $$tag $(GOPKGS) || exit 1; 	done

# `build` stays host-agent (fake) + brain, unchanged from before this slice.
# host-agent-real is deliberately NOT folded in: it's Linux + CGO +
# libpam0g-dev always (see its header comment — both its build tags need real
# PAM), so it already doesn't build on macOS/Windows/WSL2-without-headers.
# Making it part of the default `build` would break `make build` on exactly
# the machines the inner loop is supposed to work on with no platform-specific
# setup (CLAUDE.md # Developing). It's still stamped — see its own target below
# — for anyone building it directly or via the cloud-image / nspawn lanes.
build: host-agent brain

# The fake host-agent is stamped too — it prints --version like the real
# binaries and its self-reported agent_version (internal/hostagent.AgentVersion)
# derives from the same stamped internal/version.Version, so a dev build's fake
# agent and dev brain agree without a separate hardcoded constant.
host-agent:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(DEV_DIR)/host-agent ./cmd/host-agent

host-agent-real:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(DEV_DIR)/host-agent-real ./cmd/host-agent-real

# Slim hosted-cloud host-agent (ENVIRONMENT.md # How the profile is realized —
# "A build-tagged slim cloud host-agent"; #204/C1c). The same production binary
# with the appliance's LAN/discovery stack — NetworkManager (netstate) + Avahi
# mDNS publish (avahipublisher) + the network watcher — compiled out via
# `-tags hosted`; the kept seams (PAM verify, user mgmt, health/system reporters,
# per-app logs, reboot, brain launch) are identical. Linux + CGO + libpam0g-dev,
# same as host-agent-real. The cloud image build (#203/C1b, #205/C2) consumes it.
host-agent-real-hosted:
	$(GO) build -tags hosted -ldflags "$(LDFLAGS)" -o $(DEV_DIR)/host-agent-real-hosted ./cmd/host-agent-real

brain:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(DEV_DIR)/brain ./cmd/brain

# ---- Control-plane images (M0, #163) -----------------------------------
# Build the two malmo OCI images and `docker save` them — together with the two
# third-party control-plane images the brain's compose names — into a tarball
# bundle under .dev/ (BUILD.md # 5 / # 5b; TESTING.md # Full-stack control-plane
# integration). The medium-lane VM bakes this bundle and docker-loads it at
# first boot; it has no network, so the third-party images must be in the bundle
# too. Needs only Docker (the images build hermetically — no host Go/Node).
CP_IMAGE_DIR := $(DEV_DIR)/control-plane
BRAIN_IMAGE  := malmo-brain:dev
UI_IMAGE     := malmo-ui:dev
CADDY_ACMEDNS_IMAGE := malmo-caddy-acmedns:dev
# Every third-party build input — the two images the box ships, the bases all
# four malmo/hosted images are built on, and the module compiled into the hosted
# Caddy — lives in one checked-in file, so a shipped box can be traced back to
# the exact bytes it runs (#432; BUILD.md # 5c).
include dev/control-plane/images.lock
# The tag half of a pin. We pull by digest but save under the plain tag, because
# a box loads the tarball and the compose file names the image by tag
# (dev/control-plane/compose.yml) — it never pulls, so the digest cannot be its
# lookup key there.
CADDY_TAG := $(firstword $(subst @, ,$(CADDY_IMAGE)))
PROXY_TAG := $(firstword $(subst @, ,$(PROXY_IMAGE)))

brain-image:
	docker build -f cmd/brain/Dockerfile --build-arg MALMO_COMMIT=$(MALMO_COMMIT) \
	  --build-arg BRAIN_BUILDER_IMAGE=$(BRAIN_BUILDER_IMAGE) \
	  --build-arg BRAIN_RUNTIME_IMAGE=$(BRAIN_RUNTIME_IMAGE) \
	  -t $(BRAIN_IMAGE) .

# malmo-ui's runtime base is CADDY_IMAGE, the same pin the proxy runs — one Caddy
# for both, not two pins to keep level.
ui-image:
	docker build -f web-ui/Dockerfile \
	  --build-arg UI_BUILDER_IMAGE=$(UI_BUILDER_IMAGE) \
	  --build-arg UI_RUNTIME_IMAGE=$(CADDY_IMAGE) \
	  -t $(UI_IMAGE) web-ui

control-plane-images: brain-image ui-image
	@mkdir -p $(CP_IMAGE_DIR)
	docker pull $(CADDY_IMAGE)
	docker pull $(PROXY_IMAGE)
	docker tag $(CADDY_IMAGE) $(CADDY_TAG)
	docker tag $(PROXY_IMAGE) $(PROXY_TAG)
	docker save $(BRAIN_IMAGE) -o $(CP_IMAGE_DIR)/malmo-brain.tar
	docker save $(UI_IMAGE)    -o $(CP_IMAGE_DIR)/malmo-ui.tar
	docker save $(CADDY_TAG)   -o $(CP_IMAGE_DIR)/caddy.tar
	docker save $(PROXY_TAG)   -o $(CP_IMAGE_DIR)/docker-socket-proxy.tar
	@echo "saved control-plane image bundle to $(CP_IMAGE_DIR)/"

# The hosted profile's Caddy: stock Caddy plus the caddy-dns/acmedns module, for
# the wildcard cert's ACME DNS-01 (ENVIRONMENT.md # Networking & discovery). Both
# halves of the xcaddy build and the plugin module come from the pin file, so the
# Dockerfile carries no unpinned default — build it through this target, not `docker build` by hand.
# dev/cloud/stage-control-plane.sh calls it, then docker-saves the result.
caddy-acmedns-image:
	docker build \
	  --build-arg CADDY_ACMEDNS_BUILDER_IMAGE=$(CADDY_ACMEDNS_BUILDER_IMAGE) \
	  --build-arg CADDY_ACMEDNS_BASE_IMAGE=$(CADDY_ACMEDNS_BASE_IMAGE) \
	  --build-arg CADDY_ACMEDNS_MODULE=$(CADDY_ACMEDNS_MODULE) \
	  -t $(CADDY_ACMEDNS_IMAGE) dev/control-plane/caddy-acmedns/

# Run the full suite. Requires libpam0g-dev for the pamverifier package.
# GOTESTFLAGS passes extra flags through to `go test` — CI sets it to -v so
# skipped tests are visible (a skip prints nothing without it, so a test that
# never runs reads exactly like one that passed). See .github/workflows/ci-go.yml.
test: require-gopkgs
	$(GO) test $(GOTESTFLAGS) $(GOPKGS)

# Skip the pamverifier package (no libpam0g-dev required).
test-nopam: require-gopkgs
	$(GO) test $(GOTESTFLAGS) $$($(GO) list $(GOPKGS) | grep -v pamverifier)

# Integration tests for the Avahi DBus publisher. Requires avahi-daemon
# running on the host. No sudo needed (default DBus policy allows it).
test-avahi:
	$(GO) test -tags avahitest ./internal/hostagent/avahipublisher/

# Integration tests for the NetworkManager LAN-interface provider. Requires
# NetworkManager running on the host. No sudo needed (read-only DBus calls).
test-netstate:
	$(GO) test -tags nmtest ./internal/hostagent/netstate/

# Integration tests for LinuxUserManager. Exercises real useradd + chpasswd
# against /etc/passwd and /etc/shadow. MUST run as root and is intended for
# the nspawn lane — do NOT run on a developer laptop. See
# docs/progress/0015-host-agent-set-password.md.
test-usermgr:
	sudo -E $(GO) test -tags usermgrtest ./internal/hostagent/usermgr/

# Run the usermgrtest-tagged tests inside systemd-nspawn (fast lane per
# docs/specs/TESTING.md). Bootstraps a minimal Debian rootfs at
# .dev/nspawn/rootfs on first run (cached after); each test invocation
# runs in an ephemeral overlay. Requires mmdebstrap + systemd-container.
# See docs/progress/0018-nspawn-usermgr-lane.md.
test-usermgr-nspawn:
	sudo -E ./dev/test-nspawn/run-usermgr-tests.sh

# Boot-chain fast-lane test: systemd-nspawn --boot of the dist/systemd
# units, asserting dependency shape, drop-in application, and end-to-end
# storage-verify reporter execution. Reuses the .dev/nspawn/rootfs
# bootstrapped by run-usermgr-tests.sh (bumped to v2 for systemd-sysv).
# See docs/progress/0020-nspawn-boot-chain-lane.md.
test-boot-chain-nspawn:
	sudo -E ./dev/test-nspawn/run-boot-chain-tests.sh

# Medium-lane test: QEMU+swtpm boot of a mkosi-built bookworm image
# with a real kernel, real systemd userspace, and an emulated TPM.
# Proves the scaffolding for the TESTING.md # Medium lane is operational.
# First run builds the image (~3-5 min); subsequent runs ~1-2 min.
# Requires mkosi v22+, swtpm, qemu-system-x86, ovmf — bootstrap.sh
# prints an install pointer if anything is missing.
# See docs/progress/0021-qemu-medium-lane-scaffolding.md.
test-medium-qemu:
	sudo -E ./dev/test-qemu/run-medium-tests.sh

# Cloud-lane boot proof (C2, #205): build the hosted cloud image, convert it to
# the qcow2 cloud artifact, and boot it ONCE in QEMU to prove the control plane
# comes up and serves — no swtpm, no LUKS, no installer ("the disk IS the
# installed system", ENVIRONMENT.md # Provisioning). The in-VM self-check
# (cloud-assertions.sh) asserts the baked images loaded, the four control-plane
# containers run, the dashboard answers through Caddy, and the hosted /setup gate
# returns 503 (no seed). Air-gapped (restrict=on) so a stray pull hard-fails.
# Requires mkosi v22+, qemu-system-x86, ovmf, docker, go, libpam0g-dev —
# bootstrap.sh prints an install pointer if anything is missing.
# NOTE: do not run test-cloud-qemu and build-cloud-image in parallel — both stage
# into dev/cloud/mkosi.extra.wiring/ and will race on the rm -rf at the start.
# See docs/progress/cloud-vm-boot-proof.md.
test-cloud-qemu:
	sudo -E ./dev/cloud/run-cloud-tests.sh

# Build the hosted cloud-VM image (C1b #203; first-boot wiring #242) via mkosi:
# the lean Debian + docker base PLUS the baked first-boot runtime wiring (slim
# host-agent, networkd DHCP config, control-plane image bundle, seed materializer)
# so a provisioned box self-bootstraps instead of booting network-less (#242). Then
# assert it is still lean (no NetworkManager/Avahi/Samba/mergerfs/cryptsetup/tpm2-
# tools — the wiring adds no apt packages) with /etc/malmo/profile=hosted. Output: a
# raw GPT disk image under .dev/cloud/; the cloud repo snapshots it as the tenant
# image. Needs root (control-plane image build + mkosi disk ops) + mkosi v22+, go,
# docker, libpam0g-dev; bootstrap.sh prints an install pointer if anything is missing.
# NOTE: do not run build-cloud-image and test-cloud-qemu in parallel — both stage
# into dev/cloud/mkosi.extra.wiring/ and will race on the rm -rf at the start.
# See docs/progress/hosted-cloud-image.md, docs/progress/cloud-image-first-boot-wiring.md.
build-cloud-image:
	sudo -E ./dev/cloud/bootstrap.sh

# End-to-end Caddy routing verification. Assumes `make dev` is running.
# Tests Host-header routing, confirms path-based routing does NOT work,
# and verifies route withdrawal after uninstall.
test-caddy:
	./dev/test-caddy-routing.sh

# Self-contained end-to-end test of the storage-health pipeline
# (docs/progress/0019). Builds the three binaries, spins up the fake
# host-agent + brain in a tempdir, exercises six cases through the real
# wire format, and tears down. ~3 seconds. No daemons required.
test-health:
	./dev/test-health.sh

net:
	@docker network inspect malmo-ingress >/dev/null 2>&1 || docker network create malmo-ingress

caddy: net
	docker compose -f dev/docker-compose.yml up -d

caddy-down:
	docker compose -f dev/docker-compose.yml down

run-agent: host-agent
	@mkdir -p $(DEV_DIR)
	$(DEV_DIR)/host-agent

run-brain: brain net
	@mkdir -p $(STATE_DIR)
	$(DEV_DIR)/brain

# WEB_UI.md specifies pnpm; npm is used here until pnpm is set up on the box.
ui:
	cd web-ui && npm install && npm run dev

# Guard against a root-owned dev state dir. App containers run as root in the
# skeleton and write root-owned files into instances/<id>/; a privileged run or
# a half-finished manual `rm` (which can't remove that root-owned data) can leave
# instances/ itself root-owned. The brain then fails an install mid-transaction
# with a cryptic `mkdir … permission denied` (after a SQLite row already exists).
# Catch it up front with an actionable message. The supported reset is `make
# clean` (reclaims root-owned data via a throwaway root container), never a hand `rm`.
check-state-owner:
	@if [ -d "$(STATE_DIR)/instances" ] && [ "$$(stat -c %u "$(STATE_DIR)/instances")" != "$$(id -u)" ]; then \
	  echo "error: $(STATE_DIR)/instances is owned by uid $$(stat -c %u "$(STATE_DIR)/instances"), not you (uid $$(id -u))."; \
	  echo "       App installs will fail with 'mkdir … permission denied'."; \
	  echo "       Reset with:  make clean      (or: sudo chown -R $$(id -un) $(STATE_DIR))"; \
	  exit 1; \
	fi

# One-terminal dev loop. Pure bash: backgrounds the three foreground procs,
# prefixes their output with [agent]/[brain]/[ui], and the trap kills the
# whole process group on Ctrl-C. Caddy is started detached because it's
# already a long-running container — no point supervising it here.
dev: check-state-owner build caddy
	@mkdir -p $(STATE_DIR)
	@cd web-ui && [ -d node_modules ] || npm install
	@trap 'kill 0' INT TERM EXIT; \
	  (GO="$(GO)" DEV_DIR="$(DEV_DIR)" LDFLAGS="$(LDFLAGS)" ./dev/dev-go.sh) & \
	  (cd web-ui && npm run dev 2>&1 | sed -u 's/^/[ui]    /') & \
	  wait

# ---- Curate one or more store apps (+ optionally the landing) against the
# brain -------------------------------------------------------------------
# Post-catalog-cutover (cloud #62) there is no baked os/catalog/ to boot from —
# the brain is a thin HTTP client of the control plane. `make dev-app APP=<id>`
# restores the inner loop for authoring/curating a store app: it builds a local
# snapshot from a store checkout (STORE/apps/APP) with mkcatalog, then runs the
# normal dev stack with MALMO_CATALOG_FILE pointing at it. The brain reads that
# file once at boot (internal/catalog/remote.go # loadSnapshotFile) and installs
# the app from it. The file is an input the brain never writes back — a box keeps
# no catalog on disk. A seed inlines each app's manifest and compose, because a
# staged file has no control plane behind it to serve the per-app document routes
# a real box fetches an install payload from (#434). Environment visibility is not
# in the seed either: a real box gets it from the ?env= on its own fetch, so a
# seeded store shows every app it was given.
#
# `make seed-catalog APPS="<id> <id> ..." [HOMEFILE=<path/to/home.yml>]` is the
# multi-app form: it seeds every named package into one snapshot and, when
# HOMEFILE is given, carries that home.yml's spotlight + groups too, so the
# curated landing page (docs/specs/APP_STORE.md # Landing page) is clickable
# locally. mkcatalog fails loudly if home.yml names an app that wasn't also
# passed in APPS — the point of seeding locally is to catch that before a real
# publish does. The store's categories.yml is picked up automatically when the
# checkout has one, so the seeded store renders the authored category labels
# rather than the readable-id fallback. (Named HOMEFILE, not HOME, so it can't
# collide with the shell's own $HOME.) APP=<id> keeps working unchanged — it is
# just the single-app case of the same underlying seed-catalog recipe.
#
# It uses mkcatalog, NOT cloud's catalog-sync, on purpose: catalog-sync publishes
# only listed:true records, but an app under curation has no verdict yet — you
# boot it to *decide* whether it is full or degraded. mkcatalog reads the
# manifest+compose directly and ignores status.yml, so no provisional listed:true
# is ever needed (and can't be committed by accident).
STORE ?= ../store
# CATALOG_SEED is the local snapshot seed-catalog writes and dev-app boots from
# (MALMO_CATALOG_FILE). It is dev scaffolding under .dev/, never a box path.
CATALOG_SEED := $(DEV_DIR)/catalog-seed.json

seed-catalog:
	@ids="$(APPS)"; [ -n "$$ids" ] || ids="$(APP)"; \
	  [ -n "$$ids" ] || { echo "usage: make seed-catalog APP=<id> | APPS=\"<id> <id> ...\" [STORE=../store] [HOMEFILE=path/to/home.yml]" >&2; exit 2; }; \
	  pkgflags=""; \
	  for id in $$ids; do \
	    [ -d "$(STORE)/apps/$$id" ] || { echo "error: no app package at $(STORE)/apps/$$id" >&2; exit 2; }; \
	    pkgflags="$$pkgflags -pkg $(STORE)/apps/$$id"; \
	  done; \
	  homeflag=""; \
	  [ -z "$(HOMEFILE)" ] || homeflag="-home $(HOMEFILE)"; \
	  catsflag=""; \
	  [ ! -f "$(STORE)/categories.yml" ] || catsflag="-categories $(STORE)/categories.yml"; \
	  mkdir -p $(DEV_DIR) && \
	  $(GO) run ./dev/mkcatalog $$pkgflags -out $(CATALOG_SEED) $$homeflag $$catsflag && \
	  echo "seeded [$$ids] -> $(CATALOG_SEED)$${homeflag:+, landing from $(HOMEFILE)}$${catsflag:+, category labels from $(STORE)/categories.yml}"

# The inert catalog URL and the seed file are target-specific, exported
# variables, so they are in effect for the `dev` prerequisite's recipe too — the
# brain reads both from the env, and cmd/brain defaults MALMO_CATALOG_URL to the
# real apex (https://malmo.network). Without the override the first background
# sync would succeed and replace the seed with the published catalog, silently
# dropping the app(s) under test. Port 1 has nothing listening, so the sync fails
# fast (same inert-URL trick as dev/test-health.sh). seed-catalog runs first and
# aborts the whole target if no app was given, so `dev` never starts against a
# bad seed. Accepts APP or APPS (+ optional HOMEFILE) exactly like seed-catalog.
dev-app: export MALMO_CATALOG_URL := http://127.0.0.1:1
dev-app: export MALMO_CATALOG_FILE := $(CATALOG_SEED)
dev-app: seed-catalog dev

# Regenerate the committed OpenAPI spec (api/openapi.{json,yaml}) from the huma
# handler registrations — no running brain, no port (BRAIN_UI_PROTOCOL.md
# # Codegen). The spec is the substrate for the web-ui's generated TS client
# (web-ui `npm run gen:api`) and the freshness gate below.
openapi:
	@go run ./cmd/openapi-gen -o api && echo "wrote api/openapi.json, api/openapi.yaml"

# Fail if the committed spec is stale (re-emit to a scratch dir and diff). Pure
# check — never mutates the tree; run `make openapi` to refresh. Mirrors the
# fmt-check pattern; wired into `make check` and CI so a brain DTO change that
# isn't regenerated can't merge silently.
openapi-check:
	@tmp=$$(mktemp -d); \
	  go run ./cmd/openapi-gen -o $$tmp || { rm -rf $$tmp; exit 1; }; \
	  if ! diff -q api/openapi.json $$tmp/openapi.json >/dev/null || ! diff -q api/openapi.yaml $$tmp/openapi.yaml >/dev/null; then \
	    echo "api/openapi.{json,yaml} is stale — regenerate with: make openapi"; \
	    diff -u api/openapi.json $$tmp/openapi.json || true; \
	    rm -rf $$tmp; exit 1; \
	  fi; \
	  rm -rf $$tmp; echo "openapi spec is fresh"

# Stop the native dev stack (`make dev` runs brain/host-agent/vite outside
# Docker). Without this, `clean` leaves the brain running with the deleted
# malmo.db still open (deleted-but-open inode), so it keeps serving the old
# state and the wiped DB silently comes back — `clean` looks like a no-op.
# Best-effort: pkill exits non-zero when nothing matches, hence the `-` prefix.
# The supervisor is matched by its MALMO_DEV_AVAHI env prefix; the binaries by
# their $(DEV_DIR) path; vite by this repo's absolute path so we don't reap an
# unrelated Vite on the box.
stop:
	-@pkill -f 'MALMO_DEV_AVAHI=1' 2>/dev/null
	-@pkill -f '$(DEV_DIR)/brain' 2>/dev/null
	-@pkill -f '$(DEV_DIR)/host-agent' 2>/dev/null
	-@pkill -f '$(CURDIR)/web-ui/node_modules/.bin/vite' 2>/dev/null
	@echo "stopped native dev stack (brain/host-agent/vite)"

# clean = back to a blank slate: stop the native stack (stop) and the Caddy
# container (caddy-down), remove app containers/networks, then wipe dev state.
# stop must run before the rm or the live brain keeps the DB inode alive.
clean: stop caddy-down
	-@docker ps -aq --filter "label=com.docker.compose.project" --filter "name=malmo-" | xargs -r docker rm -f
	-@docker network ls -q --filter "name=malmo-app-" | xargs -r docker network rm
	@# App containers (Postgres et al.) write their data as root inside bind
	@# mounts, so instances/<id>/data is root-owned on the host — same as prod,
	@# where the privileged uninstall path removes it. A plain `rm` as the dev
	@# user can't, so reclaim it via a throwaway root container first. No sudo.
	-@docker run --rm -v $(abspath $(DEV_DIR)/state):/state alpine:3 rm -rf /state 2>/dev/null || true
	rm -rf $(DEV_DIR)/state $(DEV_DIR)/next
