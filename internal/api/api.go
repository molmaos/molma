// Package api serves the Brain ↔ UI protocol (BRAIN_UI_PROTOCOL.md): REST via
// huma (OpenAPI emitted as a byproduct) plus the raw SSE event stream.
// Skeleton scope: catalog browse, app list/detail, install/uninstall jobs,
// global event stream. Auth is a dev bypass for now (AUTH.md lands later).
package api

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/malmoos/malmo/internal/admission"
	"github.com/malmoos/malmo/internal/applog"
	"github.com/malmoos/malmo/internal/audit"
	"github.com/malmoos/malmo/internal/auth"
	"github.com/malmoos/malmo/internal/catalog"
	"github.com/malmoos/malmo/internal/events"
	"github.com/malmoos/malmo/internal/health"
	"github.com/malmoos/malmo/internal/hostclient"
	"github.com/malmoos/malmo/internal/lifecycle"
	"github.com/malmoos/malmo/internal/manifest"
	"github.com/malmoos/malmo/internal/profile"
	"github.com/malmoos/malmo/internal/protocol"
	"github.com/malmoos/malmo/internal/store"
	"github.com/malmoos/malmo/internal/systemlive"
)

type Server struct {
	store    *store.Store
	catalog  *catalog.Catalog
	life     *lifecycle.Manager
	bus      *events.Bus
	auth     *auth.Manager
	throttle *auth.LoginThrottle
	host     *hostclient.Client
	auditor  *audit.Recorder
	health   *health.Manager
	live     *systemlive.Hub
	applogs  *applog.Registry
	streams  *streamCap
	limiter  *rateLimiter
	jobs     *Jobs

	// Environment profile and hosted-only provisioning identity, set once at
	// startup via SetEnvironment (ENVIRONMENT.md # Provisioning). On appliance
	// these stay zero-valued and every hosted seam is a no-op. boxID is surfaced
	// on /me; assertionKey is the portal's Ed25519 verification key the SSO
	// handler verifies ownership assertions against (GET /_malmo/sso). A nil
	// assertionKey on a hosted box means "no seed yet" — SSO returns 503.
	profile      profile.Profile
	boxID        string
	assertionKey ed25519.PublicKey

	// Staged control-plane compose dir (MALMO_CONTROL_PLANE_DIR), set once at
	// startup via SetControlPlaneDir. Read by GET /api/v1/system/version to
	// report the UI image the box is pinned to. Empty in dev, where the UI is
	// Vite and no compose exists — the version read then omits the UI field
	// rather than erroring.
	controlPlaneDir string

	// Proxies whose X-Forwarded-For the brain reads when deriving the client IP
	// the per-IP throttles key on (clientip.go, #329). Set once at startup via
	// SetTrustedProxies; nil means "trust nothing", so every request keys on its
	// peer address.
	trustedProxies []netip.Prefix
}

func NewServer(
	st *store.Store,
	cat *catalog.Catalog,
	life *lifecycle.Manager,
	bus *events.Bus,
	authMgr *auth.Manager,
	host *hostclient.Client,
	auditor *audit.Recorder,
	healthMgr *health.Manager,
	live *systemlive.Hub,
	applogs *applog.Registry,
) *Server {
	return &Server{
		store: st, catalog: cat, life: life, bus: bus,
		auth: authMgr, throttle: auth.NewLoginThrottle(), host: host, auditor: auditor,
		health: healthMgr, live: live, applogs: applogs,
		streams: newStreamCap(maxStreamsPerSession),
		limiter: newRateLimiter(time.Now),
		jobs:    newJobs(),
	}
}

// SetControlPlaneDir records where the staged control-plane compose lives
// (MALMO_CONTROL_PLANE_DIR), so GET /api/v1/system/version can report the UI
// image the box is pinned to. cmd/brain passes the same value it hands
// lifecycle.EnsureControlPlane, which is what makes the reported UI image the
// one the box actually reconciles to. Empty in dev — the version read then omits
// the UI field. Not concurrency-guarded: called before the server starts
// serving, same as SetEnvironment.
func (s *Server) SetControlPlaneDir(dir string) {
	s.controlPlaneDir = dir
}

// SetEnvironment records the resolved environment profile and, on a hosted box,
// its provisioning identity (ENVIRONMENT.md # Provisioning). cmd/brain calls it
// once at startup after reading the profile marker and (on hosted) the seed.
// box-id is surfaced on /me; assertionKey is the portal's Ed25519 verification
// key the SSO handler checks ownership assertions against — nil means the hosted
// box has no seed yet (SSO returns 503). Appliance passes an empty box-id and a
// nil key, so every hosted seam is a no-op. Not concurrency-guarded: called
// before the server starts serving.
func (s *Server) SetEnvironment(prof profile.Profile, boxID string, assertionKey ed25519.PublicKey) {
	s.profile = prof
	s.boxID = boxID
	s.assertionKey = assertionKey
	// On hosted, scope the forward-auth cookie to the box apex so the browser
	// carries it to every app subdomain (issue #305). Appliance leaves it empty,
	// which disables minting — no forward-auth cookie is ever issued there, and the
	// appliance login/logout paths stay byte-for-byte unchanged.
	if prof == profile.Hosted && boxID != "" {
		s.auth.ForwardAuthDomain = profile.HostedDashboardHost(boxID)
	}
}

// OpenAPI document identity. Shared by Handler (live serving) and
// OpenAPIDocument (build-time emission) so both describe the same surface.
const (
	openAPITitle   = "malmo brain"
	openAPIVersion = "0.0.1"
)

// Handler builds the mux: huma-registered REST routes + the raw SSE endpoint.
// The chain is CORS → auth → rate-limit → mux. CORS handles OPTIONS preflight
// (no auth needed); auth gates everything else except the small public
// allowlist; the limiter then throttles per resolved session (or per IP on the
// allowlist) before the mux dispatches (BRAIN_UI_PROTOCOL.md # Rate limiting).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig(openAPITitle, openAPIVersion))
	s.registerAll(api)

	// SSE is registered raw (huma streaming adds no value here and the raw
	// handler keeps the wire format curl-debuggable per BRAIN_UI_PROTOCOL.md).
	mux.HandleFunc("GET /api/v1/events", s.events)
	mux.HandleFunc("GET /api/v1/system/live", s.systemLive)
	mux.HandleFunc("GET /api/v1/apps/{id}/log", s.appLog)

	// Liveness probe for the control-plane updater (healthz.go). Raw, public,
	// and outside /api/v1 — an operational probe, not part of the versioned API
	// surface, so it is absent from the OpenAPI document and the TS client.
	mux.HandleFunc("GET "+healthzPath, s.healthz)

	// Catalog assets (icon/screenshots) serve raw image bytes, not JSON, so they
	// bypass huma and stay out of the OpenAPI surface — the store loads them
	// directly in <img> tags (APP_STORE.md # Catalog schema).
	mux.HandleFunc("GET /api/v1/catalog/{id}/icon", s.catalogIcon)
	mux.HandleFunc("GET /api/v1/catalog/{id}/screenshots/{n}", s.catalogScreenshot)

	// Portal-to-box SSO landing (hosted only): the portal redirects the owner's
	// browser here with a signed ownership assertion; the handler verifies it,
	// auto-creates the owner admin on first use, issues a box session, and 303s to
	// the dashboard. A redirect-and-Set-Cookie endpoint, not JSON — registered raw
	// and outside the OpenAPI surface (sso.go; cloud specs/AUTH_AND_ACCESS.md #
	// Portal-to-box SSO). Public (the assertion is the credential).
	mux.HandleFunc("GET /_malmo/sso", s.ssoLanding)

	// Hosted per-app forward-auth verify (issue #305; wired into per-app Caddy
	// routes by #306). The box Caddy's forward_auth handler calls this per request
	// to a restricted app, carrying the app request's forward-auth cookie; the
	// brain answers 200 + identity headers for the box owner, 401 otherwise. Raw
	// and outside the OpenAPI surface — a proxy-internal probe, not a client API —
	// and public to the middleware (the cookie is the credential), hosted-gated in
	// the handler. Registered method-agnostic so it answers whatever verb
	// forward_auth sends.
	mux.HandleFunc(forwardAuthVerifyPath, s.forwardAuthVerify)

	return withCORS(s.authMiddleware(s.rateLimit(mux)))
}

// registerAll registers every huma (OpenAPI-described) route on api. It is the
// single source of the REST surface, shared by Handler (live serving) and
// OpenAPIDocument (build-time spec emission), so the emitted spec can never
// drift from what the server actually serves. The raw SSE endpoints
// (/api/v1/events, /api/v1/system/live) are deliberately not registered here:
// they bypass huma and stay out of the OpenAPI surface (BRAIN_UI_PROTOCOL.md
// # Codegen — typed SSE is a separate follow-up).
func (s *Server) registerAll(api huma.API) {
	s.register(api)
	s.registerAuth(api)
	s.registerUsers(api)
	s.registerMeRoutes(api)
	s.registerSSHRoutes(api)
	s.registerHealth(api)
	s.registerNotifications(api)
	s.registerMail(api)
	s.registerSystem(api)
	s.registerSystemUpdate(api)
	s.registerFirstRun(api)
	s.registerAppSecrets(api)
	s.registerAppConfig(api)
	s.registerAppExposure(api)
}

// OpenAPIDocument builds the brain's full REST surface against a throwaway mux
// and returns the resulting OpenAPI 3 document. No server is started and no
// handler runs — huma.Register reflects the typed request/response structs to
// produce the schema, so a zero-value Server (no live dependencies) suffices.
// The one exception is health, whose registration is guarded on a non-nil
// manager (so test servers can opt out); we hand it a store-less manager here —
// never invoked, only reflected — so GET /api/v1/health and its Issue schema
// land in the committed spec and the dashboard's wire types generate from it.
// This is the build-time spec emitter behind `make openapi` and the CI
// freshness check (BRAIN_UI_PROTOCOL.md # Codegen / # CI enforcement); cmd/openapi-gen
// serializes it to api/openapi.{json,yaml}.
func OpenAPIDocument() *huma.OpenAPI {
	s := &Server{health: health.NewManager(nil)}
	api := humago.New(http.NewServeMux(), huma.DefaultConfig(openAPITitle, openAPIVersion))
	s.registerAll(api)
	return api.OpenAPI()
}

func (s *Server) register(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "list-catalog", Method: "GET", Path: "/api/v1/catalog",
		Summary: "List installable apps from the catalog",
	}, s.listCatalog)

	// The segmented store surface (issue #63): the box UI browses through these
	// same-origin, box-identity-gated endpoints instead of pulling the whole
	// catalog and filtering client-side. They mirror the control plane's public
	// /catalog/{home,category,search} (cloud specs/CATALOG.md # Serve) but are served
	// from the box's own synced snapshot, so browse needs no round trip to the
	// control plane. "home" and "search" are literal path segments; "category"
	// takes ?name= rather than a /category/{cat} path so it does not collide with the
	// /catalog/{id}/install-plan route under net/http's mux precedence.
	huma.Register(api, huma.Operation{
		OperationID: "catalog-home", Method: "GET", Path: "/api/v1/catalog/home",
		Summary: "Store landing: available categories + featured apps",
	}, s.catalogHome)

	huma.Register(api, huma.Operation{
		OperationID: "catalog-category", Method: "GET", Path: "/api/v1/catalog/category",
		Summary: "Apps in one category (+ featured), selected by ?name=",
	}, s.catalogCategory)

	huma.Register(api, huma.Operation{
		OperationID: "catalog-search", Method: "GET", Path: "/api/v1/catalog/search",
		Summary: "Search the catalog by ?q= (name, tagline, categories)",
	}, s.catalogSearch)

	huma.Register(api, huma.Operation{
		OperationID: "get-catalog-app", Method: "GET", Path: "/api/v1/catalog/{id}",
		Summary: "Full detail-page view of one catalog app",
	}, s.getCatalogApp)

	huma.Register(api, huma.Operation{
		OperationID: "list-apps", Method: "GET", Path: "/api/v1/apps",
		Summary: "List installed app instances",
	}, s.listApps)

	huma.Register(api, huma.Operation{
		OperationID: "get-app", Method: "GET", Path: "/api/v1/apps/{id}",
		Summary: "Get one app instance",
	}, s.getApp)

	huma.Register(api, huma.Operation{
		OperationID: "install-app", Method: "POST", Path: "/api/v1/apps",
		Summary: "Install an app (job)", DefaultStatus: http.StatusAccepted,
	}, s.installApp)

	huma.Register(api, huma.Operation{
		OperationID: "inspect-custom-app", Method: "POST", Path: "/api/v1/apps/custom/inspect",
		Summary: "Inspect a pasted (Door-2) compose: service names + best-effort main port (admin-only, read-only)",
	}, s.inspectCustomApp)

	huma.Register(api, huma.Operation{
		OperationID: "render-custom-overlay", Method: "POST", Path: "/api/v1/apps/custom/overlay/render",
		Summary: "Render elected Door-2 permissions as the Edit-as-YAML overlay (admin-only)",
	}, s.renderCustomOverlay)

	huma.Register(api, huma.Operation{
		OperationID: "parse-custom-overlay", Method: "POST", Path: "/api/v1/apps/custom/overlay/parse",
		Summary: "Parse + validate an Edit-as-YAML Door-2 permissions overlay back to form fields (admin-only)",
	}, s.parseCustomOverlay)

	huma.Register(api, huma.Operation{
		OperationID: "install-custom-app", Method: "POST", Path: "/api/v1/apps/custom",
		Summary: "Install a user-pasted (Door-2) compose (job)", DefaultStatus: http.StatusAccepted,
	}, s.installCustomApp)

	huma.Register(api, huma.Operation{
		OperationID: "uninstall-app", Method: "DELETE", Path: "/api/v1/apps/{id}",
		Summary: "Uninstall an app (job)", DefaultStatus: http.StatusAccepted,
	}, s.uninstallApp)

	huma.Register(api, huma.Operation{
		OperationID: "stop-app", Method: "POST", Path: "/api/v1/apps/{id}/stop",
		Summary: "Stop a running app instance (job)", DefaultStatus: http.StatusAccepted,
	}, s.stopApp)

	huma.Register(api, huma.Operation{
		OperationID: "start-app", Method: "POST", Path: "/api/v1/apps/{id}/start",
		Summary: "Start a stopped app instance (job)", DefaultStatus: http.StatusAccepted,
	}, s.startApp)

	huma.Register(api, huma.Operation{
		OperationID: "get-job", Method: "GET", Path: "/api/v1/jobs/{id}",
		Summary: "Get job status",
	}, s.getJob)

	huma.Register(api, huma.Operation{
		OperationID: "get-install-plan", Method: "GET", Path: "/api/v1/catalog/{id}/install-plan",
		Summary: "Permission/scope plan for installing a catalog app",
	}, s.installPlan)
}

// --- DTOs ----------------------------------------------------------------

type InstanceDTO struct {
	ID            string `json:"id"`
	ManifestID    string `json:"manifest_id"`
	Name          string `json:"name"`
	Slug          string `json:"slug"`
	Version       string `json:"version"`
	State         string `json:"state"`
	URL           string `json:"url"`
	OwnerUserID   string `json:"owner_user_id"`
	OwnerUsername string `json:"owner_username"`
	Scope         string `json:"scope"`
	// Exposure is the per-app access gate (#306): "restricted" (owner-only, behind
	// the box forward-auth) or "public" (anonymous). Meaningful on hosted, where
	// the dashboard shows the Only-me / Public toggle (#307); always "public" on
	// the appliance, which has no public app subdomains.
	Exposure  string `json:"exposure" enum:"restricted,public"`
	IconURL   string `json:"icon_url,omitempty"`
	IconGlyph string `json:"icon_glyph,omitempty"`
	// Mail fields are detail-page enrichments set only by getApp (list
	// responses omit them): MailSupported reports the manifest's mail block,
	// MailProviderID the current binding ("" ⇒ unbound). They drive the
	// rebind picker (SERVICE_PROVISIONING.md # BYO outgoing mail).
	MailSupported  bool   `json:"mail_supported,omitempty"`
	MailProviderID string `json:"mail_provider_id,omitempty"`
	// PublicPaths are the request paths the app's manifest declares as anonymous
	// even while Exposure is "restricted" (#415). Detail-page enrichment, like the
	// mail fields. It is read from the instance's own manifest copy — the same
	// file the route builder reads — so the dashboard label cannot claim a
	// narrower app than Caddy is actually serving. Empty ⇒ the access toggle means
	// exactly what it says.
	PublicPaths []string `json:"public_paths,omitempty"`
}

func (s *Server) toDTO(i store.Instance, ownerUsername string, e *catalog.Entry) InstanceDTO {
	// On hosted, the app's sole URL is public HTTPS at
	// "<slug>.<box-id>.malmo.network" (ENVIRONMENT.md # Networking & discovery) —
	// no mDNS, no ".local" fallback. On appliance, prefer the name actually
	// announced over Avahi (MDNSName), which may be the box-qualified collision
	// fallback "<slug>-<box>.local"; fall back to the reconstructed primary
	// "<slug>.local" for rows predating the published-name plumbing or where mDNS
	// publish failed.
	url := ""
	switch {
	case s.profile == profile.Hosted && s.boxID != "" && i.Slug != "":
		url = profile.HostedAppURL(s.boxID, i.Slug)
	case i.MDNSName != "":
		url = "http://" + i.MDNSName
	case i.Slug != "":
		url = "http://" + i.Slug + protocol.AppHostSuffix
	}
	dto := InstanceDTO{
		ID: i.ID, ManifestID: i.ManifestID, Name: i.Name, Slug: i.Slug,
		Version: i.Version, State: i.State, URL: url,
		OwnerUserID: i.OwnerUserID, OwnerUsername: ownerUsername, Scope: i.Scope,
		Exposure: i.Exposure,
	}
	if e != nil {
		dto.IconURL = e.IconURL
		dto.IconGlyph = e.IconGlyph
	}
	return dto
}

// withPublicPaths fills in the app's manifest-declared anonymous paths (#415).
// Every response that carries an app's exposure must carry these too: the
// dashboard's access label is built from the pair, and a response with the
// exposure but not the paths says "Only me" about an app that is partly open.
//
// The source is the INSTANCE's manifest copy, not the catalog's. That is the
// file the route builder read, so the label cannot claim a narrower app than
// Caddy is serving even after the catalog moves on. A missing or unreadable copy
// leaves the field empty rather than failing the request — the app's own detail
// page must still render.
func (s *Server) withPublicPaths(dto *InstanceDTO) {
	if man, err := s.life.InstanceManifest(dto.ID); err == nil {
		dto.PublicPaths = man.Access.PublicPaths
	}
}

// --- handlers ------------------------------------------------------------

func (s *Server) listCatalog(ctx context.Context, _ *struct{}) (*struct {
	Body struct {
		Apps []catalog.Entry `json:"apps"`
	}
}, error) {
	apps, err := s.catalog.List()
	if err != nil {
		return nil, huma.Error500InternalServerError("catalog read failed", err)
	}
	out := &struct {
		Body struct {
			Apps []catalog.Entry `json:"apps"`
		}
	}{}
	out.Body.Apps = apps
	return out, nil
}

func (s *Server) getCatalogApp(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{ Body catalog.Detail }, error) {
	d, err := s.catalog.Detail(in.ID)
	if errors.Is(err, catalog.ErrNotFound) {
		return nil, huma.Error404NotFound("no such app")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("catalog read failed", err)
	}
	return &struct{ Body catalog.Detail }{Body: d}, nil
}

// catalogHome serves the store landing (issue #63): the categories present on this
// box's surface plus the curated featured row, so the browser never pulls the whole
// catalog to render the entry point.
func (s *Server) catalogHome(ctx context.Context, _ *struct{}) (*struct{ Body catalog.Home }, error) {
	h, err := s.catalog.Home()
	if err != nil {
		return nil, huma.Error500InternalServerError("catalog read failed", err)
	}
	return &struct{ Body catalog.Home }{Body: h}, nil
}

// catalogCategory serves one category's apps (+ featured), selected by ?name=. An
// empty or unknown category is a 404, matching the flat detail route's not-found
// semantics — the UI only ever links categories the landing advertised.
func (s *Server) catalogCategory(ctx context.Context, in *struct {
	Name string `query:"name"`
}) (*struct{ Body catalog.CategoryPage }, error) {
	c, err := s.catalog.Category(in.Name)
	if errors.Is(err, catalog.ErrNotFound) {
		return nil, huma.Error404NotFound("no such category")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("catalog read failed", err)
	}
	return &struct{ Body catalog.CategoryPage }{Body: c}, nil
}

// catalogSearch serves the apps matching ?q= over name, tagline, and categories. A
// blank query returns an empty list (search narrows; browse is the whole store), so
// the field clears back to no results rather than dumping the catalog.
func (s *Server) catalogSearch(ctx context.Context, in *struct {
	Q string `query:"q"`
}) (*struct {
	Body struct {
		Apps []catalog.Entry `json:"apps"`
	}
}, error) {
	apps, err := s.catalog.Search(in.Q)
	if err != nil {
		return nil, huma.Error500InternalServerError("catalog read failed", err)
	}
	out := &struct {
		Body struct {
			Apps []catalog.Entry `json:"apps"`
		}
	}{}
	out.Body.Apps = apps
	return out, nil
}

// catalogIcon and catalogScreenshot serve an app's raw image bytes from the
// catalog directory. They resolve the on-disk path through the catalog (which
// guards against path traversal) and hand off to http.ServeFile, which sets the
// content-type and handles range/conditional requests.
func (s *Server) catalogIcon(w http.ResponseWriter, r *http.Request) {
	path, err := s.catalog.IconPath(r.PathValue("id"))
	if s.serveAsset(w, r, path, err) {
		http.ServeFile(w, r, path)
	}
}

func (s *Server) catalogScreenshot(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	path, perr := s.catalog.ScreenshotPath(r.PathValue("id"), n)
	if s.serveAsset(w, r, path, perr) {
		http.ServeFile(w, r, path)
	}
}

// serveAsset maps a catalog asset-path lookup error to the right status and
// reports whether the caller should proceed to serve the file. ErrNotFound →
// 404 (unknown app / no such asset); any other error → 500.
func (s *Server) serveAsset(w http.ResponseWriter, _ *http.Request, _ string, err error) bool {
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
		return false
	case err != nil:
		slog.Error("catalog asset lookup failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return false
	}
	return true
}

func (s *Server) listApps(ctx context.Context, _ *struct{}) (*struct {
	Body struct {
		Apps []InstanceDTO `json:"apps"`
	}
}, error) {
	id, ok := auth.FromContext(ctx)
	if !ok {
		return nil, huma.Error401Unauthorized("unauthenticated")
	}
	insts, err := s.store.ListVisibleTo(id.User.ID, id.IsAdmin())
	if err != nil {
		return nil, huma.Error500InternalServerError("list failed", err)
	}
	names, err := s.ownerUsernames()
	if err != nil {
		return nil, huma.Error500InternalServerError("owner lookup failed", err)
	}
	out := &struct {
		Body struct {
			Apps []InstanceDTO `json:"apps"`
		}
	}{}
	out.Body.Apps = []InstanceDTO{}
	for _, i := range insts {
		// Enrich by id, not via List(): an app unlisted after install still owns a
		// card here. Entry doesn't apply the store-visibility filter. Best-effort —
		// icon/name fields are cosmetic, so a lookup miss just renders the fallback.
		var ce *catalog.Entry
		if e, err := s.catalog.Entry(i.ManifestID); err == nil {
			ce = &e
		}
		dto := s.toDTO(i, names[i.OwnerUserID], ce)
		// The tile badge is exposure + public paths together, the same pair the
		// detail page's access label is built from — a list DTO carrying only the
		// exposure would draw a fully closed app that is in fact partly open.
		// Costs one manifest read per app; best-effort, so a missing copy just
		// leaves the field empty.
		s.withPublicPaths(&dto)
		out.Body.Apps = append(out.Body.Apps, dto)
	}
	return out, nil
}

func (s *Server) getApp(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{ Body InstanceDTO }, error) {
	id, ok := auth.FromContext(ctx)
	if !ok {
		return nil, huma.Error401Unauthorized("unauthenticated")
	}
	i, err := s.store.Get(in.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, huma.Error404NotFound("no such app")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("get failed", err)
	}
	// Leak guard: a member asking for someone else's personal instance gets 404
	// (not 403), so existence of another user's app isn't disclosed.
	if !canSee(id, i) {
		return nil, huma.Error404NotFound("no such app")
	}
	owner, err := s.store.GetUser(i.OwnerUserID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, huma.Error500InternalServerError("owner lookup failed", err)
	}
	// Enrich by id via Entry (honest), not Detail (store-filtered): an installed
	// app that has since been unlisted must still render its own card.
	var catEntry *catalog.Entry
	if e, err := s.catalog.Entry(i.ManifestID); err == nil {
		catEntry = &e
	}
	dto := s.toDTO(i, owner.Username, catEntry)
	s.withPublicPaths(&dto)
	// Mail enrichment for the rebind picker. The manifest comes from the
	// INSTANCE's own copy, the one the installer persisted (#434): the app is
	// already installed, so asking the catalog service would put a routine page
	// load behind the network and hide the picker for an app the store no longer
	// publishes. An unreadable copy leaves the picker hidden rather than failing
	// the request.
	if man, err := s.life.InstanceManifest(i.ID); err == nil && man.Mail != nil {
		dto.MailSupported = true
		if mp, err := s.store.GetInstanceMailProvider(i.ID); err == nil {
			dto.MailProviderID = mp.ID
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, huma.Error500InternalServerError("mail binding lookup failed", err)
		}
	}
	return &struct{ Body InstanceDTO }{Body: dto}, nil
}

// canSee mirrors store.ListVisibleTo's visibility predicate for a single
// instance: admins see all; members see household instances and their own
// personal instances. KEEP IN SYNC with store.ListVisibleTo's SQL WHERE clause
// and with checkDuplicate/uninstallApp's callers — when per-app member grants
// land (NEXT.md), both this and the SQL must change together.
func canSee(id auth.Identity, i store.Instance) bool {
	return id.IsAdmin() || i.Scope == store.ScopeHousehold || i.OwnerUserID == id.User.ID
}

// ownerUsernames maps user id -> username for rendering instance ownership in
// the list response. One query; N is small at v1 user counts.
func (s *Server) ownerUsernames() (map[string]string, error) {
	users, err := s.store.ListUsers()
	if err != nil {
		return nil, err
	}
	m := make(map[string]string, len(users))
	for _, u := range users {
		m[u.ID] = u.Username
	}
	return m, nil
}

func (s *Server) installApp(ctx context.Context, in *struct {
	Body struct {
		ManifestID string `json:"manifest_id"`
		Scope      string `json:"scope,omitempty"`   // "household" | "personal"; default household for admins, forced personal for members
		Confirm    bool   `json:"confirm,omitempty"` // proceed past the duplicate-install warning
		Config     struct {
			Folders []FolderElection `json:"folders,omitempty"`
			// MailProviderID binds a mail-capable app to a registered outgoing-mail
			// provider (SERVICE_PROVISIONING.md # BYO outgoing mail). Empty ⇒ unbound.
			MailProviderID string `json:"mail_provider_id,omitempty"`
			// Fields carries the user-supplied config answers keyed by app_env
			// (APP_MANIFEST.md # D4). Validated against the manifest; required fields
			// must be present, optional-blank injects nothing.
			Fields map[string]string `json:"fields,omitempty"`
		} `json:"config,omitempty"` // per-folder source/subfolder elections (consent screen)
	}
}) (*struct{ Body Job }, error) {
	manifestID := in.Body.ManifestID
	if manifestID == "" {
		return nil, huma.Error422UnprocessableEntity("manifest_id is required")
	}
	owner, scope, err := s.resolveOwnerScope(ctx, in.Body.Scope, audit.ActionAppInstall, map[string]any{"manifest_id": manifestID})
	if err != nil {
		return nil, err
	}
	// Load the manifest to validate the folder elections authoritatively (the
	// install-plan endpoint is advisory). A validation failure is an
	// elevation-class mutation rejection, so it audits success=false.
	// ONE load for the whole install. The elections below are validated against
	// this exact manifest, and this exact pair is handed to the job that runs the
	// transaction — the job must never re-fetch, or it could install a payload
	// nothing validated (lifecycle.CatalogApp).
	app, err := s.life.LoadCatalogApp(ctx, manifestID)
	if errors.Is(err, catalog.ErrNotFound) {
		return nil, huma.Error404NotFound("no such catalog app")
	}
	if err != nil {
		slog.Error("install: catalog entry failed to load", "manifest_id", manifestID, "err", err)
		return nil, huma.Error500InternalServerError("catalog entry is malformed")
	}
	man := app.Manifest
	// An unlisted app (`listed: false`) is pulled from the store: not installable.
	// Treat it as absent — same 404 as a missing manifest — so a stale store link
	// or direct API call can't install a deliberately-withdrawn app.
	if !man.IsListed() {
		return nil, huma.Error404NotFound("no such catalog app")
	}
	mounts, err := resolveElections(man, scope, in.Body.Config.Folders)
	if err != nil {
		s.auditor.Record(ctx, audit.ActionAppInstall, audit.Target{Kind: "app"},
			map[string]any{"manifest_id": manifestID, "scope": scope, "owner_user_id": owner.UserID}, false)
		return nil, err
	}
	// Validate the mail-provider election authoritatively, like folder
	// elections above: the app must declare mail support and the provider must
	// exist. Same elevation-class rejection ⇒ audits success=false.
	mailProviderID := in.Body.Config.MailProviderID
	if mailProviderID != "" {
		failMeta := map[string]any{"manifest_id": manifestID, "scope": scope, "owner_user_id": owner.UserID}
		if man.Mail == nil {
			s.auditor.Record(ctx, audit.ActionAppInstall, audit.Target{Kind: "app"}, failMeta, false)
			return nil, huma.Error422UnprocessableEntity("this app does not support outgoing email")
		}
		if _, err := s.store.GetMailProvider(mailProviderID); errors.Is(err, store.ErrNotFound) {
			s.auditor.Record(ctx, audit.ActionAppInstall, audit.Target{Kind: "app"}, failMeta, false)
			return nil, huma.Error422UnprocessableEntity("no such mail provider")
		} else if err != nil {
			slog.Error("install: mail provider lookup failed", "manifest_id", manifestID, "err", err)
			return nil, huma.Error500InternalServerError("mail provider lookup failed")
		}
	}
	// Resolve the user-supplied config answers against the manifest, like the
	// folder/mail elections above. An invalid answer is an elevation-class
	// rejection ⇒ audits success=false.
	config, err := resolveInstallConfig(man, in.Body.Config.Fields)
	if err != nil {
		s.auditor.Record(ctx, audit.ActionAppInstall, audit.Target{Kind: "app"},
			map[string]any{"manifest_id": manifestID, "scope": scope, "owner_user_id": owner.UserID}, false)
		return nil, err
	}
	if err := s.checkDuplicate(ctx, manifestID, in.Body.Confirm, audit.ActionAppInstall); err != nil {
		return nil, err
	}
	jobCtx := ctx // capture for audit inside the job goroutine
	job := s.jobs.run("app-install", func(job *Job) (map[string]any, error) {
		inst, err := s.life.Install(context.Background(), app, owner, scope, mounts, mailProviderID, config, job.setStep)
		target := audit.Target{Kind: "app"}
		// confirm records a deliberate override of the duplicate-install warning,
		// so the Activity view can see "installed a second copy on purpose".
		meta := map[string]any{"manifest_id": manifestID, "scope": scope, "owner_user_id": owner.UserID, "confirm": in.Body.Confirm}
		if err == nil {
			target.ID = inst.ID
			meta["slug"] = inst.Slug
		}
		s.auditor.Record(jobCtx, audit.ActionAppInstall, target, meta, err == nil)
		if err != nil {
			return nil, err
		}
		return map[string]any{"instance_id": inst.ID, "slug": inst.Slug}, nil
	})
	return &struct{ Body Job }{Body: job.snapshot()}, nil
}

// resolveOwnerScope applies the install authorization table (DASHBOARD.md #
// install authorization): members always install a personal instance owned by
// themselves; admins choose household (the default) or personal. It audits a
// failed attempt when a member explicitly requests a household install.
// scopeMenu() is the read-path mirror of this authorization table — it drives
// the install-plan consent screen. Keep the two in sync.
// meta is the action-specific audit metadata (e.g. {"manifest_id": …} for a
// catalog install, {"name": …} for a custom one); resolveOwnerScope adds the
// requested scope before recording a rejected attempt.
func (s *Server) resolveOwnerScope(ctx context.Context, requested, action string, meta map[string]any) (lifecycle.Owner, string, error) {
	id, ok := auth.FromContext(ctx)
	if !ok {
		return lifecycle.Owner{}, "", huma.Error401Unauthorized("unauthenticated")
	}
	owner := lifecycle.Owner{UserID: id.User.ID, Username: id.User.Username}
	scope := requested
	if id.IsAdmin() {
		if scope == "" {
			scope = store.ScopeHousehold
		}
	} else {
		if scope == store.ScopeHousehold {
			meta["scope"] = scope
			s.auditor.Record(ctx, action, audit.Target{Kind: "app"}, meta, false)
			return lifecycle.Owner{}, "", huma.Error403Forbidden("members can only install personal instances")
		}
		scope = store.ScopePersonal
	}
	if scope != store.ScopeHousehold && scope != store.ScopePersonal {
		return lifecycle.Owner{}, "", huma.Error422UnprocessableEntity("scope must be household or personal")
	}
	return owner, scope, nil
}

// checkDuplicate implements warn-don't-block (DASHBOARD.md # warn, don't block).
// This is an API-layer UX guarantee, NOT a lifecycle invariant: lifecycle.Install
// does not enforce it, so any non-API caller (a CLI, reconciler, import script)
// installs without the warning by design. Duplicate installs are always allowed;
// this only gates the interactive confirm step.
// When an instance of this manifest already exists that the caller could see
// (a household instance or their own personal one) and they haven't confirmed,
// return 409 "duplicate-install" carrying a summary of the existing copies so
// the UI can offer "open it" or "install my own copy". A confirmed retry skips
// the check.
func (s *Server) checkDuplicate(ctx context.Context, manifestID string, confirm bool, action string) error {
	if confirm {
		return nil
	}
	id, _ := auth.FromContext(ctx)
	existing, err := s.store.InstancesByManifest(manifestID)
	if err != nil {
		return huma.Error500InternalServerError("duplicate check failed", err)
	}
	var summaries []error
	for _, i := range existing {
		if !canSee(id, i) {
			continue
		}
		if i.Scope == store.ScopeHousehold {
			summaries = append(summaries, fmt.Errorf("%s is already installed as a household app", i.Name))
		} else {
			summaries = append(summaries, fmt.Errorf("%s is already installed as your personal app", i.Name))
		}
	}
	if len(summaries) == 0 {
		return nil
	}
	s.auditor.Record(ctx, action, audit.Target{Kind: "app"},
		map[string]any{"manifest_id": manifestID, "duplicate": true}, false)
	return huma.Error409Conflict("duplicate-install", summaries...)
}

// inspectCustomApp is the read-only companion to installCustomApp: it parses a
// pasted compose so the Door-2 form can drive its service dropdown and prefill
// the main port (DASHBOARD.md # The form). Admin-only (Door 2 is admin-only) and
// host-call-free; admission is deliberately NOT run here — it gates on submit,
// where its field-named rejections are coached inline.
func (s *Server) inspectCustomApp(ctx context.Context, in *struct {
	Body struct {
		Compose     string `json:"compose"`
		MainService string `json:"main_service,omitempty"` // optional; lets the form re-infer the port after picking a service
	}
}) (*struct {
	Body struct {
		Services []string `json:"services"`
		MainPort int      `json:"main_port"` // 0 = could not infer; the form asks
	}
}, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	services, err := manifest.ComposeServiceNames([]byte(in.Body.Compose))
	if err != nil {
		return nil, huma.Error422UnprocessableEntity(err.Error())
	}
	// Resolve which service's expose: to read for the port prefill: the one the
	// form already picked, or the sole service when there's exactly one.
	main := in.Body.MainService
	if main == "" && len(services) == 1 {
		main = services[0]
	}
	out := &struct {
		Body struct {
			Services []string `json:"services"`
			MainPort int      `json:"main_port"`
		}
	}{}
	out.Body.Services = services
	out.Body.MainPort = manifest.InferMainPort([]byte(in.Body.Compose), main)
	return out, nil
}

func (s *Server) installCustomApp(ctx context.Context, in *struct {
	Body struct {
		Name        string            `json:"name"`
		Compose     string            `json:"compose"`
		MainService string            `json:"main_service,omitempty"`
		MainPort    int               `json:"main_port"`
		Scope       string            `json:"scope,omitempty"` // "household" | "personal"; default household for admins, forced personal for members
		Permissions *customPermsInput `json:"permissions,omitempty"`
		Overlay     string            `json:"overlay,omitempty"` // Edit-as-YAML escape hatch; wins over permissions when set (DASHBOARD.md # Form is a projection)
	}
}) (*struct{ Body Job }, error) {
	// Admin-only gate: elevation-class rejection audits before synthesize/admission (APP_ISOLATION.md, DECISIONS.md 2026-06-02).
	if err := requireAdmin(ctx); err != nil {
		s.auditor.Record(ctx, audit.ActionAppCustomCreate, audit.Target{Kind: "app"},
			map[string]any{"name": in.Body.Name}, false)
		return nil, err
	}
	// The form sends structured permissions; the Edit-as-YAML toggle sends a raw
	// overlay instead, parsed + validated through the same gate (DASHBOARD.md #
	// Permissions). A malformed overlay surfaces inline as a 422.
	perms, err := resolveCustomPerms(in.Body.Permissions, in.Body.Overlay)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity(err.Error())
	}
	spec := lifecycle.CustomSpec{
		Name:        in.Body.Name,
		Compose:     in.Body.Compose,
		MainService: in.Body.MainService,
		MainPort:    in.Body.MainPort,
		Permissions: perms,
	}
	owner, scope, err := s.resolveOwnerScope(ctx, in.Body.Scope, audit.ActionAppCustomCreate, map[string]any{"name": spec.Name})
	if err != nil {
		return nil, err
	}

	// Sync pre-checks so the user gets immediate, specific feedback instead of
	// a failed job: synthesize (catches missing name/port, ambiguous service)
	// and admit the compose (catches ports:/privileged/etc).
	if _, _, err := manifest.Synthesize(spec.Name, []byte(spec.Compose), spec.MainService, spec.MainPort, perms); err != nil {
		return nil, huma.Error422UnprocessableEntity(err.Error())
	}
	if err := admission.Check(ctx, []byte(spec.Compose)); err != nil {
		return nil, huma.Error422UnprocessableEntity(err.Error())
	}

	jobCtx := ctx
	job := s.jobs.run("app-install", func(job *Job) (map[string]any, error) {
		inst, err := s.life.InstallCustom(context.Background(), spec, owner, scope, job.setStep)
		target := audit.Target{Kind: "app"}
		meta := map[string]any{"name": spec.Name, "scope": scope, "owner_user_id": owner.UserID}
		if err == nil {
			target.ID = inst.ID
			meta["slug"] = inst.Slug
		}
		s.auditor.Record(jobCtx, audit.ActionAppCustomCreate, target, meta, err == nil)
		if err != nil {
			return nil, err
		}
		return map[string]any{"instance_id": inst.ID, "slug": inst.Slug}, nil
	})
	return &struct{ Body Job }{Body: job.snapshot()}, nil
}

func (s *Server) uninstallApp(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{ Body Job }, error) {
	id := in.ID
	actor, ok := auth.FromContext(ctx)
	if !ok {
		return nil, huma.Error401Unauthorized("unauthenticated")
	}
	inst, err := s.store.Get(id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, huma.Error404NotFound("no such app")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("get failed", err)
	}
	// Leak guard, then authorization: a member can uninstall only their own
	// personal instances; household instances are admin-only (DASHBOARD.md #
	// install authorization — uninstall mirrors it).
	if !canSee(actor, inst) {
		return nil, huma.Error404NotFound("no such app")
	}
	if !actor.IsAdmin() && inst.Scope == store.ScopeHousehold {
		s.auditor.Record(ctx, audit.ActionAppUninstall, audit.Target{Kind: "app", ID: id}, nil, false)
		return nil, huma.Error403Forbidden("only an admin can uninstall a household app")
	}
	jobCtx := ctx
	job := s.jobs.run("app-uninstall", func(job *Job) (map[string]any, error) {
		job.setStep("tearing_down")
		err := s.life.Uninstall(context.Background(), id)
		s.auditor.Record(jobCtx, audit.ActionAppUninstall, audit.Target{Kind: "app", ID: id}, nil, err == nil)
		if err != nil {
			return nil, err
		}
		return map[string]any{"instance_id": id}, nil
	})
	return &struct{ Body Job }{Body: job.snapshot()}, nil
}

// authorizeAppMutation is the shared gate for stop/start: the actor must be
// able to see the instance (else 404, no existence leak), and household
// instances are admin-only — a member may only act on their own personal
// instance (DASHBOARD.md # install authorization, mirrored by stop/start/
// uninstall). Returns the loaded instance on success.
func (s *Server) authorizeAppMutation(ctx context.Context, id string) (store.Instance, error) {
	actor, ok := auth.FromContext(ctx)
	if !ok {
		return store.Instance{}, huma.Error401Unauthorized("unauthenticated")
	}
	inst, err := s.store.Get(id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Instance{}, huma.Error404NotFound("no such app")
	}
	if err != nil {
		return store.Instance{}, huma.Error500InternalServerError("get failed", err)
	}
	if !canSee(actor, inst) {
		return store.Instance{}, huma.Error404NotFound("no such app")
	}
	if !actor.IsAdmin() && inst.Scope == store.ScopeHousehold {
		return store.Instance{}, huma.Error403Forbidden("only an admin can control a household app")
	}
	return inst, nil
}

func (s *Server) stopApp(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{ Body Job }, error) {
	id := in.ID
	inst, err := s.authorizeAppMutation(ctx, id)
	if err != nil {
		return nil, err
	}
	// Synchronous guard so an illegal transition is a clean 409 instead of a
	// failed job the UI has to poll for. Start re-checks under the per-instance
	// lock, which is the authority if state races between here and the goroutine.
	if inst.State != "running" {
		return nil, huma.Error409Conflict("app is not running")
	}
	job := s.jobs.run("app-stop", func(job *Job) (map[string]any, error) {
		job.setStep("stopping")
		if err := s.life.Stop(context.Background(), id); err != nil {
			return nil, err
		}
		return map[string]any{"instance_id": id}, nil
	})
	return &struct{ Body Job }{Body: job.snapshot()}, nil
}

func (s *Server) startApp(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{ Body Job }, error) {
	id := in.ID
	inst, err := s.authorizeAppMutation(ctx, id)
	if err != nil {
		return nil, err
	}
	// Legal from `stopped` (start) and `failed` (click-to-retry, #154) — both run
	// the identical Start transaction. Any other state is an illegal transition.
	if inst.State != "stopped" && inst.State != "failed" {
		return nil, huma.Error409Conflict("app is not stopped or failed")
	}
	job := s.jobs.run("app-start", func(job *Job) (map[string]any, error) {
		job.setStep("starting")
		if err := s.life.Start(context.Background(), id); err != nil {
			return nil, err
		}
		return map[string]any{"instance_id": id}, nil
	})
	return &struct{ Body Job }{Body: job.snapshot()}, nil
}

func (s *Server) getJob(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{ Body Job }, error) {
	j, ok := s.jobs.get(in.ID)
	if !ok {
		return nil, huma.Error404NotFound("no such job")
	}
	return &struct{ Body Job }{Body: j.snapshot()}, nil
}

// events is the global SSE stream (BRAIN_UI_PROTOCOL.md Pattern C, stream 2).
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	release, ok := s.beginStream(w, r)
	if !ok {
		return // beginStream wrote 401 (no session) or 429 (over the per-session cap)
	}
	defer release()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, unsub := s.bus.Subscribe()
	defer unsub()

	// Initial comment so proxies flush headers immediately.
	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			data, _ := json.Marshal(ev.Data)
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.ID, ev.Kind, data)
			flusher.Flush()
		}
	}
}

// systemLive is the live system-resources stream (BRAIN_UI_PROTOCOL.md Pattern
// C, stream 3). On-demand and ref-counted by the hub: this connection opens the
// upstream host-agent poll if it's the first, and closing it stops the poll if
// it's the last. No reconnect replay — a reconnecting client resumes at the next
// `sample` (BRAIN_UI_PROTOCOL.md:179). Available to every signed-in user with no
// role gate: host-level resource state isn't per-user data (LOCAL_ANALYTICS.md #
// Privacy model). beginStream re-checks the session (belt-and-suspenders over
// authMiddleware) and reserves the stream's slot under the per-session cap
// (BRAIN_UI_PROTOCOL.md:188 — system/live counts against the ≤16-stream cap).
func (s *Server) systemLive(w http.ResponseWriter, r *http.Request) {
	release, ok := s.beginStream(w, r)
	if !ok {
		return // beginStream wrote 401 (no session) or 429 (over the per-session cap)
	}
	defer release()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, unsub := s.live.Subscribe()
	defer unsub()

	// Initial comment so proxies flush headers immediately.
	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case sample, ok := <-ch:
			if !ok {
				return
			}
			data, _ := json.Marshal(sample)
			fmt.Fprintf(w, "event: sample\ndata: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// withCORS lets the Vite dev server (different origin) call the brain during
// development. Tightened to same-origin behind Caddy in production.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
