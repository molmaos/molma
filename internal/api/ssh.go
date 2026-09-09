package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"golang.org/x/crypto/ssh"

	"github.com/malmoos/malmo/internal/audit"
	"github.com/malmoos/malmo/internal/auth"
	"github.com/malmoos/malmo/internal/profile"
	"github.com/malmoos/malmo/internal/protocol"
	"github.com/malmoos/malmo/internal/store"
)

// Device access — the per-account SSH opt-in (AUTH.md # Device access).
//
// These routes live under /api/v1/me because Device access is a My-account
// panel: every signed-in user manages their own SSH, and no admin manages it for
// them (SETTINGS.md # panel inventory). Nothing here takes a user id from the
// caller; the account is always the session's own, so there is no cross-account
// surface to get wrong.
//
// Every write is elevation-class and audits success and failure, because turning
// SSH on changes who can get a shell on the box (CLAUDE.md # Elevation-class
// mutations).

// maxSSHKeysPerUser caps how many keys one account may hold. Several is normal
// (a laptop and a desktop); dozens means something is wrong, and every key is a
// credential that authenticates as that user until someone notices it.
const maxSSHKeysPerUser = 10

// maxSSHKeyBytes bounds a submitted key. A real key line is a few hundred bytes;
// this leaves room for a long comment and refuses a pasted file.
const maxSSHKeyBytes = 4096

func (s *Server) registerSSHRoutes(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "get-my-ssh", Method: "GET", Path: "/api/v1/me/ssh",
		Summary: "Read my SSH access settings and keys (auth required)",
	}, s.getMySSH)

	huma.Register(api, huma.Operation{
		OperationID: "set-my-ssh", Method: "PUT", Path: "/api/v1/me/ssh",
		Summary: "Turn my SSH access on or off (auth required, elevation-class)",
	}, s.setMySSH)

	huma.Register(api, huma.Operation{
		OperationID: "add-my-ssh-key", Method: "POST", Path: "/api/v1/me/ssh/keys",
		Summary: "Add one of my SSH public keys (auth required, elevation-class)",
	}, s.addMySSHKey)

	huma.Register(api, huma.Operation{
		OperationID: "delete-my-ssh-key", Method: "DELETE", Path: "/api/v1/me/ssh/keys/{id}",
		Summary: "Remove one of my SSH public keys (auth required, elevation-class)", DefaultStatus: 204,
	}, s.deleteMySSHKey)
}

// SSHKeyDTO is one stored key as the dashboard sees it. The key material is sent
// back so the user can recognise what they added; the fingerprint is what the UI
// shows, because it is short and is what other tools display.
type SSHKeyDTO struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	PublicKey   string `json:"public_key"`
	Fingerprint string `json:"fingerprint"`
	AddedAt     int64  `json:"added_at"`
}

// SSHAccessDTO is the whole Device access panel state for one account.
//
// KeyRequired tells the UI which factor this profile makes mandatory, so the
// panel does not have to know the rule and the two cannot drift. The server
// enforces it regardless of what the UI does with it.
type SSHAccessDTO struct {
	Enabled         bool        `json:"enabled"`
	RequirePassword bool        `json:"require_password"`
	KeyRequired     bool        `json:"key_required"`
	Keys            []SSHKeyDTO `json:"keys"`
}

// keyRequired reports whether this profile makes a public key the mandatory
// factor. Hosted does: the box answers on the public internet, so a household
// password is not a credential there (DECISIONS.md 2026-09-09). The appliance
// does not: :22 is scoped to the LAN and the mesh by nftables, which is the
// perimeter the one-password model was designed around.
func (s *Server) keyRequired() bool {
	return s.profile == profile.Hosted
}

func (s *Server) getMySSH(ctx context.Context, _ *struct{}) (*struct {
	Body SSHAccessDTO
}, error) {
	id, ok := auth.FromContext(ctx)
	if !ok {
		return nil, huma.Error401Unauthorized("unauthenticated")
	}
	dto, err := s.sshAccessDTO(id.User.ID)
	if err != nil {
		return nil, err
	}
	return &struct{ Body SSHAccessDTO }{Body: dto}, nil
}

func (s *Server) setMySSH(ctx context.Context, in *struct {
	Body struct {
		Enabled bool `json:"enabled"`
		// Optional: an account that does not want the second lock simply omits it.
		RequirePassword bool `json:"require_password,omitempty" required:"false"`
	}
}) (*struct {
	Body SSHAccessDTO
}, error) {
	id, ok := auth.FromContext(ctx)
	if !ok {
		return nil, huma.Error401Unauthorized("unauthenticated")
	}
	tgt := audit.Target{Kind: "user", ID: id.User.ID}
	meta := map[string]any{"enabled": in.Body.Enabled, "require_password": in.Body.RequirePassword}

	if err := requireElevated(ctx); err != nil {
		s.auditor.Record(ctx, audit.ActionSSHAccessSet, tgt, meta, false)
		return nil, err
	}

	keys, err := s.store.ListSSHKeys(id.User.ID)
	if err != nil {
		s.auditor.Record(ctx, audit.ActionSSHAccessSet, tgt, meta, false)
		return nil, huma.Error500InternalServerError("read ssh keys failed", err)
	}

	// The profile's mandatory factor, enforced here and not only in the UI. On
	// hosted an account with no key could only ever authenticate by password, on
	// a port the open internet can reach — which is the thing this design exists
	// to prevent.
	if in.Body.Enabled && s.keyRequired() && len(keys) == 0 {
		s.auditor.Record(ctx, audit.ActionSSHAccessSet, tgt, meta, false)
		return nil, huma.Error422UnprocessableEntity(
			"add an SSH key before turning SSH on; a password alone is not enough on this box")
	}

	// Brain commits first; the host is reconstructible. On host failure the row is
	// rolled back to what it was, so the two sides cannot disagree about who has a
	// shell (CLAUDE.md # Brain commits first).
	prev, err := s.store.SSHAccessFor(id.User.ID)
	if err != nil {
		s.auditor.Record(ctx, audit.ActionSSHAccessSet, tgt, meta, false)
		return nil, huma.Error500InternalServerError("read ssh access failed", err)
	}
	if err := s.store.SetSSHAccess(id.User.ID, in.Body.Enabled, in.Body.RequirePassword); err != nil {
		s.auditor.Record(ctx, audit.ActionSSHAccessSet, tgt, meta, false)
		return nil, huma.Error500InternalServerError("save ssh access failed", err)
	}
	if err := s.applySSH(ctx, id.User.Username, in.Body.Enabled, in.Body.RequirePassword, keys); err != nil {
		if rbErr := s.store.SetSSHAccess(id.User.ID, prev.Enabled, prev.RequirePassword); rbErr != nil {
			slog.Error("ssh access rollback failed", "user_id", id.User.ID,
				"username", id.User.Username, "service", "ssh", "err", rbErr)
		}
		s.auditor.Record(ctx, audit.ActionSSHAccessSet, tgt, meta, false)
		return nil, huma.Error502BadGateway("host-agent ssh set-access failed", err)
	}

	s.auditor.Record(ctx, audit.ActionSSHAccessSet, tgt, meta, true)
	dto, err := s.sshAccessDTO(id.User.ID)
	if err != nil {
		return nil, err
	}
	return &struct{ Body SSHAccessDTO }{Body: dto}, nil
}

func (s *Server) addMySSHKey(ctx context.Context, in *struct {
	Body struct {
		PublicKey string `json:"public_key"`
		// Optional: the key's own comment names it when the user gives no label.
		Label string `json:"label,omitempty" required:"false"`
	}
}) (*struct {
	Body SSHAccessDTO
}, error) {
	id, ok := auth.FromContext(ctx)
	if !ok {
		return nil, huma.Error401Unauthorized("unauthenticated")
	}
	tgt := audit.Target{Kind: "user", ID: id.User.ID}

	if err := requireElevated(ctx); err != nil {
		s.auditor.Record(ctx, audit.ActionSSHKeyAdd, tgt, nil, false)
		return nil, err
	}

	line, fingerprint, err := parseSSHPublicKey(in.Body.PublicKey)
	if err != nil {
		// A rejected paste is a validation 422, not an elevation-class failure, so
		// it does not audit (CLAUDE.md: validation 422s don't audit).
		return nil, err
	}

	existing, err := s.store.ListSSHKeys(id.User.ID)
	if err != nil {
		s.auditor.Record(ctx, audit.ActionSSHKeyAdd, tgt, nil, false)
		return nil, huma.Error500InternalServerError("read ssh keys failed", err)
	}
	if len(existing) >= maxSSHKeysPerUser {
		// Also a guard rejection rather than a malformed request, so it audits.
		s.auditor.Record(ctx, audit.ActionSSHKeyAdd, tgt, nil, false)
		return nil, huma.Error422UnprocessableEntity(
			fmt.Sprintf("you can have at most %d SSH keys; remove one first", maxSSHKeysPerUser))
	}

	meta := map[string]any{"fingerprint": fingerprint}
	key := store.SSHKey{
		ID:          newID(),
		UserID:      id.User.ID,
		Label:       strings.TrimSpace(in.Body.Label),
		PublicKey:   line,
		Fingerprint: fingerprint,
	}
	if err := s.store.AddSSHKey(key); err != nil {
		if errors.Is(err, store.ErrDuplicateSSHKey) {
			s.auditor.Record(ctx, audit.ActionSSHKeyAdd, tgt, meta, false)
			return nil, huma.Error409Conflict("that key is already on your account")
		}
		s.auditor.Record(ctx, audit.ActionSSHKeyAdd, tgt, meta, false)
		return nil, huma.Error500InternalServerError("save ssh key failed", err)
	}

	// Push the new key set to the host only while SSH is on. Adding a key to a
	// disabled account changes nothing the host needs to know, and pushing it
	// would write an authorized_keys file for an account sshd is not admitting.
	if err := s.syncSSHIfEnabled(ctx, id.User.ID, id.User.Username); err != nil {
		if rbErr := s.store.DeleteSSHKey(id.User.ID, key.ID); rbErr != nil {
			slog.Error("ssh key rollback failed", "user_id", id.User.ID,
				"service", "ssh", "err", rbErr)
		}
		s.auditor.Record(ctx, audit.ActionSSHKeyAdd, tgt, meta, false)
		return nil, huma.Error502BadGateway("host-agent ssh set-access failed", err)
	}

	s.auditor.Record(ctx, audit.ActionSSHKeyAdd, tgt, meta, true)
	dto, err := s.sshAccessDTO(id.User.ID)
	if err != nil {
		return nil, err
	}
	return &struct{ Body SSHAccessDTO }{Body: dto}, nil
}

func (s *Server) deleteMySSHKey(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{}, error) {
	id, ok := auth.FromContext(ctx)
	if !ok {
		return nil, huma.Error401Unauthorized("unauthenticated")
	}
	tgt := audit.Target{Kind: "user", ID: id.User.ID}

	if err := requireElevated(ctx); err != nil {
		s.auditor.Record(ctx, audit.ActionSSHKeyDelete, tgt, nil, false)
		return nil, err
	}

	access, err := s.store.SSHAccessFor(id.User.ID)
	if err != nil {
		s.auditor.Record(ctx, audit.ActionSSHKeyDelete, tgt, nil, false)
		return nil, huma.Error500InternalServerError("read ssh access failed", err)
	}
	keys, err := s.store.ListSSHKeys(id.User.ID)
	if err != nil {
		s.auditor.Record(ctx, audit.ActionSSHKeyDelete, tgt, nil, false)
		return nil, huma.Error500InternalServerError("read ssh keys failed", err)
	}

	// Removing the last key while SSH is on would leave an enabled account with no
	// mandatory factor on hosted. Refuse rather than silently turning SSH off: the
	// user asked to remove a key, not to lose their access, and they may be about
	// to add a replacement.
	if access.Enabled && s.keyRequired() && len(keys) == 1 && keys[0].ID == in.ID {
		// A guard rejection, the same shape as the last-admin guard, so it audits
		// (CLAUDE.md # Elevation-class mutations). Not a plain validation 422.
		s.auditor.Record(ctx, audit.ActionSSHKeyDelete, tgt, nil, false)
		return nil, huma.Error422UnprocessableEntity(
			"this is your only SSH key; add another one or turn SSH off before removing it")
	}

	if err := s.store.DeleteSSHKey(id.User.ID, in.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Audited: an attempt to revoke a key that is not there is still an
			// attempted elevation-class mutation, and it is what probing another
			// account's key ids would look like from the Activity view.
			s.auditor.Record(ctx, audit.ActionSSHKeyDelete, tgt, nil, false)
			return nil, huma.Error404NotFound("key not found")
		}
		s.auditor.Record(ctx, audit.ActionSSHKeyDelete, tgt, nil, false)
		return nil, huma.Error500InternalServerError("delete ssh key failed", err)
	}
	if err := s.syncSSHIfEnabled(ctx, id.User.ID, id.User.Username); err != nil {
		// The row is already gone and the key is one the user asked to revoke, so
		// this does not roll back: leaving a revoked key live on the host is the
		// worse of the two failures. The reconciler re-pushes the set on its next
		// pass, and the audit record says the attempt failed.
		slog.Error("ssh key removed in brain but host push failed", "user_id", id.User.ID,
			"username", id.User.Username, "service", "ssh", "err", err)
		s.auditor.Record(ctx, audit.ActionSSHKeyDelete, tgt, nil, false)
		return nil, huma.Error502BadGateway("host-agent ssh set-access failed", err)
	}

	s.auditor.Record(ctx, audit.ActionSSHKeyDelete, tgt, nil, true)
	return nil, nil
}

// syncSSHIfEnabled pushes the account's current key set to the host, but only
// when the account has SSH turned on.
func (s *Server) syncSSHIfEnabled(ctx context.Context, userID, username string) error {
	access, err := s.store.SSHAccessFor(userID)
	if err != nil {
		return err
	}
	if !access.Enabled {
		return nil
	}
	keys, err := s.store.ListSSHKeys(userID)
	if err != nil {
		return err
	}
	return s.applySSH(ctx, username, access.Enabled, access.RequirePassword, keys)
}

// applySSH sends one account's full desired state to host-agent.
func (s *Server) applySSH(ctx context.Context, username string, enabled, requirePassword bool, keys []store.SSHKey) error {
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, k.PublicKey)
	}
	return s.host.SetSSHAccess(ctx, protocol.SetSSHAccessRequest{
		User:            username,
		Enabled:         enabled,
		AuthorizedKeys:  lines,
		RequirePassword: requirePassword,
	})
}

func (s *Server) sshAccessDTO(userID string) (SSHAccessDTO, error) {
	access, err := s.store.SSHAccessFor(userID)
	if err != nil {
		return SSHAccessDTO{}, huma.Error500InternalServerError("read ssh access failed", err)
	}
	keys, err := s.store.ListSSHKeys(userID)
	if err != nil {
		return SSHAccessDTO{}, huma.Error500InternalServerError("read ssh keys failed", err)
	}
	out := SSHAccessDTO{
		Enabled:         access.Enabled,
		RequirePassword: access.RequirePassword,
		KeyRequired:     s.keyRequired(),
		Keys:            []SSHKeyDTO{},
	}
	for _, k := range keys {
		out.Keys = append(out.Keys, SSHKeyDTO{
			ID:          k.ID,
			Label:       k.Label,
			PublicKey:   k.PublicKey,
			Fingerprint: k.Fingerprint,
			AddedAt:     k.AddedAt.Unix(),
		})
	}
	return out, nil
}

// parseSSHPublicKey validates a submitted key and returns the canonical
// authorized_keys line plus its SHA-256 fingerprint.
//
// The line is re-serialised from the parsed key rather than stored as typed,
// which drops anything the user pasted around it. That matters more than tidiness:
// authorized_keys accepts leading *options* (command=, from=, and others) that
// change what the key can do, and accepting them verbatim would let a paste
// carry behaviour nobody reviewed. Only the comment is kept, since a user names
// their keys by it.
func parseSSHPublicKey(in string) (line, fingerprint string, err error) {
	raw := strings.TrimSpace(in)
	if raw == "" {
		return "", "", huma.Error422UnprocessableEntity("paste or upload a public key")
	}
	if len(raw) > maxSSHKeyBytes {
		return "", "", huma.Error422UnprocessableEntity("that does not look like a public key; it is too long")
	}
	// The single most likely user error, and the one with the worst consequence,
	// so it gets its own message rather than a generic parse failure.
	if strings.Contains(raw, "PRIVATE KEY") {
		return "", "", huma.Error422UnprocessableEntity(
			"that is a private key; keep it secret and paste the matching .pub file instead")
	}
	// A pasted file can carry several lines. Take the first non-empty, non-comment
	// one rather than silently using the last, and say so if there are more.
	var candidate string
	for _, l := range strings.Split(raw, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		candidate = l
		break
	}
	if candidate == "" {
		return "", "", huma.Error422UnprocessableEntity("paste or upload a public key")
	}

	pub, comment, _, _, parseErr := ssh.ParseAuthorizedKey([]byte(candidate))
	if parseErr != nil {
		return "", "", huma.Error422UnprocessableEntity(
			"that does not look like an SSH public key; it should start with something like 'ssh-ed25519'")
	}

	line = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
	if comment != "" {
		line += " " + sanitizeKeyComment(comment)
	}
	sum := sha256.Sum256(pub.Marshal())
	fingerprint = "SHA256:" + strings.TrimRight(base64.StdEncoding.EncodeToString(sum[:]), "=")
	return line, fingerprint, nil
}

// sanitizeKeyComment strips anything that would break the one-key-per-line shape
// of authorized_keys. A newline in a comment would otherwise let a single
// submitted "key" write a second, unreviewed line into the file.
func sanitizeKeyComment(comment string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, comment)
}
