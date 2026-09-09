// Package sshaccess implements hostagent.SSHAccessManager against a real sshd.
// It is kept out of the shared internal/hostagent package so that package has no
// sshd or systemd dependency; only cmd/host-agent-real imports it.
//
// The unit of work is one account's full desired state
// (BRAIN_HOST_PROTOCOL.md # SSH access). Three things happen per call, in this
// order, and the order is the point:
//
//  1. The account's authorized_keys file is written (or removed).
//  2. The sshd drop-in is re-rendered from the whole enabled set and validated
//     with `sshd -t` before it is allowed to take effect.
//  3. The daemon is started or stopped so that :22 is open exactly while at
//     least one account is enabled (BUILD.md # SSH).
//
// Keys are written before the config so that an account never becomes reachable
// a moment before the key that authenticates it exists. Validation happens
// before the reload so a bad render cannot take a running sshd down and lock out
// the accounts that were already working.
package sshaccess

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/malmoos/malmo/internal/protocol"
)

// DefaultDropInPath is the sshd config fragment malmo owns. It is rendered
// whole on every change, never line-edited: the file is a projection of the
// enabled set, so anything hand-added to it is not preserved (which is also why
// the brain surfaces drift here rather than silently re-applying — see the
// asymmetric drift policy in BRAIN_HOST_PROTOCOL.md # B).
const DefaultDropInPath = "/etc/ssh/sshd_config.d/malmo-allowed.conf"

// DefaultUnit is the systemd unit for sshd on Debian.
const DefaultUnit = "ssh.service"

// Manager applies SSH access on the local system.
type Manager struct {
	// DropInPath is the sshd config fragment to render. Empty → DefaultDropInPath.
	DropInPath string
	// Unit is the sshd systemd unit. Empty → DefaultUnit.
	Unit string
	// Runner runs a command and returns its combined output. Empty → exec.
	// Present so the tests can drive the render and lifecycle logic without a
	// real sshd or systemd; nothing else swaps it.
	Runner func(name string, args ...string) ([]byte, error)
	// KeysDir holds the root-owned per-account key files. Empty → ManagedKeysDir.
	KeysDir string

	// mu serialises SetAccess. Every call is a read-modify-write of one drop-in
	// that holds the whole enabled set, so two concurrent calls could each render
	// from the same starting point and the second would drop the first's account —
	// silently revoking someone's access, or stopping sshd while a user still has
	// it on. The brain fans out per user, so concurrent calls are expected.
	mu sync.Mutex
}

func (m *Manager) dropInPath() string {
	if m.DropInPath != "" {
		return m.DropInPath
	}
	return DefaultDropInPath
}

func (m *Manager) unit() string {
	if m.Unit != "" {
		return m.Unit
	}
	return DefaultUnit
}

func (m *Manager) run(name string, args ...string) ([]byte, error) {
	if m.Runner != nil {
		return m.Runner(name, args...)
	}
	return exec.Command(name, args...).CombinedOutput()
}

// account is one enabled account as the drop-in records it. The rendered file is
// the only persistent store of the enabled set on the host: host-agent keeps no
// state of its own, so a restart re-reads reality rather than trusting a cache.
type account struct {
	Username        string
	KeyCount        int
	RequirePassword bool
}

// SetAccess applies one account's full desired state.
func (m *Manager) SetAccess(req protocol.SetSSHAccessRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if req.User == "" {
		return fmt.Errorf("sshaccess: user is required")
	}

	current, err := m.readDropIn()
	if err != nil {
		return err
	}

	if err := m.writeKeys(req.User, req.AuthorizedKeys, req.Enabled); err != nil {
		return err
	}

	next := make([]account, 0, len(current)+1)
	for _, a := range current {
		if a.Username != req.User {
			next = append(next, a)
		}
	}
	if req.Enabled {
		next = append(next, account{
			Username:        req.User,
			KeyCount:        len(req.AuthorizedKeys),
			RequirePassword: req.RequirePassword,
		})
	}
	sort.Slice(next, func(i, j int) bool { return next[i].Username < next[j].Username })

	if err := m.writeDropIn(next); err != nil {
		return err
	}
	return m.applyDaemon(len(next) > 0)
}

// State reports what the host actually has: the daemon's run state from systemd,
// and the enabled set from the rendered drop-in.
func (m *Manager) State() (protocol.SSHState, error) {
	accounts, err := m.readDropIn()
	if err != nil {
		return protocol.SSHState{}, err
	}
	users := make([]protocol.SSHUserState, 0, len(accounts))
	for _, a := range accounts {
		users = append(users, protocol.SSHUserState{
			Username:        a.Username,
			KeyCount:        a.KeyCount,
			RequirePassword: a.RequirePassword,
		})
	}
	// `systemctl is-active` exits non-zero for every not-running state, so the
	// error is the answer here and is deliberately not propagated: "inactive" is
	// not a failure to read state.
	out, _ := m.run("systemctl", "is-active", m.unit())
	return protocol.SSHState{
		DaemonRunning: strings.TrimSpace(string(out)) == "active",
		Users:         users,
	}, nil
}

// ManagedKeysDir holds one root-owned file per enabled account. malmo's keys
// live here and **never** in the user's home directory.
//
// This is the whole answer to a class of attack. host-agent runs as root, and
// `~/.ssh` is a path the account controls: it can be replaced with a symlink
// between any check and any use. Writing there as root means a `chown` that can
// be redirected onto `/etc`, and a read-modify-write that can be redirected into
// disclosing a root-only file to the user. Owning our own directory removes the
// user from the path entirely — there is nothing to race.
//
// The per-account Match block points sshd at this file *and* at the user's own
// `~/.ssh/authorized_keys`, so keys a user added from their own shell keep
// working and malmo never has to parse, preserve or delete them. sshd reads a
// path outside the home as root and requires it to be root-owned and not
// group- or world-writable, which is what the 0755/0644 modes below are for.
const ManagedKeysDir = "/etc/ssh/malmo-authorized-keys"

func (m *Manager) managedKeysDir() string {
	if m.KeysDir != "" {
		return m.KeysDir
	}
	return ManagedKeysDir
}

// managedKeysPath is the account's key file. The username is validated rather
// than escaped because it comes from the brain, which took it from a Linux
// account: anything with a separator in it is a bug or an attack, and joining it
// blindly would let it climb out of the directory.
func (m *Manager) managedKeysPath(username string) (string, error) {
	if username == "" || username == "." || username == ".." ||
		strings.ContainsAny(username, `/\`+"\x00") {
		return "", fmt.Errorf("sshaccess: refusing unsafe username %q", username)
	}
	return filepath.Join(m.managedKeysDir(), username), nil
}

// writeKeys writes the account's malmo-managed keys, or removes the file when
// the account is disabled or has none. Every path here is root-owned, so nothing
// the account can change is followed or trusted.
func (m *Manager) writeKeys(username string, keys []string, enabled bool) error {
	path, err := m.managedKeysPath(username)
	if err != nil {
		return err
	}

	if !enabled || len(keys) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("sshaccess: remove %s: %w", path, err)
		}
		return nil
	}

	if err := os.MkdirAll(m.managedKeysDir(), 0o755); err != nil {
		return fmt.Errorf("sshaccess: mkdir %s: %w", m.managedKeysDir(), err)
	}
	body := strings.Join(keys, "\n") + "\n"
	if err := writeFileAtomic(path, []byte(body), 0o644, -1, -1); err != nil {
		return fmt.Errorf("sshaccess: write %s: %w", path, err)
	}
	return nil
}

// writeDropIn renders the config for the whole enabled set and installs it only
// if sshd accepts it.
//
// Two validations, because they catch different things and only one of them can
// happen before the file is live:
//
//  1. `sshd -t -f <candidate>` reads the candidate on its own, so a syntax error
//     in what we rendered is caught while nothing is installed.
//  2. Plain `sshd -t` reads the real config, which is the only way to see our
//     fragment in combination with the box's own sshd_config. That one can only
//     run once the file is in place, so a failure there restores the previous
//     content before returning.
//
// The restore is the part that matters. Leaving a fragment sshd rejects would
// mean the next start or reload fails, which locks out every account that was
// working a moment ago — the exact outcome validating before the reload exists
// to prevent.
func (m *Manager) writeDropIn(accounts []account) error {
	path := m.dropInPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("sshaccess: mkdir %s: %w", filepath.Dir(path), err)
	}

	// Previous content, kept so a rejected combined config can be undone. A
	// missing file is recorded as "did not exist", which restores by removing.
	previous, prevErr := os.ReadFile(path)
	hadPrevious := prevErr == nil
	if prevErr != nil && !os.IsNotExist(prevErr) {
		return fmt.Errorf("sshaccess: read %s: %w", path, prevErr)
	}

	candidate := path + ".new"
	if err := writeFileAtomic(candidate, []byte(render(accounts, m.managedKeysDir())), 0o644, -1, -1); err != nil {
		return fmt.Errorf("sshaccess: write %s: %w", candidate, err)
	}
	if out, err := m.run("sshd", "-t", "-f", candidate); err != nil {
		_ = os.Remove(candidate)
		return fmt.Errorf("sshaccess: sshd rejected the rendered config: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if err := os.Rename(candidate, path); err != nil {
		_ = os.Remove(candidate)
		return fmt.Errorf("sshaccess: install %s: %w", path, err)
	}

	if out, err := m.run("sshd", "-t"); err != nil {
		if rbErr := restore(path, previous, hadPrevious); rbErr != nil {
			// Both the change and the undo failed. Say so plainly: the box now
			// holds a fragment sshd rejects, and a human has to look.
			return fmt.Errorf("sshaccess: sshd rejected the combined config and %s could not be restored: %w (restore: %v)",
				path, err, rbErr)
		}
		return fmt.Errorf("sshaccess: sshd rejected the combined config: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// restore puts the drop-in back the way it was before a failed write.
func restore(path string, previous []byte, hadPrevious bool) error {
	if !hadPrevious {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return writeFileAtomic(path, previous, 0o644, -1, -1)
}

// applyDaemon brings sshd to the state the enabled set implies. With no enabled
// account the unit is stopped and disabled, which is what closes :22 — on hosted
// that is the only control over the port (ENVIRONMENT.md # Access & files).
//
// Reload rather than restart when it should be running: a restart drops live
// sessions, and an admin fixing something over SSH is exactly who is most likely
// to be connected while this runs.
func (m *Manager) applyDaemon(shouldRun bool) error {
	unit := m.unit()
	if !shouldRun {
		if out, err := m.run("systemctl", "disable", "--now", unit); err != nil {
			return fmt.Errorf("sshaccess: stop %s: %w: %s", unit, err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if out, err := m.run("systemctl", "enable", "--now", unit); err != nil {
		return fmt.Errorf("sshaccess: start %s: %w: %s", unit, err, strings.TrimSpace(string(out)))
	}
	if out, err := m.run("systemctl", "reload", unit); err != nil {
		return fmt.Errorf("sshaccess: reload %s: %w: %s", unit, err, strings.TrimSpace(string(out)))
	}
	return nil
}
