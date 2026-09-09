package sshaccess

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// managedHeader marks the file as generated. It is also what the parser keys on
// to know it is reading its own output rather than something a person wrote.
const managedHeader = "# Managed by malmo. Generated from the per-account SSH opt-in; edits are not preserved."

// keyCountPrefix carries the number of keys an account has through the file, so
// GET /v1/ssh/state can report it without reading every home directory. sshd
// ignores comments, so this is inert configuration-wise.
const keyCountPrefix = "#   malmo-keys: "

// render produces the whole drop-in from the enabled set.
//
// Two halves, and the split matters. AllowUsers is global and gates *which*
// accounts sshd will consider at all; it cannot live in a Match block. The
// per-account auth policy has to be a Match block, because AuthenticationMethods
// is what makes one account key-only and another key-and-password.
//
// PasswordAuthentication is set globally to yes because it is a prerequisite for
// the password half of any account's AuthenticationMethods. On its own it grants
// nothing: an account whose Match block says "publickey" cannot get in with a
// password no matter what the global says, and an account with no Match block
// cannot get in at all because it is not in AllowUsers.
func render(accounts []account, keysDir string) string {
	sorted := make([]account, len(accounts))
	copy(sorted, accounts)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Username < sorted[j].Username })

	var b strings.Builder
	b.WriteString(managedHeader + "\n")
	b.WriteString("# See AUTH.md # Device access and BUILD.md # SSH.\n\n")
	b.WriteString("PermitRootLogin no\n")
	b.WriteString("PubkeyAuthentication yes\n")
	b.WriteString("PasswordAuthentication yes\n")
	b.WriteString("KbdInteractiveAuthentication no\n")

	if len(sorted) == 0 {
		// No AllowUsers line at all would mean "every account", the opposite of
		// what an empty enabled set means. sshd has no way to spell "nobody", so
		// DenyUsers * is the explicit closed state. The daemon is stopped in this
		// case anyway; this is the belt to that suspenders, and it is what makes a
		// hand-started sshd still refuse everyone.
		b.WriteString("DenyUsers *\n")
		return b.String()
	}

	names := make([]string, 0, len(sorted))
	for _, a := range sorted {
		names = append(names, a.Username)
	}
	b.WriteString("AllowUsers " + strings.Join(names, " ") + "\n")

	// Match blocks go last: everything after a Match line belongs to that block
	// until the next one, so any global keyword written below would silently
	// become part of the final account's policy.
	for _, a := range sorted {
		b.WriteString("\n")
		b.WriteString(keyCountPrefix + a.Username + " " + strconv.Itoa(a.KeyCount) + "\n")
		b.WriteString("Match User " + a.Username + "\n")
		b.WriteString("    AuthenticationMethods " + methods(a) + "\n")
		// Two paths, in this order. The first is malmo's root-owned file, which the
		// account cannot write. The second is the user's own, so keys they added
		// from their shell keep working and malmo never touches that file.
		b.WriteString("    AuthorizedKeysFile " + filepath.Join(keysDir, a.Username) + " .ssh/authorized_keys\n")
	}
	return b.String()
}

// methods spells one account's required factors.
//
// A comma joins methods that are ALL required, in the order sshd will try them.
// That is the whole point of the design: the optional factor is a second lock,
// never a second door (AUTH.md # Device access). Spaces would mean "any of
// these", which is the reading this must never produce.
//
// An account with no keys is password-only, which is the appliance default. An
// account with keys is key-first, and RequirePassword appends the password as a
// second required method.
func methods(a account) string {
	if a.KeyCount == 0 {
		return "password"
	}
	if a.RequirePassword {
		return "publickey,password"
	}
	return "publickey"
}

// readDropIn recovers the enabled set from the rendered file. host-agent keeps
// no state of its own, so the file is the source of truth and a restart re-reads
// reality instead of trusting a cache.
//
// A missing file is an empty set, not an error: that is a box where nobody has
// ever enabled SSH. A file without the managed header is refused, because
// overwriting a config a person wrote by hand would destroy work and could lock
// them out of their own box.
func (m *Manager) readDropIn() ([]account, error) {
	path := m.dropInPath()
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sshaccess: open %s: %w", path, err)
	}
	defer f.Close()

	var (
		accounts []account
		keyCount = map[string]int{}
		managed  bool
		current  string
	)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == managedHeader:
			managed = true
		case strings.HasPrefix(line, keyCountPrefix):
			name, n, ok := parseKeyCount(line)
			if ok {
				keyCount[name] = n
			}
		case strings.HasPrefix(line, "Match User "):
			current = strings.TrimSpace(strings.TrimPrefix(line, "Match User "))
			accounts = append(accounts, account{Username: current})
		case strings.HasPrefix(line, "AuthenticationMethods ") && current != "":
			spec := strings.TrimSpace(strings.TrimPrefix(line, "AuthenticationMethods "))
			accounts[len(accounts)-1].RequirePassword = strings.Contains(spec, "password") &&
				strings.Contains(spec, "publickey")
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("sshaccess: read %s: %w", path, err)
	}
	if !managed {
		return nil, fmt.Errorf("sshaccess: %s exists but is not malmo-managed; refusing to overwrite it", path)
	}
	for i := range accounts {
		accounts[i].KeyCount = keyCount[accounts[i].Username]
	}
	return accounts, nil
}

// parseKeyCount reads back a "#   malmo-keys: <user> <n>" line.
func parseKeyCount(line string) (string, int, bool) {
	fields := strings.Fields(strings.TrimPrefix(line, keyCountPrefix))
	if len(fields) != 2 {
		return "", 0, false
	}
	n, err := strconv.Atoi(fields[1])
	if err != nil {
		return "", 0, false
	}
	return fields[0], n, true
}
