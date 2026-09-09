//go:build hosted

package main

import (
	"github.com/malmoos/malmo/internal/hostagent"
	"github.com/malmoos/malmo/internal/hostagent/clockhealth"
	"github.com/malmoos/malmo/internal/hostagent/diskusage"
	"github.com/malmoos/malmo/internal/hostagent/healthsource"
	"github.com/malmoos/malmo/internal/hostagent/journalsource"
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

// buildAgent wires the slim hosted-cloud host integration (ENVIRONMENT.md
// # How the profile is realized — "A build-tagged slim cloud host-agent").
//
// KEEPS the same seams as the appliance that a cloud VM still needs: real PAM
// verify_password, user create/delete/set-role/set-password (usermgr), the
// health/system reporters (storage, services, time, resources, reboot, system
// resources, disk usage), and the per-app log tail. None of these touch the
// LAN, NetworkManager, or DBus.
//
// DROPS the appliance's LAN/discovery stack — no NetworkManager (netstate), no
// Avahi/mDNS publish (avahipublisher), no network watcher — because a hosted VM
// has a single provider-managed NIC and no `.local` discovery (ENVIRONMENT.md
// # Networking & discovery). LUKS/TPM unlock, the Samba allowlist, and nftables
// LAN-scoping are likewise absent. Note: the nftables package IS installed as a
// docker-ce hard dep (#241, DECISIONS.md 2026-06-23); the appliance's
// SSH/SMB LAN-scoping ruleset is absent, but a standing forward-hook DROP of
// app-container egress to 169.254.169.254 ships as a static image-baked oneshot
// (malmo-metadata-firewall.service, #251) outside host-agent. A future general
// default-deny backstop would be wired here (NEXT.md # In-guest nftables).
//
// Net is left nil: with no NetworkManager there is no LAN set to report.
// GET /v1/discovery/state then reports an empty interfaces list ("not
// measured") — a diagnostic read that is moot without mDNS. A kernel single-NIC
// reader is a deliberate follow-up if a hosted consumer ever needs the interface
// name; we do not pull NetworkManager into the hosted build to get it.
//
// Built with `go build -tags hosted ./cmd/host-agent-real` for the cloud image.
// The returned cleanup is a no-op — there is no watcher or DBus handle to close.
func buildAgent() (*hostagent.Agent, func()) {
	a := hostagent.New(
		&pamverifier.PAMVerifier{Service: "malmo"},
		noopPublisher{},
	)
	a.UserMgr = &usermgr.LinuxUserManager{}
	a.Timezone = timezone.New()
	// Same manager as the appliance: which factor is mandatory is the brain's
	// decision, not host-agent's. Here it also carries more weight than on the
	// appliance — with no LAN to scope :22 to and no malmo firewall, the daemon's
	// run state is the only control over the port (ENVIRONMENT.md # Access & files).
	a.SSH = &sshaccess.Manager{}
	a.Health = healthsource.New(healthsource.DefaultPath)
	a.Services = servicehealth.New(servicehealth.HostedUnits)
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

	return a, func() {}
}

// noopPublisher satisfies hostagent.Publisher for the hosted build, where mDNS
// publish is absent (ENVIRONMENT.md # Networking & discovery). The brain skips
// POST /v1/discovery/publish in `hosted` via the C1a profile marker, so these
// are never called in practice; the no-op is belt-and-suspenders so the
// still-mounted publish/unpublish routes can't nil-panic on a stray call.
type noopPublisher struct{}

func (noopPublisher) Publish(slug string) (string, error) { return slug + protocol.AppHostSuffix, nil }
func (noopPublisher) Unpublish(string) error              { return nil }
