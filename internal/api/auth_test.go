package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/malmoos/malmo/internal/audit"
	"github.com/malmoos/malmo/internal/auth"
	"github.com/malmoos/malmo/internal/catalog"
	"github.com/malmoos/malmo/internal/events"
	"github.com/malmoos/malmo/internal/hostclient"
	"github.com/malmoos/malmo/internal/lifecycle"
	"github.com/malmoos/malmo/internal/protocol"
	"github.com/malmoos/malmo/internal/store"
	"github.com/malmoos/malmo/internal/systemlive"
	"golang.org/x/crypto/bcrypt"
)

// harness wires a real api.Server to a real store and a real (in-memory)
// host-agent stand-in over a unix socket. We deliberately don't mock the
// store or hostclient — those have their own tests, and the value of these
// tests is exercising the full HTTP boundary (CORS → auth middleware →
// huma handler → store + host) end-to-end.

// harnessFreeBytes is the canned data-drive free figure the harness host-agent
// reports on GET /v1/system/status, so install-plan footprint tests can assert
// free_bytes flows through (≈412 GiB).
const harnessFreeBytes = 412 << 30

// harnessAgentVersion is the version the mocked host-agent self-reports on
// GET /v1/system/status, so TestSystemVersion_* can assert it reaches the
// caller rather than being silently dropped. Deliberately not the brain's own
// version: a test that used the same string either side could pass while the
// handler reported the brain's version twice.
const harnessAgentVersion = "9.9.9-agent"

type harness struct {
	srv  *httptest.Server
	jar  http.CookieJar // shared cookie jar across helpers
	t    *testing.T
	pwds map[string][]byte
	pmu  *sync.Mutex
	st   *store.Store
	// deleteCalls records every username passed to the host-agent
	// /v1/auth/delete-user mock. Guarded by pmu. Lets rollback tests assert
	// that the brain called host.DeleteUser on a failed mutation path
	// (closes the 0015/0016 orphan-on-rollback gap; see
	// docs/progress/0017-host-agent-delete-user.md).
	deleteCalls *[]string
	// tzCalls records every zone passed to the host-agent
	// /v1/system/set-timezone mock. Guarded by pmu. Lets first-run tests assert
	// the zone reached the host. The sentinel tzFailSentinel makes the mock
	// return 500 so the brain's host-502 path is reachable.
	tzCalls *[]string
	// sshCalls records every /v1/ssh/set-access request the brain made. Guarded
	// by pmu. Lets the Device access tests assert what actually reached the host —
	// the auth methods and the key set — rather than only what the brain stored.
	// A request for sshFailUser makes the mock answer 500, so the host-502 and
	// rollback paths are reachable.
	sshCalls *[]protocol.SetSSHAccessRequest
	apiSrv   *Server // the underlying api.Server, for direct method tests
	// catalogDir is the root the harness's catalog reads from. catalog.Load
	// hits the filesystem on each call, so tests write manifest fixtures into
	// this dir *after* construction and the live server picks them up — no need
	// to swap the catalog on the already-listening server.
	catalogDir string
	// stateDir is the lifecycle Manager's state root, so a test can write an
	// instance's persisted manifest at <stateDir>/instances/<id>/manifest.yml —
	// the copy the brain reads for an app that is already installed (#434).
	stateDir string
}

// srvServer exposes the underlying *Server for tests that exercise handler
// helpers directly (resolveOwnerScope, checkDuplicate) instead of over HTTP.
func (h *harness) srvServer() *Server { return h.apiSrv }

// newHarness builds the in-process auth test stack. Optional opts run against
// the *Server after construction but before it starts listening, so they can
// set startup-time state (e.g. SetEnvironment for the hosted /setup gate)
// without racing the request goroutines.
func newHarness(t *testing.T, opts ...func(*Server)) *harness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Spin up an in-process host-agent on a unix socket implementing only
	// the auth surface. We don't need discovery for auth tests.
	pwds := map[string][]byte{}
	var pmu sync.Mutex
	sock := filepath.Join(t.TempDir(), "agent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := http.NewServeMux()
	roles := map[string]string{}
	mux.HandleFunc("POST /v1/auth/set-password", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.SetPasswordRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		h, _ := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.MinCost)
		pmu.Lock()
		pwds[req.User] = h
		pmu.Unlock()
		_ = json.NewEncoder(w).Encode(struct{}{})
	})
	mux.HandleFunc("POST /v1/auth/verify-password", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.VerifyPasswordRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		pmu.Lock()
		h, ok := pwds[req.User]
		pmu.Unlock()
		valid := ok && bcrypt.CompareHashAndPassword(h, []byte(req.Password)) == nil
		_ = json.NewEncoder(w).Encode(protocol.VerifyPasswordResponse{Valid: valid})
	})
	mux.HandleFunc("POST /v1/auth/set-role", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.SetRoleRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		pmu.Lock()
		roles[req.User] = req.Role
		pmu.Unlock()
		_ = json.NewEncoder(w).Encode(struct{}{})
	})
	// tzFailSentinel is a syntactically-valid zone (passes the brain's tzRe) that
	// the mock answers with 500, so first-run tests can drive the host-502 path.
	tzCalls := []string{}
	mux.HandleFunc("POST /v1/system/set-timezone", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.SetTimezoneRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Zone == tzFailSentinel {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(protocol.Error{Code: "set-timezone-failed", Message: "boom"})
			return
		}
		pmu.Lock()
		tzCalls = append(tzCalls, req.Zone)
		pmu.Unlock()
		_ = json.NewEncoder(w).Encode(struct{}{})
	})
	sshCalls := []protocol.SetSSHAccessRequest{}
	mux.HandleFunc("POST /v1/ssh/set-access", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.SetSSHAccessRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.User == sshFailUser {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(protocol.Error{Code: "ssh-set-access-failed", Message: "boom"})
			return
		}
		pmu.Lock()
		sshCalls = append(sshCalls, req)
		pmu.Unlock()
		_ = json.NewEncoder(w).Encode(struct{}{})
	})
	deleteCalls := []string{}
	mux.HandleFunc("POST /v1/auth/delete-user", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.DeleteUserRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		pmu.Lock()
		delete(pwds, req.User)
		deleteCalls = append(deleteCalls, req.User)
		pmu.Unlock()
		_ = json.NewEncoder(w).Encode(struct{}{})
	})
	// Canned data-drive free/total so the install-plan footprint's free_bytes has
	// a stable figure to assert (harnessFreeBytes); Disks backs the Storage-bars
	// endpoint test (TestSystemStorage_*).
	mux.HandleFunc("GET /v1/system/status", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(protocol.SystemStatus{
			AgentVersion:       harnessAgentVersion,
			DataDiskFreeBytes:  harnessFreeBytes,
			DataDiskTotalBytes: 1 << 40,
			Disks: []protocol.DiskSpace{
				{Label: "System", FreeBytes: 18 << 30, TotalBytes: 64 << 30},
				{Label: "Data", FreeBytes: harnessFreeBytes, TotalBytes: 1 << 40},
			},
		})
	})
	hostHTTP := &http.Server{Handler: mux}
	go func() { _ = hostHTTP.Serve(ln) }()
	t.Cleanup(func() { _ = hostHTTP.Close() })

	host := hostclient.New(sock)
	catDir := t.TempDir()
	cat := catalog.New(catDir)
	bus := events.NewBus()
	authMgr := auth.NewManager(st)

	// A live-resources hub backed by a canned sampler; inert unless a stream
	// subscribes (only TestSystemLive_* does). hubCancel reaps any poll
	// goroutine at test end.
	hubCtx, hubCancel := context.WithCancel(context.Background())
	t.Cleanup(hubCancel)
	live := systemlive.New(hubCtx, &constSampler{}, 5*time.Millisecond)

	// A partial lifecycle.Manager (nil caddy/docker) so the install-plan handler
	// can call s.life.InstallFootprint. The install/uninstall *job* bodies are
	// still never run here — every install/uninstall test asserts a synchronous
	// boundary (auth fence, 422 pre-check, 404) that returns before the job — and
	// the install-plan fixtures declare no images, so the nil docker is never
	// reached. host (*hostclient.Client) satisfies HostDriver, including the new
	// SystemStatus the footprint reads.
	stateDir := t.TempDir()
	life := lifecycle.NewManager(st, cat, host, nil, nil, bus, stateDir)
	srv := NewServer(st, cat, life, bus, authMgr, host, audit.New(st), nil, live, nil)
	for _, opt := range opts {
		opt(srv)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	jar, _ := newJar()
	return &harness{srv: ts, jar: jar, t: t, pwds: pwds, pmu: &pmu, st: st, deleteCalls: &deleteCalls, tzCalls: &tzCalls, sshCalls: &sshCalls, apiSrv: srv, catalogDir: catDir, stateDir: stateDir}
}

func (h *harness) do(method, path string, body any) *http.Response {
	h.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rdr)
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, c := range h.jar.Cookies(req.URL) {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("do: %v", err)
	}
	if cs := resp.Cookies(); len(cs) > 0 {
		h.jar.SetCookies(req.URL, cs)
	}
	return resp
}

func decodeJSON[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	return v
}

func newJar() (http.CookieJar, error) {
	return cookiejar.New(nil)
}

// --- tests ---------------------------------------------------------------

func TestAuthStateProgression(t *testing.T) {
	h := newHarness(t)

	// Fresh box: has_users is false, no auth required.
	resp := h.do("GET", "/api/v1/auth/state", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("auth/state on fresh box: %d", resp.StatusCode)
	}
	body := decodeJSON[struct {
		HasUsers bool `json:"has_users"`
	}](t, resp)
	if body.HasUsers {
		t.Fatal("fresh box reported has_users=true")
	}

	// Setup the admin.
	resp = h.do("POST", "/api/v1/setup", map[string]string{
		"username": "andrei", "password": "hunter2",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("setup: %d", resp.StatusCode)
	}
	setupBody := decodeJSON[struct {
		User         UserDTO `json:"user"`
		RecoveryCode string  `json:"recovery_code"`
	}](t, resp)
	if setupBody.User.Username != "andrei" || setupBody.User.Role != store.RoleAdmin {
		t.Fatalf("setup user = %+v", setupBody.User)
	}
	if len(setupBody.RecoveryCode) != 24 {
		t.Fatalf("recovery code len = %d; want 24", len(setupBody.RecoveryCode))
	}

	// After setup, the same endpoint should refuse with 409.
	resp = h.do("POST", "/api/v1/setup", map[string]string{
		"username": "cindy", "password": "doesntmatter",
	})
	if resp.StatusCode != 409 {
		t.Fatalf("second setup = %d; want 409", resp.StatusCode)
	}

	// auth/state now reports true, still public.
	resp = h.do("GET", "/api/v1/auth/state", nil)
	body = decodeJSON[struct {
		HasUsers bool `json:"has_users"`
	}](t, resp)
	if !body.HasUsers {
		t.Fatal("after setup, has_users=false")
	}
}

func TestProtectedRoutesRequireSession(t *testing.T) {
	h := newHarness(t)
	// No setup, no session: protected route 401s.
	resp := h.do("GET", "/api/v1/apps", nil)
	if resp.StatusCode != 401 {
		t.Fatalf("apps unauthenticated = %d; want 401", resp.StatusCode)
	}
	resp = h.do("GET", "/api/v1/me", nil)
	if resp.StatusCode != 401 {
		t.Fatalf("me unauthenticated = %d; want 401", resp.StatusCode)
	}
}

// TestAuthUsersPublicPicker covers GET /api/v1/auth/users — the public login
// picker source (AUTH.md # Login screen UX). It must be reachable with no
// session and expose only {id, username}: leaking role/hash here would hand an
// unauthenticated visitor the admin roster.
func TestAuthUsersPublicPicker(t *testing.T) {
	h := newHarness(t)
	h.do("POST", "/api/v1/setup", map[string]string{
		"username": "alice", "password": "hunter2",
	}).Body.Close()
	h.addMember("u_bob", "bob", "bobpass")

	// Drop the session minted by setup — the picker is consumed pre-login.
	jar, _ := newJar()
	h.jar = jar

	resp := h.do("GET", "/api/v1/auth/users", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("auth/users unauthenticated = %d; want 200 (public)", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var body struct {
		Users []struct {
			ID       string `json:"id"`
			Username string `json:"username"`
			Role     string `json:"role"` // must stay empty — not serialized
		} `json:"users"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Users) != 2 {
		t.Fatalf("picker users = %d; want 2", len(body.Users))
	}
	for _, u := range body.Users {
		if u.ID == "" || u.Username == "" {
			t.Fatalf("picker user missing id/username: %+v", u)
		}
		if u.Role != "" {
			t.Fatalf("picker leaked role %q for %s", u.Role, u.Username)
		}
	}
	// Belt and suspenders: the raw payload must not carry a role key at all.
	if bytes.Contains(raw, []byte("\"role\"")) {
		t.Fatalf("auth/users payload leaked a role field: %s", raw)
	}
}

func TestLoginLogoutFlow(t *testing.T) {
	h := newHarness(t)
	// Bootstrap admin so we have credentials to log in with.
	resp := h.do("POST", "/api/v1/setup", map[string]string{
		"username": "andrei", "password": "hunter2",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("setup: %d", resp.StatusCode)
	}
	resp.Body.Close()
	// /v1/setup also issues a session, so we're already logged in. /me works.
	resp = h.do("GET", "/api/v1/me", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("me after setup = %d", resp.StatusCode)
	}
	me := decodeJSON[UserDTO](t, resp)
	if me.Username != "andrei" {
		t.Fatalf("me = %+v", me)
	}

	// Logout clears the session. huma returns 204 (no body) which is fine.
	resp = h.do("POST", "/api/v1/logout", nil)
	if resp.StatusCode != 200 && resp.StatusCode != 204 {
		t.Fatalf("logout = %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = h.do("GET", "/api/v1/me", nil)
	if resp.StatusCode != 401 {
		t.Fatalf("me after logout = %d; want 401", resp.StatusCode)
	}

	// Wrong password -> 401, no session minted.
	resp = h.do("POST", "/api/v1/login", map[string]string{
		"username": "andrei", "password": "wrong",
	})
	if resp.StatusCode != 401 {
		t.Fatalf("bad login = %d; want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Unknown user -> 401 (no leakage of which usernames exist).
	resp = h.do("POST", "/api/v1/login", map[string]string{
		"username": "ghost", "password": "anything",
	})
	if resp.StatusCode != 401 {
		t.Fatalf("ghost login = %d; want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Correct password -> 200, session minted, /me works again.
	resp = h.do("POST", "/api/v1/login", map[string]string{
		"username": "andrei", "password": "hunter2",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("login = %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = h.do("GET", "/api/v1/me", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("me after relogin = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestLoginLockoutAfterRepeatedFailures drives 20 wrong-password logins to the
// 15-minute lock and proves (a) the throttle gates BEFORE the PAM round-trip —
// a 21st attempt with the CORRECT password is still 429'd — and (b) exactly one
// login.lockout audit row is emitted at the crossing (AUTH.md # Rate limiting).
func TestLoginLockoutAfterRepeatedFailures(t *testing.T) {
	h := newHarness(t)
	resp := h.do("POST", "/api/v1/setup", map[string]string{
		"username": "andrei", "password": "hunter2",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("setup: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Crank the throttle clock so each wrong attempt clears the previous backoff
	// tier and actually reaches PAM (otherwise attempts 4+ are 429'd before they
	// count, and the account never accrues 20 real failures). The session minted
	// by /setup rides the auth.Manager's independent real clock, so it stays
	// valid for the audit read below.
	cur := time.Unix(1_700_000_000, 0)
	h.srvServer().throttle.Clock = func() time.Time { return cur }

	for i := 0; i < 20; i++ {
		cur = cur.Add(16 * time.Minute) // past the 60s max pre-lock cooldown
		r := h.do("POST", "/api/v1/login", map[string]string{
			"username": "andrei", "password": "wrong",
		})
		if r.StatusCode != 401 {
			t.Fatalf("wrong-login #%d = %d; want 401", i+1, r.StatusCode)
		}
		r.Body.Close()
	}

	// Locked now. Correct password, but still rejected with 429 — only possible
	// if the gate precedes VerifyPassword.
	cur = cur.Add(time.Second)
	r := h.do("POST", "/api/v1/login", map[string]string{
		"username": "andrei", "password": "hunter2",
	})
	if r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("locked correct-password login = %d; want 429", r.StatusCode)
	}
	r.Body.Close()

	// Exactly one login.lockout row, readable by the admin session from /setup.
	resp = h.do("GET", "/api/v1/audit?limit=200", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("audit list = %d", resp.StatusCode)
	}
	got := decodeJSON[struct {
		Events []AuditEventDTO `json:"events"`
	}](t, resp)
	lockouts := 0
	for _, e := range got.Events {
		if e.Action == audit.ActionLoginLockout {
			lockouts++
		}
	}
	if lockouts != 1 {
		t.Fatalf("login.lockout count = %d; want 1", lockouts)
	}
}

func TestSetupRollsBackOnHostFailure(t *testing.T) {
	h := newHarness(t)
	// Replace the host-agent stand-in's handler with one that always 500s
	// for set-password — exercises the rollback path. Easiest way: shut
	// down the existing harness host server and assert that setup fails and
	// the user row was rolled back. We can do that by killing one entry in
	// the password map's underlying mutex … or, simpler: point the brain
	// at a dead socket via a NEW harness with no agent.
	// Approach: replace via a tighter harness. Inline below.
	st, err := store.Open(filepath.Join(t.TempDir(), "rb.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// host-agent that 500s set-password.
	sock := filepath.Join(t.TempDir(), "agent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/set-password", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"code":"boom","message":"nope"}`, 500)
	})
	var delCalls []string
	var delMu sync.Mutex
	mux.HandleFunc("POST /v1/auth/delete-user", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.DeleteUserRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		delMu.Lock()
		delCalls = append(delCalls, req.User)
		delMu.Unlock()
		_ = json.NewEncoder(w).Encode(struct{}{})
	})
	hostHTTP := &http.Server{Handler: mux}
	go func() { _ = hostHTTP.Serve(ln) }()
	t.Cleanup(func() { _ = hostHTTP.Close() })

	srv := NewServer(st, catalog.New(t.TempDir()), nil, events.NewBus(),
		auth.NewManager(st), hostclient.New(sock), audit.New(st), nil, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/api/v1/setup", "application/json",
		bytes.NewReader([]byte(`{"username":"andrei","password":"hunter2"}`)))
	if err != nil {
		t.Fatalf("setup post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Fatalf("setup with broken host = %d; want 502", resp.StatusCode)
	}
	// Row must have been rolled back: HasAnyUser is false again.
	if has, _ := st.HasAnyUser(); has {
		t.Fatal("user row survived host failure; rollback broken")
	}
	// Best-effort host cleanup must have fired: covers the sliver where
	// useradd succeeded but chpasswd failed inside UpsertPassword. See
	// docs/progress/0017-host-agent-delete-user.md.
	delMu.Lock()
	got := append([]string(nil), delCalls...)
	delMu.Unlock()
	if len(got) != 1 || got[0] != "andrei" {
		t.Fatalf("rollback did not call host.DeleteUser(%q) exactly once; got %v", "andrei", got)
	}

	events, _ := st.ListAuditEvents(store.AuditFilter{Limit: 10})
	var found bool
	for _, e := range events {
		if e.Action == "setup.failure" && !e.Success {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("setup.failure audit event not recorded on set-password failure")
	}
	_ = h
}

// TestSetupRollsBackOnSetRoleFailure: SetPassword succeeds, SetRole 500s.
// /setup must roll the brain row back so the bootstrap can be retried.
// USERS_AND_GROUPS.md:32 — first admin is added to sudo at account creation;
// if that step fails the user shouldn't survive in a half-bootstrapped state.
func TestSetupRollsBackOnSetRoleFailure(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "rb.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	sock := filepath.Join(t.TempDir(), "agent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/set-password", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(struct{}{})
	})
	mux.HandleFunc("POST /v1/auth/set-role", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"code":"boom","message":"nope"}`, 500)
	})
	var delCalls []string
	var delMu sync.Mutex
	mux.HandleFunc("POST /v1/auth/delete-user", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.DeleteUserRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		delMu.Lock()
		delCalls = append(delCalls, req.User)
		delMu.Unlock()
		_ = json.NewEncoder(w).Encode(struct{}{})
	})
	hostHTTP := &http.Server{Handler: mux}
	go func() { _ = hostHTTP.Serve(ln) }()
	t.Cleanup(func() { _ = hostHTTP.Close() })

	srv := NewServer(st, catalog.New(t.TempDir()), nil, events.NewBus(),
		auth.NewManager(st), hostclient.New(sock), audit.New(st), nil, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/api/v1/setup", "application/json",
		bytes.NewReader([]byte(`{"username":"andrei","password":"hunter2"}`)))
	if err != nil {
		t.Fatalf("setup post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Fatalf("setup with broken set-role = %d; want 502", resp.StatusCode)
	}
	if has, _ := st.HasAnyUser(); has {
		t.Fatal("user row survived set-role failure; rollback broken")
	}
	// Best-effort host cleanup: SetPassword already created the Linux
	// account, so without this call the user would be orphaned on the host
	// (docs/progress/0017-host-agent-delete-user.md).
	delMu.Lock()
	got := append([]string(nil), delCalls...)
	delMu.Unlock()
	if len(got) != 1 || got[0] != "andrei" {
		t.Fatalf("rollback did not call host.DeleteUser(%q) exactly once; got %v", "andrei", got)
	}

	// Failure must be auditable per CLAUDE.md elevation-class rule.
	events, _ := st.ListAuditEvents(store.AuditFilter{Limit: 10})
	var found bool
	for _, e := range events {
		if e.Action == "setup.failure" && !e.Success {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("setup.failure audit event not recorded")
	}
}

func TestListAuditAdminSeesAll(t *testing.T) {
	h := newHarness(t)
	// Setup creates the first admin (records setup.complete) and leaves
	// a valid session cookie in h.jar.
	resp := h.do("POST", "/api/v1/setup", map[string]string{
		"username": "alice", "password": "hunter2",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("setup: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Login records login.success.
	resp = h.do("POST", "/api/v1/login", map[string]string{
		"username": "alice", "password": "hunter2",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = h.do("GET", "/api/v1/audit", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/v1/audit = %d; want 200", resp.StatusCode)
	}
	body := decodeJSON[struct {
		Events []AuditEventDTO `json:"events"`
	}](t, resp)
	if len(body.Events) < 2 {
		t.Fatalf("admin audit: got %d events, want >= 2", len(body.Events))
	}
	// Newest first — last login.success should appear before setup.complete.
	if body.Events[0].Action != "login.success" {
		t.Errorf("first event action = %q, want login.success", body.Events[0].Action)
	}
}

func TestListAuditRequiresAuth(t *testing.T) {
	h := newHarness(t)
	// Use the raw client (no cookie jar) to verify 401.
	resp, err := http.Get(h.srv.URL + "/api/v1/audit")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("unauthenticated audit = %d; want 401", resp.StatusCode)
	}
}

// elevate calls POST /api/v1/auth/elevate with the given password, asserting
// success. Call after setupAdmin or loginAs to enter the elevation window.
func (h *harness) elevate(password string) {
	h.t.Helper()
	resp := h.do("POST", "/api/v1/auth/elevate", map[string]string{"password": password})
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		h.t.Fatalf("elevate: %d %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

// addMember writes a member user directly into the store and seeds the fake
// host-agent's password map. There's no create-user API yet (Tier-A item 2),
// so tests that need a second user bypass the wire.
func (h *harness) addMember(id, username, password string) {
	h.t.Helper()
	if err := h.st.CreateUser(store.User{
		ID: id, Username: username, Role: store.RoleMember, CreatedAt: time.Now(),
	}); err != nil {
		h.t.Fatalf("create member: %v", err)
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	h.pmu.Lock()
	h.pwds[username] = hash
	h.pmu.Unlock()
}

func TestListAuditMemberVisibility(t *testing.T) {
	h := newHarness(t)

	// Admin bootstrap records setup.complete (target = admin user).
	resp := h.do("POST", "/api/v1/setup", map[string]string{
		"username": "alice", "password": "hunter2",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("setup: %d", resp.StatusCode)
	}
	resp.Body.Close()

	alice, err := h.st.GetUserByUsername("alice")
	if err != nil {
		t.Fatalf("lookup alice: %v", err)
	}
	// Admin installs an app — synthesized directly since the lifecycle is nil
	// in the harness. Bob must not see this row (actor=alice, target=app).
	if err := h.st.InsertAuditEvent(store.AuditEvent{
		TS: time.Now().UnixMilli(), ActorUserID: alice.ID, ActorRole: "admin",
		Action: "app.install", TargetKind: "app", TargetID: "inst_xyz", Success: true,
	}); err != nil {
		t.Fatalf("seed admin row: %v", err)
	}

	const bobID = "u_bob"
	h.addMember(bobID, "bob", "bobpass")

	// Switch sessions: logout admin, login as bob (records login.success
	// with actor=bob, target=user:bob).
	h.do("POST", "/api/v1/logout", nil).Body.Close()
	resp = h.do("POST", "/api/v1/login", map[string]string{
		"username": "bob", "password": "bobpass",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("bob login: %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = h.do("GET", "/api/v1/audit", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("bob GET /audit: %d", resp.StatusCode)
	}
	body := decodeJSON[struct {
		Events []AuditEventDTO `json:"events"`
	}](t, resp)

	if len(body.Events) == 0 {
		t.Fatal("bob should see at least their own login.success")
	}
	for _, e := range body.Events {
		actorIsBob := e.ActorUserID == bobID
		targetIsBob := e.TargetKind == "user" && e.TargetID == bobID
		if !actorIsBob && !targetIsBob {
			t.Errorf("bob sees unrelated event: action=%s actor=%q target=%s/%s",
				e.Action, e.ActorUserID, e.TargetKind, e.TargetID)
		}
	}
}

// --- recover tests -------------------------------------------------------

// setupAdminWithCode bootstraps an admin and returns the recovery code.
func setupAdminWithCode(t *testing.T, h *harness, username, password string) string {
	t.Helper()
	resp := h.do("POST", "/api/v1/setup", map[string]string{
		"username": username, "password": password,
	})
	if resp.StatusCode != 200 {
		t.Fatalf("setup: %d", resp.StatusCode)
	}
	body := decodeJSON[struct {
		User         UserDTO `json:"user"`
		RecoveryCode string  `json:"recovery_code"`
	}](t, resp)
	return body.RecoveryCode
}

func TestRecoverHappyPath(t *testing.T) {
	h := newHarness(t)
	code := setupAdminWithCode(t, h, "andrei", "oldpass")

	// Log out so we start without a session.
	h.do("POST", "/api/v1/logout", nil).Body.Close()

	resp := h.do("POST", "/api/v1/recover", map[string]string{
		"username":      "andrei",
		"recovery_code": code,
		"new_password":  "newpass99",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("recover: %d", resp.StatusCode)
	}
	body := decodeJSON[struct {
		NewRecoveryCode string `json:"new_recovery_code"`
	}](t, resp)

	// New recovery code must be a fresh 24-char hex string, distinct from old.
	if len(body.NewRecoveryCode) != 24 {
		t.Fatalf("new recovery code len = %d; want 24", len(body.NewRecoveryCode))
	}
	if body.NewRecoveryCode == code {
		t.Fatal("new recovery code is same as old — rotation did not happen")
	}

	// No session should have been issued; /me must 401.
	resp = h.do("GET", "/api/v1/me", nil)
	if resp.StatusCode != 401 {
		t.Fatalf("me after recover = %d; want 401 (no session issued)", resp.StatusCode)
	}

	// Old password must no longer work.
	resp = h.do("POST", "/api/v1/login", map[string]string{
		"username": "andrei", "password": "oldpass",
	})
	if resp.StatusCode != 401 {
		t.Fatalf("old password still works after recovery = %d; want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// New password must work.
	resp = h.do("POST", "/api/v1/login", map[string]string{
		"username": "andrei", "password": "newpass99",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("new password login = %d; want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// Old recovery code must be rejected now.
	resp = h.do("POST", "/api/v1/recover", map[string]string{
		"username":      "andrei",
		"recovery_code": code,
		"new_password":  "anotherpass",
	})
	if resp.StatusCode != 401 {
		t.Fatalf("old code after rotation = %d; want 401", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestRecoverWrongCode(t *testing.T) {
	h := newHarness(t)
	setupAdminWithCode(t, h, "andrei", "oldpass")

	resp := h.do("POST", "/api/v1/recover", map[string]string{
		"username":      "andrei",
		"recovery_code": "000000000000000000000000", // wrong but valid length
		"new_password":  "newpass99",
	})
	if resp.StatusCode != 401 {
		t.Fatalf("wrong code = %d; want 401", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestRecoverUnknownUser(t *testing.T) {
	h := newHarness(t)
	setupAdminWithCode(t, h, "andrei", "oldpass")

	// Unknown username should return 401, same as wrong code — no leakage.
	resp := h.do("POST", "/api/v1/recover", map[string]string{
		"username":      "ghost",
		"recovery_code": "000000000000000000000000",
		"new_password":  "newpass99",
	})
	if resp.StatusCode != 401 {
		t.Fatalf("unknown user = %d; want 401", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestRecoverMissingFields(t *testing.T) {
	h := newHarness(t)
	// No setup needed — 422 fires before any store lookup.

	cases := []map[string]string{
		{"username": "andrei", "recovery_code": "abc123"},      // missing new_password
		{"username": "andrei", "new_password": "newpass"},      // missing recovery_code
		{"recovery_code": "abc123", "new_password": "newpass"}, // missing username
		{}, // all missing
	}
	for _, body := range cases {
		resp := h.do("POST", "/api/v1/recover", body)
		if resp.StatusCode != 422 {
			t.Errorf("body %v: got %d; want 422", body, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestRecoverHostFailureRestoresOldHash(t *testing.T) {
	// Spin up a store + a broken host-agent (set-password always 500).
	st, err := store.Open(filepath.Join(t.TempDir(), "rec.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// First: bootstrap the admin using a working host-agent so the user row and
	// OS password exist. We do this with a temporary real harness.
	var savedCode string
	{
		h := newHarness(t)
		// We only need the harness's store for comparison; use a fresh store for
		// the broken-host harness below.
		savedCode = setupAdminWithCode(t, h, "andrei", "hunter2")
		// Copy the user from h.st into our dedicated st.
		u, lerr := h.st.GetUserByUsername("andrei")
		if lerr != nil {
			t.Fatalf("lookup andrei: %v", lerr)
		}
		if cerr := st.CreateFirstAdmin(u); cerr != nil {
			t.Fatalf("seed user: %v", cerr)
		}
	}

	// Broken host-agent: set-password always fails.
	sock := filepath.Join(t.TempDir(), "agent-bad.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/set-password", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"code":"boom","message":"nope"}`, 500)
	})
	hostHTTP := &http.Server{Handler: mux}
	go func() { _ = hostHTTP.Serve(ln) }()
	t.Cleanup(func() { _ = hostHTTP.Close() })

	srv := NewServer(st, nil, nil, nil, auth.NewManager(st), hostclient.New(sock), audit.New(st), nil, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, err2 := http.Post(ts.URL+"/api/v1/recover", "application/json",
		bytes.NewReader([]byte(`{"username":"andrei","recovery_code":"`+savedCode+`","new_password":"newpass"}`)))
	if err2 != nil {
		t.Fatalf("recover post: %v", err2)
	}
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Fatalf("broken host recover = %d; want 502", resp.StatusCode)
	}

	// The original recovery hash must have been restored — old code still valid.
	u, lerr := st.GetUserByUsername("andrei")
	if lerr != nil {
		t.Fatalf("lookup after failure: %v", lerr)
	}
	if bcrypt.CompareHashAndPassword([]byte(u.RecoveryHash), []byte(savedCode)) != nil {
		t.Fatal("old recovery hash was not restored after host failure")
	}
}

func TestRecoverSessionsAreRevoked(t *testing.T) {
	h := newHarness(t)
	code := setupAdminWithCode(t, h, "andrei", "oldpass")
	// setup leaves a session in the jar; /me should work.
	resp := h.do("GET", "/api/v1/me", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("me before recover = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Perform recovery using a separate fresh jar so our existing session
	// survives in h.jar. Then verify the old session cookie no longer works.
	recoverBody, _ := json.Marshal(map[string]string{
		"username":      "andrei",
		"recovery_code": code,
		"new_password":  "newpass99",
	})
	recReq, _ := http.NewRequest("POST", h.srv.URL+"/api/v1/recover", bytes.NewReader(recoverBody))
	recReq.Header.Set("Content-Type", "application/json")
	recResp, err := http.DefaultClient.Do(recReq)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	recResp.Body.Close()
	if recResp.StatusCode != 200 {
		t.Fatalf("recover = %d; want 200", recResp.StatusCode)
	}

	// The old session cookie in h.jar should now be revoked.
	resp = h.do("GET", "/api/v1/me", nil)
	if resp.StatusCode != 401 {
		t.Fatalf("me after session revocation = %d; want 401", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestRecoverAuditsFailureOnWrongCode(t *testing.T) {
	h := newHarness(t)
	setupAdminWithCode(t, h, "andrei", "oldpass")

	// Attempt recovery with wrong code.
	resp := h.do("POST", "/api/v1/recover", map[string]string{
		"username":      "andrei",
		"recovery_code": "000000000000000000000000",
		"new_password":  "newpass",
	})
	if resp.StatusCode != 401 {
		t.Fatalf("wrong code = %d; want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Admin must see a recover.failure audit row.
	resp = h.do("GET", "/api/v1/audit", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("audit = %d", resp.StatusCode)
	}
	body := decodeJSON[struct {
		Events []AuditEventDTO `json:"events"`
	}](t, resp)

	found := false
	for _, e := range body.Events {
		if e.Action == "recover.failure" {
			found = true
			if e.Success {
				t.Error("recover.failure event has success=true")
			}
		}
	}
	if !found {
		t.Error("no recover.failure audit row found")
	}
}

func TestListAuditLimitClamped(t *testing.T) {
	h := newHarness(t)
	resp := h.do("POST", "/api/v1/setup", map[string]string{
		"username": "alice", "password": "hunter2",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("setup: %d", resp.StatusCode)
	}
	resp.Body.Close()

	for i := 0; i < 250; i++ {
		if err := h.st.InsertAuditEvent(store.AuditEvent{
			TS: int64(i), ActorRole: "system", Action: "login.success", Success: true,
		}); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}

	resp = h.do("GET", "/api/v1/audit?limit=500", nil)
	defer resp.Body.Close()
	body := decodeJSON[struct {
		Events []AuditEventDTO `json:"events"`
	}](t, resp)
	if len(body.Events) != maxAuditLimit {
		t.Fatalf("limit=500 with 250+ rows returned %d events, want %d (clamped)",
			len(body.Events), maxAuditLimit)
	}
}

func TestListAuditCursorPagination(t *testing.T) {
	h := newHarness(t)
	resp := h.do("POST", "/api/v1/setup", map[string]string{
		"username": "alice", "password": "hunter2",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("setup: %d", resp.StatusCode)
	}
	resp.Body.Close()

	for i := 0; i < 5; i++ {
		_ = h.st.InsertAuditEvent(store.AuditEvent{
			TS: int64(i), ActorRole: "system", Action: "login.success", Success: true,
		})
	}

	resp = h.do("GET", "/api/v1/audit?limit=3", nil)
	page1 := decodeJSON[struct {
		Events []AuditEventDTO `json:"events"`
	}](t, resp)
	if len(page1.Events) != 3 {
		t.Fatalf("page 1: %d events, want 3", len(page1.Events))
	}
	cursor := page1.Events[2].ID

	resp = h.do("GET", fmt.Sprintf("/api/v1/audit?limit=3&after_id=%d", cursor), nil)
	page2 := decodeJSON[struct {
		Events []AuditEventDTO `json:"events"`
	}](t, resp)
	for _, e := range page2.Events {
		if e.ID >= cursor {
			t.Errorf("page 2 event id %d should be < cursor %d", e.ID, cursor)
		}
	}
	// setup.complete + 5 seeded = 6 admin-visible rows. Page 1 returns
	// ids 6,5,4 (cursor=4); page 2 returns ids 3,2,1.
	if len(page2.Events) != 3 {
		t.Fatalf("page 2: %d events, want 3", len(page2.Events))
	}
}

// --- Elevate endpoint tests -----------------------------------------------

func TestElevateHappyPath(t *testing.T) {
	h := newHarness(t)
	h.do("POST", "/api/v1/setup", map[string]string{
		"username": "alice", "password": "hunter2",
	}).Body.Close()

	resp := h.do("POST", "/api/v1/auth/elevate", map[string]string{
		"password": "hunter2",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("elevate: %d", resp.StatusCode)
	}
	body := decodeJSON[struct {
		ElevatedUntil int64 `json:"elevated_until"`
	}](t, resp)
	if body.ElevatedUntil == 0 {
		t.Fatal("elevated_until is zero")
	}

	// Audit row: auth.elevate.success
	resp = h.do("GET", "/api/v1/audit", nil)
	audit := decodeJSON[struct {
		Events []AuditEventDTO `json:"events"`
	}](t, resp)
	found := false
	for _, e := range audit.Events {
		if e.Action == "auth.elevate.success" && e.Success {
			found = true
		}
	}
	if !found {
		t.Error("no auth.elevate.success audit row found")
	}
}

func TestElevateWrongPasswordFails(t *testing.T) {
	h := newHarness(t)
	h.do("POST", "/api/v1/setup", map[string]string{
		"username": "alice", "password": "hunter2",
	}).Body.Close()

	resp := h.do("POST", "/api/v1/auth/elevate", map[string]string{
		"password": "wrongpass",
	})
	if resp.StatusCode != 401 {
		t.Fatalf("elevate with wrong password = %d; want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Audit row: auth.elevate.failure
	resp = h.do("GET", "/api/v1/audit", nil)
	body := decodeJSON[struct {
		Events []AuditEventDTO `json:"events"`
	}](t, resp)
	found := false
	for _, e := range body.Events {
		if e.Action == "auth.elevate.failure" && !e.Success {
			found = true
		}
	}
	if !found {
		t.Error("no auth.elevate.failure audit row found")
	}
}

func TestElevateRequiresAuth(t *testing.T) {
	h := newHarness(t)
	h.do("POST", "/api/v1/setup", map[string]string{
		"username": "alice", "password": "hunter2",
	}).Body.Close()
	// Clear session.
	jar, _ := newJar()
	h.jar = jar

	resp := h.do("POST", "/api/v1/auth/elevate", map[string]string{"password": "hunter2"})
	if resp.StatusCode != 401 {
		t.Fatalf("unauthenticated elevate = %d; want 401", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSessionIdleExpiry(t *testing.T) {
	h := newHarness(t)
	h.do("POST", "/api/v1/setup", map[string]string{
		"username": "alice", "password": "hunter2",
	}).Body.Close()

	// Set the auth manager's clock forward past the idle window. We need to
	// reach into the server's auth manager clock — do this by pulling the
	// session token from the jar and using the store directly.
	alice, _ := h.st.GetUserByUsername("alice")
	sessions, err := h.st.ListSessionsForUser(alice.ID)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(sessions) == 0 {
		t.Fatal("no sessions for alice")
	}
	// Move last_seen_at back by 31 days so the session appears idle-expired.
	oldTime := sessions[0].LastSeenAt.Add(-31 * 24 * time.Hour)
	if err := h.st.TouchSession(sessions[0].Token, oldTime); err != nil {
		t.Fatalf("touch session: %v", err)
	}

	resp := h.do("GET", "/api/v1/me", nil)
	if resp.StatusCode != 401 {
		t.Fatalf("me after idle expiry = %d; want 401", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- Elevation window gates user-CRUD ------------------------------------

func TestUserCRUDRequiresElevation(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	// Add a member to have a target for patch/delete.
	h.addMember("u_bob", "bob", "bobpass")
	bob, _ := h.st.GetUserByUsername("bob")

	// Without elevation all these should return 403.
	cases := []struct {
		method string
		path   string
		body   any
	}{
		{"POST", "/api/v1/users", map[string]string{"username": "eve", "password": "x"}},
		{"PATCH", "/api/v1/users/" + bob.ID, map[string]string{"role": "admin"}},
		{"DELETE", "/api/v1/users/" + bob.ID, nil},
		{"POST", "/api/v1/users/" + bob.ID + "/password", map[string]string{"password": "x"}},
	}
	for _, tc := range cases {
		resp := h.do(tc.method, tc.path, tc.body)
		if resp.StatusCode != 403 {
			t.Errorf("%s %s without elevation = %d; want 403", tc.method, tc.path, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// After elevation, POST /users should succeed.
	h.elevate("pass1")
	resp := h.do("POST", "/api/v1/users", map[string]string{"username": "eve", "password": "x"})
	if resp.StatusCode != 200 {
		t.Fatalf("POST /users after elevate = %d; want 200", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestLoginThrottleIgnoresForgedForwardedFor is the login half of #329. The
// per-IP login bucket (10 attempts/min, AUTH.md # Rate limiting) is the only
// gate that slows a username spray, because each fresh username starts with no
// per-username backoff. It used to key on the first X-Forwarded-For hop, so an
// attacker rotating that header got a fresh 10-attempt budget per request. Now
// the header is inert unless the peer is a trusted proxy — a spray from one
// source drains one bucket however it labels itself.
func TestLoginThrottleIgnoresForgedForwardedFor(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("admin", "correct-horse-battery")

	// A distinct username per attempt so only the per-IP gate can fire, and a
	// distinct forged X-Forwarded-For per attempt so a spoofable key would hand
	// out a fresh bucket every time.
	attempt := func(i int) *http.Response {
		body, _ := json.Marshal(map[string]string{
			"username": fmt.Sprintf("victim%d", i),
			"password": "guess",
		})
		req, err := http.NewRequest("POST", h.srv.URL+"/api/v1/login", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("10.9.%d.%d", i/256, i%256))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		return resp
	}

	// ipBucketCapacity (10) attempts are spent, then the gate must fire. The loop
	// runs one past capacity and asserts the throttle showed up by then.
	throttledAt := -1
	for i := 0; i < 11; i++ {
		resp := attempt(i)
		code := resp.StatusCode
		resp.Body.Close()
		if code == http.StatusTooManyRequests {
			throttledAt = i
			break
		}
		if code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: want 401 (bad credentials) or 429, got %d", i, code)
		}
	}
	if throttledAt < 0 {
		t.Fatal("rotating a forged X-Forwarded-For defeated the per-IP login throttle: no 429 in 11 attempts")
	}
}
