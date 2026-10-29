// SPDX-License-Identifier: MIT

package logx

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"runtime/debug"
	"time"
)

// HeaderReqID carries the request id in and out.
const HeaderReqID = "X-Request-Id"

var reqIDRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)

func newReqID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Middleware gives every request of one listener a request id, recovers
// panics (500) and writes one access log line. trusted reports whether the
// direct peer may set the id (X-Request-Id); nil trusts nobody. The line
// has the route pattern of the mux that served the request, or the path
// without query; /health logs at debug.
func Middleware(log *slog.Logger, listener string, trusted func(*http.Request) bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderReqID)
		if trusted == nil || !reqIDRE.MatchString(id) || !trusted(r) {
			id = newReqID()
		}
		h := &holder{f: Fields{ReqID: id}}
		ctx := context.WithValue(r.Context(), ctxKey{}, h)
		r = r.WithContext(ctx)
		w.Header().Set(HeaderReqID, id)
		rec := &recorder{ResponseWriter: w}
		start := time.Now()
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler { // the handler's own abort: no 500, no stack
					panic(v)
				}
				log.ErrorContext(ctx, "panic", "listener", listener, "method", r.Method, "path", r.URL.Path,
					"panic", fmt.Sprint(v), "stack", string(debug.Stack()))
				if !rec.wrote {
					http.Error(rec, "internal error", http.StatusInternalServerError)
				}
			}
			route := r.Pattern
			if route == "" {
				h.mu.Lock()
				route = h.route
				h.mu.Unlock()
			}
			if route == "" {
				route = r.URL.Path
			}
			lvl := slog.LevelInfo
			if r.URL.Path == "/health" {
				lvl = slog.LevelDebug
			}
			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}
			log.LogAttrs(ctx, lvl, "http", slog.String("listener", listener), slog.String("method", r.Method),
				slog.String("route", route), slog.Int("status", status), slog.Int64("bytes", rec.bytes),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()))
		}()
		next.ServeHTTP(rec, r)
	})
}

// Route wraps the mux of a handler behind other routing (portal behind the
// router): its matched pattern goes to the access log.
func Route(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r) // sets r.Pattern on r itself
		if h := get(r.Context()); h != nil && r.Pattern != "" {
			h.mu.Lock()
			h.route = r.Pattern
			h.mu.Unlock()
		}
	})
}

// recorder counts status and bytes; Flush, Hijack and Unwrap keep SSE and
// WebSocket working through it.
type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (r *recorder) WriteHeader(code int) {
	if !r.wrote || code < 200 { // 1xx are informational, the final status follows
		r.status = code
	}
	if code >= 200 {
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.wrote = true
		if r.status == 0 || r.status < 200 {
			r.status = http.StatusOK
		}
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *recorder) Flush() {
	if !r.wrote {
		r.wrote, r.status = true, http.StatusOK
	}
	http.NewResponseController(r.ResponseWriter).Flush()
}

func (r *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, rw, err := http.NewResponseController(r.ResponseWriter).Hijack()
	if err == nil && !r.wrote {
		r.wrote, r.status = true, http.StatusSwitchingProtocols
	}
	return c, rw, err
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Transport logs every call to an upstream service: debug with method,
// host, path (never the query), status, duration and size; warn on a
// transport error or a status >= 500, info on 4xx (debug on 404 and 412),
// with the first 512 bytes of the body when Body is set. With Propagate the request id goes along as
// X-Request-Id (own services only, never third-party sites).
type Transport struct {
	Upstream  string // name in the log: litellm, gotenberg, nextcloud, ...
	Next      http.RoundTripper
	Propagate bool
	Body      bool
	NoPath    bool // third-party URLs can carry private data in their path
}

func (t Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	if id := ReqID(ctx); t.Propagate && id != "" && req.Header.Get(HeaderReqID) == "" {
		req = req.Clone(ctx) // a RoundTripper must not change the caller's request
		req.Header.Set(HeaderReqID, id)
	}
	next := t.Next
	if next == nil {
		next = http.DefaultTransport
	}
	start := time.Now()
	resp, err := next.RoundTrip(req)
	attrs := []slog.Attr{slog.String("upstream", t.Upstream), slog.String("method", req.Method),
		slog.String("host", req.URL.Host),
		slog.Int64("duration_ms", time.Since(start).Milliseconds())}
	if !t.NoPath {
		attrs = append(attrs, slog.String("path", req.URL.Path))
	}
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			slog.Default().LogAttrs(ctx, slog.LevelWarn, "upstream error", append(attrs, slog.String("err", err.Error()))...)
		}
		return nil, err
	}
	attrs = append(attrs, slog.Int("status", resp.StatusCode), slog.Int64("bytes", resp.ContentLength))
	if resp.StatusCode < 400 {
		slog.Default().LogAttrs(ctx, slog.LevelDebug, "upstream", attrs...)
		return resp, nil
	}
	if t.Body && resp.Body != nil {
		peek, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(peek), resp.Body), resp.Body}
		attrs = append(attrs, slog.String("body", Truncate(peek)))
	}
	lvl := slog.LevelWarn
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusPreconditionFailed:
		lvl = slog.LevelDebug // answers callers act on: WebDAV state, login flow poll every 2 s
	case resp.StatusCode < 500:
		lvl = slog.LevelInfo
	}
	slog.Default().LogAttrs(ctx, lvl, "upstream status", attrs...)
	return resp, nil
}
