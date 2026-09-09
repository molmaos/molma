//go:build !hosted

package main

import (
	"context"
	"log/slog"

	"github.com/malmoos/malmo/internal/hostagent"
	"github.com/malmoos/malmo/internal/hostagent/avahipublisher"
	"github.com/malmoos/malmo/internal/hostagent/clockhealth"
	"github.com/malmoos/malmo/internal/hostagent/diskusage"
	"github.com/malmoos/malmo/internal/hostagent/healthsource"
	"github.com/malmoos/malmo/internal/hostagent/journalsource"
	"github.com/malmoos/malmo/internal/hostagent/netstate"
	"github.com/malmoos/malmo/internal/hostagent/pamverifier"
	"github.com/malmoos/malmo/internal/hostagent/procsource"
	"github.com/malmoos/malmo/internal/hostagent/rampressure"
	"github.com/malmoos/malmo/internal/hostagent/rebootrequired"
	"github.com/malmoos/malmo/internal/hostagent/servicehealth"
	"github.com/malmoos/malmo/internal/hostagent/sshaccess"
	"github.com/malmoos/malmo/internal/hostagent/timezone"
	"github.com/malmoos/malmo/internal/hostagent/usermgr"
	"github.com/malmoos/malmo/internal/protocol"
)

// buildAgent wires the full appliance host integration: real PAM, user
// management, the health/system reporters, the per-app log tail, and the
// LAN-facing discovery stack.
//
// Discovery announces per LAN interface: NetworkManager (via DBus, netstate) is
// the source of truth for the active ethernet/WiFi interfaces (#130); each app
// name is published on every LAN interface with that interface's own address
// (avahipublisher), and a watcher replays the announcements (and keeps
// avahi-daemon.conf's allow-interfaces current) when the network changes. See
// DISCOVERY.md # Interface scoping.
//
// This is the default (untagged) build; `go build -tags hosted` selects the
// slim hosted variant in wiring_hosted.go instead. The returned cleanup stops
// the network watcher and closes both DBus connections (the publisher's and the
// provider's).
func buildAgent() (*hostagent.Agent, func()) {
	// NetworkManager is the source of truth for the LAN set (#130): the
	// publisher announces every app name per LAN interface with that
	// interface's address, and the sync keeps avahi-daemon.conf's
	// allow-interfaces plus the committed entry groups aligned across IP
	// changes and interface add/remove.
	prov := &netstate.NMProvider{}
	pub := &avahipublisher.DBusPublisher{HostSuffix: protocol.AppHostSuffix, LAN: prov.LANInterfaces}
	avahiSync := &avahipublisher.Sync{Publisher: pub, LAN: prov.LANInterfaces}

	// verifyPassword uses real PAM; Avahi A records are published via DBus;
	// set-password writes to /etc/shadow via useradd+chpasswd; set-role
	// flips sudo group membership via gpasswd; delete-user shells out to
	// userdel -r -f (see docs/progress/host-agent-delete-user.md).
	a := hostagent.New(
		&pamverifier.PAMVerifier{Service: "malmo"},
		pub,
	)
	a.UserMgr = &usermgr.LinuxUserManager{}
	a.Timezone = timezone.New()
	// SSH access is wired in both build profiles: the per-account opt-in exists
	// on the appliance and on hosted alike, and only the *mandatory* factor
	// differs — a key on hosted, the password here (AUTH.md # Device access).
	// That choice is the brain's, so the same manager serves both.
	a.SSH = &sshaccess.Manager{}
	a.Health = healthsource.New(healthsource.DefaultPath)
	a.Services = servicehealth.New(servicehealth.ApplianceUnits)
	a.Time = clockhealth.New()
	a.Logs = journalsource.New()
	a.Resources = rampressure.New()
	// One diskusage.Reporter satisfies both disk seams: DataDisk() for the
	// install-plan free_bytes (Disk) and Disks() for the Storage bars (DiskSpace).
	du := diskusage.New()
	a.Disk = du
	a.DiskSpace = du
	a.Reboot = rebootrequired.New()
	a.System = procsource.New()
	a.Net = prov

	// Align avahi-daemon with the current LAN set once at startup, then keep
	// it aligned from the NetworkManager watcher. Startup failure is non-fatal:
	// unprivileged dev runs can't write /etc/avahi or restart the daemon, and
	// every step is idempotent — the next network event retries.
	if err := avahiSync.Apply(); err != nil {
		slog.Warn("avahi sync at startup", "err", err)
	}
	watchCtx, stopWatch := context.WithCancel(context.Background())
	go func() {
		if err := prov.Watch(watchCtx, func() {
			if err := avahiSync.Apply(); err != nil {
				slog.Error("avahi sync", "err", err)
			}
		}); err != nil {
			slog.Error("netstate watch unavailable; IP-change replay disabled", "err", err)
		}
	}()

	cleanup := func() {
		stopWatch()
		_ = pub.Close()
		_ = prov.Close()
	}
	return a, cleanup
}
