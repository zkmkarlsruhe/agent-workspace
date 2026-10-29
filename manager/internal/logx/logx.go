// SPDX-License-Identifier: MIT

// Package logx is the logging of all three binaries: JSON to stdout, the
// level from OC_LOG_LEVEL, and request-scoped fields (req_id, uid, slot,
// session_id) taken from the context of every *Context log call.
//
// Never log chat content, prompts, model output, file contents, secrets,
// cookies or query strings: ids, names, sizes, statuses and durations only.
package logx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
)

// Attribute keys shared by all binaries.
const (
	KeyReqID     = "req_id"
	KeyUID       = "uid"
	KeySlot      = "slot"
	KeySessionID = "session_id"
)

// ParseLevel reads OC_LOG_LEVEL: debug, info, warn or error; empty is info.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("OC_LOG_LEVEL %q: want debug, info, warn or error", s)
}

// Setup builds the logger from OC_LOG_LEVEL, writing JSON to w, and makes it
// the slog default (packages that log via the slog functions use it too).
// An invalid level is reported after the logger runs at info.
func Setup(w io.Writer) (*slog.Logger, error) {
	lvl, err := ParseLevel(os.Getenv("OC_LOG_LEVEL"))
	log := slog.New(NewHandler(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})))
	slog.SetDefault(log)
	return log, err
}

// Fields are the request-scoped attributes; zero values are left out.
type Fields struct {
	ReqID, UID, SessionID string
	Slot                  int
}

type holder struct {
	mu    sync.Mutex
	f     Fields
	route string // access log: the pattern of an inner mux (Route)
}

type ctxKey struct{}

func get(ctx context.Context) *holder {
	h, _ := ctx.Value(ctxKey{}).(*holder)
	return h
}

func merge(dst *Fields, f Fields) {
	if f.ReqID != "" {
		dst.ReqID = f.ReqID
	}
	if f.UID != "" {
		dst.UID = f.UID
	}
	if f.SessionID != "" {
		dst.SessionID = f.SessionID
	}
	if f.Slot != 0 {
		dst.Slot = f.Slot
	}
}

// Set adds f to the fields of ctx in place, so the request's access log sees
// them too. Without fields in ctx it behaves like With.
func Set(ctx context.Context, f Fields) context.Context {
	h := get(ctx)
	if h == nil {
		return With(ctx, f)
	}
	h.mu.Lock()
	merge(&h.f, f)
	h.mu.Unlock()
	return ctx
}

// With returns a child context whose fields are those of ctx plus f; ctx is
// unchanged (background loops over several people use this).
func With(ctx context.Context, f Fields) context.Context {
	n := &holder{}
	if h := get(ctx); h != nil {
		n.f = h.Get()
	}
	merge(&n.f, f)
	return context.WithValue(ctx, ctxKey{}, n)
}

// FromContext returns the fields of ctx.
func FromContext(ctx context.Context) Fields {
	if h := get(ctx); h != nil {
		return h.Get()
	}
	return Fields{}
}

func (h *holder) Get() Fields {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.f
}

// ReqID returns the request id of ctx, "" if none.
func ReqID(ctx context.Context) string { return FromContext(ctx).ReqID }

// Handler adds the fields of the record's context to every record. A key the
// call site already set wins.
type Handler struct{ next slog.Handler }

func NewHandler(next slog.Handler) *Handler { return &Handler{next} }

func (h *Handler) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	if ctx == nil {
		return h.next.Handle(ctx, r)
	}
	f := FromContext(ctx)
	if f == (Fields{}) {
		return h.next.Handle(ctx, r)
	}
	var have map[string]bool
	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case KeyReqID, KeyUID, KeySlot, KeySessionID:
			if have == nil {
				have = map[string]bool{}
			}
			have[a.Key] = true
		}
		return true
	})
	add := func(k string, v slog.Value, set bool) {
		if set && !have[k] {
			r.AddAttrs(slog.Attr{Key: k, Value: v})
		}
	}
	add(KeyReqID, slog.StringValue(f.ReqID), f.ReqID != "")
	add(KeyUID, slog.StringValue(f.UID), f.UID != "")
	add(KeySlot, slog.IntValue(f.Slot), f.Slot != 0)
	add(KeySessionID, slog.StringValue(f.SessionID), f.SessionID != "")
	return h.next.Handle(ctx, r)
}

func (h *Handler) WithAttrs(as []slog.Attr) slog.Handler { return &Handler{h.next.WithAttrs(as)} }
func (h *Handler) WithGroup(name string) slog.Handler    { return &Handler{h.next.WithGroup(name)} }

// Truncate cuts an upstream error body to at most 512 bytes for a log line.
func Truncate(b []byte) string {
	const max = 512
	if len(b) > max {
		b = b[:max]
	}
	return strings.ToValidUTF8(strings.TrimSpace(string(b)), "")
}

// ArgSizes maps the top-level keys of a JSON object (tool arguments) to the
// byte size of their values: the shape of a call, never its content.
func ArgSizes(raw []byte) map[string]int {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = len(v)
	}
	return out
}
