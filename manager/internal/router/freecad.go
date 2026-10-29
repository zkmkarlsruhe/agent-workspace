// SPDX-License-Identifier: MIT

package router

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var cadHost = regexp.MustCompile(`^cad-([a-f0-9]{32})$`)

func (rt *Router) serveCAD(w http.ResponseWriter, r *http.Request, uid, session, host string) {
	if rt.FreeCADTarget == nil {
		http.NotFound(w, r)
		return
	}
	if rt.RuntimeAccess != nil {
		if err := rt.RuntimeAccess(r.Context(), uid); err != nil {
			w.WriteHeader(403)
			return
		}
	}
	upgrade := r.Header.Get("Upgrade") != ""
	origin := r.Header.Get("Origin")
	if (upgrade && origin == "") || (origin != "" && origin != "https://"+host) {
		w.WriteHeader(403)
		return
	}
	target, ok := rt.FreeCADTarget(r.Context(), uid, session)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx, release, ok := target.Bind(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	defer release()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Recheck revocations during a stream, not only at its handshake.
	go func() {
		timer := time.NewTicker(5 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				if consented, err := rt.Consented(ctx, uid); err != nil || !consented {
					cancel()
					return
				}
				if rt.RuntimeAccess != nil && rt.RuntimeAccess(ctx, uid) != nil {
					cancel()
					return
				}
				if _, ok := rt.FreeCADTarget(ctx, uid, session); !ok {
					cancel()
					return
				}
			}
		}
	}()
	if upgrade {
		defer rt.LC.StreamOpened(uid)()
	} else {
		rt.LC.Touch(uid)
	}
	upstream, _ := url.Parse(target.BaseURL)
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			pr.Out.Host = r.Host
			pr.Out.Header.Del("Cookie")
			pr.Out.SetBasicAuth("workspace", target.Password)
			h := rt.Checker.Headers.WithDefaults()
			for _, key := range []string{h.UID, h.Username, h.Email, h.Name, h.Groups, rt.Checker.SubjectHeader} {
				pr.Out.Header.Del(key)
			}
			for key := range pr.Out.Header {
				if strings.HasPrefix(strings.ToLower(key), "x-authentik-") {
					pr.Out.Header.Del(key)
				}
			}
			pr.SetXForwarded()
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, _ error) {
			http.Error(w, "FreeCAD session unavailable", http.StatusBadGateway)
		},
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del("Set-Cookie")
			resp.Header.Set("Cache-Control", "no-store")
			resp.Header.Set("Referrer-Policy", "no-referrer")
			return nil
		},
	}
	proxy.ServeHTTP(w, r.WithContext(ctx))
}
