// SPDX-License-Identifier: MIT

// Package portal serves apps.example.org: German server-rendered pages, htmx only
// as enhancement (docs/adr/README.md). Identity comes from the request context
// (WithIdentity, set by the core after checking the Authentik headers); all
// state lives behind Backend.
package portal

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistantchat"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/consent"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/logx"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// UserGroup is the Authentik group that grants access (live: "opencode-users").
const UserGroup = "opencode-users"

type Identity struct {
	Slug     string
	UID      string // stored account key, 64 hex
	Blocked  bool
	Username string
	Email    string
	Name     string
	Groups   []string
}

type ctxKey struct{}

// WithIdentity is called by the core's identity middleware.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

func IdentityFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(Identity)
	return id, ok && id.UID != ""
}

type Config struct {
	Knowledge         *store.Store // nil disables authored knowledge and teams in the portal.
	KnowledgeReadOnly bool         // Keep export, cleanup and recovery when the integration is disabled.
	// Capabilities describes runtime services/models actually wired by the manager.
	Capabilities   map[string]bool
	LogoutURL      string // OC_LOGOUT_URL; empty uses config.DefaultLogoutURL
	TaskAssistants *assistantchat.Service
	Backend        Backend
	// Assets holds templates/ and static/ (manager/web.FS in production,
	// os.DirFS("../../web") in tests).
	Assets fs.FS
	// Origin is the public portal origin, e.g. "https://apps.example.org". If set,
	// non-safe requests carrying another Origin header are refused.
	Origin string
	// Group is the Authentik group that grants access; "" lets every
	// authenticated person in (D56: open to all staff). Tests use UserGroup.
	Group string
	// UploadMaxBytes limits one upload request (default 2 GiB, D38).
	UploadMaxBytes int64
	// PDF converts Markdown to PDF (OC_PDF_URL); nil hides "Als PDF".
	PDF func(ctx context.Context, md []byte, name string) ([]byte, error)
	// Tools backs /tools (OC_CATALOG_DIR); nil hides the page and its nav entry.
	Tools Tools
	// Connections backs /connections (OC_NEXTCLOUD_URL); nil hides the page and its nav entry.
	Connections Connections
	// Onboarding stores the first-visit mark of /onboarding; nil = no Onboarding.
	Onboarding Onboarding
	// Sync links projects to Nextcloud folders (OC_NEXTCLOUD_URL); nil hides the section.
	Sync Sync
	// Settings backs /settings and the Settings step of the Onboarding; nil hides both.
	Settings Settings
	// Consent stores the acceptance of the prototype notice; nil = no /consent.
	Consent Consent
	// DayCapEUR is the per-person daily LLM budget (OC_COST_CAP_DAY_EUR); 0 = none.
	DayCapEUR float64
	// IdleTimeout (OC_IDLE_TIMEOUT) and MaxRunning (OC_MAX_RUNNING) are shown on
	// /how-it-works, Help and Environment; 0 = not mentioned.
	IdleTimeout time.Duration
	MaxRunning  int
	// DayStart is the start of the budget day of t (config.DayStart).
	DayStart func(t time.Time) time.Time
	// Spent is the sum the budget gate checks (store.SpentSince, with the
	// spend of a deleted account of the same uid); nil = the Usage rows.
	Spent func(ctx context.Context, uid string, since time.Time) (float64, error)
	// Admin backs /admin (internal/evaluation), only for members of AdminGroup
	// (OC_ADMIN_GROUP). IdentityGuard also enables /admin without evaluation.
	IdentityGuard IdentityGuard
	Admin         Admin
	AdminGroup    string
	// Prefs stores the chosen language per person; nil = cookie only.
	Prefs Prefs
	// Feedback stores messages to alice (form, /admin/feedback); nil hides both.
	Feedback Feedback
}

type Server struct {
	knowledgeReadOnly bool
	knowledge         *store.Store
	capabilities      map[string]bool
	logoutURL         string
	taskAssistants    *assistantchat.Service
	be                Backend
	tools             Tools
	conns             Connections
	onboard           Onboarding
	sync              Sync
	settings          Settings
	consent           Consent
	admin             Admin
	adminGroup        string
	group             string
	dayCap            float64
	idle              time.Duration
	maxRun            int
	dayFrom           func(time.Time) time.Time
	spent             func(ctx context.Context, uid string, since time.Time) (float64, error)
	origin            string
	maxUp             int64
	pdf               func(ctx context.Context, md []byte, name string) ([]byte, error)
	prefs             Prefs
	feedback          Feedback
	pages             map[string]map[string]*template.Template // language, page
	pending           pendingStore
	now               func() time.Time

	identityGuard IdentityGuard
}

// New returns the portal handler. Mount it on the apps.example.org host.
func New(cfg Config) (http.Handler, error) {
	s := &Server{knowledgeReadOnly: cfg.KnowledgeReadOnly, knowledge: cfg.Knowledge, capabilities: cfg.Capabilities, logoutURL: cmp.Or(cfg.LogoutURL, config.DefaultLogoutURL), taskAssistants: cfg.TaskAssistants, identityGuard: cfg.IdentityGuard, be: cfg.Backend, tools: cfg.Tools, conns: cfg.Connections, onboard: cfg.Onboarding, sync: cfg.Sync, settings: cfg.Settings, consent: cfg.Consent, admin: cfg.Admin, adminGroup: cfg.AdminGroup, prefs: cfg.Prefs, feedback: cfg.Feedback, dayCap: cfg.DayCapEUR, idle: cfg.IdleTimeout, maxRun: cfg.MaxRunning, dayFrom: cfg.DayStart, spent: cfg.Spent, origin: cfg.Origin, group: cfg.Group, maxUp: cfg.UploadMaxBytes, pdf: cfg.PDF, now: time.Now}
	if s.maxUp <= 0 {
		s.maxUp = 2 << 30
	}
	if err := s.parse(cfg.Assets); err != nil {
		return nil, err
	}
	static, err := fs.Sub(cfg.Assets, "static")
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	// Browsers ask for /favicon.ico regardless of the <link rel="icon">.
	mux.HandleFunc("POST /lang", s.setLang)
	mux.Handle("GET /favicon.ico", http.RedirectHandler("/static/favicon.svg", http.StatusMovedPermanently))
	page := func(pattern string, h func(http.ResponseWriter, *http.Request, Identity)) {
		mux.Handle(pattern, s.auth(h))
	}
	for from, to := range legacyRoutes {
		mux.HandleFunc("GET "+from, legacyRedirect(to))
	}
	page("GET /{$}", s.home)
	page("GET /projects", s.home)
	page("GET /projects/new", s.newForm)
	page("POST /projects/start", s.start)
	page("GET /projects/{name}", s.detail)
	page("POST /projects/{name}/capabilities", s.setProjectCapability)
	page("GET /watchdog", s.watchdogFragment)
	page("GET /projects/{name}/watchdog", s.watchdogFragment)
	page("POST /watchdog/{session}/stop", s.watchdogStop)
	page("POST /projects/{name}/watchdog/{session}/stop", s.watchdogStop)
	page("POST /projects/{name}/chats/{session}/restore", s.restoreSession)
	page("GET /projects/{name}/resume", s.resume)
	page("POST /projects/{name}/open", s.open)
	page("GET /projects/{name}/files/{path...}", s.download)
	page("GET /projects/{name}/zip", s.zip)
	page("GET /projects/{name}/pdf", s.pdfDownload)
	page("GET /projects/{name}/export", s.export)
	page("POST /projects/{name}/upload", s.upload)
	page("POST /projects/{name}/rename", s.rename)
	page("GET /projects/{name}/delete", s.trashConfirm)
	page("POST /projects/{name}/delete", s.trash)
	page("GET /trash", s.trashList)
	page("POST /trash/{id}/restore", s.restore)
	page("GET /trash/{id}/delete", s.purgeConfirm)
	page("POST /trash/{id}/delete", s.purge)
	page("GET /environment", s.env)
	page("GET /environment/starting", s.envStarting)
	page("GET /environment/reset", s.envResetConfirm)
	page("GET /environment/stop", s.envStopConfirm)
	page("GET /environment/export", s.export)
	page("GET /environment/delete", s.deleteConfirm)
	page("POST /environment/delete", s.deleteAccount)
	page("GET /deleted", s.static("deleted.html"))
	page("POST /environment/{action}", s.envAction)
	page("GET /usage", s.usage)
	page("GET /usage/method", s.usageMethod)
	s.taskAssistantRoutes(page)
	s.gapRoutes(page)
	s.knowledgeRoutes(page)
	s.consentRoutes(page)
	s.toolsRoutes(page)
	s.connectionsRoutes(page)
	s.syncRoutes(page)
	s.settingsRoutes(page)
	s.adminRoutes(page)
	s.feedbackRoutes(page)
	page("/", func(w http.ResponseWriter, r *http.Request, id Identity) {
		s.errorPage(w, r, http.StatusNotFound, "Page not found", "This page does not exist.")
	})

	// CrossOriginProtection rejects Sec-Fetch-Site: same-site, which covers the
	// preview hosts <slug>--<port>.apps.example.org; sameOrigin adds an explicit
	// Origin match for browsers without Fetch metadata.
	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.errorPage(w, r, http.StatusForbidden, "Request refused", "The request did not come from this site and was therefore not carried out.")
	}))
	return securityHeaders(cfg.Origin, s.withLang(s.blocked(s.sameOrigin(cop.Handler(logx.Route(mux)))))), nil
}

func (s *Server) blocked(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := r.Context().Value(ctxKey{}).(Identity)
		if id.Blocked && !((r.Method == http.MethodGet && (strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/favicon.ico")) ||
			(r.Method == http.MethodPost && r.URL.Path == "/lang")) {
			s.render(w, r, http.StatusForbidden, "identity-blocked.html", view{})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// securityHeaders sets the CSP. Chrome applies form-action to redirect targets
// too, and "Öffnen"/"Loslegen" POST here and 303 to <slug>.<domain>, so the
// person hosts are allowed as form targets.
func securityHeaders(origin string, next http.Handler) http.Handler {
	formAction := "'self'"
	if host, ok := strings.CutPrefix(origin, "https://"); ok && host != "" {
		formAction += " https://*." + host
	}
	csp := "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; frame-ancestors 'none'; form-action " + formAction + "; base-uri 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if o := r.Header.Get("Origin"); s.origin != "" && o != "" && o != s.origin {
				s.errorPage(w, r, http.StatusForbidden, "Request refused", "The request did not come from this site and was therefore not carried out.")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) auth(h func(http.ResponseWriter, *http.Request, Identity)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := IdentityFrom(r.Context())
		if !ok || (s.group != "" && !slices.Contains(id.Groups, s.group)) {
			s.render(w, r, http.StatusForbidden, "no-access.html", view{User: id})
			return
		}
		h(w, r, id)
	})
}

// view is the data every template gets.
type view struct {
	TeamInvitations   []store.Team
	InvitationAccount string
	KnowledgeWrite    bool
	Knowledge         bool
	Capabilities      map[string]bool
	CapabilityCopy    map[string]string
	ProjectScoped     bool
	Watchdog          watchdogView

	User           Identity
	Nav            string
	Notice         string
	Errors         []fieldError
	D              any
	Tools          bool // nav shows Tools; set by render
	Conns          bool // nav shows Connections; set by render
	Setup          bool // Onboarding exists (links on Umgebung, Hilfe); set by render
	TaskAssistants bool
	Settings       bool   // nav shows Settings; set by render
	Admin          bool   // nav shows Admin; set by render
	Feedback       bool   // the feedback form shows; set by render
	Lang           string // "de" or "en"; set by render
	Path           string // where the language switch returns to; set by render
}

type fieldError struct{ Field, Msg string }

var pageFiles = []string{
	"start.html", "projects.html", "project.html", "project-delete.html",
	"trash.html", "trash-delete.html", "environment.html", "environment-starting.html",
	"environment-reset.html", "environment-stop.html", "environment-delete.html", "deleted.html", "usage.html", "usage-method.html", "help.html", "privacy.html", "no-access.html", "error.html",
}

func (s *Server) parse(assets fs.FS) error {
	s.pages = map[string]map[string]*template.Template{}
	for _, l := range []string{langDE, langEN} {
		s.pages[l] = map[string]*template.Template{}
		for _, p := range pageFiles {
			t, err := template.New(p).Funcs(s.funcs(l)).ParseFS(assets, "templates/layout.html", "templates/usage-short.html", "templates/feedback-form.html", "templates/watchdog.html", "templates/assistant-card.html", "templates/"+p)
			if err != nil {
				return err
			}
			s.pages[l][p] = t
		}
	}
	return nil
}

// funcs are the template functions of one language; every page is parsed once per language.
func (s *Server) funcs(l string) template.FuncMap {
	return template.FuncMap{
		"supportParts": supportParts,
		"hasString":    func(values []string, value string) bool { return slices.Contains(values, value) },
		"logoutURL":    func() string { return s.logoutURL },
		// t translates a German text (escaped, U+00A0 as &nbsp; like the markup
		// before), th one with markup (catalog only, arguments escaped)
		"t": func(de string, args ...any) template.HTML {
			return template.HTML(strings.ReplaceAll(template.HTMLEscapeString(trf(l, de, args...)), "\u00a0", "&nbsp;"))
		},
		"th": func(de string, args ...any) template.HTML { return th(l, de, args...) },
		// chatrow feeds the "chatrow" partial of the project list (projects.html)
		"chatrow": func(c Chat, primary bool) map[string]any { return map[string]any{"Chat": c, "Primary": primary} },
		// nextstep feeds the "next" partial of project.html (head and upload message)
		"nextstep": func(d detailData, primary bool) map[string]any { return map[string]any{"D": d, "Primary": primary} },
		"fielderror": func(errs []fieldError, field string) string {
			for _, e := range errs {
				if e.Field == field {
					return e.Msg
				}
			}
			return ""
		},
		"eur":   func(v float64) string { return eurL(l, v) },
		"gb":    func(n int64) string { return num(l, gb(n)) },
		"when":  func(t time.Time) string { return s.when(l, t) },
		"date":  func(t time.Time) string { return dateOrDash(t, dayLayout(l)) },
		"clock": func(t time.Time) string { return t.Local().Format("15:04") },
		"co2":   func(lo, hi float64) string { return num(l, co2Range(lo, hi)) },
		"car":   func(lo, hi float64) string { return carRange(l, lo, hi) },
		"number": func(v float64) string {
			return num(l, strings.Replace(strconv.FormatFloat(v, 'f', -1, 64), ".", ",", 1))
		},
		"status": func(st string) string { return tr(l, statusLabel(st)) },
		"stale":  func(t time.Time) bool { return !noTime(t) && s.now().Sub(t) > 90*24*time.Hour },
		// soon: the inactivity rule moves the project within a week
		"soon":  func(t time.Time) bool { return !noTime(t) && t.Sub(s.now()) < 7*24*time.Hour },
		"badge": statusBadge,
		"envbadge": func(st EnvState) string {
			return map[EnvState]string{EnvRunning: "done", EnvStarting: "working", EnvError: "error", EnvStopped: "idle", EnvFull: "waiting"}[st]
		},
		"envlabel": func(st EnvState) string {
			return tr(l, map[EnvState]string{EnvRunning: "running", EnvStarting: "starting", EnvError: "does not start", EnvStopped: "asleep", EnvFull: "all places taken"}[st])
		},
		// pct returns p percent of n, for <meter low/high>.
		"pct": func(n int64, p int64) int64 { return n * p / 100 },
		// share is part/total in whole percent.
		"share": func(part, total float64) int {
			if total <= 0 {
				return 0
			}
			return int(math.Round(part / total * 100))
		},
		"startURL": startURL,
		// args passes several values to a partial (connection-buttons)
		"args":          func(a ...any) []any { return a },
		"mul":           func(a, b float64) float64 { return a * b },
		"iv":            instance.Value,
		"ic":            func(key string) string { return instance.Current().CopyText(key, l) },
		"noticeTitle":   func() string { return consent.Text(l).Title },
		"noticePoints":  func() []string { return consent.Text(l).Points },
		"noticeShort":   func() string { return consent.Text(l).Short },
		"failed":        func(f FlowState) bool { return f == FlowFailed || f == FlowMismatch || f == FlowTimeout },
		"lines":         transcriptLines,
		"iso":           func(t time.Time) string { return t.Format("2006-01-02") },
		"flaglabel":     func(f string) string { return tr(l, flagLabels[f]) },
		"feedbackKinds": func() []string { return feedbackKinds },
		"feedbackKind":  func(a string) string { return tr(l, feedbackLabels[a]) },
		// zonedate, datetime: in the zone of the budget day (Berlin), the image runs in UTC
		"zonedate": func(t time.Time) string { return dateOrDash(t.In(s.dayStart(t).Location()), dayLayout(l)) },
		"datetime": func(t time.Time) string { return dateOrDash(t.In(s.dayStart(t).Location()), dayLayout(l)+", 15:04") },
		// configured limits for the explanatory pages; "" / 0 = none
		"dayBudget": func() string {
			if s.dayCap <= 0 {
				return ""
			}
			return eurL(l, s.dayCap)
		},
		"idleMinutes": func() int { return int(s.idle.Minutes()) },
		"slots":       func() int { return s.maxRun },
	}
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page string, v view) {
	if strings.Contains(page, "assistant") {
		w.Header().Set("Cache-Control", "no-store")
	}
	if v.User.UID == "" {
		v.User, _ = IdentityFrom(r.Context())
	}
	v.Tools = s.tools != nil
	v.Conns = s.conns != nil
	v.Setup = s.onboard != nil
	v.Settings = s.settings != nil
	v.TaskAssistants = s.taskAssistants != nil
	v.Knowledge = s.knowledge != nil && !s.knowledgeReadOnly
	v.KnowledgeWrite = v.Knowledge
	if s.knowledge != nil && status < 400 && v.User.UID != "" {
		actor, err := s.knowledge.KnowledgeActor(r.Context(), v.User.UID)
		if err == nil {
			v.InvitationAccount = actor.Stamp
			v.TeamInvitations, err = s.knowledge.TeamInvitations(r.Context(), actor)
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			s.serverError(w, r, err)
			return
		}
	}
	v.Admin = s.isAdmin(v.User)
	v.Feedback = s.feedback != nil
	l := lang(r)
	v.Lang, v.Path = l, "/"
	if r.Method == http.MethodGet && status < 400 {
		v.Path = r.URL.RequestURI() // the language switch comes back here; error pages do not echo their URL
	}
	v.Notice = tr(l, v.Notice)
	for i := range v.Errors {
		v.Errors[i].Msg = tr(l, v.Errors[i].Msg)
	}
	if e, ok := v.D.(errorData); ok {
		e.Title, e.Text = tr(l, e.Title), tr(l, e.Text)
		v.D = e
	}
	if page == "project.html" || page == "start.html" || page == "environment.html" {
		project := ""
		if page == "project.html" {
			project = r.PathValue("name")
		}
		v.Watchdog, _ = s.watchdogView(r, v.User.UID, project)
	}
	var buf bytes.Buffer
	if err := s.pages[l][page].ExecuteTemplate(&buf, "layout", v); err != nil {
		slog.ErrorContext(r.Context(), "portal: template", "page", page, "err", err)
		http.Error(w, tr(l, "Internal error"), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if page == "error.html" && r.Header.Get("HX-Request") == "true" {
		// An htmx caller targets a fragment (#neu, #inbox, a form); show the error as the page content instead.
		w.Header().Set("HX-Retarget", "#content")
		w.Header().Set("HX-Reselect", "#content")
		w.Header().Set("HX-Reswap", "outerHTML")
	}
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (s *Server) static(page string) func(http.ResponseWriter, *http.Request, Identity) {
	nav := strings.TrimSuffix(page, ".html")
	return func(w http.ResponseWriter, r *http.Request, id Identity) {
		s.render(w, r, http.StatusOK, page, view{User: id, Nav: nav})
	}
}

type errorData struct{ Title, Text, ErrorID string }

func (s *Server) errorPage(w http.ResponseWriter, r *http.Request, status int, title, text string) {
	s.render(w, r, status, "error.html", view{D: errorData{Title: title, Text: text}})
}

// serverError answers a backend error: the sentinel errors get their own
// German page, anything else is logged under a short id shown to the user.
func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		s.errorPage(w, r, http.StatusNotFound, "Not found", "The project or file does not exist (any more).")
		return
	case errors.Is(err, ErrBusy):
		s.errorPage(w, r, http.StatusConflict, "The helper is working right now", "This is only possible once the helper has finished. Wait until it is done, or stop the environment under “Environment”.")
		return
	case errors.Is(err, ErrNotRunning):
		s.errorPage(w, r, http.StatusServiceUnavailable, "Your environment is not running", "Your environment has to be running for this. You can start it under “Environment”; “Continue” or “New task” starts it automatically.")
		return
	case errors.Is(err, ErrQuota):
		s.errorPage(w, r, http.StatusInsufficientStorage, "Storage full", "Your storage is full. Empty the Trash or delete old projects. Under “Environment” you can see how much is used.")
		return
	case errors.Is(err, ErrUnavailable):
		s.errorPage(w, r, http.StatusServiceUnavailable, "Not possible right now", "This is not possible on this installation at the moment. Please contact [[team]].")
		return
	case errors.Is(err, ErrFull):
		s.errorPage(w, r, http.StatusServiceUnavailable, "All places taken", fullText)
		return
	case errors.Is(err, ErrExists):
		s.errorPage(w, r, http.StatusConflict, "Already exists", "A project with this name already exists. Please choose a different name.")
		return
	}
	b := make([]byte, 4)
	rand.Read(b)
	eid := hex.EncodeToString(b)
	slog.ErrorContext(r.Context(), "portal: request failed", "error_id", eid, "path", r.URL.Path, "err", err)
	s.render(w, r, http.StatusInternalServerError, "error.html", view{D: errorData{
		Title:   "Something went wrong",
		Text:    "The action could not be carried out. Please try again. If the error persists, give [[team]] the error ID.",
		ErrorID: eid,
	}})
}

const fullText = "All places are taken right now, please try again in a few minutes."

// navigate sends the browser to url with a full page load. htmx would follow
// a 303 inside its XHR (cross-origin for deep links), so it gets HX-Redirect.
func navigate(w http.ResponseWriter, r *http.Request, url string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", url)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, url, http.StatusSeeOther)
}

// noTime is true for the zero time and for Unix epoch 0, which a backend
// produces from a missing timestamp (time.UnixMilli(0) is not IsZero).
func noTime(t time.Time) bool { return t.IsZero() || t.Unix() == 0 }

func dateOrDash(t time.Time, layout string) string {
	if noTime(t) {
		return "–"
	}
	return t.Local().Format(layout)
}

// eur formats German style: 1.234,56 €.
func eur(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	intPart, frac, _ := strings.Cut(s, ".")
	neg := strings.HasPrefix(intPart, "-")
	intPart = strings.TrimPrefix(intPart, "-")
	var b strings.Builder
	for i, c := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String() + "," + frac + "\u00a0€"
	}
	return b.String() + "," + frac + "\u00a0€"
}

// gb uses 1024 steps labelled kB/MB/GB (like Windows), so the 20 GiB quota reads "20,0 GB".
func gb(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d\u00a0B", n)
	case n < 1<<20:
		return strings.Replace(fmt.Sprintf("%.0f\u00a0kB", float64(n)/(1<<10)), ".", ",", 1)
	case n < 1<<30:
		return strings.Replace(fmt.Sprintf("%.1f\u00a0MB", float64(n)/(1<<20)), ".", ",", 1)
	}
	return strings.Replace(fmt.Sprintf("%.1f\u00a0GB", float64(n)/(1<<30)), ".", ",", 1)
}

// co2Range shows a range in grams rounded to one significant figure ("6–20 g"),
// as the estimate is uncertain (docs/adr/README.md B.2.2).
func co2Range(lo, hi float64) string {
	l, h := sig1(lo), sig1(hi)
	if l == h {
		return l + "\u00a0g"
	}
	return l + "–" + h + "\u00a0g"
}

// sig1 rounds v to one significant figure, German decimal comma.
func sig1(v float64) string {
	if v <= 0 {
		return "0"
	}
	p := math.Pow(10, math.Floor(math.Log10(v)))
	r := math.Round(v/p) * p
	prec := max(0, -int(math.Floor(math.Log10(r))))
	return strings.Replace(strconv.FormatFloat(r, 'f', prec, 64), ".", ",", 1)
}

// carGPerKm is the greenhouse gas of one car-km in Germany: 164 g CO2e per
// person-km at 1.4 persons per car (Umweltbundesamt, TREMOD, Bezugsjahr 2024).
const carGPerKm = 164 * 1.4

// carRange turns a CO2 range in g into the distance a car emits it on, in
// metres or, from 1000 m on, kilometres: "30 bis 90 Metern" / "30 to 90
// metres"; "" for no CO2.
func carRange(lang string, lo, hi float64) string {
	if hi <= 0 {
		return ""
	}
	lo, hi = lo/carGPerKm*1000, hi/carGPerKm*1000
	km := hi >= 1000
	if km {
		lo, hi = lo/1000, hi/1000
	}
	l, h := sig1(lo), sig1(hi)
	if lang == langEN {
		unit := map[bool]string{false: "metre", true: "kilometre"}[km]
		if h != "1" || l != h {
			unit += "s"
		}
		if l == h {
			return num(lang, h) + "\u00a0" + unit
		}
		return num(lang, l) + " to " + num(lang, h) + "\u00a0" + unit
	}
	unit := map[bool]string{false: "Meter", true: "Kilometer"}[km]
	if l == h {
		if h != "1" {
			unit += "n" // dative plural: "etwa 30 Metern Autofahrt"
		}
		return h + "\u00a0" + unit
	}
	return l + " bis " + h + "\u00a0" + unit + "n"
}

func statusLabel(st string) string {
	switch st {
	case StatusWorking:
		return "helper is working"
	case StatusWaiting:
		return "waiting for your reply"
	case StatusDone:
		return "done"
	case StatusFailed:
		return "Error"
	}
	return ""
}

// statusBadge maps a project/chat status to the badge modifier of app.css.
func statusBadge(st string) string {
	return map[string]string{StatusWorking: "working", StatusWaiting: "waiting", StatusDone: "done", StatusFailed: "error"}[st]
}

// when shows a localized time for today or yesterday, otherwise the date.
func (s *Server) when(l string, t time.Time) string {
	if noTime(t) {
		return "–"
	}
	t = t.Local()
	y, m, d := s.now().Local().Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, time.Local)
	switch {
	case !t.Before(today):
		return tr(l, "today, ") + t.Format("15:04")
	case !t.Before(today.AddDate(0, 0, -1)):
		return tr(l, "yesterday, ") + t.Format("15:04")
	}
	return t.Format(dayLayout(l))
}

func startURL(project, token string) string {
	if project == "" || token == "" {
		return "/environment/starting"
	}
	return "/environment/starting?project=" + url.QueryEscape(project) + "&t=" + token
}
