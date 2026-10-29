// SPDX-License-Identifier: MIT

// Package router serves the edge listener: the portal on <domain> and one
// reverse proxy per person on <slug>.<domain> (docs/adr/README.md §3.8, docs/adr/README.md §3.3).
package router

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/logx"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

type Users interface {
	CheckIdentity(ctx context.Context, id identity.Identity) (string, error)
	EnsureUser(ctx context.Context, id identity.Identity) (store.User, error)
}

// Backend is the part of lifecycle.Service the proxy needs.
type Backend interface {
	Target(uid string) (lifecycle.Target, bool)
	Touch(uid string)
	StreamOpened(uid string) (done func())
	Suspect(uid string)
}

type Router struct {
	ToolTitles    func(context.Context, store.User, string) (map[string]string, error)
	FreeCADTarget func(context.Context, string, string) (lifecycle.Target, bool)
	RuntimeAccess func(context.Context, string) error
	Lang          func(context.Context, string) (string, error)
	Domain        string
	Checker       identity.Checker
	Users         Users
	LC            Backend
	// Portal serves <domain>; it finds the identity via identity.FromContext.
	Portal http.Handler
	// Consented reports whether the person accepted the current prototype
	// notice (internal/consent). Required: without consent nothing but the
	// paths in consentFree is served.
	Consented func(ctx context.Context, uid string) (bool, error)
	Log       *slog.Logger

	proxy *httputil.ReverseProxy
}

type proxyKey struct{}

type proxyTarget struct {
	uid    string
	lang   string
	target lifecycle.Target
}

func New(r Router) *Router {
	rt := &r
	rt.proxy = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			h := rt.Checker.Headers.WithDefaults()
			for _, name := range []string{h.UID, h.Username, h.Email, h.Name, h.Groups, rt.Checker.SubjectHeader} {
				r.Out.Header.Del(name)
			}
			rewrite(r)
		},
		ModifyResponse: func(resp *http.Response) error {
			if err := fixModelResponse(resp); err != nil {
				return err
			}
			return rt.brandResponse(resp)
		},
		FlushInterval: -1, // SSE must not be buffered
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			DisableCompression:    true, // never re-encode SSE
			MaxIdleConnsPerHost:   32,
			ResponseHeaderTimeout: 60 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			if pt, ok := req.Context().Value(proxyKey{}).(proxyTarget); ok && !errors.Is(err, context.Canceled) {
				var ue *url.Error
				if errors.As(err, &ue) {
					err = ue.Err // the URL may carry a PTY ticket in its query
				}
				rt.Log.WarnContext(req.Context(), "proxy error", "upstream", "opencode", "method", req.Method, "path", req.URL.Path, "err", err)
				go rt.LC.Suspect(pt.uid)
			}
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	return rt
}

func rewrite(r *httputil.ProxyRequest) {
	pt := r.In.Context().Value(proxyKey{}).(proxyTarget)
	u, _ := url.Parse(pt.target.BaseURL)
	r.SetURL(u)
	r.Out.Host = r.In.Host // OpenCode compares Origin host with Host
	r.SetXForwarded()
	r.Out.Header.Set("X-Forwarded-Proto", "https")
	for k := range r.Out.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-authentik-") {
			delete(r.Out.Header, k)
		}
	}
	r.Out.Header.Del("Cookie")
	if isModelList(r.Out) || wantsHTML(r.Out) {
		r.Out.Header.Del("Accept-Encoding") // fixModelResponse and brandResponse need the plain body
	}
	r.Out.SetBasicAuth("opencode", pt.target.Password)
}

func (rt *Router) portalURL(path string) string { return "https://" + rt.Domain + path }

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ctx := r.Context()
	id, err := rt.Checker.FromRequest(r)
	if errors.Is(err, identity.ErrUntrusted) || errors.Is(err, identity.ErrInvalid) {
		rt.Log.WarnContext(ctx, "router: identity refused", "host", host, "err", err)
		w.WriteHeader(http.StatusForbidden) // only a misconfiguration gets here
		return
	}
	// shortcut: existing streams retain their lease; add revocation for active-session termination.
	if err == nil {
		uid, guardErr := rt.Users.CheckIdentity(ctx, id)
		if guardErr != nil {
			rt.identityError(w, r, guardErr)
			return
		}
		id.UID = uid
	}
	if id.UID != "" {
		logx.Set(ctx, logx.Fields{UID: id.UID})
	}
	upgrade := r.Header.Get("Upgrade") != ""
	consented := false
	if err == nil {
		if consented, err = rt.Consented(ctx, id.UID); err != nil {
			rt.Log.ErrorContext(ctx, "consent lookup failed", "err", err)
			http.Error(w, german("Internal error"), http.StatusInternalServerError)
			return
		}
	}
	rt.Log.DebugContext(ctx, "router: request", "host", host, "username", id.Username, "identity_err", errString(err),
		"consented", consented, "upgrade", upgrade)
	if host == rt.Domain {
		if err == nil && !consented && !consentFree(r.URL.Path) {
			rt.Log.DebugContext(ctx, "router: to consent", "reason", "no consent", "path", r.URL.Path)
			rt.toConsent(w, r, upgrade, r.URL.RequestURI())
			return
		}
		// no account for people outside the group (the portal shows "kein Zugang")
		// nor before consent, so a person who declines leaves nothing behind
		if err == nil && consented {
			u, uerr := rt.Users.EnsureUser(ctx, id)
			if uerr != nil {
				rt.identityError(w, r, uerr)
				return
			}
			id.UID, id.Slug = u.UID, u.Slug
			logx.Set(ctx, logx.Fields{Slot: u.Slot})
		}
		if rt.Portal == nil {
			http.Error(w, german("Portal not available"), http.StatusServiceUnavailable)
			return
		}
		rt.Portal.ServeHTTP(w, r.WithContext(identity.WithIdentity(r.Context(), id)))
		return
	}
	slug, ok := strings.CutSuffix(host, "."+rt.Domain)
	if err != nil || !ok || strings.Contains(slug, ".") {
		rt.Log.DebugContext(ctx, "router: away", "reason", "unknown host or no identity", "host", host, "identity_err", errString(err))
		rt.away(w, r, upgrade)
		return
	}
	if isClear(r) {
		rt.Log.DebugContext(ctx, "router: clear site", "host", host)
		// account deletion removed consent and account; the cleanup must still run
		rt.clearSite(w, r)
		return
	}
	if !consented {
		rt.Log.DebugContext(ctx, "router: to consent", "reason", "no consent", "host", host)
		rt.toConsent(w, r, upgrade, "https://"+host+r.URL.RequestURI())
		return
	}
	u, err := rt.Users.EnsureUser(ctx, id)
	if err != nil {
		rt.identityError(w, r, err)
		return
	}
	id.UID = u.UID
	logx.Set(ctx, logx.Fields{Slot: u.Slot})
	if m := cadHost.FindStringSubmatch(slug); m != nil {
		rt.serveCAD(w, r, id.UID, m[1], host)
		return
	}
	if u.Slug != slug { // unknown slug or another person's
		rt.Log.DebugContext(ctx, "router: away", "reason", "slug mismatch", "slug", slug, "own_slug", u.Slug)
		rt.away(w, r, upgrade)
		return
	}
	// Pairing would be a second way in next to the injected Basic header.
	if (r.Method == http.MethodPost && r.URL.Path == "/api/pair") || strings.HasPrefix(r.URL.Path, "/auth/connect/") {
		rt.Log.DebugContext(ctx, "router: refused", "reason", "pairing path", "path", r.URL.Path)
		http.NotFound(w, r)
		return
	}
	// OpenCode does not check Origin on normal API calls; the proxy injects
	// credentials, so cross-site writes and WS handshakes must stop here.
	if o := r.Header.Get("Origin"); (upgrade || (r.Method != http.MethodGet && r.Method != http.MethodHead)) &&
		o != "" && o != "https://"+host {
		rt.Log.InfoContext(ctx, "router: refused", "reason", "foreign origin", "origin", o, "method", r.Method)
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if upgrade && r.Header.Get("Origin") == "" {
		rt.Log.InfoContext(ctx, "router: refused", "reason", "websocket without origin")
		w.WriteHeader(http.StatusForbidden) // browsers always send Origin on WS handshakes
		return
	}
	if rt.serveBrand(w, r) {
		return
	}
	if rt.RuntimeAccess != nil {
		if err := rt.RuntimeAccess(ctx, id.UID); err != nil {
			http.Error(w, "Assistant runtime unavailable.", http.StatusForbidden)
			return
		}
	}
	t, ok := rt.LC.Target(id.UID)
	if !ok {
		rt.Log.DebugContext(ctx, "router: no container", "navigation", navigation(r), "upgrade", upgrade)
		if r.Method == http.MethodGet && !upgrade && navigation(r) {
			http.Redirect(w, r, rt.portalURL("/environment"), http.StatusFound)
			return
		}
		// SSE reconnects of a forgotten tab must not start the container (docs/adr/README.md §3.8)
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if upgrade || r.URL.Path == "/api/event" {
		defer rt.LC.StreamOpened(id.UID)()
	} else {
		rt.LC.Touch(id.UID)
	}
	// bound to the container generation: account deletion cancels and drains it before the slot is reused
	ctx, release, ok := t.Bind(ctx)
	if !ok {
		rt.Log.DebugContext(ctx, "router: container generation ended", "reason", "lease gone")
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	defer release()
	if r.URL.Path == brandToolTitlesPath() {
		rt.serveToolTitles(w, r.WithContext(ctx), u)
		return
	}
	l := ""
	if wantsHTML(r) {
		l = rt.personLanguage(r, id.UID)
	}
	ctx = context.WithValue(ctx, proxyKey{}, proxyTarget{uid: id.UID, lang: l, target: t})
	rt.proxy.ServeHTTP(w, r.WithContext(ctx))
}

func (rt *Router) identityError(w http.ResponseWriter, r *http.Request, err error) {
	var blocked *store.IdentityBlockedError
	if errors.As(err, &blocked) {
		rt.Log.ErrorContext(r.Context(), "identity: account blocked", "stored_subject_hash", blocked.StoredHash, "incoming_subject_hash", blocked.IncomingHash)
		w.Header().Set("Cache-Control", "no-store")
		if rt.Portal != nil {
			rt.Portal.ServeHTTP(w, r.WithContext(identity.WithIdentity(r.Context(), identity.Identity{Blocked: true})))
		} else {
			w.WriteHeader(http.StatusForbidden)
		}
		return
	}
	rt.Log.ErrorContext(r.Context(), "user lookup failed", "err", err)
	http.Error(w, german("Internal error"), http.StatusInternalServerError)
}

// away sends a request for a host the person may not use back to the portal.
// A WebSocket handshake cannot follow a redirect, so it gets 403.
func (rt *Router) away(w http.ResponseWriter, r *http.Request, upgrade bool) {
	if upgrade {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	http.Redirect(w, r, rt.portalURL("/"), http.StatusFound)
}

// consentFree lists the portal paths served before consent: the notice
// itself, what it links to, and what the page needs to render.
func consentFree(p string) bool {
	switch p {
	case "/health", "/favicon.ico", "/privacy", "/how-it-works", "/deleted", "/consent", "/consent/declined", "/lang",
		// old paths, redirect-only (compatibility, remove after one release)
		"/datenschutz", "/so-gehts", "/geloescht", "/zustimmung", "/zustimmung/abgelehnt", "/sprache":
		return true
	}
	return strings.HasPrefix(p, "/static/")
}

// toConsent sends a page load to the notice and back to next afterwards;
// anything else (API, SSE, WebSocket, form posts) gets 403.
func (rt *Router) toConsent(w http.ResponseWriter, r *http.Request, upgrade bool, next string) {
	if upgrade || r.Method != http.MethodGet || !navigation(r) {
		http.Error(w, german("Please confirm the prototype notice in the portal first: %s", rt.portalURL("/consent")), http.StatusForbidden)
		return
	}
	http.Redirect(w, r, rt.portalURL("/consent?next="+url.QueryEscape(next)), http.StatusFound)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func navigation(r *http.Request) bool {
	return r.Header.Get("Sec-Fetch-Mode") == "navigate" || strings.Contains(r.Header.Get("Accept"), "text/html")
}
