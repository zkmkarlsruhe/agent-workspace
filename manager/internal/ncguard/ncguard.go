// SPDX-License-Identifier: MIT

// Package ncguard is the one gate for requests that carry a person's
// Nextcloud app password (D58). The password opens the whole account (files,
// calendars, contacts, shares and public links, Talk, Deck, Mail), so every
// client that sends it checks each request here before it leaves the manager:
//
//   - PROPFIND, GET on /remote.php/dav/files/<user>/... (reading files)
//   - MKCOL, PUT, MOVE only below /remote.php/dav/files/<user>/Agent Workspace;
//     PUT only with If-None-Match: * or a concrete If-Match, MOVE only with
//     Overwrite: F and a Destination below the same folder
//   - SEARCH on /remote.php/dav/ (the scope in the body is built by ncsync)
//   - GET /ocs/v2.php/cloud/user (own user id), DELETE /ocs/v2.php/core/apppassword (revoke)
//
// Everything else, including DELETE, COPY, PROPPATCH, another host and any
// path with "..", ".", "//" or an encoded dot, slash, backslash or NUL, fails.
package ncguard

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
)

// Root is the transfer folder, the only place a write may touch.
func Root() string { return instance.Value("nextcloud_root") }

var ErrRefused = errors.New("ncguard: request refused (D58)")

// Check reports whether r may go out to the Nextcloud at base.
func Check(base *url.URL, r *http.Request) error {
	p, ok := clean(base, r.URL)
	if !ok {
		return ErrRefused
	}
	switch {
	case r.Method == http.MethodGet && p == "/ocs/v2.php/cloud/user",
		r.Method == http.MethodDelete && p == "/ocs/v2.php/core/apppassword",
		r.Method == "SEARCH" && p == "/remote.php/dav/" && r.URL.RawQuery == "":
		return nil
	}
	user, sub, ok := files(p)
	if !ok || r.URL.RawQuery != "" {
		return ErrRefused
	}
	switch r.Method {
	case "PROPFIND", http.MethodGet:
		return nil
	case "MKCOL":
	case http.MethodPut:
		m := r.Header.Get("If-Match")
		if r.Header.Get("If-None-Match") != "*" && (m == "" || strings.Contains(m, "*")) {
			return ErrRefused
		}
	case "MOVE":
		d, err := url.Parse(r.Header.Get("Destination"))
		if err != nil || r.Header.Get("Overwrite") != "F" {
			return ErrRefused
		}
		dp, ok := clean(base, d)
		if !ok || d.RawQuery != "" {
			return ErrRefused
		}
		if du, ds, ok := files(dp); !ok || du != user || !inRoot(ds) {
			return ErrRefused
		}
	default:
		return ErrRefused
	}
	if !inRoot(sub) {
		return ErrRefused
	}
	return nil
}

// files returns the user and the part after /remote.php/dav/files/<user>/ ("" = the user root).
func files(p string) (string, string, bool) {
	rest, ok := strings.CutPrefix(p, "/remote.php/dav/files/")
	if !ok {
		return "", "", false
	}
	user, sub, _ := strings.Cut(rest, "/")
	return user, sub, user != ""
}

func inRoot(sub string) bool { return sub == Root() || strings.HasPrefix(sub, Root()+"/") }

// clean returns the decoded path of u relative to base, if u points at the
// same scheme and host, below base's path, without anything a server could
// resolve differently.
func clean(base, u *url.URL) (string, bool) {
	if u.Scheme != base.Scheme || u.Host != base.Host || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return "", false
	}
	esc := u.EscapedPath()
	low := strings.ToLower(esc)
	for _, bad := range []string{"//", "%2e", "%2f", "%5c", "%00", "\\"} {
		if strings.Contains(low, bad) {
			return "", false
		}
	}
	p, err := url.PathUnescape(esc)
	if err != nil || strings.ContainsRune(p, 0) {
		return "", false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return "", false
		}
	}
	p, ok := strings.CutPrefix(p, strings.TrimRight(base.Path, "/"))
	if !ok || !strings.HasPrefix(p, "/") {
		return "", false
	}
	return p, true
}

// Transport checks every request that carries credentials; requests without
// an Authorization header (Login Flow v2) pass unchanged.
type Transport struct {
	Base *url.URL
	Next http.RoundTripper // nil = http.DefaultTransport
}

func (t Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Header.Get("Authorization") != "" {
		if err := Check(t.Base, r); err != nil {
			if r.Body != nil {
				r.Body.Close()
			}
			return nil, err
		}
	}
	next := t.Next
	if next == nil {
		next = http.DefaultTransport
	}
	return next.RoundTrip(r)
}
