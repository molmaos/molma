// Package hostagent contains the shared HTTP handler layer for both
// cmd/host-agent (fake) and cmd/host-agent-real. It speaks the real
// BRAIN_HOST_PROTOCOL.md wire format over a UNIX socket.
package hostagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/malmoos/malmo/internal/hostagent/netstate"
	"github.com/malmoos/malmo/internal/protocol"
	"github.com/malmoos/malmo/internal/version"
	"golang.org/x/crypto/bcrypt"
)

// ErrUnknownUser is returned by UserManager.ResolveHome when the user does not
// exist on the host system. The handler maps this to a 404 with code
// "unknown-user" so the brain can distinguish "user gone" from "host error."
var ErrUnknownUser = errors.New("unknown user")

// AgentVersion is the systemStatus handler's self-reported agent_version.
// Despite living in a file full of fake-agent scaffolding, systemStatus is
// SHARED code — it's compiled into and called by both cmd/host-agent (fake)
// and cmd/host-agent-real, and today reports placeholder data for both alike
// (Hostname below is hardcoded "malmo-dev" the same way, a known, separate
// gap). So this derives from the real stamped internal/version.Version rather
// than a "-fake" literal: a "-fake" suffix would mislabel host-agent-real too,
// which this same constant also feeds until real per-binary system-status
// reporting is built out. It also keeps a `make dev` brain (also stamped from
// the same VERSION file) and the fake agent trivially in range of each
// other's minimumAgentVersion check (cmd/brain's checkAgentVersion) without
// a prerelease-suffix special case.
var AgentVersion = version.Version

// PasswordVerifier is a consumer-side interface: it lives here because this is
// the package that calls it (the verifyPassword handler). Provider packages
// (FakeVerifier, PAMVerifier) export concrete types only.
type PasswordVerifier interface {
	Verify(user, password string) (bool, error)
}

// SSHAccessManager is a consumer-side interface for the per-account SSH opt-in
// (BRAIN_HOST_PROTOCOL.md # SSH access). Lives here because the /v1/ssh/*
// handlers are the consumers; sshaccess.Manager is the concrete Linux provider.
//
// SetAccess takes one account's full desired state and is expected to be
// idempotent: the same call twice leaves the same config and the same daemon
// state. State reports what the host actually has, for the brain's reconciler.
type SSHAccessManager interface {
	SetAccess(req protocol.SetSSHAccessRequest) error
	State() (protocol.SSHState, error)
}

// Publisher is a consumer-side interface for writing/removing Avahi service
// files. Lives here because the publish/unpublish HTTP handlers are the
// consumers. Provider packages (FakePublisher, avahipublisher.FilePublisher)
// export concrete types only.
type Publisher interface {
	Publish(slug string) (name string, err error)
	Unpublish(slug string) error
}

// NetState is a consumer-side interface for the LAN interface set reported by
// GET /v1/discovery/state (DISCOVERY.md # Interface scoping). Provider
// packages export concrete types: netstate.NMProvider (NetworkManager over
// DBus) for cmd/host-agent-real, FakeNetState for the fake binary and tests.
type NetState interface {
	LANInterfaces() ([]netstate.LANInterface, error)
}

// HealthSource is a consumer-side interface for reading the latest storage
// findings written by malmo-storage-verify (BOOT.md # The storage-ready
// target). It backs the storage category of GET /v1/health/system. Provider
// packages return concrete types: FakeHealthSource for the fake binary,
// healthsource.FilesystemHealthSource (which reads
// /run/malmo/health/storage.json) for cmd/host-agent-real.
//
// Read must always return a usable StorageHealth — missing report = empty
// findings, malformed report = a single "health-report-malformed" finding.
// host-agent never propagates "I couldn't read the file" as an HTTP error;
// the brain needs a clean payload to drive its health registry.
type HealthSource interface {
	Read() (protocol.StorageHealth, error)
}

// ServiceReporter is a consumer-side interface for the locus-B service-down
// detector: `systemctl is-active` over the core-unit allowlist, one
// service-down Finding (with the unit as instance_key) per non-active unit. It
// backs the services category of GET /v1/health/system. Provider packages return
// concrete types: servicehealth.Reporter (real systemctl) for cmd/host-agent-real,
// FakeServiceReporter for the fake binary and tests.
//
// Read always returns a usable slice (nil = all services healthy) and never
// errors — inactive units are data, not failures.
type ServiceReporter interface {
	Read() []protocol.Finding
}

// ClockReporter is a consumer-side interface for the locus-B clock-not-synced
// detector: it parses `chronyc tracking` and emits a clock-not-synced finding
// when the host clock hasn't synced in 6h or its offset exceeds 10s. It backs the
// time category of GET /v1/health/system. Provider packages return concrete
// types: clockhealth.Reporter (real chronyc) for cmd/host-agent-real,
// FakeClockReporter for the fake binary and tests.
//
// Read always returns a usable slice (nil = clock healthy) and never errors —
// like ServiceReporter, an out-of-sync clock is data, not a failure.
type ClockReporter interface {
	Read() []protocol.Finding
}

// LogSource is a consumer-side interface for the per-app log tail behind
// GET /v1/journal/follow (LOGGING.md # Per-app logs, BRAIN_HOST_PROTOCOL.md
// # Pattern C). Follow returns a channel of log lines for the given container;
// the channel is closed when the source ends (the underlying follower exits)
// and the follow stops when ctx is cancelled (the brain disconnected). Provider
// packages return concrete types: journalsource.Reader (real
// `journalctl CONTAINER_NAME=<container> -f`) for cmd/host-agent-real,
// FakeLogSource (a synthetic ticker) for the fake binary and tests.
//
// When nil (no source wired), GET /v1/journal/follow returns 501 so the brain
// surfaces "logs unavailable" rather than a silently empty stream.
type LogSource interface {
	Follow(ctx context.Context, container string) (<-chan protocol.JournalLine, error)
}

// RAMReporter is a consumer-side interface for the locus-B ram-pressure
// detector: it reads the kernel's PSI memory file (/proc/pressure/memory, the
// `some avg60` field) and emits a ram-pressure finding when memory-stall
// pressure is sustained above threshold. It backs the resources category of GET
// /v1/health/system. Provider packages return concrete types:
// rampressure.Reporter (real /proc) for cmd/host-agent-real, FakeRAMReporter for
// the fake binary and tests.
//
// Read always returns a usable slice (nil = pressure below threshold) and never
// errors — like ServiceReporter, a PSI reading is data, not a failure; a missing
// PSI file fails open to nil.
type RAMReporter interface {
	Read() []protocol.Finding
}

// DiskReporter is a consumer-side interface for the data-drive free/total
// space behind GET /v1/system/status (DataDiskFreeBytes/DataDiskTotalBytes).
// It backs the install-plan free_bytes figure (BRAIN_UI_PROTOCOL.md #
// install-plan). Provider packages return concrete types: diskusage.Reporter
// (real syscall.Statfs on /srv/malmo) for cmd/host-agent-real, FakeDiskReporter
// for the fake binary and tests.
//
// DataDisk always returns usable levels and never errors — a statfs failure
// fails open to (0, 0), which the brain reads as "not measured" rather than a
// scary empty disk.
type DiskReporter interface {
	DataDisk() (free, total int64)
}

// DiskSpaceReporter is a consumer-side interface for the per-volume fullness
// behind GET /v1/system/status (SystemStatus.Disks), backing the
// system-resources panel's Storage bars (LOCAL_ANALYTICS.md # Real-time system
// resources). Provider packages return concrete types: diskusage.Reporter (real
// statfs on / and /srv/malmo) for cmd/host-agent-real, FakeDiskSpaceReporter for
// the fake binary and tests.
//
// Disks always returns a usable slice and never errors — it omits a volume
// whose statfs fails or that isn't a distinct mount (a Level-0 box has no data
// drive), so the brain only ever sees real volumes, never zero-filled phantoms.
type DiskSpaceReporter interface {
	Disks() []protocol.DiskSpace
}

// GPUReporter is a consumer-side interface for the host GPU capability report
// behind GET /v1/system/gpu (BRAIN_HOST_PROTOCOL.md # GPU capability query):
// presence + vendor + the render group GID the brain group_adds onto /dev/dri
// for a `gpu: true` install. Provider packages return concrete types: the real
// /dev/dri scanner lands with cmd/host-agent-real (#125), FakeGPUReporter
// serves the fake binary and tests.
//
// Read always returns a usable report and never errors — a detection failure
// reports Present: false, which the brain turns into the install refusal
// (the safe side of the gate), never a half-wired override.
type GPUReporter interface {
	Read() protocol.SystemGPU
}

// RebootReporter is a consumer-side interface for the locus-B reboot-required
// detector: it stats Debian's /var/run/reboot-required flag (+ .pkgs for the
// package list) and emits a reboot-required finding when a pending reboot is
// flagged. It backs the system category of GET /v1/health/system. Provider
// packages return concrete types: rebootrequired.Reporter (real /var/run) for
// cmd/host-agent-real, FakeRebootReporter for tests.
//
// Read always returns a usable slice (nil = no reboot pending) and never errors —
// a pending reboot is data, not a failure; an unexpected stat error fails open.
type RebootReporter interface {
	Read() []protocol.Finding
}

// UpdateTargetReporter is a consumer-side interface for GET
// /v1/system/update-target: what the update-target loop last decided
// (UPDATES.md # 8.4). Provider packages return concrete types — the adapter in
// cmd/host-agent-real, which reads the live loop and the box's own control-plane
// declaration; a canned reporter in the fake binary and tests.
//
// Read always returns a usable report and never errors. Every way this can go
// wrong is already a state on the wire: a box with no loop reports "disabled",
// a source that could not be read reports "unreachable". An HTTP error here
// would tell the brain "ask again later" about facts that are not going to
// change on their own.
//
// When nil (no reporter wired), the endpoint reports "unknown" — "not
// measured", matching the other nil-able reporters. It is never read as "you
// are up to date".
type UpdateTargetReporter interface {
	Read() protocol.UpdateTarget
}

// UserManager is a consumer-side interface for the system-level user account
// operations behind /v1/auth/set-password (and, later, /set-role and
// /delete-user). Provider packages (usermgr.LinuxUserManager) export concrete
// types only.
//
// UpsertPassword is upsert: creates the user if missing, otherwise updates the
// password. SetRole updates Linux group membership to match the role
// (admin → in `sudo`, member → not in `sudo`); idempotent. ResolveHome returns
// the user's home directory path and POSIX UID/GID; returns ErrUnknownUser when
// the user does not exist. WellKnownIdentity returns the fixed service-account
// UIDs/GIDs for malmo-app and malmo-shared. AllocateAppService reserves a
// fresh UID/GID pair from the app-service band [protocol.AppServiceUIDMin,
// protocol.AppServiceUIDMax] for a `service_user: true` instance;
// ReleaseAppService returns one to the band (idempotent — releasing an
// unallocated UID is a no-op). See BRAIN_HOST_PROTOCOL.md # User info
// endpoints and USERS_AND_GROUPS.md # Roles.
type UserManager interface {
	UpsertPassword(user, password string) error
	SetRole(user, role string) error
	DeleteUser(user string) error
	ResolveHome(user string) (home string, uid, gid int, err error)
	WellKnownIdentity() (appUID, appGID, sharedGID int, err error)
	AllocateAppService(instanceID string) (uid, gid int, err error)
	ReleaseAppService(uid int) error
}

// TimezoneSetter is a consumer-side interface for the system-timezone op behind
// POST /v1/system/set-timezone (TIME.md # System TZ): the first-run wizard's
// time-zone step and the later Settings → System → Time surface drive it
// through the brain. Provider packages (timezone.Setter, real `timedatectl
// set-timezone`) export concrete types only. zone is a pre-validated IANA tz
// database name. Applies to both build profiles — a hosted VM and an appliance
// both run in a timezone (ENVIRONMENT.md # Provisioning keeps the time-zone
// step in the trimmed wizard).
type TimezoneSetter interface {
	SetTimezone(zone string) error
}

// Agent is the HTTP handler set for host-agent. It holds both the
// PasswordVerifier (swapped per binary) and the in-memory fake maps used by
// setPassword / setRole / deleteUser when UserMgr is nil (the fake binary).
// cmd/host-agent-real wires UserMgr so all three delegate to /etc/passwd via
// usermgr.LinuxUserManager.
type Agent struct {
	mu sync.Mutex
	// published is a write-through cache of announced names, keyed by slug.
	// Updated on every successful Publish/Unpublish call so GET /v1/discovery/state
	// can answer without requiring the Publisher to expose a listing method.
	published map[string]protocol.PublishedName
	// passwords is the in-memory bcrypt map used by setPassword/deleteUser
	// when UserMgr is nil (the fake binary). FakeVerifier reads from it.
	// In cmd/host-agent-real, UserMgr is wired and these handlers bypass
	// the map entirely — /etc/shadow is the source of truth there.
	passwords map[string][]byte
	roles     map[string]string
	// sshAccess is the in-memory stand-in for the sshd drop-in, used by the
	// /v1/ssh/* handlers when SSH is nil (the fake binary). Keyed by username and
	// holding only enabled accounts, so len() answers "is the daemon running".
	sshAccess map[string]protocol.SSHUserState
	// statePath, when non-empty, backs passwords+roles with a JSON file so the
	// fake binary's accounts survive a restart (a dev stand-in for /etc/shadow,
	// which the real agent persists for free). Empty by default — tests and the
	// real binary keep the maps purely in memory. Set via EnablePersistence.
	statePath string
	startedAt time.Time

	// Verifier handles POST /v1/auth/verify-password.
	// Swapped per binary: FakeVerifier (fake) vs PAMVerifier (real).
	Verifier PasswordVerifier

	// Publisher handles POST /v1/discovery/publish and /v1/discovery/unpublish.
	// Swapped per binary: FakePublisher (fake) vs avahipublisher.FilePublisher (real).
	Publisher Publisher

	// Health, when non-nil, backs the storage category of GET /v1/health/system.
	// Swapped per binary: FakeHealthSource (fake) vs
	// healthsource.FilesystemHealthSource (real). When nil, the storage category
	// reports empty findings — useful for the fake binary in dev where no
	// storage-verify reporter is running.
	Health HealthSource

	// Services, when non-nil, backs the services category of GET /v1/health/system
	// (the service-down detector). Swapped per binary: servicehealth.Reporter
	// (real systemctl) vs FakeServiceReporter. When nil, the report omits the
	// services category entirely — the brain then leaves service issues alone
	// rather than treating "no services measured" as "all services up".
	Services ServiceReporter

	// Time, when non-nil, backs the time category of GET /v1/health/system (the
	// clock-not-synced detector). Swapped per binary: clockhealth.Reporter (real
	// chronyc) vs FakeClockReporter. When nil, the report omits the time category
	// entirely — the brain then leaves clock issues alone rather than treating
	// "no clock measured" as "clock healthy".
	Time ClockReporter

	// Logs, when non-nil, backs GET /v1/journal/follow (the per-app log tail).
	// Swapped per binary: journalsource.Reader (real journalctl) for
	// cmd/host-agent-real vs FakeLogSource (synthetic ticker) for the fake
	// binary and tests. When nil, GET /v1/journal/follow returns 501.
	Logs LogSource

	// Resources, when non-nil, backs the resources category of GET
	// /v1/health/system (the ram-pressure detector). Swapped per binary:
	// rampressure.Reporter (real /proc/pressure/memory) vs FakeRAMReporter. When
	// nil, the report omits the resources category entirely — the brain then
	// leaves resource issues alone rather than treating "no pressure measured"
	// as "pressure healthy".
	Resources RAMReporter

	// Disk, when non-nil, backs the data-drive free/total fields of GET
	// /v1/system/status. Swapped per binary: diskusage.Reporter (real statfs on
	// /srv/malmo) vs FakeDiskReporter. When nil, both fields report 0 ("not
	// measured"), which the brain surfaces as "free space unknown" in the
	// install plan rather than a misleading empty disk.
	Disk DiskReporter

	// DiskSpace, when non-nil, backs the per-volume Disks field of GET
	// /v1/system/status (the Storage bars). Swapped per binary: diskusage.Reporter
	// (real statfs on / and /srv/malmo) vs FakeDiskSpaceReporter. When nil, Disks
	// is an empty slice — the panel shows no Storage section rather than phantom
	// bars. cmd/host-agent-real wires the same diskusage.Reporter to Disk and
	// DiskSpace.
	DiskSpace DiskSpaceReporter

	// Reboot, when non-nil, backs the system category of GET /v1/health/system
	// (the reboot-required detector). Swapped per binary: rebootrequired.Reporter
	// (real /var/run/reboot-required) vs FakeRebootReporter. When nil, the report
	// omits the system category entirely — the brain then leaves the
	// reboot-required issue alone rather than treating "not measured" as "no
	// reboot pending".
	Reboot RebootReporter

	// UserMgr, when non-nil, takes over POST /v1/auth/set-password,
	// /v1/auth/set-role, and /v1/auth/delete-user: handlers delegate to the
	// manager instead of writing to the in-memory maps. cmd/host-agent leaves
	// this nil (fake path); cmd/host-agent-real wires usermgr.LinuxUserManager
	// so /etc/passwd + /etc/shadow + /etc/group become the source of truth.
	UserMgr UserManager

	// SSH, when non-nil, backs POST /v1/ssh/set-access and GET /v1/ssh/state
	// (real sshd drop-in + systemctl). Wired by cmd/host-agent-real in both build
	// profiles — SSH is per-account on the appliance and on hosted alike, only
	// the mandatory auth factor differs, and that is the brain's decision
	// (AUTH.md # Device access). When nil (cmd/host-agent fake, dev loop), the
	// handlers keep the same state in the in-memory sshAccess map so the whole
	// flow is exercisable under `make dev` without touching the developer's sshd.
	SSH SSHAccessManager

	// Timezone, when non-nil, backs POST /v1/system/set-timezone (real
	// `timedatectl set-timezone`). Wired by cmd/host-agent-real in both build
	// profiles (timezone applies to a hosted VM and an appliance alike). When
	// nil (cmd/host-agent fake, dev loop), set-timezone is an accepted no-op:
	// the dev box's clock is the developer's, not the brain's to retune.
	Timezone TimezoneSetter

	// GPU, when non-nil, backs GET /v1/system/gpu. Swapped per binary: the real
	// /dev/dri detector (cmd/host-agent-real, #125) vs FakeGPUReporter. When
	// nil, the endpoint reports present: false — an agent with no detector
	// wired has no usable GPU to offer, so a `gpu: true` install refuses
	// rather than emitting an override against unknown hardware.
	GPU GPUReporter

	// System, when non-nil, backs GET /v1/system/resources with real kernel
	// counters. Swapped per binary: procsource.Sampler (real /proc, Linux-only)
	// in cmd/host-agent-real vs nil in the fake binary, which keeps the
	// synthetic monotonically-climbing counters so the inner dev loop needs no
	// host /proc.
	System SystemSampler

	// Updater, when non-nil, backs POST /v1/jobs/system-update — the
	// control-plane update transaction (UPDATES.md # 3, jobs.go). Swapped per
	// binary: cpupdate.Runner (real `docker` + HTTP probe) in
	// cmd/host-agent-real vs nil in the fake binary, where the endpoint returns
	// 501: the dev loop has no control plane to update, and a fake success
	// would tell the brain a box moved when it did not.
	Updater SystemUpdater

	// jobs holds the in-memory job records and the one global job lock
	// (jobs.go). Always non-nil — New builds it.
	jobs *jobRegistry

	// UpdateTarget, when non-nil, backs GET /v1/system/update-target — what the
	// update-target loop last decided. Swapped per binary: the live-loop adapter
	// in cmd/host-agent-real vs a canned reporter in the fake binary. When nil,
	// the endpoint reports state "unknown".
	UpdateTarget UpdateTargetReporter

	// Net, when non-nil, backs the interfaces field of GET /v1/discovery/state
	// with the LAN set. Swapped per binary: netstate.NMProvider (NetworkManager
	// over DBus) vs FakeNetState. When nil, interfaces reports empty — "not
	// measured", matching the other nil-able reporters.
	Net NetState
}

// SystemSampler is a consumer-side interface for the raw system-resources
// sample behind GET /v1/system/resources. One call = one snapshot of raw
// cumulative counters (never rates — the brain derives those by diffing
// successive samples, BRAIN_HOST_PROTOCOL.md # Pattern A). Provider:
// procsource.Sampler.
type SystemSampler interface {
	Sample() (protocol.SystemResources, error)
}

// New constructs an Agent with the given PasswordVerifier and Publisher.
// Either may be nil at construction time and set later (useful for the
// FakeVerifier pointer-back pattern), but both must be non-nil before
// Mount is called and requests arrive.
func New(v PasswordVerifier, pub Publisher) *Agent {
	return &Agent{
		published: map[string]protocol.PublishedName{},
		passwords: map[string][]byte{},
		roles:     map[string]string{},
		startedAt: time.Now(),
		jobs:      newJobRegistry(),
		Verifier:  v,
		Publisher: pub,
	}
}

// Mount registers all routes on mux.
func (a *Agent) Mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/discovery/publish", a.publish)
	mux.HandleFunc("POST /v1/discovery/unpublish", a.unpublish)
	mux.HandleFunc("GET /v1/discovery/state", a.discoveryState)
	mux.HandleFunc("GET /v1/system/status", a.systemStatus)
	mux.HandleFunc("GET /v1/system/resources", a.systemResources)
	mux.HandleFunc("GET /v1/system/gpu", a.systemGPU)
	mux.HandleFunc("GET /v1/system/update-target", a.systemUpdateTarget)
	mux.HandleFunc("POST /v1/auth/verify-password", a.verifyPassword)
	mux.HandleFunc("POST /v1/auth/set-password", a.setPassword)
	mux.HandleFunc("POST /v1/auth/set-role", a.setRole)
	mux.HandleFunc("POST /v1/auth/delete-user", a.deleteUser)
	mux.HandleFunc("POST /v1/system/set-timezone", a.setTimezone)
	mux.HandleFunc("POST /v1/ssh/set-access", a.setSSHAccess)
	mux.HandleFunc("GET /v1/ssh/state", a.sshState)
	mux.HandleFunc("GET /v1/health/system", a.systemHealth)
	mux.HandleFunc("GET /v1/journal/follow", a.journalFollow)
	mux.HandleFunc("POST /v1/jobs/system-update", a.startSystemUpdate)
	mux.HandleFunc("GET /v1/jobs/{id}", a.jobStatus)
	mux.HandleFunc("GET /v1/users/{username}/home", a.resolveHome)
	mux.HandleFunc("GET /v1/identity/well-known", a.wellKnownIdentity)
	mux.HandleFunc("POST /v1/identity/app-service", a.allocateAppService)
	mux.HandleFunc("POST /v1/identity/app-service/release", a.releaseAppService)
}

func (a *Agent) publish(w http.ResponseWriter, r *http.Request) {
	var req protocol.PublishRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Slug == "" {
		writeErr(w, http.StatusBadRequest, "bad-request", "slug is required")
		return
	}
	name, err := a.Publisher.Publish(req.Slug)
	if err != nil {
		slog.Error("publish: publisher error", "slug", req.Slug, "err", err)
		writeErr(w, http.StatusInternalServerError, "publish-failed", err.Error())
		return
	}
	// Write-through cache: keep the in-memory map in sync so GET /v1/discovery/state
	// can answer without requiring the Publisher to expose a listing method.
	a.mu.Lock()
	a.published[req.Slug] = protocol.PublishedName{Slug: req.Slug, Name: name, State: "established"}
	a.mu.Unlock()
	slog.Info("publish", "slug", req.Slug, "name", name, "state", "established")
	writeJSON(w, http.StatusOK, protocol.PublishResponse{Name: name, State: "established"})
}

func (a *Agent) unpublish(w http.ResponseWriter, r *http.Request) {
	var req protocol.UnpublishRequest
	if !decode(w, r, &req) {
		return
	}
	if err := a.Publisher.Unpublish(req.Slug); err != nil {
		slog.Error("unpublish: publisher error", "slug", req.Slug, "err", err)
		writeErr(w, http.StatusInternalServerError, "unpublish-failed", err.Error())
		return
	}
	// Keep write-through cache in sync.
	a.mu.Lock()
	delete(a.published, req.Slug)
	a.mu.Unlock()
	slog.Info("unpublish", "slug", req.Slug)
	writeJSON(w, http.StatusOK, struct{}{})
}

func (a *Agent) discoveryState(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	names := make([]protocol.PublishedName, 0, len(a.published))
	for _, n := range a.published {
		names = append(names, n)
	}
	a.mu.Unlock()
	// Interfaces is the live LAN set when a netstate provider is wired; empty
	// means "not measured" (no provider, or the provider failed — logged, not
	// surfaced: discovery state is a debug read, never a gate).
	ifaces := []string{}
	if a.Net != nil {
		lis, err := a.Net.LANInterfaces()
		if err != nil {
			slog.Warn("discovery state: LAN interfaces", "err", err)
		}
		for _, li := range lis {
			ifaces = append(ifaces, li.Name)
		}
	}
	writeJSON(w, http.StatusOK, protocol.DiscoveryState{
		Publisher:  "avahi-fake",
		HostName:   "malmo",
		RenamedTo:  nil,
		Published:  names,
		Interfaces: ifaces,
	})
}

func (a *Agent) systemStatus(w http.ResponseWriter, r *http.Request) {
	// Data-drive free/total is 0/0 unless a disk reporter is wired (real statfs
	// in cmd/host-agent-real, a canned reporter in the fake binary); the brain
	// reads 0 as "not measured".
	var free, total int64
	if a.Disk != nil {
		free, total = a.Disk.DataDisk()
	}
	// Per-volume Storage bars; empty unless a reporter is wired (the brain reads
	// an empty slice as "no Storage section" rather than phantom bars).
	disks := []protocol.DiskSpace{}
	if a.DiskSpace != nil {
		disks = a.DiskSpace.Disks()
	}
	writeJSON(w, http.StatusOK, protocol.SystemStatus{
		Hostname:           "malmo-dev",
		UptimeS:            int64(time.Since(a.startedAt).Seconds()),
		DiskPressure:       false,
		AgentVersion:       AgentVersion,
		DataDiskFreeBytes:  free,
		DataDiskTotalBytes: total,
		Disks:              disks,
	})
}

// systemResources serves one raw cumulative-counter sample for the live
// system-resources view (BRAIN_HOST_PROTOCOL.md # Pattern A). When a System
// sampler is wired (cmd/host-agent-real injects procsource.Sampler), the
// sample is real kernel counters with the iface/device allowlist applied at
// the source; a sampler error is a 500 the brain logs and skips, keeping its
// previous rate baseline. When System is nil (the fake binary and tests), the
// fallback below synthesizes monotonically-climbing counters off a.startedAt
// so two successive 1 Hz polls always diff to a non-zero, plausible rate in
// the dev loop. Both paths are stateless per request — the property the spec
// requires. ts_ns advances on every call so the brain's rate denominator
// (ts_ns delta) is always positive.
func (a *Agent) systemResources(w http.ResponseWriter, r *http.Request) {
	if a.System != nil {
		res, err := a.System.Sample()
		if err != nil {
			slog.Error("system-resources: sampler error", "err", err)
			writeErr(w, http.StatusInternalServerError, "sample-failed", "")
			return
		}
		writeJSON(w, http.StatusOK, res)
		return
	}
	elapsed := time.Since(a.startedAt)
	secs := elapsed.Seconds()
	writeJSON(w, http.StatusOK, protocol.SystemResources{
		TsNs:    time.Now().UnixNano(),
		CPU:     protocol.CPUCounters{TotalJiffies: int64(secs * 400), IdleJiffies: int64(secs * 300)},
		LoadAvg: [3]float64{0.42, 0.51, 0.48},
		Mem: protocol.MemCounters{
			TotalBytes:     16728338432,
			AvailableBytes: 9214455808,
			UsedBytes:      7513882624,
		},
		Net: []protocol.NetCounters{
			{Iface: "eth0", RxBytes: int64(secs * 120000), TxBytes: int64(secs * 48000)},
		},
		Disk: []protocol.DiskCounters{
			{Dev: "sda", ReadBytes: int64(secs * 90000), WriteBytes: int64(secs * 14000)},
		},
		UptimeS: int64(secs),
	})
}

// systemGPU serves the host GPU capability report (BRAIN_HOST_PROTOCOL.md
// # GPU capability query). With no reporter wired it reports present: false
// rather than erroring: "no detector" means "no usable GPU", which the brain
// turns into the specced `gpu: true` install refusal — the safe side of the
// gate.
func (a *Agent) systemGPU(w http.ResponseWriter, r *http.Request) {
	if a.GPU != nil {
		writeJSON(w, http.StatusOK, a.GPU.Read())
		return
	}
	writeJSON(w, http.StatusOK, protocol.SystemGPU{})
}

// systemUpdateTarget reports what the update-target loop last decided
// (UPDATES.md # 8.4). A pure read: it starts nothing, and the trigger stays
// POST /v1/jobs/system-update.
//
// 200 always, like systemHealth and for the same reason — every failure this
// endpoint can have is a state in the payload, not an HTTP status. A nil
// reporter reports "unknown" rather than an error, because a brain that reads a
// 501 has learned nothing it can show anyone.
func (a *Agent) systemUpdateTarget(w http.ResponseWriter, r *http.Request) {
	if a.UpdateTarget != nil {
		writeJSON(w, http.StatusOK, a.UpdateTarget.Read())
		return
	}
	writeJSON(w, http.StatusOK, protocol.UpdateTarget{State: protocol.UpdateTargetUnknown})
}

// systemHealth returns the locus-B findings report across categories
// (HEALTH.md # Detector catalog). The storage category is always present
// (empty findings when no source is wired — "storage looks healthy" per
// BOOT.md); the services category is present only when a service reporter is
// wired, so the brain doesn't read "no services measured" as "all services up".
//
// Even when a source returns an error, the handler returns 200 with whatever
// payload the source produced (the storage source synthesizes a
// "health-report-malformed" finding on parse error and an empty list on a
// missing file). The contract: 200 always, payload always parseable. The
// brain's polling loop must never have to retry on a 5xx for this endpoint.
func (a *Agent) systemHealth(w http.ResponseWriter, r *http.Request) {
	cats := map[protocol.HealthCategory][]protocol.Finding{}

	// Storage category (locus A/B): always measured.
	storage := []protocol.Finding{}
	if a.Health != nil {
		sh, err := a.Health.Read()
		if err != nil {
			slog.Error("system-health: storage source error", "err", err)
		}
		if sh.Findings != nil {
			storage = sh.Findings
		}
	}
	cats[protocol.HealthCategoryStorage] = storage

	// Services category (locus B): only when a service reporter is wired.
	if a.Services != nil {
		svc := a.Services.Read()
		if svc == nil {
			svc = []protocol.Finding{}
		}
		cats[protocol.HealthCategoryServices] = svc
	}

	// Time category (locus B): only when a clock reporter is wired.
	if a.Time != nil {
		clk := a.Time.Read()
		if clk == nil {
			clk = []protocol.Finding{}
		}
		cats[protocol.HealthCategoryTime] = clk
	}

	// Resources category (locus B): only when a RAM reporter is wired.
	if a.Resources != nil {
		res := a.Resources.Read()
		if res == nil {
			res = []protocol.Finding{}
		}
		cats[protocol.HealthCategoryResources] = res
	}

	// System category (locus B): only when a reboot reporter is wired.
	if a.Reboot != nil {
		rb := a.Reboot.Read()
		if rb == nil {
			rb = []protocol.Finding{}
		}
		cats[protocol.HealthCategorySystem] = rb
	}

	writeJSON(w, http.StatusOK, protocol.SystemHealth{
		CheckedAt:  time.Now().UTC().Format(time.RFC3339),
		Categories: cats,
	})
}

// journalFollow streams a container's log lines as SSE (BRAIN_HOST_PROTOCOL.md
// # Pattern C — the per-app log tail behind the dashboard Logs tab). Each line
// is one `id: <n>\ndata: {json}\n\n` frame with a per-connection monotonic id;
// no `event:` field, so the brain (and a curl) read them as default-type
// messages.
//
// Reconnect: the follower here is fresh per connection with no history before
// "now", so a reconnect carrying Last-Event-ID can't be replayed — the handler
// emits one {"lost":true} frame and resumes live (the spec's buffer-overflow
// path). The authoritative rolling-buffer replay lives one hop up, in the
// brain's per-instance log hub (BRAIN_UI_PROTOCOL.md Pattern C: "the brain
// re-emits IDs from its own monotonic counter"); a host-side shared-follower
// buffer is deferred until a second consumer (job / Tier-2 service logs) needs it.
func (a *Agent) journalFollow(w http.ResponseWriter, r *http.Request) {
	container := r.URL.Query().Get("container")
	if container == "" {
		writeErr(w, http.StatusBadRequest, "bad-request", "container is required")
		return
	}
	if a.Logs == nil {
		writeErr(w, http.StatusNotImplemented, "logs-unsupported", "this host-agent has no log source")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "no-flush", "streaming unsupported")
		return
	}

	ch, err := a.Logs.Follow(r.Context(), container)
	if err != nil {
		slog.Error("journal-follow: source error", "container", container, "err", err)
		writeErr(w, http.StatusInternalServerError, "journal-follow-failed", "could not follow logs")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	var id uint64
	// A reconnect carrying Last-Event-ID can't be replayed by a fresh follower —
	// emit one {"lost":true} so the consumer knows the gap, then resume live.
	if r.Header.Get("Last-Event-ID") != "" {
		id++
		writeJournalFrame(w, flusher, id, protocol.JournalLine{Lost: true})
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case line, ok := <-ch:
			if !ok {
				return
			}
			id++
			writeJournalFrame(w, flusher, id, line)
		}
	}
}

func writeJournalFrame(w http.ResponseWriter, f http.Flusher, id uint64, line protocol.JournalLine) {
	data, _ := json.Marshal(line)
	fmt.Fprintf(w, "id: %d\ndata: %s\n\n", id, data)
	f.Flush()
}

// verifyPassword delegates to a.Verifier so the verification strategy
// (fake bcrypt map vs. real PAM) is swapped per binary.
//
// Per BRAIN_HOST_PROTOCOL.md: the response is always {valid: bool} — we never
// reveal *why* verification failed (wrong password, unknown user, locked
// account, PAM config error). Even a Verifier transport/config error returns
// {valid: false} rather than a 5xx so the brain's rate-limiter sees a clean
// false and the brain never leaks the distinction.
func (a *Agent) verifyPassword(w http.ResponseWriter, r *http.Request) {
	var req protocol.VerifyPasswordRequest
	if !decode(w, r, &req) {
		return
	}
	ok, err := a.Verifier.Verify(req.User, req.Password)
	if err != nil {
		slog.Error("verify-password: verifier error", "user", req.User, "err", err)
		// Never reveal why — return false, not 5xx. See doc comment above.
		writeJSON(w, http.StatusOK, protocol.VerifyPasswordResponse{Valid: false})
		return
	}
	writeJSON(w, http.StatusOK, protocol.VerifyPasswordResponse{Valid: ok})
}

// setPassword is upsert per BRAIN_HOST_PROTOCOL.md: creates the user if
// missing, otherwise updates the password.
//
// When UserMgr is non-nil (cmd/host-agent-real), delegates to UpsertPassword
// which writes to /etc/shadow via useradd+chpasswd. When nil (cmd/host-agent),
// writes a bcrypt hash to the in-memory map used by FakeVerifier so the fake
// binary's tests and the bootstrap flow (POST /setup → SetPassword) still work.
//
// Never reveals system-level failure detail in the HTTP response body — same
// posture as verify-password. The structured log captures the underlying error.
func (a *Agent) setPassword(w http.ResponseWriter, r *http.Request) {
	var req protocol.SetPasswordRequest
	if !decode(w, r, &req) {
		return
	}
	if req.User == "" || req.Password == "" {
		writeErr(w, http.StatusBadRequest, "bad-request", "user and password are required")
		return
	}

	if a.UserMgr != nil {
		if err := a.UserMgr.UpsertPassword(req.User, req.Password); err != nil {
			slog.Error("set-password: user-manager error", "user", req.User, "err", err)
			writeErr(w, http.StatusInternalServerError, "set-password-failed", "set-password failed")
			return
		}
		slog.Info("set-password", "user", req.User)
		writeJSON(w, http.StatusOK, struct{}{})
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.MinCost)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "hash-failed", err.Error())
		return
	}
	a.mu.Lock()
	a.passwords[req.User] = hash
	a.persistLocked()
	a.mu.Unlock()
	slog.Info("set-password", "user", req.User)
	writeJSON(w, http.StatusOK, struct{}{})
}

// setRole updates Linux group membership to match the role.
//
// When UserMgr is non-nil (cmd/host-agent-real), delegates to SetRole which
// runs `gpasswd -a/-d <user> sudo`. When nil (cmd/host-agent), records the
// role in the in-memory map. Body never leaks system detail on error — same
// opaque-error posture as verify-password / set-password.
func (a *Agent) setRole(w http.ResponseWriter, r *http.Request) {
	var req protocol.SetRoleRequest
	if !decode(w, r, &req) {
		return
	}
	if req.User == "" {
		writeErr(w, http.StatusBadRequest, "bad-request", "user is required")
		return
	}
	if req.Role != "admin" && req.Role != "member" {
		writeErr(w, http.StatusBadRequest, "bad-request", "role must be admin or member")
		return
	}

	if a.UserMgr != nil {
		if err := a.UserMgr.SetRole(req.User, req.Role); err != nil {
			slog.Error("set-role: user-manager error", "user", req.User, "role", req.Role, "err", err)
			writeErr(w, http.StatusInternalServerError, "set-role-failed", "set-role failed")
			return
		}
		slog.Info("set-role", "user", req.User, "role", req.Role)
		writeJSON(w, http.StatusOK, struct{}{})
		return
	}

	a.mu.Lock()
	a.roles[req.User] = req.Role
	a.persistLocked()
	a.mu.Unlock()
	slog.Info("set-role", "user", req.User, "role", req.Role)
	writeJSON(w, http.StatusOK, struct{}{})
}

// deleteUser removes the user. When UserMgr is wired (cmd/host-agent-real),
// delegates to UserMgr.DeleteUser (userdel -r -f); otherwise drops the entry
// from the in-memory fake maps. Idempotent per BRAIN_HOST_PROTOCOL.md # Auth
// endpoints: unknown user returns 200.
func (a *Agent) deleteUser(w http.ResponseWriter, r *http.Request) {
	var req protocol.DeleteUserRequest
	if !decode(w, r, &req) {
		return
	}
	if req.User == "" {
		writeErr(w, http.StatusBadRequest, "bad-request", "user is required")
		return
	}

	if a.UserMgr != nil {
		if err := a.UserMgr.DeleteUser(req.User); err != nil {
			slog.Error("delete-user: user-manager error", "user", req.User, "err", err)
			writeErr(w, http.StatusInternalServerError, "delete-user-failed", "delete-user failed")
			return
		}
		slog.Info("delete-user", "user", req.User)
		writeJSON(w, http.StatusOK, struct{}{})
		return
	}

	a.mu.Lock()
	delete(a.passwords, req.User)
	delete(a.roles, req.User)
	a.persistLocked()
	a.mu.Unlock()
	slog.Info("delete-user", "user", req.User)
	writeJSON(w, http.StatusOK, struct{}{})
}

// setTimezone applies the system timezone (TIME.md # System TZ).
//
// When Timezone is wired (cmd/host-agent-real, both build profiles), delegates
// to the real `timedatectl set-timezone <zone>`. When nil (cmd/host-agent fake,
// dev loop), it is an accepted no-op — the dev box's clock isn't the brain's to
// retune, but the wizard's time-zone step must still get a clean 200 so the
// inner dev loop walks the whole flow. zone format is validated by the brain
// before the call and re-validated by the real setter at the privileged
// boundary; here we only reject the empty string. Error detail stays out of the
// body, same opaque-error posture as set-password.
func (a *Agent) setTimezone(w http.ResponseWriter, r *http.Request) {
	var req protocol.SetTimezoneRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Zone == "" {
		writeErr(w, http.StatusBadRequest, "bad-request", "zone is required")
		return
	}
	if a.Timezone == nil {
		slog.Info("set-timezone (fake)", "zone", req.Zone)
		writeJSON(w, http.StatusOK, struct{}{})
		return
	}
	if err := a.Timezone.SetTimezone(req.Zone); err != nil {
		slog.Error("set-timezone: setter error", "err", err)
		writeErr(w, http.StatusInternalServerError, "set-timezone-failed", "set-timezone failed")
		return
	}
	slog.Info("set-timezone", "zone", req.Zone)
	writeJSON(w, http.StatusOK, struct{}{})
}

// setSSHAccess applies one account's full desired SSH state. The request is
// deliberately not a delta (BRAIN_HOST_PROTOCOL.md # SSH access): AuthorizedKeys
// replaces the account's key set outright, so a retry after a partial failure
// converges rather than compounding.
//
// The only validation here is structural — a username is required, and an
// enabled account with no keys and no password is refused because it could
// never authenticate. Everything policy-shaped stays in the brain: which factor
// is mandatory depends on the environment profile, and host-agent does not know
// the profile. Same division as setTimezone, where the brain validates the zone.
//
// When SSH is nil (the fake binary) the state is kept in memory and no sshd is
// touched, so `make dev` on a developer's own machine exercises the flow without
// reconfiguring their SSH.
func (a *Agent) setSSHAccess(w http.ResponseWriter, r *http.Request) {
	var req protocol.SetSSHAccessRequest
	if !decode(w, r, &req) {
		return
	}
	if req.User == "" {
		writeErr(w, http.StatusBadRequest, "bad-request", "user is required")
		return
	}
	if req.Enabled && len(req.AuthorizedKeys) == 0 && !req.RequirePassword {
		writeErr(w, http.StatusBadRequest, "bad-request",
			"an enabled account needs at least one key or the password")
		return
	}

	if a.SSH == nil {
		a.mu.Lock()
		if a.sshAccess == nil {
			a.sshAccess = map[string]protocol.SSHUserState{}
		}
		if req.Enabled {
			a.sshAccess[req.User] = protocol.SSHUserState{
				Username:        req.User,
				KeyCount:        len(req.AuthorizedKeys),
				RequirePassword: req.RequirePassword,
			}
		} else {
			delete(a.sshAccess, req.User)
		}
		a.mu.Unlock()
		slog.Info("ssh set-access (fake)", "username", req.User, "enabled", req.Enabled,
			"keys", len(req.AuthorizedKeys), "service", "ssh")
		writeJSON(w, http.StatusOK, struct{}{})
		return
	}

	if err := a.SSH.SetAccess(req); err != nil {
		slog.Error("ssh set-access failed", "username", req.User, "enabled", req.Enabled,
			"service", "ssh", "err", err)
		writeErr(w, http.StatusInternalServerError, "ssh-set-access-failed", "ssh set-access failed")
		return
	}
	slog.Info("ssh set-access", "username", req.User, "enabled", req.Enabled,
		"keys", len(req.AuthorizedKeys), "service", "ssh")
	writeJSON(w, http.StatusOK, struct{}{})
}

// sshState reports actual SSH state for the brain's reconciler: whether the
// daemon is running, and which accounts the rendered config enables. Read-only.
//
// With SSH nil, DaemonRunning is derived from the in-memory map — an honest
// answer for the fake, where "the daemon" is exactly the set of enabled
// accounts and nothing else.
func (a *Agent) sshState(w http.ResponseWriter, r *http.Request) {
	if a.SSH == nil {
		a.mu.Lock()
		users := make([]protocol.SSHUserState, 0, len(a.sshAccess))
		for _, u := range a.sshAccess {
			users = append(users, u)
		}
		a.mu.Unlock()
		sort.Slice(users, func(i, j int) bool { return users[i].Username < users[j].Username })
		writeJSON(w, http.StatusOK, protocol.SSHState{DaemonRunning: len(users) > 0, Users: users})
		return
	}

	st, err := a.SSH.State()
	if err != nil {
		slog.Error("ssh state read failed", "service", "ssh", "err", err)
		writeErr(w, http.StatusInternalServerError, "ssh-state-failed", "ssh state read failed")
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// resolveHome returns the user's home directory path, UID, and GID.
//
// When UserMgr is wired (cmd/host-agent-real), delegates to ResolveHome which
// reads /etc/passwd via os/user.Lookup. When nil (cmd/host-agent), returns a
// deterministic fake: /home/<username> and a stable UID/GID derived from the
// username so the dev loop (fake agent over socket) produces coherent output.
//
// 404 with code "unknown-user" when the real manager reports the user is gone.
// The brain maps this to a 422 or installation error, not a 500 retry.
func (a *Agent) resolveHome(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	if username == "" {
		writeErr(w, http.StatusBadRequest, "bad-request", "username is required")
		return
	}

	if a.UserMgr != nil {
		home, uid, gid, err := a.UserMgr.ResolveHome(username)
		if err != nil {
			if errors.Is(err, ErrUnknownUser) {
				writeErr(w, http.StatusNotFound, "unknown-user", "user not found")
				return
			}
			slog.Error("resolve-home: user-manager error", "username", username, "err", err)
			writeErr(w, http.StatusInternalServerError, "resolve-home-failed", "resolve-home failed")
			return
		}
		slog.Info("resolve-home", "username", username, "home", home)
		writeJSON(w, http.StatusOK, protocol.ResolveHomeResponse{HomePath: home, UID: uid, GID: gid})
		return
	}

	// Fake branch: the dev loop runs the brain and this agent as the same
	// unprivileged operator, so resolve-home returns that operator's *own*
	// uid/gid and home dir — not a synthetic fakeUID + /home/<user> the brain
	// can't chown to. The brain (same operator) then already owns every private
	// bind dir it creates, so the per-dir chowns are no-op successes needing no
	// privilege (#147). The home dir is ensured to exist + operator-owned so the
	// use-case folder bind source is writable too.
	home, uid, gid, err := devIdentity()
	if err != nil {
		slog.Error("resolve-home (fake): resolve operator identity", "err", err)
		writeErr(w, http.StatusInternalServerError, "resolve-home-failed", "resolve-home failed")
		return
	}
	slog.Info("resolve-home (fake)", "username", username, "home", home, "uid", uid)
	writeJSON(w, http.StatusOK, protocol.ResolveHomeResponse{HomePath: home, UID: uid, GID: gid})
}

// wellKnownIdentity returns the fixed service-account UIDs/GIDs for the
// malmo-app system user and the malmo-shared group.
//
// When UserMgr is wired (cmd/host-agent-real), delegates to WellKnownIdentity
// which resolves the real system user/group via os/user.Lookup. When nil
// (cmd/host-agent fake), returns fixed dev constants that sit below the
// per-user FNV hash range [3000, 3999] so service identities don't collide.
func (a *Agent) wellKnownIdentity(w http.ResponseWriter, r *http.Request) {
	if a.UserMgr != nil {
		appUID, appGID, sharedGID, err := a.UserMgr.WellKnownIdentity()
		if err != nil {
			slog.Error("well-known-identity: user-manager error", "err", err)
			writeErr(w, http.StatusInternalServerError, "well-known-identity-failed", "well-known-identity failed")
			return
		}
		writeJSON(w, http.StatusOK, protocol.WellKnownIdentityResponse{
			MalmoAppUID:    appUID,
			MalmoAppGID:    appGID,
			MalmoSharedGID: sharedGID,
		})
		return
	}

	// Fake branch: resolve the malmo-app service identity to the dev operator's
	// own uid/gid (not fixed 2000/2001) for the same reason as resolve-home — a
	// household-scope folder app then runs as an identity the unprivileged dev
	// brain owns, so Part A's bind-dir chowns are no-op successes (#147). The
	// shared GID is the operator's egid, a group the operator is actually in, so
	// the group_add on shared-source mounts is valid in dev.
	_, uid, gid, err := devIdentity()
	if err != nil {
		slog.Error("well-known-identity (fake): resolve operator identity", "err", err)
		writeErr(w, http.StatusInternalServerError, "well-known-identity-failed", "well-known-identity failed")
		return
	}
	writeJSON(w, http.StatusOK, protocol.WellKnownIdentityResponse{
		MalmoAppUID:    uid,
		MalmoAppGID:    gid,
		MalmoSharedGID: gid,
	})
}

// allocateAppService reserves a UID/GID pair from the app-service band
// [protocol.AppServiceUIDMin, AppServiceUIDMax] for a folderless
// `service_user: true` instance (APP_ISOLATION.md # Runtime identity & data
// ownership). Idempotent per instance: re-allocating for an instance that
// already holds a reservation returns the same pair.
//
// When UserMgr is wired (cmd/host-agent-real), the reservation is a real
// system account (malmo-svc-<uid> in /etc/passwd, durable across restarts).
// When nil (cmd/host-agent fake), it resolves to the dev operator's own
// uid/gid — the same chownable-identity rule as resolveHome/wellKnownIdentity
// (#147): the unprivileged dev brain runs as this operator, so it already owns
// every bind dir it creates and the per-dir chowns are no-op successes. A
// band UID (≥ 2100) would be un-chownable in dev and the service_user app's
// data dir would stay operator-owned while the container ran as the band UID —
// the crash-loop this avoids.
func (a *Agent) allocateAppService(w http.ResponseWriter, r *http.Request) {
	var req protocol.AllocateAppServiceIdentityRequest
	if !decode(w, r, &req) {
		return
	}
	if req.InstanceID == "" {
		writeErr(w, http.StatusBadRequest, "bad-request", "instance_id is required")
		return
	}

	if a.UserMgr != nil {
		uid, gid, err := a.UserMgr.AllocateAppService(req.InstanceID)
		if err != nil {
			slog.Error("allocate-app-service: user-manager error", "instance_id", req.InstanceID, "err", err)
			writeErr(w, http.StatusInternalServerError, "allocate-app-service-failed", "allocate-app-service failed")
			return
		}
		slog.Info("allocate-app-service", "instance_id", req.InstanceID, "uid", uid)
		writeJSON(w, http.StatusOK, protocol.AllocateAppServiceIdentityResponse{UID: uid, GID: gid})
		return
	}

	// Fake branch: the dev operator's own identity (see doc comment).
	uid, gid := os.Getuid(), os.Getgid()
	slog.Info("allocate-app-service (fake)", "instance_id", req.InstanceID, "uid", uid)
	writeJSON(w, http.StatusOK, protocol.AllocateAppServiceIdentityResponse{UID: uid, GID: gid})
}

// releaseAppService returns an allocated app-service identity to the band
// (uninstall, or install rollback). Idempotent: releasing a UID that is not
// allocated returns 200.
func (a *Agent) releaseAppService(w http.ResponseWriter, r *http.Request) {
	var req protocol.ReleaseAppServiceIdentityRequest
	if !decode(w, r, &req) {
		return
	}

	if a.UserMgr != nil {
		// Band guard, real agent only: a release runs userdel, so it must
		// never be usable to delete an arbitrary account — reject any UID
		// outside the reserved app-service band. The fake branch allocates the
		// dev operator's own out-of-band UID (allocateAppService), so the guard
		// can't live before the split.
		if req.UID < protocol.AppServiceUIDMin || req.UID > protocol.AppServiceUIDMax {
			writeErr(w, http.StatusBadRequest, "bad-request", "uid outside the app-service band")
			return
		}
		if err := a.UserMgr.ReleaseAppService(req.UID); err != nil {
			slog.Error("release-app-service: user-manager error", "uid", req.UID, "err", err)
			writeErr(w, http.StatusInternalServerError, "release-app-service-failed", "release-app-service failed")
			return
		}
		slog.Info("release-app-service", "uid", req.UID)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Fake branch: nothing to release — the dev operator's identity is not a
	// reservation (allocateAppService hands out os.Getuid()/os.Getgid()).
	slog.Info("release-app-service (fake)", "uid", req.UID)
	w.WriteHeader(http.StatusOK)
}

// devIdentity returns the operator identity the fake host-agent hands out for
// resolve-home and well-known-identity: the process's own uid/gid (the dev
// brain runs as this same operator, so everything it creates is already
// chownable) and its home dir, ensured to exist so the use-case folder bind
// source is writable (#147). os.UserHomeDir reads $HOME — set in every dev
// shell — and the dir is the operator's, so MkdirAll is operator-owned.
func devIdentity() (home string, uid, gid int, err error) {
	home, err = os.UserHomeDir()
	if err != nil {
		return "", 0, 0, fmt.Errorf("resolve home dir: %w", err)
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return "", 0, 0, fmt.Errorf("ensure home dir %q: %w", home, err)
	}
	return home, os.Getuid(), os.Getgid(), nil
}

// --- HTTP helpers ---

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "bad-json", err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, protocol.Error{Code: code, Message: msg})
}

// LogRequests is a minimal middleware that lets the binary log requests if desired.
// Currently a no-op (mirrors the fake's original stub); exported so cmd/ can
// wrap with its own logger if needed.
func LogRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}
