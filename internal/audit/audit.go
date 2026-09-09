// Package audit provides the single write path for the append-only audit log
// (LOGGING.md # Write path). One function: Record. On INSERT failure it logs
// and returns — callers never see the error.
package audit

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/malmoos/malmo/internal/auth"
	"github.com/malmoos/malmo/internal/store"
)

// v1 action vocabulary (LOGGING.md # Write path).
const (
	ActionSetupComplete = "setup.complete"
	ActionSetupFailure  = "setup.failure"
	ActionLoginSuccess  = "login.success"
	ActionLoginFailure  = "login.failure"
	ActionLoginLockout  = "login.lockout"
	ActionLogout        = "logout"
	// Portal-to-box SSO handshake (hosted only; cloud
	// specs/AUTH_AND_ACCESS.md # Portal-to-box SSO). Success mints a box
	// session (and, on first use, the owner admin); failure covers a rejected,
	// replayed, wrong-box, or non-owner assertion.
	ActionSSOSuccess      = "sso.success"
	ActionSSOFailure      = "sso.failure"
	ActionAppInstall      = "app.install"
	ActionAppUninstall    = "app.uninstall"
	ActionAppCustomCreate = "app.custom.create"

	// User management actions (USERS_AND_GROUPS.md).
	ActionUserCreate         = "user.create"
	ActionUserRoleChange     = "user.role.change"
	ActionUserDelete         = "user.delete"
	ActionUserPasswordReset  = "user.password.reset"
	ActionUserPasswordChange = "user.password.change"

	// Recovery-code redemption (AUTH.md # Using the recovery code).
	ActionRecoverSuccess = "recover.success"
	ActionRecoverFailure = "recover.failure"

	// Elevation window (USERS_AND_GROUPS.md # Elevation in the UI).
	ActionElevateSuccess = "auth.elevate.success"
	ActionElevateFailure = "auth.elevate.failure"

	// Health issue transitions (HEALTH.md # Persistence, LOGGING.md).
	ActionHealthIssueRaised  = "health.issue.raised"
	ActionHealthIssueCleared = "health.issue.cleared"

	// Outgoing-mail providers (SERVICE_PROVISIONING.md # BYO outgoing mail).
	// CRUD is elevation-class; test sends real mail through the credential, so
	// it audits too. Rebind is the per-app binding change.
	ActionMailProviderCreate = "mail.provider.create"
	ActionMailProviderUpdate = "mail.provider.update"
	ActionMailProviderDelete = "mail.provider.delete"
	ActionMailProviderTest   = "mail.provider.test"
	ActionAppMailRebind      = "app.mail.rebind"

	// User-supplied app config update (APP_MANIFEST.md # D4). Changing a config
	// value (an API token, a connection string) is elevation-class, so it audits
	// success and failure.
	ActionAppConfigUpdate = "app.config.update"

	// Per-app access-mode change (ENVIRONMENT.md #306, hosted): flipping an app
	// between owner-only (restricted) and public alters who can reach it, so it is
	// elevation-class and audits success and failure.
	ActionAppExposureSet = "app.exposure.set"

	// Device access (AUTH.md # Device access). Enabling SSH on an account changes
	// who can get a shell on the box, so it is elevation-class and audits failure
	// as well as success — the Activity view has to be able to answer "did someone
	// try to open a shell path into this box?" the way it answers it for logins.
	ActionSSHAccessSet = "ssh.access.set"
	ActionSSHKeyAdd    = "ssh.key.add"
	ActionSSHKeyDelete = "ssh.key.delete"

	// Control-plane update start (UPDATES.md # 3). Starting an update replaces
	// the brain and UI containers on the box, so it is elevation-class: it
	// audits the start and every refusal. It does not audit the outcome — the
	// brain that accepted the update is the container being replaced, so it may
	// not be alive to see how the job ended (internal/api/systemupdate.go).
	ActionSystemUpdate = "system.update"
)

// Target describes the object the action acts on. Both fields are optional.
type Target struct {
	Kind string // "app" | "user" | …
	ID   string // slug, user_id, etc.
}

// EventStore is the persistence surface audit needs. Declared here so the
// audit package doesn't depend on store's full API (consumer-side interface,
// CLAUDE.md).
type EventStore interface {
	InsertAuditEvent(store.AuditEvent) error
}

// Recorder writes audit events. Construct once via New and inject into
// handlers that need to emit audit records.
type Recorder struct {
	store EventStore
}

// New returns a Recorder backed by the given store.
func New(s EventStore) *Recorder {
	return &Recorder{store: s}
}

// Record writes one audit event derived from ctx (actor identity + client IP)
// and the supplied arguments. If the INSERT fails, it logs at Error level and
// returns — callers must not inspect or propagate this failure.
func (r *Recorder) Record(ctx context.Context, action string, target Target, metadata map[string]any, success bool) {
	id, hasIdentity := auth.FromContext(ctx)
	ip, _ := ClientIPFromContext(ctx)

	evt := store.AuditEvent{
		TS:         time.Now().UnixMilli(),
		Action:     action,
		TargetKind: target.Kind,
		TargetID:   target.ID,
		SourceIP:   ip,
		Success:    success,
	}

	if hasIdentity {
		evt.ActorUserID = id.User.ID
		evt.ActorRole = id.User.Role
	} else {
		evt.ActorRole = "system"
	}

	if len(metadata) > 0 {
		b, err := json.Marshal(metadata)
		if err != nil {
			slog.Warn("audit metadata marshal failed", "action", action, "err", err)
		} else {
			evt.Metadata = string(b)
		}
	}

	if err := r.store.InsertAuditEvent(evt); err != nil {
		slog.Error("audit insert failed",
			"action", action,
			"actor_user_id", evt.ActorUserID,
			"target_kind", target.Kind,
			"target_id", target.ID,
			"err", err)
	}
}

type ipKey struct{}

// WithClientIP attaches the client IP to ctx. Called by authMiddleware so all
// downstream handlers (and audit.Record) can read it without knowing about HTTP.
func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, ipKey{}, ip)
}

// ClientIPFromContext returns the client IP attached by WithClientIP.
func ClientIPFromContext(ctx context.Context) (string, bool) {
	ip, ok := ctx.Value(ipKey{}).(string)
	return ip, ok && ip != ""
}
