// SPDX-License-Identifier: MIT

// Command manager is the Agent Workspace session manager: portal host, per-person
// proxy, container lifecycle and llm-gw in one process. Configuration: env
// vars only, see docs/INSTALL.md.
package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistantchat"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/catalog"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/convert"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/filetools"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/gateway/central"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/gateway/llm"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/gateway/mcp"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/health"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/house"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/logx"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/provision"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/router"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/runtime"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// newPortal is set by an optional file of this package (portal.go, added
// with the portal package). Without it the portal host answers 503.
var portalTaskAssistants *assistantchat.Service
var portalKnowledge *store.Store

var newPortal func(svc *lifecycle.Service, cfg config.Config, pdf pdfFunc) (http.Handler, error)

type pdfFunc = func(ctx context.Context, md []byte, name string) ([]byte, error)

func main() {
	log, err := logx.Setup(os.Stdout)
	if err == nil {
		err = run(log)
	}
	if err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	if cfg.AdminToken == "" {
		log.Warn("OC_ADMIN_TOKEN is empty; all /admin/* requests will be refused")
	}
	instance.Select(cfg.Instance)
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.LoadAssistants(context.Background()); err != nil {
		return err
	}
	if id := os.Getenv("OC_RESET_ASSISTANT"); id != "" {
		if err := resetAssistant(context.Background(), st, cfg.Instance, id); err != nil {
			return err
		}
	}
	cfg.Instance = instance.Current()
	if err := st.CheckIdentityIssuer(context.Background(), cfg.IdentityIssuer); err != nil {
		return err
	}
	if cfg.Instance.Capabilities.Projects && cfg.CatalogDir == "" {
		return errors.New("capabilities.projects requires OC_CATALOG_DIR; set capabilities.projects to false to disable project scoping")
	}
	rt, err := runtime.NewDocker()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	upstream, err := url.Parse(cfg.LLMUpstreamURL)
	if err != nil {
		return err
	}
	var mcpUpstream *url.URL
	if cfg.MCPUpstreamURL != "" {
		mcpUpstream, err = url.Parse(cfg.MCPUpstreamURL)
		if err != nil {
			return err
		}
	} else {
		log.Info("central MCP integration disabled: no upstream configured", "option", "OC_MCP_UPSTREAM_URL")
	}
	bud := budget(cfg, st)
	gw := llm.New(llm.Gateway{Accounting: &cfg.Accounting, Upstream: upstream, APIKey: cfg.LLMUpstreamKey, Flavor: cfg.LLMUpstreamFlavor, SlotOf: cfg.SlotOf, Users: st, Usage: st, Log: log, Budget: bud, MaxBodyMB: &cfg.LLMMaxBodyMB})
	mcp.SetImageDecodeParallel(cfg.ImageDecodeParallel)
	searx, err := url.Parse(cfg.SearXNGURL)
	if err != nil {
		return err
	}
	svc := lifecycle.New(cfg, rt, st, log)
	var servers []*http.Server
	var loopDone <-chan struct{}
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		shutdownManager(shutdown, servers, stop, loopDone, svc.Close)
	}()
	var pdf pdfFunc
	workspace := mcp.Server{SearXNG: searx, SlotOf: cfg.SlotOf, Users: st, Usage: st, Log: log, Lang: st.Lang,
		Files: svc, DenyHosts: fetchDenyHosts(cfg), Reserve: bud.ReserveEUR, SearchChargeEUR: &cfg.SearchChargeEUR, FetchChargeEUR: &cfg.FetchChargeEUR, PageCacheMB: &cfg.PageCacheMB}
	if cfg.Instance.Enabled("workspace-rooms") && cfg.RoomsURL != nil {
		workspace.Rooms = mcp.HouseRooms(cfg.RoomsURL, cfg.BikesURL) // D61: read-only, catalog opt-in "rooms"
	}
	if cfg.Instance.Enabled("feedback") {
		workspace.Feedback, workspace.FeedbackContext = st, svc.FeedbackContext
		portalFeedback = feedback{st, log}
	}
	if cfg.Instance.Enabled("images") && cfg.VisionModel != "" {
		workspace.Look = func(ctx context.Context, u store.User, session, question string, jpeg []byte) (string, error) {
			return gw.Look(ctx, u, session, cfg.VisionModel, question, jpeg, 4096) // room for the model's thinking
		}
	}
	if cfg.Instance.Enabled("images") && len(cfg.ImageModels) > 0 {
		workspace.CanEdit = cfg.ImageEditModel != ""
		workspace.Generate = func(ctx context.Context, u store.User, session, prompt, size string, sourceImage []byte) ([]byte, string, error) {
			if sourceImage != nil {
				return gw.Image(ctx, u, session, []string{cfg.ImageEditModel}, prompt, size, sourceImage)
			}
			return gw.Image(ctx, u, session, cfg.ImageModels, prompt, size, nil)
		}
	}
	if cfg.Instance.Enabled("file-tools") && cfg.FileToolsURL != "" {
		workspace.ProcessFiles = filetools.New(cfg.FileToolsURL)
		if cfg.HandwritingModel != "" {
			workspace.Transcribe = func(ctx context.Context, u store.User, session string, png []byte) (llm.Transcription, error) {
				return gw.Transcribe(ctx, u, session, cfg.HandwritingModel, png)
			}
		}
	}
	if cfg.Instance.Enabled("pdf") && cfg.PDFURL != "" {
		pdf = convert.New(cfg.PDFURL).MarkdownToPDF
		workspace.Files, workspace.PDF = svc, pdf
	}
	portalIdentityGuard = st
	portalOnboarding, portalConsent, portalPrefs, portalSpent = onboarding{st}, consentStore{st}, st, st.SpentSince
	if len(cfg.House) > 0 {
		hs, err := house.New(cfg.House, st, cfg.MasterKey, log)
		if err != nil {
			return err
		}
		go hs.Loop(ctx, 5*time.Minute) // failed revocations
		// Revocation and account cleanup remain available when the integration is disabled.
		svc.Disconnect = hs.DisconnectAll
		hs.Invalidate = func(ctx context.Context, uid, service string) error { return svc.CloseOffice(ctx, uid) }
		if cfg.Instance.Enabled("office") && cfg.OfficeImage != "" {
			svc.OfficeCreds = func(ctx context.Context, uid string) (string, string, error) {
				_, login, password, err := hs.Creds(ctx, uid, cfg.House[0].ID)
				return login, password, err
			}
			workspace.Office = svc.CallOffice
		}
		if cfg.Instance.Enabled("nextcloud") {
			portalConns = conns{hs}
			workspace.Nextcloud = ncTools(hs, cfg.House[0], svc)
			ncs := newSync(hs, cfg.House[0], st, svc, log)
			workspace.Nextcloud.Sync, portalSync = ncs, syncPortal{ncs}
			go ncs.Loop(ctx)
		}
	}
	var cat *catalog.Service
	if cfg.CatalogDir != "" {
		items, _ := catalog.Load(cfg.CatalogDir) // unrelated broken entries retain catalog skip behaviour
		if err := catalog.ValidateDefaults(items); err != nil {
			return err
		}
		if err := central.ValidateSources(instance.Current().KnowledgeSources, items); err != nil {
			return err
		}
		cat = &catalog.Service{Dir: cfg.CatalogDir, Store: st, Projects: st, SessionDir: svc.SessionDir, Shell: svc.Shell, Client: svc.Client, Running: svc.RunningUIDs, Log: log}
		workspace.ProjectScoped, workspace.ProjectAccess, svc.PrepareCapabilities = cat.ProjectEnabled(), cat.ProjectAccess, cat.Prepare
		workspace.Grants, svc.AfterStart, svc.AfterReap, portalTools = cat.Grants, cat.AfterStart, cat.RetryPending, tools{cat}
		// D60: tool containers of switched-on werkzeuge (Demo/Test)
		workspace.Toolboxes = &mcp.Toolboxes{ChargeEUR: &cfg.Accounting.ToolboxEUR, Run: svc.RunToolbox, SessionDir: svc.SessionDir, Reserve: bud.ReserveEUR,
			On: func(ctx context.Context, uid string) []catalog.Item {
				return cat.OnItems(ctx, uid, catalog.KindToolbox)
			}}
	}
	if cfg.Instance.Enabled("freecad") && cfg.FreeCADImage != "" && cat != nil {
		svc.FreeCADAllowed = func(ctx context.Context, uid, project string) bool {
			if st.SharedRuntimeAccess(ctx, uid) != nil {
				return false
			}
			allowed, _ := cat.Grants(catalog.WithProject(ctx, project), uid, "en")
			return allowed["freecad_open"]
		}
		workspace.FreeCAD = svc.CallFreeCAD
	}
	portalSettings = newSettings(cfg, st, svc, gw, log) // after the catalog: chains its AfterStart
	if cfg.Instance.Enabled("evaluation") && cfg.AdminGroup != "" {
		aw := newEvaluation(st, gw, log)
		svc.CollectChats = true // transcripts are kept only for the evaluation
		portalAdmin = aw
		go aw.Loop(ctx)
	}
	// Workspace-MCP on the llm-gw listener: same per-person source check, already in NO_PROXY of the containers
	portalKnowledge = st
	portalTaskAssistants = &assistantchat.Service{Store: st, PreviewComplete: gw.AssistantDraftCompletion, Complete: gw.AssistantCompletion}
	gwMux := http.NewServeMux()
	if cfg.Instance.HasBuiltinTools() {
		workspace.AssistantSession = svc.AuthorizeAssistantSession
		if cfg.Instance.Enabled("knowledge") {
			workspace.Knowledge = st
			workspace.KnowledgeSession = svc.KnowledgeSession
		}
		organisationServer := mcp.New(workspace)
		gwMux.Handle("/mcp/"+cfg.Instance.MCPServer, organisationServer)
		gwMux.Handle("/mcp/assistant/{project}/"+cfg.Instance.MCPServer, organisationServer)
		gwMux.Handle("/mcp/project/{capProject}/"+cfg.Instance.MCPServer, organisationServer)
	}
	gwMux.Handle("GET /workspace/usage", usageHandler{slotOf: cfg.SlotOf, st: st, capEUR: cfg.CostCapDayEUR, now: time.Now})
	var externalTitles func(context.Context, store.User, string) (map[string]string, error)
	if cat != nil {
		// D59: catalog mcp-central entries, LiteLLM MCP gateway with the manager's key
		centralServer := central.New(central.Server{ChargeEUR: &cfg.Accounting.MCPEUR, Upstream: mcpUpstream, APIKey: cfg.MCPUpstreamKey, SlotOf: cfg.SlotOf,
			Users: st, Usage: st, Log: log, Reserve: bud.ReserveEUR,
			ProjectScoped: cat.ProjectEnabled(), AssistantItems: cat.AssistantItems, AssistantSession: svc.AuthorizeAssistantSession,
			Assistant: func(ctx context.Context, uid string) (assistants.Pin, error) {
				// Shared network identity cannot authenticate an assistant session.
				return assistants.Pin{}, st.SharedRuntimeAccess(ctx, uid)
			}, Items: func(ctx context.Context, uid string) []catalog.Item {
				return cat.OnItems(ctx, uid, catalog.KindCentral)
			}})
		externalTitles = centralServer.DisplayTitles
		gwMux.Handle("/mcp/{name}", centralServer)
		gwMux.Handle("/mcp/project/{capProject}/{name}", centralServer)
		gwMux.Handle("/mcp/assistant/{project}/{name}", centralServer)
		// Keep existing project configurations usable during the naming transition.
		gwMux.Handle("/mcp/profile/{project}/{name}", centralServer)
		portalTaskAssistants = &assistantchat.Service{Store: st, PreviewComplete: gw.AssistantDraftCompletion, Items: cat.AssistantItems, Tools: centralServer.AssistantTools, Call: centralServer.AssistantCall, Complete: gw.AssistantCompletion}
	}
	gwMux.Handle("/", gw)

	svc.Suggest = gw.Suggest
	svc.Complete = gw.Complete
	if cfg.ProvisionSocket != "" {
		svc.Provision = provision.NewClient(cfg.ProvisionSocket).Ensure
		svc.DiskUsage = provision.NewClient(cfg.ProvisionSocket).Usage
		svc.PurgeData = provision.NewClient(cfg.ProvisionSocket).Purge
	}
	rctx, cancel := context.WithTimeout(ctx, time.Minute)
	err = svc.Reconcile(rctx)
	cancel()
	if err != nil {
		return err
	}
	loopEnded := make(chan struct{})
	loopDone = loopEnded
	go func() {
		defer close(loopEnded)
		svc.Loop(ctx)
	}()

	var portal http.Handler
	if newPortal != nil {
		if portal, err = newPortal(svc, cfg, pdf); err != nil {
			return err
		}
	}
	checker := identity.Checker{Trusted: cfg.TrustedProxy, RequiredGroup: cfg.RequiredGroup,
		Issuer: cfg.IdentityIssuer, SubjectHeader: cfg.IdentitySubjectHeader,
		Headers: cfg.IdentityHeaders, GroupsSeparator: cfg.IdentityGroupsSeparator}
	edge := router.New(router.Router{
		ToolTitles:    externalTitles,
		FreeCADTarget: svc.FreeCADTarget,
		RuntimeAccess: st.SharedRuntimeAccess,
		Lang:          st.Lang,
		Domain:        cfg.Domain,
		Checker:       checker,
		Users:         st, LC: svc, Portal: portal, Consented: consented(st), Log: log,
	})
	checks := map[string]func(context.Context) error{"store": st.Ping, "engine": rt.Ping}
	if cfg.ProvisionSocket != "" {
		checks["provision"] = provision.NewClient(cfg.ProvisionSocket).Ping
	}
	optional := map[string]bool{"pdf": true, "catalog": true} // integrations: down degrades, never 503
	if cfg.Instance.Enabled("pdf") && cfg.PDFURL != "" {
		checks["pdf"] = health.HTTPOK(http.DefaultClient, strings.TrimSuffix(cfg.PDFURL, "/")+"/health")
	}
	if cfg.CatalogDir != "" {
		checks["catalog"] = health.DirReadable(cfg.CatalogDir)
	}
	admin := http.NewServeMux()
	admin.Handle("GET /health", &health.Handler{DiskPath: filepath.Dir(cfg.DBPath), Running: svc.Running, Checks: checks, Optional: optional})
	if cat != nil {
		admin.Handle("POST /admin/catalog/reload", catalogReload(cat))
	}

	servers = []*http.Server{
		// no WriteTimeout: SSE and WebSocket streams stay open
		{Addr: cfg.EdgeAddr, Handler: logx.Middleware(log, "edge", checker.SourceTrusted, edge), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second},
		{Addr: cfg.LLMGWAddr, Handler: logx.Middleware(log, "llm-gw", nil, gwGate(cfg, st, bud, gwMux)), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second},
		{Addr: cfg.AdminAddr, Handler: logx.Middleware(log, "admin", nil, notFromUserNets(adminAuth(admin, cfg.AdminToken), cfg)), ReadHeaderTimeout: 10 * time.Second},
	}
	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func() {
			log.Info("listening", "addr", s.Addr, "log_level", os.Getenv("OC_LOG_LEVEL"))
			if err := s.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}
	if cat != nil {
		// Reapply scopes to adopted containers after the gateway starts accepting discovery.
		go cat.ReloadAll(ctx)
	}
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	return err
}

func shutdownManager(ctx context.Context, servers []*http.Server, stop context.CancelFunc, loopDone <-chan struct{}, closeLifecycle func()) {
	stop()
	var drained sync.WaitGroup
	for _, server := range servers {
		drained.Go(func() {
			if err := server.Shutdown(ctx); err != nil {
				_ = server.Close()
			}
		})
	}
	drained.Wait()
	if loopDone != nil {
		<-loopDone
	}
	// Office workers close; main runtime containers remain available for adoption.
	closeLifecycle()
}

func adminAuth(h http.Handler, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/admin/") && (token == "" ||
			subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// notFromUserNets guards the admin listener: the manager sits on every user
// network as llm-gw, so :9100 is reachable from the people's containers too.
func notFromUserNets(h http.Handler, cfg config.Config) http.Handler {
	nets := netip.MustParsePrefix(cfg.UserNetPrefix + ".0.0/16") // validated by config.Load
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ap, err := netip.ParseAddrPort(r.RemoteAddr); err != nil || nets.Contains(ap.Addr().Unmap()) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// Integration hosts may trust our source IP even when their DNS addresses are public.
func fetchDenyHosts(cfg config.Config) []string {
	hosts := append([]string(nil), cfg.FetchDenyHosts...)
	urls := []*url.URL{cfg.RoomsURL, cfg.BikesURL}
	for _, raw := range []string{cfg.LiteLLMBaseURL, cfg.LLMUpstreamURL, cfg.MCPUpstreamURL, cfg.SearXNGURL, cfg.PDFURL, cfg.FileToolsURL} {
		if u, err := url.Parse(raw); err == nil {
			urls = append(urls, u)
		}
	}
	for _, h := range cfg.House {
		urls = append(urls, h.URL)
	}
	for _, u := range urls {
		if u != nil && u.Hostname() != "" {
			hosts = append(hosts, u.Hostname())
		}
	}
	return hosts
}

// resetAssistant is an explicit, SQL-free recovery path for a quarantined definition.
func resetAssistant(ctx context.Context, st *store.Store, defaults *instance.Definition, id string) error {
	p, ok := defaults.Assistant(id)
	if !ok {
		return fmt.Errorf("reset assistant %q: no instance definition", id)
	}
	definitions, err := st.StoredAssistants(ctx)
	if err != nil {
		return err
	}
	for _, old := range definitions {
		if old.ID == id {
			_, err = st.SaveAssistant(ctx, p, instance.Current().AssistantPin(old.Definition).Hash, "operator-reset")
			return err
		}
	}
	return fmt.Errorf("reset assistant %q: no stored definition", id)
}
