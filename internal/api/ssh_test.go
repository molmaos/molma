package api

import (
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/malmoos/malmo/internal/audit"
	"github.com/malmoos/malmo/internal/profile"
	"github.com/malmoos/malmo/internal/protocol"
	"github.com/malmoos/malmo/internal/store"
)

// sshFailUser makes the harness's /v1/ssh/set-access mock answer 500, so the
// host-502 and rollback paths are reachable.
const sshFailUser = "hostfail"

// A real key pair's public halves, generated once and pasted here. Two distinct
// keys so the duplicate and multi-key cases are exercised with material that
// actually parses.
const (
	testKeyA = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIfJnhGAA/rWbxmvMGuZvXV6in+czTK5F8Ie7QGTKOT+ alex@laptop"
	testKeyB = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHmywREXaNctQmxNs8UMGg8mSDO4MP1SfJnIhUAeEoY9 alex@desktop"
)

// hostedSSHHarness is a hosted-profile harness with a signed-in, elevated admin.
func hostedSSHHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, func(s *Server) { s.SetEnvironment(profile.Hosted, "cindy-fox", nil) })
	seedAdminSession(t, h)
	return h
}

// applianceSSHHarness is the same, on the default appliance profile.
func applianceSSHHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	seedAdminSession(t, h)
	return h
}

// seedAdminSession creates an admin directly in the store (so it works on hosted,
// where /setup is disabled), signs in, and elevates.
func seedAdminSession(t *testing.T, h *harness) {
	t.Helper()
	if err := h.st.CreateUser(store.User{
		ID: "u_alex", Username: "alex", Role: store.RoleAdmin,
	}); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	h.seedPassword("alex", "pass1")
	h.loginAs("alex", "pass1")
	h.elevate("pass1")
}

// seedPassword writes a bcrypt hash straight into the harness's fake host-agent,
// so an account created directly in the store can sign in. Needed because
// /setup — the usual way an admin gets a password — is disabled on hosted.
func (h *harness) seedPassword(username, password string) {
	h.t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		h.t.Fatalf("hash password: %v", err)
	}
	h.pmu.Lock()
	h.pwds[username] = hash
	h.pmu.Unlock()
}

func (h *harness) sshCallsSnapshot() []protocol.SetSSHAccessRequest {
	h.pmu.Lock()
	defer h.pmu.Unlock()
	out := make([]protocol.SetSSHAccessRequest, len(*h.sshCalls))
	copy(out, *h.sshCalls)
	return out
}

func (h *harness) addKey(t *testing.T, key string) SSHAccessDTO {
	t.Helper()
	resp := h.do("POST", "/api/v1/me/ssh/keys", map[string]string{"public_key": key})
	if resp.StatusCode != 200 {
		t.Fatalf("add key = %d", resp.StatusCode)
	}
	return decodeJSON[SSHAccessDTO](t, resp)
}

// On hosted a public key is the mandatory factor, enforced by the brain and not
// only by the UI. Enabling with no key would leave an account reachable by
// password alone on a port the open internet can reach.
func TestHostedEnableWithoutKeyIsRefused(t *testing.T) {
	h := hostedSSHHarness(t)

	resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{"enabled": true})
	defer resp.Body.Close()
	if resp.StatusCode != 422 {
		t.Fatalf("hosted enable with no key = %d; want 422", resp.StatusCode)
	}
	if calls := h.sshCallsSnapshot(); len(calls) != 0 {
		t.Fatalf("refused enable still reached the host: %+v", calls)
	}
	assertAudited(t, h, audit.ActionSSHAccessSet, false)
}

// The appliance keeps the password as its mandatory factor, so enabling with no
// key is allowed there. :22 is LAN- and mesh-scoped by nftables, which is the
// perimeter the one-password model was designed around.
func TestApplianceEnableWithoutKeyIsAllowed(t *testing.T) {
	h := applianceSSHHarness(t)

	resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{"enabled": true})
	if resp.StatusCode != 200 {
		t.Fatalf("appliance enable with no key = %d; want 200", resp.StatusCode)
	}
	body := decodeJSON[SSHAccessDTO](t, resp)
	if !body.Enabled || body.KeyRequired {
		t.Fatalf("appliance state = %+v; want enabled and key_required false", body)
	}
	calls := h.sshCallsSnapshot()
	if len(calls) != 1 || !calls[0].Enabled || calls[0].User != "alex" {
		t.Fatalf("host call = %+v; want one enable for alex", calls)
	}
}

// The optional password rides to the host as require_password, which is what
// makes it a second required method rather than an alternative one.
func TestRequirePasswordReachesTheHost(t *testing.T) {
	h := hostedSSHHarness(t)
	h.addKey(t, testKeyA)

	resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{"enabled": true, "require_password": true})
	if resp.StatusCode != 200 {
		t.Fatalf("enable = %d", resp.StatusCode)
	}
	resp.Body.Close()

	calls := h.sshCallsSnapshot()
	last := calls[len(calls)-1]
	if !last.RequirePassword || len(last.AuthorizedKeys) != 1 {
		t.Fatalf("host call = %+v; want require_password with one key", last)
	}
}

// A private key is the worst paste a user can make, so it gets its own message
// and is never stored.
func TestPrivateKeyPasteIsRefused(t *testing.T) {
	h := hostedSSHHarness(t)

	resp := h.do("POST", "/api/v1/me/ssh/keys", map[string]string{
		"public_key": "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEA\n-----END OPENSSH PRIVATE KEY-----",
	})
	defer resp.Body.Close()
	if resp.StatusCode != 422 {
		t.Fatalf("private key paste = %d; want 422", resp.StatusCode)
	}
	keys, err := h.st.ListSSHKeys("u_alex")
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("a private key was stored: %+v", keys)
	}
}

// authorized_keys options (command=, from=, …) change what a key can do. The
// stored line is re-serialised from the parsed key, so a paste cannot smuggle
// them in.
func TestAuthorizedKeysOptionsAreStripped(t *testing.T) {
	h := hostedSSHHarness(t)

	body := h.addKey(t, `command="/bin/sh",no-pty `+testKeyA)
	if len(body.Keys) != 1 {
		t.Fatalf("keys = %d; want 1", len(body.Keys))
	}
	stored := body.Keys[0].PublicKey
	if strings.Contains(stored, "command=") || strings.Contains(stored, "no-pty") {
		t.Fatalf("stored line kept authorized_keys options: %q", stored)
	}
	if !strings.HasPrefix(stored, "ssh-ed25519 ") {
		t.Fatalf("stored line = %q; want a bare key line", stored)
	}
}

// The same key twice is a 409, not a silent success: the user should learn the
// key was already there rather than wonder which one is live.
func TestDuplicateKeyIsConflict(t *testing.T) {
	h := hostedSSHHarness(t)
	h.addKey(t, testKeyA)

	resp := h.do("POST", "/api/v1/me/ssh/keys", map[string]string{"public_key": testKeyA})
	defer resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("duplicate key = %d; want 409", resp.StatusCode)
	}
}

// Adding a key while SSH is off changes nothing the host needs to know, so it
// must not push an authorized_keys file for an account sshd is not admitting.
func TestKeyAddDoesNotTouchHostWhileDisabled(t *testing.T) {
	h := hostedSSHHarness(t)
	h.addKey(t, testKeyA)

	if calls := h.sshCallsSnapshot(); len(calls) != 0 {
		t.Fatalf("key add on a disabled account reached the host: %+v", calls)
	}
}

// Removing the last key on hosted while SSH is on would leave an enabled account
// with no mandatory factor. Refuse, rather than silently turning SSH off.
func TestHostedLastKeyCannotBeRemovedWhileEnabled(t *testing.T) {
	h := hostedSSHHarness(t)
	body := h.addKey(t, testKeyA)
	keyID := body.Keys[0].ID

	if resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{"enabled": true}); resp.StatusCode != 200 {
		t.Fatalf("enable = %d", resp.StatusCode)
	}

	resp := h.do("DELETE", "/api/v1/me/ssh/keys/"+keyID, nil)
	defer resp.Body.Close()
	if resp.StatusCode != 422 {
		t.Fatalf("removing the only key = %d; want 422", resp.StatusCode)
	}
	keys, _ := h.st.ListSSHKeys("u_alex")
	if len(keys) != 1 {
		t.Fatalf("key was removed anyway: %+v", keys)
	}
}

// A host failure rolls the brain row back, so the two sides cannot disagree
// about who has a shell (CLAUDE.md # Brain commits first).
func TestHostFailureRollsBackTheAccessRow(t *testing.T) {
	h := newHarness(t)
	if err := h.st.CreateUser(store.User{ID: "u_fail", Username: sshFailUser, Role: store.RoleAdmin}); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	h.seedPassword(sshFailUser, "pass1")
	h.loginAs(sshFailUser, "pass1")
	h.elevate("pass1")

	resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{"enabled": true})
	defer resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Fatalf("host failure = %d; want 502", resp.StatusCode)
	}
	access, err := h.st.SSHAccessFor("u_fail")
	if err != nil {
		t.Fatalf("read access: %v", err)
	}
	if access.Enabled {
		t.Fatal("brain row stayed enabled after the host refused")
	}
	assertAudited(t, h, audit.ActionSSHAccessSet, false)
}

// Every write here is elevation-class, and a rejection audits so the Activity
// view can answer "did someone try to open a shell into this box?".
func TestSSHWritesRequireElevation(t *testing.T) {
	h := newHarness(t)
	if err := h.st.CreateUser(store.User{ID: "u_alex", Username: "alex", Role: store.RoleAdmin}); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	h.seedPassword("alex", "pass1")
	h.loginAs("alex", "pass1") // signed in, deliberately not elevated

	// Bodies are valid on purpose: huma validates the schema before the handler
	// runs, so an empty body would 422 and never reach the elevation gate this
	// test is about.
	for _, c := range []struct {
		method, path string
		body         map[string]any
	}{
		{"PUT", "/api/v1/me/ssh", map[string]any{"enabled": true}},
		{"POST", "/api/v1/me/ssh/keys", map[string]any{"public_key": testKeyA}},
		{"DELETE", "/api/v1/me/ssh/keys/whatever", nil},
	} {
		resp := h.do(c.method, c.path, c.body)
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Fatalf("%s %s unelevated = %d; want 403", c.method, c.path, resp.StatusCode)
		}
	}
	assertAudited(t, h, audit.ActionSSHAccessSet, false)
	assertAudited(t, h, audit.ActionSSHKeyAdd, false)
	assertAudited(t, h, audit.ActionSSHKeyDelete, false)
}

// The panel state tells the UI which factor this profile makes mandatory, so the
// two cannot drift.
func TestKeyRequiredReflectsTheProfile(t *testing.T) {
	hosted := hostedSSHHarness(t)
	resp := hosted.do("GET", "/api/v1/me/ssh", nil)
	if got := decodeJSON[SSHAccessDTO](t, resp); !got.KeyRequired {
		t.Fatalf("hosted key_required = false; want true")
	}

	appliance := applianceSSHHarness(t)
	resp = appliance.do("GET", "/api/v1/me/ssh", nil)
	if got := decodeJSON[SSHAccessDTO](t, resp); got.KeyRequired {
		t.Fatalf("appliance key_required = true; want false")
	}
}

func assertAudited(t *testing.T, h *harness, action string, success bool) {
	t.Helper()
	events, err := h.st.ListAuditEvents(store.AuditFilter{Limit: 100})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	for _, e := range events {
		if e.Action == action && e.Success == success {
			return
		}
	}
	t.Fatalf("no %s audit event with success=%v", action, success)
}

// A failed elevation-class delete leaves a trace, including one against an id
// that is not there — which is what probing another account's key ids would look
// like from the Activity view.
func TestFailedKeyDeleteIsAudited(t *testing.T) {
	h := hostedSSHHarness(t)

	resp := h.do("DELETE", "/api/v1/me/ssh/keys/nope", nil)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("delete unknown key = %d; want 404", resp.StatusCode)
	}
	assertAudited(t, h, audit.ActionSSHKeyDelete, false)
}

// The last-key guard is a guard rejection in the CLAUDE.md sense, the same shape
// as the last-admin guard, so it audits rather than passing silently as a plain
// validation failure.
func TestLastKeyGuardIsAudited(t *testing.T) {
	h := hostedSSHHarness(t)
	body := h.addKey(t, testKeyA)
	if resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{"enabled": true}); resp.StatusCode != 200 {
		t.Fatalf("enable = %d", resp.StatusCode)
	}

	resp := h.do("DELETE", "/api/v1/me/ssh/keys/"+body.Keys[0].ID, nil)
	resp.Body.Close()
	if resp.StatusCode != 422 {
		t.Fatalf("removing the only key = %d; want 422", resp.StatusCode)
	}
	assertAudited(t, h, audit.ActionSSHKeyDelete, false)
}

// A second key makes the first removable, which is the escape hatch the guard
// leaves open.
func TestSecondKeyMakesTheFirstRemovable(t *testing.T) {
	h := hostedSSHHarness(t)
	first := h.addKey(t, testKeyA)
	h.addKey(t, testKeyB)
	if resp := h.do("PUT", "/api/v1/me/ssh", map[string]any{"enabled": true}); resp.StatusCode != 200 {
		t.Fatalf("enable = %d", resp.StatusCode)
	}

	resp := h.do("DELETE", "/api/v1/me/ssh/keys/"+first.Keys[0].ID, nil)
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("delete with a spare key = %d; want 204", resp.StatusCode)
	}
	calls := h.sshCallsSnapshot()
	last := calls[len(calls)-1]
	if len(last.AuthorizedKeys) != 1 {
		t.Fatalf("host got %d keys after the revoke; want 1", len(last.AuthorizedKeys))
	}
}
