package sshaccess

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/malmoos/malmo/internal/protocol"
)

// newManager builds a Manager pointed at a temp drop-in, with sshd and systemctl
// faked. Every command is recorded so the daemon-lifecycle assertions can read
// what would have run.
// testKey is a real ed25519 public key, so anything that parses it agrees with
// what a box would see.
const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIfJnhGAA/rWbxmvMGuZvXV6in+czTK5F8Ie7QGTKOT+ alex@laptop"

func newManager(t *testing.T, keysDir string) (*Manager, *[]string) {
	t.Helper()
	var ran []string
	m := &Manager{
		DropInPath: filepath.Join(t.TempDir(), "sshd_config.d", "malmo-allowed.conf"),
		KeysDir:    keysDir,
		Runner: func(name string, args ...string) ([]byte, error) {
			ran = append(ran, name+" "+strings.Join(args, " "))
			if name == "systemctl" && len(args) > 0 && args[0] == "is-active" {
				return []byte("active\n"), nil
			}
			return nil, nil
		},
	}
	return m, &ran
}

func mustSet(t *testing.T, m *Manager, req protocol.SetSSHAccessRequest) {
	t.Helper()
	if err := m.SetAccess(req); err != nil {
		t.Fatalf("SetAccess(%+v): %v", req, err)
	}
}

func readDropInFile(t *testing.T, m *Manager) string {
	t.Helper()
	b, err := os.ReadFile(m.dropInPath())
	if err != nil {
		t.Fatalf("read drop-in: %v", err)
	}
	return string(b)
}

// The optional factor must render as a comma-joined AuthenticationMethods, which
// sshd reads as "all of these are required". A space-separated list would mean
// "any of these" — the exact misreading that would turn the second lock into a
// second door and undo the hosted key requirement (AUTH.md # Device access).
func TestMethodsAreRequiredNotAlternatives(t *testing.T) {
	cases := map[string]struct {
		acct account
		want string
	}{
		"key only":         {account{Username: "a", KeyCount: 1}, "publickey"},
		"key and password": {account{Username: "a", KeyCount: 1, RequirePassword: true}, "publickey,password"},
		"password only":    {account{Username: "a", KeyCount: 0}, "password"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := methods(c.acct)
			if got != c.want {
				t.Fatalf("methods = %q; want %q", got, c.want)
			}
			if strings.Contains(got, " ") {
				t.Fatalf("methods %q is space-separated; sshd would read that as "+
					"'any of these', making the second factor an alternative", got)
			}
		})
	}
}

// An empty enabled set must lock everyone out explicitly. Omitting AllowUsers
// means "every account" to sshd, which is the opposite of what an empty set
// means, so a hand-started sshd would admit the whole machine.
func TestRenderEmptySetDeniesEveryone(t *testing.T) {
	out := render(nil, "/etc/ssh/malmo-authorized-keys")
	if !strings.Contains(out, "DenyUsers *") {
		t.Fatalf("empty set did not deny everyone:\n%s", out)
	}
	if strings.Contains(out, "AllowUsers") {
		t.Fatalf("empty set wrote an AllowUsers line:\n%s", out)
	}
}

// Every global keyword has to precede the first Match block: sshd scopes
// everything after a Match line to that block, so a global written afterwards
// would silently become the last account's policy.
func TestRenderGlobalsPrecedeMatchBlocks(t *testing.T) {
	out := render([]account{{Username: "alex", KeyCount: 1}}, "/etc/ssh/malmo-authorized-keys")
	firstMatch := strings.Index(out, "Match User ")
	if firstMatch < 0 {
		t.Fatalf("no Match block rendered:\n%s", out)
	}
	for _, global := range []string{"PermitRootLogin", "PasswordAuthentication", "AllowUsers", "PubkeyAuthentication", "KbdInteractiveAuthentication"} {
		if at := strings.Index(out, global); at < 0 || at > firstMatch {
			t.Fatalf("global %q at %d is not before the first Match at %d:\n%s", global, at, firstMatch, out)
		}
	}
}

// Enabling the first account starts the daemon and disabling the last stops it —
// this is what opens and closes :22, and on hosted it is the only control over
// that port (ENVIRONMENT.md # Access & files).
func TestDaemonFollowsTheEnabledSet(t *testing.T) {
	home := t.TempDir()
	m, ran := newManager(t, home)

	mustSet(t, m, protocol.SetSSHAccessRequest{
		User: "alex", Enabled: true, AuthorizedKeys: []string{"ssh-ed25519 AAAAKEY alex@laptop"},
	})
	if !containsCmd(*ran, "systemctl enable --now") {
		t.Fatalf("first enable did not start the daemon: %v", *ran)
	}

	*ran = nil
	mustSet(t, m, protocol.SetSSHAccessRequest{
		User: "bo", Enabled: true, AuthorizedKeys: []string{"ssh-ed25519 AAAAKEY bo@desktop"},
	})
	if containsCmd(*ran, "systemctl disable") {
		t.Fatalf("adding a second account stopped the daemon: %v", *ran)
	}

	*ran = nil
	mustSet(t, m, protocol.SetSSHAccessRequest{User: "alex", Enabled: false})
	if containsCmd(*ran, "systemctl disable") {
		t.Fatalf("daemon stopped while bo was still enabled: %v", *ran)
	}

	*ran = nil
	mustSet(t, m, protocol.SetSSHAccessRequest{User: "bo", Enabled: false})
	if !containsCmd(*ran, "systemctl disable --now") {
		t.Fatalf("disabling the last account did not stop the daemon: %v", *ran)
	}
	if got := readDropInFile(t, m); !strings.Contains(got, "DenyUsers *") {
		t.Fatalf("drop-in did not close after the last account left:\n%s", got)
	}
}

// A running sshd is reloaded, never restarted: a restart drops live sessions,
// and the admin fixing something over SSH is exactly who is connected.
func TestRunningDaemonIsReloadedNotRestarted(t *testing.T) {
	m, ran := newManager(t, t.TempDir())
	mustSet(t, m, protocol.SetSSHAccessRequest{
		User: "alex", Enabled: true, AuthorizedKeys: []string{"ssh-ed25519 AAAAKEY alex@laptop"},
	})
	if !containsCmd(*ran, "systemctl reload") {
		t.Fatalf("config change did not reload sshd: %v", *ran)
	}
	if containsCmd(*ran, "systemctl restart") {
		t.Fatalf("config change restarted sshd, dropping live sessions: %v", *ran)
	}
}

// The rendered config must be validated before it is allowed to take effect. A
// bad render that reaches a reload takes sshd down and locks out the accounts
// that were working a moment ago.
func TestBadRenderIsRefusedBeforeReload(t *testing.T) {
	var ran []string
	m := &Manager{
		DropInPath: filepath.Join(t.TempDir(), "malmo-allowed.conf"),
		Runner: func(name string, args ...string) ([]byte, error) {
			ran = append(ran, name+" "+strings.Join(args, " "))
			if name == "sshd" {
				return []byte("bad configuration option"), os.ErrInvalid
			}
			return nil, nil
		},
		KeysDir: t.TempDir(),
	}
	err := m.SetAccess(protocol.SetSSHAccessRequest{
		User: "alex", Enabled: true, AuthorizedKeys: []string{"ssh-ed25519 AAAAKEY alex@laptop"},
	})
	if err == nil {
		t.Fatal("SetAccess accepted a config sshd -t rejected")
	}
	if containsCmd(ran, "systemctl reload") || containsCmd(ran, "systemctl enable") {
		t.Fatalf("daemon was touched after a failed validation: %v", ran)
	}
}

// The enabled set survives a restart because it is read back from the rendered
// file — host-agent keeps no state of its own.
func TestStateIsReadBackFromTheRenderedFile(t *testing.T) {
	home := t.TempDir()
	m, _ := newManager(t, home)
	mustSet(t, m, protocol.SetSSHAccessRequest{
		User: "alex", Enabled: true, RequirePassword: true,
		AuthorizedKeys: []string{"ssh-ed25519 AAAAKEY alex@laptop", "ssh-ed25519 AAAAKEY2 alex@desktop"},
	})

	fresh := &Manager{DropInPath: m.DropInPath, KeysDir: m.KeysDir, Runner: m.Runner}
	st, err := fresh.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if len(st.Users) != 1 {
		t.Fatalf("users = %d; want 1", len(st.Users))
	}
	got := st.Users[0]
	if got.Username != "alex" || got.KeyCount != 2 || !got.RequirePassword {
		t.Fatalf("recovered %+v; want alex with 2 keys and require_password", got)
	}
	if !st.DaemonRunning {
		t.Fatal("DaemonRunning false while systemctl reported active")
	}
}

// A config a person wrote by hand is never overwritten: doing so would destroy
// their work and could lock them out of their own box.
func TestUnmanagedConfigIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "malmo-allowed.conf")
	if err := os.WriteFile(path, []byte("AllowUsers someone\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	m := &Manager{DropInPath: path, Runner: func(string, ...string) ([]byte, error) { return nil, nil }}
	if err := m.SetAccess(protocol.SetSSHAccessRequest{User: "alex", Enabled: true, RequirePassword: true}); err == nil {
		t.Fatal("SetAccess overwrote a config it did not write")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if string(b) != "AllowUsers someone\n" {
		t.Fatalf("hand-written config was modified: %q", b)
	}
}

// malmo's keys live in a root-owned file, never in the account's home. The home
// directory is a path the account controls and can replace with a symlink between
// any check and any use, so writing there as root is a privilege-escalation path
// rather than a hardening problem. Owning the file removes the user from it.
func TestKeysAreWrittenOutsideTheUsersHome(t *testing.T) {
	keysDir := t.TempDir()
	m, _ := newManager(t, keysDir)

	mustSet(t, m, protocol.SetSSHAccessRequest{
		User: "alex", Enabled: true, AuthorizedKeys: []string{testKey},
	})
	assertContains(t, filepath.Join(keysDir, "alex"), testKey)

	// And sshd is pointed at both that file and the user's own, so keys they added
	// from their shell keep working without malmo ever touching that file.
	conf := readDropInFile(t, m)
	if !strings.Contains(conf, "AuthorizedKeysFile "+filepath.Join(keysDir, "alex")+" .ssh/authorized_keys") {
		t.Fatalf("drop-in does not point sshd at both key files:\n%s", conf)
	}
}

// A username that could climb out of the managed directory is refused rather
// than joined blindly.
func TestUnsafeUsernameIsRefused(t *testing.T) {
	m, _ := newManager(t, t.TempDir())
	for _, name := range []string{"../root", "a/b", ".", ".."} {
		err := m.SetAccess(protocol.SetSSHAccessRequest{
			User: name, Enabled: true, AuthorizedKeys: []string{testKey},
		})
		if err == nil {
			t.Fatalf("SetAccess accepted username %q", name)
		}
	}
}

// Disabling an account removes its key file. A stale key on a disabled account
// is a credential nobody is tracking.
func TestDisablingRemovesTheKeyFile(t *testing.T) {
	keysDir := t.TempDir()
	m, _ := newManager(t, keysDir)
	keyFile := filepath.Join(keysDir, "alex")

	mustSet(t, m, protocol.SetSSHAccessRequest{
		User: "alex", Enabled: true, AuthorizedKeys: []string{testKey},
	})
	if _, err := os.Stat(keyFile); err != nil {
		t.Fatalf("keys not written: %v", err)
	}
	mustSet(t, m, protocol.SetSSHAccessRequest{User: "alex", Enabled: false})
	if _, err := os.Stat(keyFile); !os.IsNotExist(err) {
		t.Fatalf("key file survived a disable: %v", err)
	}
}

// Two accounts enabled at once must both survive. Every call re-renders one
// drop-in that holds the whole enabled set, so an unsynchronised read-modify-write
// would silently drop whichever account lost the race.
func TestConcurrentEnablesDoNotLoseAnAccount(t *testing.T) {
	m, _ := newManager(t, t.TempDir())

	var wg sync.WaitGroup
	for _, name := range []string{"alex", "bo", "cy", "di"} {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			if err := m.SetAccess(protocol.SetSSHAccessRequest{
				User: u, Enabled: true, AuthorizedKeys: []string{testKey},
			}); err != nil {
				t.Errorf("SetAccess(%s): %v", u, err)
			}
		}(name)
	}
	wg.Wait()

	st, err := m.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if len(st.Users) != 4 {
		t.Fatalf("enabled accounts = %d; want 4 — a concurrent write dropped one", len(st.Users))
	}
}

func assertContains(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(b), want) {
		t.Fatalf("%s does not contain %q:\n%s", path, want, b)
	}
}

func containsCmd(ran []string, prefix string) bool {
	for _, c := range ran {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}
