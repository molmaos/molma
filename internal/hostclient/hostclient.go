// Package hostclient is the brain's client for the host-agent UNIX socket
// (BRAIN_HOST_PROTOCOL.md). HTTP/JSON over net.Dial("unix", ...).
package hostclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/malmoos/malmo/internal/protocol"
)

type Client struct {
	http *http.Client
	// stream is a timeout-less client for long-lived SSE follows (JournalFollow).
	// It shares the request client's transport — and thus its connection pool —
	// but drops the 30s Timeout, which would otherwise sever a healthy tail. A
	// follow is bounded by the caller's context, not a wall-clock deadline.
	stream *http.Client
}

// New dials the given UNIX socket path. The host part of the URL is ignored
// by the dialer; we use "http://agent" as a stable placeholder.
func New(sockPath string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sockPath)
		},
	}
	return &Client{
		http:   &http.Client{Timeout: 30 * time.Second, Transport: transport},
		stream: &http.Client{Transport: transport},
	}
}

func (c *Client) Publish(ctx context.Context, slug string) (protocol.PublishResponse, error) {
	var out protocol.PublishResponse
	err := c.do(ctx, "POST", "/v1/discovery/publish", protocol.PublishRequest{Slug: slug}, &out)
	return out, err
}

func (c *Client) Unpublish(ctx context.Context, slug string) error {
	return c.do(ctx, "POST", "/v1/discovery/unpublish", protocol.UnpublishRequest{Slug: slug}, nil)
}

// VerifyPassword asks host-agent whether (user, password) is valid.
// Returned bool is the only signal — host-agent deliberately doesn't
// distinguish wrong-password from unknown-user. See BRAIN_HOST_PROTOCOL.md.
func (c *Client) VerifyPassword(ctx context.Context, user, password string) (bool, error) {
	var out protocol.VerifyPasswordResponse
	err := c.do(ctx, "POST", "/v1/auth/verify-password", protocol.VerifyPasswordRequest{User: user, Password: password}, &out)
	return out.Valid, err
}

// SetPassword upserts the user's password (creates the user if missing).
func (c *Client) SetPassword(ctx context.Context, user, password string) error {
	return c.do(ctx, "POST", "/v1/auth/set-password", protocol.SetPasswordRequest{User: user, Password: password}, nil)
}

// SetRole updates the user's Linux group membership to match role ("admin" or "member").
func (c *Client) SetRole(ctx context.Context, user, role string) error {
	return c.do(ctx, "POST", "/v1/auth/set-role", protocol.SetRoleRequest{User: user, Role: role}, nil)
}

// SetTimezone applies the system timezone via host-agent (timedatectl set-timezone).
// zone is an IANA tz database name; the brain validates it before calling.
func (c *Client) SetTimezone(ctx context.Context, zone string) error {
	return c.do(ctx, "POST", "/v1/system/set-timezone", protocol.SetTimezoneRequest{Zone: zone}, nil)
}

// SetSSHAccess applies one account's full desired SSH state on the host: the
// authorized keys it may use, whether the malmo password is required as a second
// method, and whether the account is enabled at all. host-agent re-renders the
// sshd drop-in from the enabled set and starts or stops the daemon to match, so
// this is also what opens and closes :22 (BRAIN_HOST_PROTOCOL.md # SSH access).
//
// The brain decides which factor is mandatory for the profile and validates the
// keys before calling; host-agent applies what it is given.
func (c *Client) SetSSHAccess(ctx context.Context, req protocol.SetSSHAccessRequest) error {
	return c.do(ctx, "POST", "/v1/ssh/set-access", req, nil)
}

// SSHState reads actual SSH state from the host for the reconciler: whether the
// daemon is running, and which accounts the rendered config currently enables.
func (c *Client) SSHState(ctx context.Context) (protocol.SSHState, error) {
	var out protocol.SSHState
	if err := c.do(ctx, "GET", "/v1/ssh/state", nil, &out); err != nil {
		return protocol.SSHState{}, err
	}
	return out, nil
}

// DeleteUser removes the user. Idempotent: unknown user returns nil.
func (c *Client) DeleteUser(ctx context.Context, user string) error {
	return c.do(ctx, "POST", "/v1/auth/delete-user", protocol.DeleteUserRequest{User: user}, nil)
}

// ErrUnknownUser is returned by ResolveHome when the host reports the user
// does not exist. The brain maps this to an installation error, not a retry.
// Check with errors.Is — the value is stable across versions.
var ErrUnknownUser = errors.New("unknown user")

// ResolveHome returns the user's home directory path, UID, and GID from the
// host. The brain calls this during install to build bind-mount sources and
// user: directives for personal app instances.
//
// Returns ErrUnknownUser (checkable with errors.Is) when the host reports the
// user does not exist — the brain maps this to an installation error, not a
// retry. Any other non-200 is a generic host error.
//
// Does not go through do because do flattens all errors to opaque strings;
// the 404 → typed-sentinel discrimination must survive to the caller.
func (c *Client) ResolveHome(ctx context.Context, username string) (protocol.ResolveHomeResponse, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		"http://agent/v1/users/"+url.PathEscape(username)+"/home", http.NoBody)
	if err != nil {
		return protocol.ResolveHomeResponse{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return protocol.ResolveHomeResponse{}, fmt.Errorf("host-agent unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return protocol.ResolveHomeResponse{}, ErrUnknownUser
	}
	if resp.StatusCode >= 300 {
		var e protocol.Error
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Code == "" {
			e.Code = "host-agent-error"
			e.Message = resp.Status
		}
		return protocol.ResolveHomeResponse{}, fmt.Errorf("host-agent /v1/users/%s/home: %s (%s)",
			url.PathEscape(username), e.Message, e.Code)
	}
	var out protocol.ResolveHomeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return protocol.ResolveHomeResponse{}, err
	}
	return out, nil
}

// WellKnownIdentity returns the fixed service-account UIDs/GIDs for malmo-app
// and malmo-shared from the host. The brain calls this during install to build
// user: and group_add directives for household-scope app instances.
//
// All non-200 responses are generic host errors (there is no unknown-user case
// here); propagated via the standard do helper.
func (c *Client) WellKnownIdentity(ctx context.Context) (protocol.WellKnownIdentityResponse, error) {
	var out protocol.WellKnownIdentityResponse
	err := c.do(ctx, "GET", "/v1/identity/well-known", nil, &out)
	return out, err
}

// AllocateAppServiceIdentity reserves a dedicated UID/GID pair from the
// host's app-service band for a folderless `service_user: true` instance
// (APP_ISOLATION.md # Runtime identity & data ownership). The brain calls it
// once at install and persists the pair on the instance row; it is never
// re-requested for the life of the instance.
func (c *Client) AllocateAppServiceIdentity(ctx context.Context, instanceID string) (protocol.AllocateAppServiceIdentityResponse, error) {
	var out protocol.AllocateAppServiceIdentityResponse
	err := c.do(ctx, "POST", "/v1/identity/app-service",
		protocol.AllocateAppServiceIdentityRequest{InstanceID: instanceID}, &out)
	return out, err
}

// ReleaseAppServiceIdentity returns an allocated app-service identity to the
// band. Called at uninstall and on install rollback; idempotent on the host
// side, so releasing an already-released UID is safe.
func (c *Client) ReleaseAppServiceIdentity(ctx context.Context, uid int) error {
	return c.do(ctx, "POST", "/v1/identity/app-service/release",
		protocol.ReleaseAppServiceIdentityRequest{UID: uid}, nil)
}

func (c *Client) SystemStatus(ctx context.Context) (protocol.SystemStatus, error) {
	var out protocol.SystemStatus
	err := c.do(ctx, "GET", "/v1/system/status", nil, &out)
	return out, err
}

// SystemResources returns one raw cumulative-counter sample (CPU jiffies,
// memory levels, per-interface and per-device byte counters, uptime) plus a
// monotonic ts_ns. host-agent computes no rates — the caller (the brain's
// live-resources hub) diffs two successive samples, using the ts_ns delta as
// the rate denominator. All non-200 responses are generic host errors,
// propagated via the standard do helper.
func (c *Client) SystemResources(ctx context.Context) (protocol.SystemResources, error) {
	var out protocol.SystemResources
	err := c.do(ctx, "GET", "/v1/system/resources", nil, &out)
	return out, err
}

// SystemGPU returns the host's GPU capability report — presence, vendor, and
// the render group GID — behind GET /v1/system/gpu. The brain calls it once
// per `gpu: true` install: Present false is the hard capacity refusal
// (APP_ISOLATION.md # GPU) and RenderGID feeds the override's group_add. All
// non-200 responses are generic host errors, propagated via the standard do
// helper.
func (c *Client) SystemGPU(ctx context.Context) (protocol.SystemGPU, error) {
	var out protocol.SystemGPU
	err := c.do(ctx, "GET", "/v1/system/gpu", nil, &out)
	return out, err
}

// SystemUpdateTarget returns what the box's update-target loop last decided
// (GET /v1/system/update-target, UPDATES.md # 8.4). A pure read — it starts
// nothing; StartSystemUpdate is the trigger.
//
// host-agent always answers 200 with a parseable payload, so a transport error
// here is genuinely "host-agent unreachable" and never "there is no update".
// Every way the box can fail to know its target is a state inside the payload.
func (c *Client) SystemUpdateTarget(ctx context.Context) (protocol.UpdateTarget, error) {
	var out protocol.UpdateTarget
	err := c.do(ctx, "GET", "/v1/system/update-target", nil, &out)
	return out, err
}

// SystemHealth returns host-agent's locus-B findings report across categories
// (HEALTH.md # Detector catalog, BRAIN_HOST_PROTOCOL.md). host-agent always
// returns 200 with a parseable payload, so a transport error here is genuinely
// "host-agent unreachable," not "something looks bad."
func (c *Client) SystemHealth(ctx context.Context) (protocol.SystemHealth, error) {
	var out protocol.SystemHealth
	err := c.do(ctx, "GET", "/v1/health/system", nil, &out)
	return out, err
}

// JournalFollow opens host-agent's per-app log tail (GET /v1/journal/follow,
// BRAIN_HOST_PROTOCOL.md # Pattern C) and streams parsed lines until ctx is
// cancelled or host-agent closes the stream. The returned channel is closed
// when the stream ends; a non-nil error means the initial connection failed —
// host unreachable, or a non-200 such as 501 when host-agent has no log source.
//
// It uses the timeout-less streaming client deliberately: the shared 30s
// request client would sever a healthy long-lived tail, so the follow is
// bounded by ctx instead. host-agent's per-connection event IDs are not
// forwarded — the brain's log hub re-IDs every line off its own monotonic
// counter (BRAIN_UI_PROTOCOL.md Pattern C) — so this client reads only the
// `data:` payloads and ignores `id:` lines.
func (c *Client) JournalFollow(ctx context.Context, container string) (<-chan protocol.JournalLine, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		"http://agent/v1/journal/follow?container="+url.QueryEscape(container), http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := c.stream.Do(req)
	if err != nil {
		return nil, fmt.Errorf("host-agent unreachable: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		var e protocol.Error
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Code == "" {
			e.Code = "host-agent-error"
			e.Message = resp.Status
		}
		return nil, fmt.Errorf("host-agent /v1/journal/follow: %s (%s)", e.Message, e.Code)
	}

	ch := make(chan protocol.JournalLine)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		// Log lines can exceed bufio's 64 KiB default; raise the cap so a long
		// line isn't truncated mid-stream.
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			// One `data:` line per event — the producer never emits multi-line
			// data (a JournalLine marshals to a single line). Every other SSE
			// field (id:, blank separators) is ignored; the brain re-IDs.
			payload, ok := strings.CutPrefix(sc.Text(), "data:")
			if !ok {
				continue
			}
			payload = strings.TrimPrefix(payload, " ")
			var jl protocol.JournalLine
			if err := json.Unmarshal([]byte(payload), &jl); err != nil {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case ch <- jl:
			}
		}
	}()
	return ch, nil
}

// ErrUpdateInProgress is returned by StartSystemUpdate when host-agent already
// has a job running (409). The brain maps it to a 409 of its own rather than a
// generic host error: "an update is already running" is a state the admin can
// act on, and it is the one refusal that is not a bug.
var ErrUpdateInProgress = errors.New("a control-plane update is already running")

// ErrJobNotFound is returned by Job when host-agent has never seen the id
// (404) — including every id from before a host-agent restart, since job
// records are in memory. The brain maps it to 404.
var ErrJobNotFound = errors.New("no such job")

// StartSystemUpdate asks host-agent to move the control plane to the given
// pair (POST /v1/jobs/system-update, UPDATES.md # 3). An empty ref leaves that
// component alone. Returns the accepted job, which the caller polls with Job.
//
// The refs come from the caller. host-agent does not fetch a target from
// anywhere — see protocol.SystemUpdateRequest for why.
//
// Does not go through do, because the 409 must survive to the caller as a
// typed sentinel instead of an opaque string.
func (c *Client) StartSystemUpdate(ctx context.Context, brainRef, uiRef string) (protocol.Job, error) {
	resp, err := c.request(ctx, "POST", "/v1/jobs/system-update",
		protocol.SystemUpdateRequest{BrainImage: brainRef, UIImage: uiRef})
	if err != nil {
		return protocol.Job{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		return protocol.Job{}, ErrUpdateInProgress
	}
	if resp.StatusCode >= 300 {
		return protocol.Job{}, statusErr("/v1/jobs/system-update", resp)
	}
	var out protocol.Job
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return protocol.Job{}, err
	}
	return out, nil
}

// Job reads one host-agent job record (GET /v1/jobs/{id}). Returns
// ErrJobNotFound (checkable with errors.Is) when host-agent does not know the
// id; any other non-200 is a generic host error.
func (c *Client) Job(ctx context.Context, id string) (protocol.Job, error) {
	path := "/v1/jobs/" + url.PathEscape(id)
	resp, err := c.request(ctx, "GET", path, nil)
	if err != nil {
		return protocol.Job{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return protocol.Job{}, ErrJobNotFound
	}
	if resp.StatusCode >= 300 {
		return protocol.Job{}, statusErr(path, resp)
	}
	var out protocol.Job
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return protocol.Job{}, err
	}
	return out, nil
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	resp, err := c.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return statusErr(path, resp)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// request sends one call and returns the response. It fails only on transport
// errors; what a non-2xx means is the caller's call, which is what lets the job
// routes discriminate 404 and 409 while everything else stays on do.
func (c *Client) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return nil, err
		}
	}
	var reqBody io.Reader = http.NoBody
	if body != nil {
		reqBody = &buf
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://agent"+path, reqBody)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("host-agent unreachable: %w", err)
	}
	return resp, nil
}

// statusErr turns a non-2xx response into the standard host-agent error string.
// It consumes the body; the caller still owns closing it.
func statusErr(path string, resp *http.Response) error {
	var e protocol.Error
	_ = json.NewDecoder(resp.Body).Decode(&e)
	if e.Code == "" {
		e.Code = "host-agent-error"
		e.Message = resp.Status
	}
	return fmt.Errorf("host-agent %s: %s (%s)", path, e.Message, e.Code)
}
