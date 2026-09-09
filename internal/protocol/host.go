// Package protocol defines the wire types shared between the brain and
// host-agent (BRAIN_HOST_PROTOCOL.md). HTTP/JSON over a UNIX socket.
package protocol

// SocketPath is the production socket location. In dev the brain and the
// fake host-agent agree on a path via MALMO_AGENT_SOCK.
const SocketPath = "/var/run/malmo/agent.sock"

// AppHostSuffix is the LAN hostname suffix for app instances: an app with slug
// <slug> is reachable at "<slug>" + AppHostSuffix, e.g. "photos.local".
//
// It is single-label on purpose. The earlier "<slug>.malmo.local" shape was
// multi-label relative to .local and is rejected outright by nss-mdns on Linux
// (and is unreliable on other resolvers), so the no-cloud LAN URL never
// resolved there. The ".malmo" infix bought nothing in mDNS (no zones, no
// delegation, no wildcards — each name is published individually regardless),
// so it was dropped. See DECISIONS.md (2026-05-31) and DISCOVERY.md.
//
// Both sides of the host socket build the name from this constant: the brain
// for the URL it shows and the Caddy route it writes, host-agent for the name
// it announces over Avahi. They must agree, so the suffix lives here, in the
// shared wire-contract package.
const AppHostSuffix = ".local"

// Major is the brain ↔ host-agent wire-protocol major version. The socket API
// is served under the /v<Major> URL prefix, and the two endpoints ship lockstep
// within one OS release — there is no connection-time negotiation
// (BRAIN_HOST_PROTOCOL.md # Versioning).
//
// host-agent additionally guards the pairing at brain launch: it reads the
// brain image's declared major from the ImageProtocolMajorLabel OCI label and
// refuses to start a brain whose major it does not speak (CONTROL_PLANE.md
// # Locked: host-agent launches the brain container). Bump this in lockstep
// with the /v<N> URL prefix and the brain image's label.
const Major = 1

// ImageProtocolMajorLabel is the OCI image label under which the brain image
// declares the wire-protocol major it implements (set in cmd/brain/Dockerfile).
// host-agent reads it at launch and compares against Major. The dotted `malmo.`
// prefix matches the runtime label convention (e.g. malmo.instance_id).
const ImageProtocolMajorLabel = "malmo.protocol.major"

// PublishRequest registers a per-app .local name (POST /v1/discovery/publish).
type PublishRequest struct {
	Slug string `json:"slug"`
}

type PublishResponse struct {
	Name  string `json:"name"`
	State string `json:"state"` // "established"
}

// UnpublishRequest removes a per-app name (POST /v1/discovery/unpublish).
type UnpublishRequest struct {
	Slug string `json:"slug"`
}

// SystemStatus is GET /v1/system/status.
//
// DataDiskFreeBytes / DataDiskTotalBytes are a statfs snapshot of the data
// drive's mount (/srv/malmo): free = available blocks × block size (Bavail ×
// Bsize, the space an unprivileged writer can actually use, already excluding
// the root reserve), total = Blocks × Bsize. They back the install-plan's
// free_bytes figure (BRAIN_UI_PROTOCOL.md # install-plan) so the install dialog
// can warn before a download that won't fit. Advisory and racy — a snapshot, no
// reservation — so the brain treats them as a hint, never a gate. 0 means "not
// measured" (no disk reporter wired, or statfs failed): the brain shows no free
// figure rather than a misleading zero.
//
// Disks is the per-volume fullness view (one entry per mounted volume of
// interest — the OS drive, plus the data drive when present) backing the
// system-resources panel's Storage bars (LOCAL_ANALYTICS.md # Real-time system
// resources). It is a display superset of DataDisk*, which are kept untouched
// for the install-plan footprint (DECISIONS.md 2026-06-13). Same Bavail/Blocks
// statfs semantics per entry; absent volumes are omitted, never zero-filled.
type SystemStatus struct {
	Hostname           string      `json:"hostname"`
	UptimeS            int64       `json:"uptime_s"`
	DiskPressure       bool        `json:"disk_pressure"`
	AgentVersion       string      `json:"agent_version"`
	DataDiskFreeBytes  int64       `json:"data_disk_free_bytes"`
	DataDiskTotalBytes int64       `json:"data_disk_total_bytes"`
	Disks              []DiskSpace `json:"disks"`
}

// DiskSpace is one mounted volume's fullness for the system-resources Storage
// bars: a human Label ("System" for the OS drive at /, "Data" for the data
// drive at /srv/malmo — STORAGE.md mount layout), plus the same statfs figures
// as DataDisk* (FreeBytes = Bavail × Bsize, TotalBytes = Blocks × Bsize). Used
// is derived UI-side as Total − Free. host-agent omits a volume that isn't a
// distinct mount (a Level-0 box has no data drive: /srv/malmo is just a
// directory on the OS drive), so the slice carries only real volumes.
type DiskSpace struct {
	Label      string `json:"label"`
	FreeBytes  int64  `json:"free_bytes"`
	TotalBytes int64  `json:"total_bytes"`
}

// SystemResources is GET /v1/system/resources: one raw cumulative-counter
// sample plus a monotonic ts_ns, the host source for the all-users live
// system-resources view (LOCAL_ANALYTICS.md # Real-time system resources).
// host-agent reads /proc/stat, /proc/meminfo, /proc/loadavg, /proc/net/dev,
// /proc/diskstats on request and computes no rates — it is stateless. The
// brain polls this once per second while ≥1 UI subscriber is connected, diffs
// successive samples (rate denominator = ts_ns delta), and fans the derived
// rates out over its own SSE channel (BRAIN_HOST_PROTOCOL.md # Pattern A).
//
// host-agent applies the interface/device allowlist — physical LAN NICs + the
// mesh interface, excluding lo/docker0/veth*/br-*, whole-disk devices only — so
// the brain never sees container-bridge noise. The counters are int64 because
// /proc byte and jiffy counters routinely exceed 2^31.
type SystemResources struct {
	TsNs    int64          `json:"ts_ns"`
	CPU     CPUCounters    `json:"cpu"`
	LoadAvg [3]float64     `json:"loadavg"`
	Mem     MemCounters    `json:"mem"`
	Net     []NetCounters  `json:"net"`
	Disk    []DiskCounters `json:"disk"`
	UptimeS int64          `json:"uptime_s"`
}

// CPUCounters are cumulative jiffy counters from the aggregate /proc/stat line.
// cpu_pct is derived by the brain as busy/total over the sample interval, where
// busy = total - idle.
type CPUCounters struct {
	TotalJiffies int64 `json:"total_jiffies"`
	IdleJiffies  int64 `json:"idle_jiffies"`
}

// MemCounters are instantaneous memory figures from /proc/meminfo, in bytes.
// These are levels, not rates — the brain passes them through unchanged.
type MemCounters struct {
	TotalBytes     int64 `json:"total_bytes"`
	AvailableBytes int64 `json:"available_bytes"`
	UsedBytes      int64 `json:"used_bytes"`
}

// NetCounters are cumulative per-interface byte counters from /proc/net/dev.
type NetCounters struct {
	Iface   string `json:"iface"`
	RxBytes int64  `json:"rx_bytes"`
	TxBytes int64  `json:"tx_bytes"`
}

// DiskCounters are cumulative per-device byte counters derived from
// /proc/diskstats (sectors × 512 — the kernel's sector unit there is a fixed
// 512 bytes regardless of the device's real sector size).
type DiskCounters struct {
	Dev        string `json:"dev"`
	ReadBytes  int64  `json:"read_bytes"`
	WriteBytes int64  `json:"write_bytes"`
}

// SystemGPU is GET /v1/system/gpu: the host's GPU capability report
// (BRAIN_HOST_PROTOCOL.md # GPU capability query). The brain asks once per
// `gpu: true` install, for two facts: Present (false → the hard capacity
// refusal, APP_ISOLATION.md # GPU) and RenderGID (the render group the
// container joins via group_add so a cap_drop:ALL app can open /dev/dri).
// Vendor is "intel" in v1 — the only supported runtime; "amd" and "nvidia"
// are reserved for the follow-on runtimes and not emitted yet. Vendor is
// empty and RenderGID 0 when Present is false.
type SystemGPU struct {
	Present   bool   `json:"present"`
	Vendor    string `json:"vendor"`
	RenderGID int    `json:"render_gid"`
}

// PublishedName is one entry in the discovery state.
type PublishedName struct {
	Slug  string `json:"slug"`
	Name  string `json:"name"`
	State string `json:"state"`
}

// DiscoveryState is GET /v1/discovery/state.
type DiscoveryState struct {
	Publisher  string          `json:"publisher"`
	HostName   string          `json:"host_name"`
	RenamedTo  *string         `json:"renamed_to"`
	Published  []PublishedName `json:"published"`
	Interfaces []string        `json:"interfaces"`
}

// VerifyPasswordRequest is POST /v1/auth/verify-password. host-agent delegates
// to PAM authenticate() in prod; the fake checks a bcrypt hash. The endpoint
// never distinguishes wrong-password from unknown-user from locked-account.
type VerifyPasswordRequest struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

type VerifyPasswordResponse struct {
	Valid bool `json:"valid"`
}

// SetPasswordRequest is POST /v1/auth/set-password. Upsert: creates the user
// if missing (real impl: useradd + passwd as one atomic op), otherwise just
// updates the password.
type SetPasswordRequest struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

// DeleteUserRequest is POST /v1/auth/delete-user. Idempotent: unknown user
// returns 200.
type DeleteUserRequest struct {
	User string `json:"user"`
}

// SetRoleRequest is POST /v1/auth/set-role. Updates the user's Linux group
// membership (malmo-admin) to match the new role. Role must be "admin" or "member".
type SetRoleRequest struct {
	User string `json:"user"`
	Role string `json:"role"`
}

// SetSSHAccessRequest is POST /v1/ssh/set-access — one account's *full* desired
// SSH state, not a delta (BRAIN_HOST_PROTOCOL.md # SSH access). AuthorizedKeys
// replaces that account's authorized_keys file outright, so a retry after a
// partial failure converges instead of compounding. Disabling is Enabled=false;
// host-agent then drops the account's Match block and its keys.
//
// RequirePassword adds the malmo password as a second required method for this
// account, rendering "AuthenticationMethods publickey,password". It never
// substitutes for the mandatory factor. Which factor is mandatory is the brain's
// call and depends on the environment profile — a key on hosted, the password on
// the appliance (AUTH.md # Device access) — and the brain refuses a hosted enable
// with no key before calling here. host-agent does not know the profile, the same
// division of labour as set-timezone, where the brain validates the zone.
type SetSSHAccessRequest struct {
	User            string   `json:"user"`
	Enabled         bool     `json:"enabled"`
	AuthorizedKeys  []string `json:"authorized_keys"`
	RequirePassword bool     `json:"require_password"`
}

// SSHState is GET /v1/ssh/state — actual host state for the reconciler, read on
// the 60-second heartbeat. DaemonRunning is the answer to "is :22 open", which on
// hosted is the only control over that port (ENVIRONMENT.md # Access & files).
// Users lists only accounts that are currently enabled.
//
// Keys are reported as a count, never as material: the brain already holds the
// desired keys, and echoing them back would put them in a second place for no
// reconcile benefit.
type SSHState struct {
	DaemonRunning bool           `json:"daemon_running"`
	Users         []SSHUserState `json:"users"`
}

// SSHUserState is one enabled account in SSHState.
type SSHUserState struct {
	Username        string `json:"username"`
	KeyCount        int    `json:"key_count"`
	RequirePassword bool   `json:"require_password"`
}

// SetTimezoneRequest is POST /v1/system/set-timezone. host-agent applies the
// system timezone via `timedatectl set-timezone <zone>` (TIME.md # System TZ);
// the brain drives it from the first-run wizard's time-zone step and the later
// Settings → System → Time surface. Zone is an IANA tz database name
// ("Europe/Stockholm", "UTC"). No response body — 200 on success.
type SetTimezoneRequest struct {
	Zone string `json:"zone"`
}

// HealthCategory is the report/reconcile taxonomy carried on the wire by
// SystemHealth. It is a *separate axis* from the brain's issue category
// (health.Category: storage | state | network | version | capacity, which
// drives display + the blocks_* nature of an issue): HealthCategory partitions
// the locus-B report so the brain can reconcile each domain independently. The
// enum is broad so downstream detectors land as pure follow-ups: ram-pressure
// (#38) emits into resources, clock-not-synced (#39) into time, disk-smart into
// drives. system (reboot-required, #40) was added after the #34 pin when that
// detector was reclassified to locus B (DECISIONS.md 2026-05-31) — no existing
// physical-measurement domain fits an OS-state flag.
type HealthCategory string

const (
	HealthCategoryStorage   HealthCategory = "storage"   // filesystem / mount / canary / mergerfs
	HealthCategoryDrives    HealthCategory = "drives"    // SMART / per-device health (reserved)
	HealthCategoryServices  HealthCategory = "services"  // systemctl is-active (service-down)
	HealthCategoryResources HealthCategory = "resources" // memory / CPU pressure (ram-pressure)
	HealthCategoryTime      HealthCategory = "time"      // chronyc tracking (clock-not-synced)
	HealthCategorySystem    HealthCategory = "system"    // OS/box state — reboot-required pending-reboot flag
)

// SystemHealth is GET /v1/health/system — the single locus-B findings report
// host-agent serves and the brain polls on its 60s heartbeat (HEALTH.md
// # Detector catalog, BRAIN_HOST_PROTOCOL.md). It carries findings across
// categories in one payload so the brain's ApplyFindings(category, …) reconcile
// can clear-absent / raise-present per category atomically — a storage poll
// never clears a service finding.
//
// Categories is keyed by HealthCategory. A key being present means host-agent
// measured that category this cycle: the brain reconciles it, clearing any
// host-reported issue in that category absent from the slice. A key being
// absent means "not measured this cycle" — the brain leaves that category's
// issues alone. An empty slice under a present key means "measured, all
// healthy" (clear all host-reported issues in the category).
type SystemHealth struct {
	CheckedAt  string                       `json:"checked_at"`
	Categories map[HealthCategory][]Finding `json:"categories"`
}

// StorageHealth is the on-disk storage findings file
// (/run/malmo/health/storage.json) written by malmo-storage-verify (BOOT.md
// # The storage-ready target) and read by host-agent's storage source, which
// folds the findings into SystemHealth's storage category. It is also the
// boot reporter's wire shape.
//
// An empty Findings slice means storage looks healthy — not "no report yet."
// A missing report file at the host-agent end is reported as empty Findings;
// a malformed report is reported as a single Finding with ID
// "health-report-malformed" so the brain has something to surface rather than
// silently passing.
type StorageHealth struct {
	CheckedAt string    `json:"checked_at"`
	Findings  []Finding `json:"findings"`
}

// Finding is one anomaly a reporter detected. ID is a stable string drawn from
// the typed taxonomy in HEALTH.md (e.g. "data-drive-missing", "service-down").
// The brain looks the ID up in its registry to derive category, severity, tier,
// and the blocks_* flags — the reporter does not re-declare those, so the
// source of truth stays in one place.
//
// InstanceKey scopes a per-instance finding (e.g. service-down carries the unit
// name, so docker-down and caddy-down are distinct issues). Empty for box-wide
// findings like data-drive-missing.
type Finding struct {
	ID          string `json:"id"`
	InstanceKey string `json:"instance_key,omitempty"`
	Details     string `json:"details,omitempty"`
}

// ResolveHomeResponse is GET /v1/users/{username}/home. Returns the owner's home
// directory path and POSIX UID/GID so the brain (containerized, no /etc/passwd
// access) can emit correct bind-mount sources and user: directives in the
// compose override. 404 with code "unknown-user" if the user does not exist.
type ResolveHomeResponse struct {
	HomePath string `json:"home_path"`
	UID      int    `json:"uid"`
	GID      int    `json:"gid"`
}

// WellKnownIdentityResponse is GET /v1/identity/well-known. Returns the fixed
// service-account identities the brain needs to emit correct user:/group_add
// directives in compose overrides for household-scope app instances.
//
// MalmoAppUID/GID is the shared service identity (compose user:).
// MalmoSharedGID is the GID of the malmo-shared group (apps electing a shared
// folder source are added to it via group_add).
type WellKnownIdentityResponse struct {
	MalmoAppUID    int `json:"malmo_app_uid"`
	MalmoAppGID    int `json:"malmo_app_gid"`
	MalmoSharedGID int `json:"malmo_shared_gid"`
}

// AppServiceUIDMin/Max bound the reserved app-service identity band host-agent
// allocates `service_user: true` instances from (APP_ISOLATION.md # Runtime
// identity & data ownership): below the malmo user floor (UID_MIN 3000,
// FIRST_RUN.md # Identity), above the fixed well-known identities (2000/2001),
// with 2002–2099 left unallocated as headroom for future fixed identities.
// Both sides of the socket validate against the band — host-agent never
// allocates or releases outside it — so the constants live in the shared
// wire-contract package.
const (
	AppServiceUIDMin = 2100
	AppServiceUIDMax = 2999
)

// AllocateAppServiceIdentityRequest is POST /v1/identity/app-service, the
// allocating sibling of GET /v1/identity/well-known. The brain calls it during
// install of a folderless `service_user: true` instance; host-agent reserves a
// fresh UID/GID pair from the app-service band and the brain persists it on
// the instance row (it is never re-requested for the life of the instance).
// InstanceID labels the reservation for debuggability (the real host-agent
// stamps it into the account's GECOS field).
type AllocateAppServiceIdentityRequest struct {
	InstanceID string `json:"instance_id"`
}

type AllocateAppServiceIdentityResponse struct {
	UID int `json:"uid"`
	GID int `json:"gid"`
}

// ReleaseAppServiceIdentityRequest is POST /v1/identity/app-service/release.
// The brain calls it at uninstall (and on install rollback) to return the
// instance's allocated identity to the band. Idempotent: releasing a UID that
// is not allocated returns 200.
type ReleaseAppServiceIdentityRequest struct {
	UID int `json:"uid"`
}

// JournalLine is one log line streamed over the per-app log tail
// (GET /v1/journal/follow, BRAIN_HOST_PROTOCOL.md # Pattern C). It is the
// `data:` payload of each SSE frame: the journald entry's timestamp, the std
// stream it came from, and the line text. Lost marks a replay gap — a single
// {"lost":true} frame the producer emits when a reconnect's Last-Event-ID
// predates what it can replay; the brain forwards it to the dashboard
// unchanged. A Lost frame carries no Ts/Stream/Line.
type JournalLine struct {
	Ts     string `json:"ts,omitempty"`
	Stream string `json:"stream,omitempty"` // "stdout" | "stderr"
	Line   string `json:"line,omitempty"`
	Lost   bool   `json:"lost,omitempty"`
}

// Error is the JSON error body shape on non-2xx responses.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// --- Jobs (BRAIN_HOST_PROTOCOL.md # Pattern B) ---
//
// One job kind exists today: system-update, the control-plane update
// transaction (UPDATES.md # 3). The full Pattern B framework — a kind registry
// with typed attributes, resource-class queueing, cancel, and the SSE log
// stream — is not built, because it would be an abstraction with a single
// consumer (CLAUDE.md # Go code discipline). What is built is the part
// system-update needs: a job record, a MaxDuration bound, and one global lock.

// JobKindSystemUpdate is the kind name of the control-plane update job.
const JobKindSystemUpdate = "system-update"

// Job status values. `cancelled`, `cancelling`, and `stalled` from the spec are
// not produced by this host-agent: there is no cancel route, and a run that
// passes its MaxDuration is cancelled and rolled back, so it lands on a known
// outcome (`failed`) rather than on "we are not sure" (`stalled`).
const (
	JobStatusRunning   = "running"
	JobStatusCompleted = "completed"
	JobStatusFailed    = "failed"
)

// Job error codes. `job-timeout` means the run passed its MaxDuration and was
// cancelled; `job-failed` means the operation itself reported a failure.
const (
	JobErrorTimeout = "job-timeout"
	JobErrorFailed  = "job-failed"
)

// SystemUpdateRequest is POST /v1/jobs/system-update: the target control-plane
// pair, as explicit image refs.
//
// The refs are in the request on purpose. There is no release manifest and no
// cloud call behind this endpoint — the box↔cloud credential that a fetched
// target would need is still undesigned (NEXT.md, Tier 1). An empty ref means
// "leave this component alone", which is how a UI-only or brain-only ship is
// expressed (UPDATES.md # 3). Both empty is rejected.
type SystemUpdateRequest struct {
	BrainImage string `json:"brain_image,omitempty"`
	UIImage    string `json:"ui_image,omitempty"`
}

// SystemUpdateResult is what a finished system-update job did. It is reported
// on failure as well as on success, because "we reverted, and here is what
// broke" is the answer the dashboard has to show (UPDATES.md # 3 step 4).
type SystemUpdateResult struct {
	BrainChanged bool `json:"brain_changed"`
	UIChanged    bool `json:"ui_changed"`
	Reverted     bool `json:"reverted"`
	// FailureMode names the step that broke: "read", "pull", "snapshot",
	// "declare", "recreate", or "health". Empty on success.
	FailureMode string `json:"failure_mode,omitempty"`
	// RevertError is set when the rollback itself failed. The box is then in a
	// state no automatic path can fix, and this is the operator's signal.
	RevertError string `json:"revert_error,omitempty"`
}

// Job is one host-agent job record. StartedAt/FinishedAt are RFC3339, matching
// the other timestamps on this socket (SystemHealth.CheckedAt).
type Job struct {
	ID         string              `json:"job_id"`
	Kind       string              `json:"kind"`
	Status     string              `json:"status"`
	StartedAt  string              `json:"started_at"`
	FinishedAt string              `json:"finished_at,omitempty"`
	Error      *Error              `json:"error,omitempty"`
	Result     *SystemUpdateResult `json:"result,omitempty"`
}

// UpdateTarget is GET /v1/system/update-target: what the update-target loop
// last decided (UPDATES.md # 8.4). A **read**. Nothing here starts, stops or
// changes an update — the trigger is POST /v1/jobs/system-update.
//
// It is the box's own view of one question: what am I running, what could I be
// running, and why is the difference not closed yet. The loop decides that every
// 15 minutes and used to say it only to the journal, which meant the appliance
// prompt UPDATES.md # 3 promises had nothing to read.
type UpdateTarget struct {
	// State is the whole answer in one word. See the UpdateTargetState
	// constants: the seven values are seven different things to tell an
	// operator, and flattening any pair of them loses the one that matters.
	State string `json:"state"`
	// Running is the control-plane pair this box is running right now, read
	// from the box's own declaration (the images.json ledger for the brain, the
	// staged compose for the UI). Read when the request arrives, not carried
	// from the last tick, because a tick that ended early never read it.
	Running ControlPlanePair `json:"running"`
	// Target is what the source offered. Present for current and available,
	// where it is a validated, digest-pinned pair the box could apply — and
	// also for **refused**, where it is the answer the box rejected and must
	// not be treated as applicable. It can be a tag there, because naming a tag
	// is one of the things that gets an answer refused. Read State before
	// reading this: only current and available mean "this is a usable pair".
	Target *ControlPlaneOffer `json:"target,omitempty"`
	// CheckedAt is when the loop last asked its source, RFC3339. Empty means it
	// has never finished a tick (states unknown and disabled).
	CheckedAt string `json:"checked_at,omitempty"`
	// Detail is why the last check produced no usable target. Set for
	// unreachable, refused and disabled; empty otherwise.
	//
	// It is a **diagnostic**, not UI copy: it carries the underlying error text
	// verbatim, which is what makes a broken source fixable. State is the field
	// the dashboard writes its sentence from (CLAUDE.md — no raw Go error
	// strings as user-facing text), and this is what an admin reads underneath
	// it.
	Detail string `json:"detail,omitempty"`
	// From is where the box got its update-target URL: "seed", "env" or
	// "default" (UPDATES.md # 8.4). Anything but "default" means this box is
	// not following the fleet, which is the first thing worth knowing when one
	// box behaves unlike the rest.
	//
	// The URL itself is deliberately not on the wire. "from" already carries
	// the fact worth showing, and an operator's hand-edited URL is the one
	// place a credential could turn up in a payload the dashboard renders.
	From string `json:"from,omitempty"`
	// Window is the update window in force, "HH:MM-HH:MM" local, and WindowFrom
	// is where it came from: "answer", "env" or "default". These are a
	// **separate** setting from From above, with values that only look alike —
	// a window can come from the source's answer, a URL never can.
	Window     string `json:"window,omitempty"`
	WindowFrom string `json:"window_from,omitempty"`
	// AutoApply is the UPDATES.md # 8.2 difference between the profiles: a
	// hosted box applies its target in the window on its own, an appliance
	// waits for an admin. It is what decides whether the dashboard says "this
	// installs tonight" or "install now".
	AutoApply bool `json:"auto_apply"`
	// Profile is the environment profile that made the decision, "appliance" or
	// "hosted" (ENVIRONMENT.md).
	Profile string `json:"profile,omitempty"`
}

// ControlPlanePair is a brain + UI image reference pair. Empty fields mean the
// box could not read that half of its own declaration.
type ControlPlanePair struct {
	Brain string `json:"brain,omitempty"`
	UI    string `json:"ui,omitempty"`
}

// ControlPlaneOffer is a target the source named: the version for display, and
// the two pinned references that would actually be pulled.
type ControlPlaneOffer struct {
	// Version is for display and nothing else. Nothing pulls by it.
	Version string `json:"version,omitempty"`
	// BrainImage / UIImage are full pinned references, repository@sha256:…
	BrainImage string `json:"brain_image"`
	UIImage    string `json:"ui_image"`
	// PublishedAt is when the release was published, RFC3339. Informational,
	// and empty when the source did not say.
	PublishedAt string `json:"published_at,omitempty"`
}

// The UpdateTarget states. They are ordered here the way an operator reads
// them: first the two that mean the loop is not answering, then the three that
// mean it asked and got nothing usable, then the two real answers.
const (
	// UpdateTargetDisabled means there is no loop on this box. host-agent
	// refused an unusable seeded update target and did not start one
	// (UPDATES.md # 8.4 — refused, never resolved away), so the box will not
	// update itself until that is fixed. Detail says what was wrong.
	//
	// This is deliberately not folded into "unknown": a box that will never
	// check and a box that has not checked yet look identical on the wire
	// otherwise, and the first one needs a human.
	UpdateTargetDisabled = "disabled"
	// UpdateTargetUnknown means the loop is running but has not finished its
	// first tick. It ticks immediately at startup, so this is a state of
	// seconds, not minutes.
	UpdateTargetUnknown = "unknown"
	// UpdateTargetNone means the source answered and has nothing to offer. A
	// normal state, not a failure: most boxes are in it.
	UpdateTargetNone = "none"
	// UpdateTargetUnreachable means the check could not be completed: the
	// source could not be read (DNS, a refused connection, a 500, a body that
	// is not JSON), or the box could not read its own running pair. Detail says
	// which. The box keeps running what it runs; it never degrades because it
	// could not ask.
	UpdateTargetUnreachable = "unreachable"
	// UpdateTargetRefused means the source answered and the box rejected the
	// answer: not pinned to a digest, an unexpected repository, only one of the
	// two images, or a digest that disagrees with its own reference. Nothing
	// was pulled. Distinct from unreachable on purpose — a source stuck on a
	// bad answer is a fleet-wide problem, and a source that is down is not.
	UpdateTargetRefused = "refused"
	// UpdateTargetCurrent means the target is what the box is already running.
	// The overwhelmingly common answer on a healthy box.
	UpdateTargetCurrent = "current"
	// UpdateTargetAvailable means the target differs from the running pair. On
	// hosted it applies in the next window on its own; on appliance it waits
	// for an admin.
	UpdateTargetAvailable = "available"
)
