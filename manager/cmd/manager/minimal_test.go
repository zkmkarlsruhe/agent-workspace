// SPDX-License-Identifier: MIT

package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/catalog"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/gateway/llm"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/gateway/mcp"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/health"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle/lctest"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/router"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/settings"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// TestRefInstanceMinimal is the M3 zero-integration acceptance test.
// See docs/REFINSTANCE-minimal.md.
func TestRefInstanceMinimal(t *testing.T) {
	if os.Getenv("REFINSTANCE") != "minimal" {
		t.Skip("set REFINSTANCE=minimal to run the M3 reference instance")
	}
	definition, err := instance.Load("../../../instance/minimal/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(instance.Select(definition))
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws, home, admin, catDir := filepath.Join(root, "workspace"), filepath.Join(root, "config"), filepath.Join(root, "admin"), filepath.Join(root, "catalog")
	for _, dir := range []string{ws, home, admin, catDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// A single local endpoint accepts only the instance's two model IDs.
	modelRequests := make(chan string, 16)
	models := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			io.WriteString(w, `{"object":"list","data":[{"id":"default"},{"id":"small"}]}`)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		b, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		modelRequests <- string(b)
		var in struct{ Model string }
		if json.Unmarshal(b, &in) != nil || (in.Model != "default" && in.Model != "small") {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":{"message":"unknown model","type":"invalid_request_error"}}`)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"sample-project"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"cost":0}}`)
	}))
	t.Cleanup(models.Close)
	cfg := config.Config{
		Instance: definition,
		Domain:   definition.Defaults["OC_DOMAIN"], TrustedProxy: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		DBPath: filepath.Join(root, "manager.db"), UsersRoot: filepath.Join(root, "users"),
		RuntimeImage: "runtime.example.org/workspace:reference", AdminConfig: admin, CatalogDir: catDir,
		ManagerContainer: "manager", EgressContainer: "egress", UserNetPrefix: "127.93",
		MasterKey: []byte("0123456789abcdef0123456789abcdef"), StartTimeout: 2 * time.Second,
		IdleTimeout: time.Hour, TabCap: time.Hour, DrainCap: time.Hour, MaxLifetime: 24 * time.Hour,
		LiteLLMBaseURL: models.URL, LiteLLMAPIKey: "reference-test-key",
	}
	rendered := definition.Render()
	baseConfig := string(rendered["opencode.jsonc"])
	for name, content := range rendered {
		filename := filepath.Join(admin, name)
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	oc := &lctest.OpenCode{}
	listener, err := net.Listen("tcp", cfg.SlotAddr(1, config.HostUser).String()+":4096")
	if err != nil {
		t.Fatal(err)
	}
	ocServer := &httptest.Server{Listener: listener, Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/mcp":
			io.WriteString(w, `{"data":[]}`) // the shared fake otherwise invents a house MCP
		case "/":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, "<!doctype html><html><head><title>Workspace</title></head><body></body></html>")
		default:
			oc.ServeHTTP(w, r)
		}
	})}}
	ocServer.Start()
	t.Cleanup(ocServer.Close)
	rt := lctest.NewRuntime()
	rt.Root, rt.OpenCode = ws, oc
	// Map every container file write into this test's temp directory.
	rt.Paths = map[string]string{"/home/user/.config/opencode": home, "/etc/opencode": admin, catalog.MountDir(): catDir}
	svc := lifecycle.New(cfg, rt, st, log)
	cat := &catalog.Service{Dir: cfg.CatalogDir, Store: st, Shell: svc.Shell, Client: svc.Client, Running: svc.RunningUIDs, Log: log}
	svc.AfterStart, svc.AfterReap = cat.AfterStart, cat.RetryPending
	upstream, err := url.Parse(cfg.LiteLLMBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	gw := llm.New(llm.Gateway{Upstream: upstream, APIKey: cfg.LiteLLMAPIKey, SlotOf: cfg.SlotOf, Users: st, Usage: st, Log: log, Budget: budget(cfg, st)})
	svc.Suggest = gw.Suggest

	// newPortal uses package globals; keep this test independent of test order.
	oldTools, oldConns, oldSync, oldAdmin := portalTools, portalConns, portalSync, portalAdmin
	oldOnboarding, oldConsent, oldPrefs, oldSpent := portalOnboarding, portalConsent, portalPrefs, portalSpent
	oldGuard := portalIdentityGuard
	oldAssistant, oldFeedback := portalSettings, portalFeedback
	t.Cleanup(func() {
		portalTools, portalConns, portalSync, portalAdmin = oldTools, oldConns, oldSync, oldAdmin
		portalOnboarding, portalConsent, portalPrefs, portalSpent = oldOnboarding, oldConsent, oldPrefs, oldSpent
		portalIdentityGuard = oldGuard
		portalSettings, portalFeedback = oldAssistant, oldFeedback
	})
	portalIdentityGuard = st
	portalTools, portalConns, portalSync, portalAdmin = tools{cat}, nil, nil, nil
	portalOnboarding, portalConsent, portalPrefs, portalSpent = onboarding{st}, consentStore{st}, st, st.SpentSince
	portalSettings, portalFeedback = newSettings(cfg, st, svc, gw, log), feedback{st, log}
	ph, err := newPortal(svc, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	edge := router.New(router.Router{Domain: cfg.Domain, Checker: identity.Checker{Trusted: cfg.TrustedProxy},
		Users: st, LC: svc, Portal: ph, Consented: consented(st), Log: log})
	uid := strings.Repeat("a", 64)
	request := func(method, path, contentType string, body io.Reader, authenticated bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://"+cfg.Domain+path, body)
		r.RemoteAddr = "127.0.0.1:12345"
		r.Header.Set("Accept", "text/html")
		r.Header.Set("Accept-Language", "en")
		if contentType != "" {
			r.Header.Set("Content-Type", contentType)
			r.Header.Set("Origin", "https://"+cfg.Domain)
		}
		if authenticated {
			r.Header.Set("X-Authentik-Uid", uid)
			r.Header.Set("X-Authentik-Username", "alice")
			r.Header.Set("X-Authentik-Name", "Alice Example")
			r.Header.Set("X-Authentik-Email", "alice@example.org")
		}
		w := httptest.NewRecorder()
		edge.ServeHTTP(w, r)
		return w
	}
	check := func(name string, w *httptest.ResponseRecorder, status int) {
		t.Run(name, func(t *testing.T) {
			if w.Code != status {
				t.Errorf("status %d, want %d; body: %.500s", w.Code, status, w.Body.String())
			}
			if status == http.StatusOK && strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") && !strings.Contains(w.Body.String(), "</html>") {
				t.Error("incomplete HTML response")
			}
			minimalNeutral(t, "response body", w.Body.String())
			minimalNeutral(t, "response headers", fmt.Sprint(w.Header()))
		})
	}
	get := func(path string) *httptest.ResponseRecorder {
		return request(http.MethodGet, path, "", nil, true)
	}
	check("login/no-identity", request(http.MethodGet, "/", "", nil, false), http.StatusForbidden)
	w := get("/")
	check("login/consent-redirect", w, http.StatusFound)
	if !strings.Contains(w.Header().Get("Location"), "/consent") {
		t.Error("first login did not require consent")
	}
	check("login/consent", get("/consent"), http.StatusOK)
	check("login/accept", request(http.MethodPost, "/consent", "application/x-www-form-urlencoded", strings.NewReader("agree=yes&next=%2F"), true), http.StatusSeeOther)
	check("login/onboarding-redirect", get("/"), http.StatusSeeOther)
	check("login/onboarding", get("/onboarding"), http.StatusOK)
	check("home", get("/"), http.StatusOK)
	// Finish the real start, including catalog/settings hooks, before file assertions.
	if _, err := svc.EnsureRunning(ctx, uid); err != nil {
		t.Fatal(err)
	}
	runtimePage := httptest.NewRequest(http.MethodGet, "https://alice."+cfg.Domain+"/", nil)
	runtimePage.RemoteAddr = "127.0.0.1:12345"
	runtimePage.Header.Set("Accept", "text/html")
	runtimePage.Header.Set("X-Authentik-Uid", uid)
	runtimePage.Header.Set("X-Authentik-Username", "alice")
	w = httptest.NewRecorder()
	edge.ServeHTTP(w, runtimePage)
	check("runtime-page", w, http.StatusOK)

	var upload bytes.Buffer
	mw := multipart.NewWriter(&upload)
	if err := mw.WriteField("description", "Summarize the attached table and write a report."); err != nil {
		t.Fatal(err)
	}
	file, err := mw.CreateFormFile("files", "input.csv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(file, "month,total\nJanuary,10\n"); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	check("create-project", request(http.MethodPost, "/projects/start", mw.FormDataContentType(), &upload, true), http.StatusSeeOther)
	ps, err := st.Projects(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 {
		t.Errorf("created %d projects, want 1", len(ps))
	} else {
		p := ps[0].Folder
		// Use the actual layout to reach downloads; assert its neutrality separately.
		result := filepath.Join(ws, p, lifecycle.Layout().Results, "report.txt")
		if err := os.WriteFile(result, []byte("Total: 10\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		check("project", get("/projects/"+p), http.StatusOK)
		w = get("/projects/" + p + "/files/" + lifecycle.Layout().Results + "/report.txt")
		check("download-file", w, http.StatusOK)
		if w.Body.String() != "Total: 10\n" {
			t.Error("result download lost its contents")
		}
		t.Run("download-results", func(t *testing.T) {
			minimalZIP(t, get("/projects/"+p+"/zip?part=results"), p+"/report.txt")
		})
	}
	for _, lang := range []string{"en", "de"} {
		if err := st.SetLang(ctx, uid, lang); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/", "/tools", "/settings", "/usage", "/consent", "/how-it-works", "/privacy", "/help"} {
			check(lang+path, get(path), http.StatusOK)
		}
		t.Run(lang+"/account-export", func(t *testing.T) {
			minimalZIP(t, get("/environment/export"), "README.txt")
		})
	}
	t.Run("generated", func(t *testing.T) {
		for _, name := range []string{filepath.Base(settings.AgentsPath), filepath.Base(catalog.ConfigFile())} {
			b, err := os.ReadFile(filepath.Join(home, name))
			if err != nil {
				t.Errorf("generated %s: %v", name, err)
				continue
			}
			minimalNeutral(t, name, string(b))
			if len(b) == 0 {
				t.Errorf("generated %s is empty", name)
			}
			if name == filepath.Base(catalog.ConfigFile()) {
				var generated struct {
					MCP struct{ Servers map[string]json.RawMessage }
				}
				if err := json.Unmarshal(b, &generated); err != nil {
					t.Errorf("generated config is not JSON: %v", err)
				} else if len(generated.MCP.Servers) != 0 {
					t.Errorf("generated config enables integration servers: %s", b)
				}
			}
		}
		minimalNeutral(t, "admin config input", baseConfig)
		minimalNeutral(t, "project layout", fmt.Sprint(lifecycle.Layout()))
		rt.Mu.Lock()
		defer rt.Mu.Unlock()
		for _, spec := range rt.Specs {
			minimalNeutral(t, "container config", fmt.Sprint(spec))
		}
		oc.Mu.Lock()
		defer oc.Mu.Unlock()
		if len(oc.Prompts) == 0 {
			t.Error("no project prompt reached OpenCode")
		}
		for _, prompt := range oc.Prompts {
			minimalNeutral(t, "project prompt", prompt)
		}
	})
	t.Run("model-requests", func(t *testing.T) {
		if len(modelRequests) == 0 {
			t.Fatal("project creation did not call the model endpoint")
		}
		for len(modelRequests) > 0 {
			body := <-modelRequests
			minimalNeutral(t, "model request", body)
			var in struct{ Model string }
			if err := json.Unmarshal([]byte(body), &in); err != nil {
				t.Fatal(err)
			}
			if in.Model != "default" && in.Model != "small" {
				t.Errorf("requested model %q, instance offers only default and small", in.Model)
			}
		}
	})
	t.Run("catalog", func(t *testing.T) {
		d, err := (tools{cat}).Tools(ctx, uid)
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		minimalNeutral(t, "catalog listing", string(b))
		if len(d.Standard)+len(d.Catalog)+len(d.Own) != 0 {
			t.Errorf("empty instance exposes catalog entries: %s", b)
		}
	})
	// Even an injected feedback store cannot override the instance selection.
	mh := mcp.New(mcp.Server{SlotOf: cfg.SlotOf, Users: st, Usage: st, Log: log, Lang: st.Lang,
		Files: svc, Feedback: st, FeedbackContext: svc.FeedbackContext, Grants: cat.Grants})
	for _, method := range []string{"initialize", "tools/list"} {
		t.Run("mcp/"+method, func(t *testing.T) {
			body := `{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":{}}`
			if method == "initialize" {
				body = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"reference"}}}`
			}
			r := httptest.NewRequest(http.MethodPost, "http://llm-gw:4000/mcp/workspace", strings.NewReader(body))
			r.RemoteAddr = cfg.SlotAddr(1, config.HostUser).String() + ":12345"
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Accept", "application/json, text/event-stream")
			w := httptest.NewRecorder()
			gwGate(cfg, st, budget(cfg, st), mh).ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Errorf("MCP status %d: %s", w.Code, w.Body)
			}
			minimalNeutral(t, "MCP response/instructions", w.Body.String())
			var reply struct {
				Error  json.RawMessage
				Result struct{ Tools []json.RawMessage }
			}
			if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
				t.Fatal(err)
			}
			if len(reply.Error) > 0 || (method == "tools/list" && len(reply.Result.Tools) != 0) {
				t.Errorf("no integrations requested, want no MCP tools or protocol errors: %s", w.Body)
			}
		})
	}
	adminMux := http.NewServeMux()
	adminMux.Handle("GET /health", &health.Handler{DiskPath: root, Running: svc.Running,
		Checks:   map[string]func(context.Context) error{"store": st.Ping, "engine": rt.Ping, "catalog": health.DirReadable(catDir)},
		Optional: map[string]bool{"catalog": true, "pdf": true}})
	w = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "http://localhost/health", nil)
	r.RemoteAddr = "127.0.0.1:12345"
	notFromUserNets(adminMux, cfg).ServeHTTP(w, r)
	check("health", w, http.StatusOK)
	if !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Error("health did not report ok")
	}
}

// Folder names are matched as words, so German prose such as "bearbeiten"
// does not fail. None of these names is an instance value in this fixture.
var minimalForbidden = []*regexp.Regexp{
	regexp.MustCompile(`(?i)privatebrand|privateteam|privatefont`),
	regexp.MustCompile(`(?i)\bprivateperson\b`),
	regexp.MustCompile(`\b(\x{65}\x{69}\x{6e}\x{67}\x{61}\x{6e}\x{67}|\x{65}\x{72}\x{67}\x{65}\x{62}\x{6e}\x{69}\x{73}|\x{61}\x{72}\x{62}\x{65}\x{69}\x{74})\b`),
}

func minimalNeutral(t *testing.T, source, body string) {
	t.Helper()
	for _, re := range minimalForbidden {
		if at := re.FindStringIndex(body); at != nil {
			t.Errorf("%s contains %q: %q", source, body[at[0]:at[1]], body[max(0, at[0]-60):min(len(body), at[1]+100)])
		}
	}
}

func minimalZIP(t *testing.T, w *httptest.ResponseRecorder, required string) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Errorf("ZIP status %d: %.500s", w.Code, w.Body.String())
		minimalNeutral(t, "ZIP error body", w.Body.String())
		return
	}
	minimalNeutral(t, "ZIP headers", fmt.Sprint(w.Header()))
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range zr.File {
		found = found || f.Name == required
		minimalNeutral(t, "ZIP entry name", f.Name)
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		minimalNeutral(t, "ZIP entry "+f.Name, string(b))
	}
	if !found {
		t.Errorf("ZIP lacks %s", required)
	}
}
