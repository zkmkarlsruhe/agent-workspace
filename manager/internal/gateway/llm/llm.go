// SPDX-License-Identifier: MIT

// Package llm is llm-gw: the only way from a user container to LiteLLM. The
// person comes from the source network, never from headers; client auth and
// every LiteLLM steering header or body field are dropped (docs/adr/README.md §3.3).
package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/co2"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/logx"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

const maxBody = 32 << 20

const costCeiling = 1000.0 // A single call cannot plausibly cost more in EUR.

func validCostEUR(f float64) bool {
	return f > 0 && !math.IsNaN(f) && !math.IsInf(f, 0) && f <= costCeiling
}

type Users interface {
	UserBySlot(ctx context.Context, slot int) (store.User, error)
}

type Recorder interface {
	InsertUsage(ctx context.Context, e store.UsageEvent) error
}

type Gateway struct {
	Accounting *instance.Accounting // nil uses the selected instance
	Upstream   *url.URL             // https://models.example.org
	APIKey     string               // opencode-unified
	Flavor     string               // empty or litellm (default); openai sends no attribution
	SlotOf     func(netip.Addr) (int, bool)
	Users      Users
	Usage      Recorder
	Log        *slog.Logger

	MaxBodyMB *int // nil = 32 MiB; 0 = unlimited
	// Budget caps Complete like the Gate caps llm-gw; store.ManagerUID is exempt.
	Budget *Budget

	proxy *httputil.ReverseProxy
}

var (
	sessionRE = regexp.MustCompile(`^ses_[0-9A-Za-z]{8,80}$`)
	projectRE = regexp.MustCompile(`^[0-9A-Za-z_-]{1,128}$`)
	// paths and their capability; everything else is 404
	routes = map[string]string{
		"POST /v1/chat/completions": "llm",
		"GET /v1/models":            "",
		"POST /v1/embeddings":       "embedding",
	}
)

type reqKey struct{}

type reqMeta struct {
	user    store.User
	session string
	ev      *store.UsageEvent // nil: not metered
	sent    atomic.Bool       // the request reached the upstream connection
}

// dispatched sets sent once the request headers went to the upstream
// connection: from then on LiteLLM may run (and bill) it, cancelled or not.
func dispatched(ctx context.Context, sent *atomic.Bool) context.Context {
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteHeaders: func() { sent.Store(true) }})
}

// unanswered charges an LLM call that reached the upstream but ended without a
// response (client gone or upstream error before the headers) the configured reserve:
// the outcome is unknown, so it counts as dear as an aborted stream. Only a
// call that provably never left (dial or connect error) stays free.
func unanswered(e *store.UsageEvent, sent bool, a instance.Accounting) {
	if sent && e.Capability == "llm" && e.CostEUR == nil && len(a.Prices) > 0 {
		c := a.ReserveEUR()
		if validCostEUR(c) {
			e.CostEUR, e.CostSource = &c, "estimate"
		}
	}
}

func New(g Gateway) *Gateway {
	gw := &g
	gw.proxy = &httputil.ReverseProxy{
		Rewrite:       gw.rewrite,
		FlushInterval: -1,
		Transport: logx.Transport{Upstream: "llm", Propagate: gw.Flavor != "openai", Next: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 620 * time.Second, // LiteLLM request_timeout 600 s
			MaxIdleConnsPerHost:   32,
			ForceAttemptHTTP2:     true,
		}},
		ModifyResponse: gw.modifyResponse,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if m, ok := r.Context().Value(reqKey{}).(*reqMeta); ok && m.ev != nil {
				m.ev.Status = http.StatusBadGateway
				unanswered(m.ev, m.sent.Load(), gw.accounting())
				gw.record(r.Context(), *m.ev)
			}
			gw.Log.WarnContext(r.Context(), "llm upstream error", "upstream", "llm", "path", r.URL.Path, "err", err)
			w.Header().Set("Retry-After", "10")
			oaiError(w, http.StatusBadGateway, german("AI service not reachable."), "upstream_error", true)
		},
	}
	return gw
}

func oaiError(w http.ResponseWriter, code int, msg, typ string, retry bool) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("x-should-retry", strconv.FormatBool(retry))
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": msg, "type": typ}})
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	addr, _ := netip.ParseAddr(host)
	slot, ok := g.SlotOf(addr)
	var u store.User
	var err error
	if ok {
		u, err = g.Users.UserBySlot(r.Context(), slot)
	}
	if !ok || err != nil {
		g.Log.InfoContext(r.Context(), "llm-gw: no person for source", "source", host, "slot_known", ok, "err", err)
		oaiError(w, http.StatusForbidden, german("No access to the AI service."), "permission_denied", false)
		return
	}
	capability, allowed := routes[r.Method+" "+r.URL.Path]
	if !allowed {
		g.Log.DebugContext(r.Context(), "llm-gw: unknown route", "method", r.Method, "path", r.URL.Path)
		oaiError(w, http.StatusNotFound, "Unbekannter Endpunkt.", "not_found", false)
		return
	}
	if strings.HasPrefix(r.Header.Get("x-opencode-session-id"), "ses_profile_") || strings.HasPrefix(r.Header.Get("x-opencode-session"), "ses_profile_") {
		oaiError(w, http.StatusForbidden, "Assistant context cannot be supplied by a runtime.", "permission_denied", false)
		return
	}
	m := &reqMeta{user: u}
	if s := r.Header.Get("x-opencode-session-id"); sessionRE.MatchString(s) {
		m.session = s
		logx.Set(r.Context(), logx.Fields{SessionID: s})
	}
	project := r.Header.Get("x-opencode-project")
	if !projectRE.MatchString(project) {
		project = ""
	}
	var body []byte
	model := ""
	if r.Body != nil && r.Method == http.MethodPost {
		limit := int64(maxBody)
		if g.MaxBodyMB != nil {
			limit = int64(*g.MaxBodyMB) << 20
		}
		var reader io.Reader = r.Body
		if limit > 0 {
			reader = http.MaxBytesReader(w, r.Body, limit)
		}
		body, err = io.ReadAll(reader)
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			oaiError(w, http.StatusRequestEntityTooLarge, german("Request too large."), "payload_too_large", false)
			return
		} else if err != nil {
			oaiError(w, http.StatusBadRequest, german("Invalid JSON."), "invalid_request_error", false)
			return
		}
		g.Log.DebugContext(r.Context(), "llm-gw: request", "path", r.URL.Path, "bytes", len(body), "project", project)
		if body, model, err = normalize(body, capability == "llm", g.accounting().MaxOutputTokens); err != nil {
			oaiError(w, http.StatusBadRequest, german("Invalid JSON."), "invalid_request_error", false)
			return
		}
		models := instance.Current().Models
		// Assistant variants use the same upstream model ID as their configured role.
		if g.Flavor == "openai" && (model == "" || (model != models.Default && model != models.Small && model != models.Thorough)) {
			oaiError(w, http.StatusBadRequest, "Model is not configured.", "invalid_request_error", false)
			return
		}
	}
	if capability != "" {
		id, _ := uuid.NewV7()
		m.ev = &store.UsageEvent{EventID: id.String(), Start: time.Now(), UID: u.UID, ProjectID: project,
			SessionID: m.session, RootSessionID: m.session, Capability: capability, Target: model}
		// x-opencode-session is the root (chat) of a subagent call (docs/adr/README.md B.1)
		if s := r.Header.Get("x-opencode-session"); sessionRE.MatchString(s) {
			m.ev.RootSessionID = s
		}
	}
	out := r.Clone(dispatched(context.WithValue(r.Context(), reqKey{}, m), &m.sent))
	out.Body = io.NopCloser(bytes.NewReader(body))
	out.ContentLength = int64(len(body))
	g.proxy.ServeHTTP(w, out)
}

// normalize drops steering keys and forces a usage chunk on streams. A chat
// call (chat) gets one choice and at most maxOutTokens output. It returns
// the model for metering. Both flavors use the same spec-defined token bounds.
func normalize(b []byte, chat bool, maxOutTokens int) ([]byte, string, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, "", err
	}
	if m == nil {
		return nil, "", errors.New("request must be an object")
	}
	for k := range m {
		if !requestKey(k) {
			delete(m, k)
			slog.Debug("llm-gw: dropped request field", "key", k)
		}
	}
	var model string
	json.Unmarshal(m["model"], &model)
	var stream bool
	json.Unmarshal(m["stream"], &stream)
	so := map[string]json.RawMessage{}
	if raw, ok := m["stream_options"]; ok {
		if err := json.Unmarshal(raw, &so); err != nil || so == nil {
			return nil, "", errors.New("stream_options must be an object")
		}
	}
	if stream {
		so["include_usage"] = json.RawMessage("true")
		m["stream_options"], _ = json.Marshal(so)
	}
	if chat {
		delete(m, "n")
		bound := func(k string, always bool) {
			var v float64
			if err := json.Unmarshal(m[k], &v); (err != nil && always) || (err == nil && (v > float64(maxOutTokens) || v < 1)) {
				m[k] = json.RawMessage(strconv.Itoa(maxOutTokens))
			}
		}
		bound("max_tokens", true)
		bound("max_completion_tokens", false)
	}
	out, err := json.Marshal(m)
	return out, model, err
}

func requestKey(k string) bool {
	switch k {
	case "model", "messages", "input", "stream", "stream_options", "tools", "tool_choice",
		"temperature", "top_p", "max_tokens", "max_completion_tokens", "stop", "response_format",
		"seed", "reasoning_effort", "parallel_tool_calls", "logprobs", "top_logprobs",
		"frequency_penalty", "presence_penalty", "logit_bias", "n", "encoding_format", "dimensions":
		return true
	}
	return false
}

func (g *Gateway) rewrite(r *httputil.ProxyRequest) {
	m := r.In.Context().Value(reqKey{}).(*reqMeta)
	r.SetURL(g.Upstream)
	// allowlist, not denylist: everything else (client auth, x-litellm-*,
	// X-OpenWebUI-*, generic session ids, tracing baggage) never leaves
	h := http.Header{}
	for _, k := range []string{"Content-Type", "Accept", "User-Agent"} {
		if v := r.In.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	h.Set("Accept-Encoding", "identity") // the usage scanner reads plain text
	callID := ""
	if m.ev != nil {
		callID = m.ev.EventID
	}
	g.upstreamHeaders(r.In.Context(), h, m.user, m.session, callID)
	r.Out.Header = h
}

// upstreamHeaders is shared by runtime, internal chat and image requests.
func (g *Gateway) upstreamHeaders(ctx context.Context, h http.Header, u store.User, session, callID string) {
	if g.APIKey != "" {
		h.Set("Authorization", "Bearer "+g.APIKey)
	}
	if g.Flavor == "openai" {
		return
	}
	h.Set("x-litellm-end-user-id", u.Username)
	if session != "" {
		h.Set("x-litellm-session-id", session)
	}
	if callID != "" {
		h.Set("x-litellm-call-id", callID) // = SpendLogs request_id for reconciliation
	}
	spendMeta(ctx, h)
}

func (g *Gateway) responseUsage(ev *store.UsageEvent, resp *http.Response) {
	ev.Status = resp.StatusCode
	if g.Flavor == "openai" {
		return
	}
	ev.DeploymentID = resp.Header.Get("x-litellm-model-id")
	if c, err := strconv.ParseFloat(resp.Header.Get("x-litellm-response-cost"), 64); err == nil {
		upstreamCost(ev, c, "header", g.accounting()) // usage.cost in the body wins
	}
}

func (g *Gateway) modifyResponse(resp *http.Response) error {
	resp.Body = responseBody{resp.Body, resp}
	m, _ := resp.Request.Context().Value(reqKey{}).(*reqMeta)
	if m != nil && m.ev != nil {
		ev := *m.ev
		g.responseUsage(&ev, resp)
		ctx := resp.Request.Context()
		resp.Body = &meter{rc: resp.Body, sse: strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream"),
			accounting: g.accounting(), flavor: g.Flavor, ev: ev, done: func(e store.UsageEvent) { g.record(ctx, e) }}
	}
	// Upstream diagnostics can expose credentials, routing and internal API bases.
	for k := range resp.Header {
		switch strings.ToLower(k) {
		case "content-type", "content-length", "cache-control", "retry-after", "x-request-id", "request-id", "openai-request-id":
		default:
			delete(resp.Header, k)
		}
	}
	resp.Trailer = nil
	return nil
}

// HTTP transports may populate trailers only when the body reaches EOF.
// Drop those too, including on unmetered /models responses.
type responseBody struct {
	io.ReadCloser
	response *http.Response
}

func (b responseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.response.Trailer = nil
	return n, err
}

// record stores one usage event; the log line links req_id to event_id, the
// LiteLLM call id (SpendLogs request_id).
func (g *Gateway) record(lctx context.Context, e store.UsageEvent) {
	e.End = time.Now()
	charge(&e, g.accounting())
	estimate(&e)
	attrs := []any{"event_id", e.EventID, "capability", e.Capability, "model", e.Target, "status", e.Status,
		"duration_ms", e.End.Sub(e.Start).Milliseconds(), "session_id", e.SessionID, "project", e.ProjectID,
		"finish_reason", e.FinishReason, "cost_source", e.CostSource}
	for _, t := range []struct {
		k string
		v *int64
	}{{"in_tokens", e.InTokens}, {"out_tokens", e.OutTokens}, {"cached_tokens", e.CachedTokens}} {
		if t.v != nil {
			attrs = append(attrs, t.k, *t.v)
		}
	}
	if e.CostEUR != nil {
		attrs = append(attrs, "cost_eur", *e.CostEUR)
	}
	lvl := slog.LevelInfo
	if e.Status >= 400 {
		lvl = slog.LevelWarn
	}
	g.Log.Log(lctx, lvl, "llm call", attrs...)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := g.Usage.InsertUsage(ctx, e); err != nil {
		g.Log.ErrorContext(lctx, "usage insert failed", "event_id", e.EventID, "err", err)
	}
}

// spendMeta puts the request id into LiteLLM's spend log metadata.
func spendMeta(ctx context.Context, h http.Header) {
	if id := logx.ReqID(ctx); id != "" {
		b, _ := json.Marshal(map[string]string{"req_id": id})
		h.Set("x-litellm-spend-logs-metadata", string(b))
	}
}

func (g *Gateway) accounting() instance.Accounting {
	if g.Accounting != nil {
		return *g.Accounting
	}
	return instance.Current().Accounting
}

// upstreamCost retains finite raw evidence even when its unit is unknown or the value is rejected.
// Invalid body cost never replaces a usable header cost.
func upstreamCost(e *store.UsageEvent, raw float64, source string, a instance.Accounting) {
	if math.IsNaN(raw) || math.IsInf(raw, 0) {
		return
	}
	unit := a.UpstreamCostCurrency
	if unit == "" {
		unit = "none"
	}
	c := raw
	if unit == "USD" {
		c /= a.USDPerEUR
	}
	valid := (unit == "EUR" || unit == "USD") && validCostEUR(raw) && validCostEUR(c)
	if valid || e.CostEUR == nil {
		e.RawCost, e.RawCostUnit, e.RawCostSource = &raw, unit, source
	}
	if !valid {
		return
	}
	if unit == "USD" {
		source += "-usd"
	}
	e.CostEUR, e.CostSource = &c, source
}

// charge labels missing billing explicitly. Partial chat usage uses configured limits for missing counters.
func charge(e *store.UsageEvent, a instance.Accounting) {
	rejectUsage(e, a)
	if e.CostEUR != nil {
		if validCostEUR(*e.CostEUR) || (*e.CostEUR == 0 && e.CostSource == "estimate") {
			return
		}
		e.CostEUR = nil
	}
	e.CostSource = "unknown"
	if e.Status/100 != 2 {
		return
	}
	var c float64
	switch e.Capability {
	case "embedding":
		if a.EmbeddingEURPerMillion == 0 || e.InTokens == nil {
			return
		}
		c = float64(max(0, *e.InTokens)) * a.EmbeddingEURPerMillion / 1e6
	case "llm":
		if len(a.Prices) == 0 {
			return
		}
		p := a.Dearest()
		roles := map[string]string{"standard": instance.Current().Models.Default, "fast": instance.Current().Models.Small, "thorough": instance.Current().Models.Thorough}
		// Shared aliases are priced conservatively across their roles, including missing prices.
		var matched bool
		for role, alias := range roles {
			if alias != e.Target {
				continue
			}
			price, ok := a.Prices[role]
			if !ok {
				price = a.Dearest()
			}
			if !matched {
				p = price
				matched = true
			} else {
				p.Input, p.Output = max(p.Input, price.Input), max(p.Output, price.Output)
			}
		}
		c = a.ReserveEUR()
		if e.InTokens != nil || e.OutTokens != nil {
			in, out := a.LargestContext, int64(a.MaxOutputTokens)
			if e.InTokens != nil {
				in = max(0, *e.InTokens)
			}
			if e.OutTokens != nil {
				out = max(0, *e.OutTokens)
			}
			c = (float64(in)*p.Input + float64(out)*p.Output) / 1e6
		}
	default:
		return
	}
	if validCostEUR(c) || c == 0 {
		e.CostEUR, e.CostSource = &c, "estimate"
	}
}

// Impossible counters invalidate all reported usage and billing for the call.
func rejectUsage(e *store.UsageEvent, a instance.Accounting) bool {
	for _, bound := range []struct {
		tokens *int64
		limit  int64
	}{{e.InTokens, a.LargestContext}, {e.OutTokens, int64(a.MaxOutputTokens)},
		{e.CachedTokens, a.LargestContext}, {e.Reasoning, int64(a.MaxOutputTokens)}} {
		if bound.tokens != nil && *bound.tokens > bound.limit {
			e.InTokens, e.OutTokens, e.CachedTokens, e.Reasoning = nil, nil, nil, nil
			e.CostEUR, e.CostSource = nil, "unknown"
			if c := a.ReserveEUR(); validCostEUR(c) {
				e.CostEUR, e.CostSource = &c, "estimate"
			}
			return true
		}
	}
	return false
}

// estimate sets energy and CO2 for LLM calls with reported tokens. Unknown
// aliases keep NULL values and get a "-unknown" method version (docs/adr/README.md §3.7).
func estimate(e *store.UsageEvent) {
	if e.Capability != "llm" || (e.InTokens == nil && e.OutTokens == nil) {
		return
	}
	val := func(p *int64) int64 {
		if p == nil {
			return 0
		}
		return *p
	}
	whLo, whHi, gLo, gHi, ok := co2.Estimate(e.Target, val(e.InTokens), val(e.CachedTokens), val(e.OutTokens))
	if !ok {
		e.MethodVersion = co2.Version + "-unknown"
		return
	}
	e.WhLo, e.WhHi, e.CO2LoG, e.CO2HiG, e.MethodVersion = &whLo, &whHi, &gLo, &gHi, co2.Version
}

const maxScan = 8 << 20

// meter passes the body through untouched and records exactly one usage
// event at EOF or close (client abort included).
type meter struct {
	accounting   instance.Accounting
	flavor       string
	invalidUsage bool
	rc           io.ReadCloser
	sse          bool
	buf          bytes.Buffer
	skip         bool // current SSE line exceeded maxScan
	ev           store.UsageEvent
	done         func(store.UsageEvent)
	once         sync.Once
}

func (m *meter) Read(p []byte) (int, error) {
	n, err := m.rc.Read(p)
	m.feed(p[:n])
	if err == io.EOF {
		m.finish()
	}
	return n, err
}

func (m *meter) Close() error {
	m.finish()
	return m.rc.Close()
}

func (m *meter) feed(p []byte) {
	if !m.sse {
		if m.buf.Len()+len(p) <= maxScan {
			m.buf.Write(p)
		}
		return
	}
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if m.buf.Len()+len(p) > maxScan {
				m.buf.Reset()
				m.skip = true
			} else if !m.skip {
				m.buf.Write(p)
			}
			return
		}
		if !m.skip {
			m.buf.Write(p[:i])
			m.line(m.buf.Bytes())
		}
		m.buf.Reset()
		m.skip = false
		p = p[i+1:]
	}
}

type usage struct {
	PromptTokens   *int64   `json:"prompt_tokens"`
	CompletionToks *int64   `json:"completion_tokens"`
	Cost           *float64 `json:"cost"`
	PromptDetails  struct {
		Cached *int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionDetails struct {
		Reasoning *int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

type chunk struct {
	Usage   *usage `json:"usage"`
	Choices []struct {
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

func (m *meter) line(l []byte) {
	data, ok := bytes.CutPrefix(l, []byte("data:"))
	if !ok {
		return
	}
	data = bytes.TrimSpace(data)
	if !bytes.Contains(data, []byte(`"usage"`)) && !bytes.Contains(data, []byte(`"finish_reason"`)) {
		return
	}
	m.apply(data)
}

func (m *meter) apply(data []byte) {
	var c chunk
	if json.Unmarshal(data, &c) != nil {
		return
	}
	for _, ch := range c.Choices {
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			m.ev.FinishReason = *ch.FinishReason
		}
	}
	if u := c.Usage; u != nil && !m.invalidUsage {
		for _, tokens := range []*int64{u.PromptTokens, u.CompletionToks, u.PromptDetails.Cached, u.CompletionDetails.Reasoning} {
			if tokens != nil && *tokens < 0 {
				*tokens = 0
			}
		}
		m.ev.InTokens, m.ev.OutTokens = u.PromptTokens, u.CompletionToks
		m.ev.CachedTokens, m.ev.Reasoning = u.PromptDetails.Cached, u.CompletionDetails.Reasoning
		if m.invalidUsage = rejectUsage(&m.ev, m.accounting); m.invalidUsage {
			return
		}
		if u.Cost != nil && m.flavor != "openai" {
			upstreamCost(&m.ev, *u.Cost, "usage", m.accounting)
		}
	}
}

func (m *meter) finish() {
	m.once.Do(func() {
		if !m.sse {
			m.apply(m.buf.Bytes())
		}
		m.done(m.ev)
	})
}

// Suggest asks the fast model for a project folder name (portal "Neues
// Projekt"). The portal normalises the answer and has a date fallback. The
// call is metered like any other request of the person.
func (g *Gateway) Suggest(ctx context.Context, u store.User, description string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return g.Complete(ctx, u, instance.Current().CopyText("folder_name_prompt", "en"),
		description, 20)
}

// Complete is one metered call of the fast model (small) on
// behalf of u, for manager features (folder names, Profile); the caller sets
// the timeout.
func (g *Gateway) Complete(ctx context.Context, u store.User, system, user string, maxTokens int) (string, error) {
	return g.chat(ctx, u, "", instance.Current().Models.Small, maxTokens, []map[string]any{
		{"role": "system", "content": system},
		{"role": "user", "content": user},
	})
}

// Look is one metered call of a vision model on behalf of u (MCP
// image_view): the question and one JPEG image, the model's text answer back.
func (g *Gateway) Look(ctx context.Context, u store.User, session, model, question string, jpeg []byte, maxTokens int) (string, error) {
	return g.chat(ctx, u, session, model, maxTokens, []map[string]any{{"role": "user", "content": []map[string]any{
		{"type": "text", "text": question},
		{"type": "image_url", "image_url": map[string]string{"url": "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpeg)}},
	}}})
}

// chat is one non-streaming chat call with the manager's key, held against
// the person's budget and recorded like a call through llm-gw.
func (g *Gateway) chat(ctx context.Context, u store.User, session, model string, maxTokens int, messages []map[string]any) (string, error) {
	raw, err := g.AssistantCompletion(ctx, u, session, model, maxTokens, messages, nil)
	if err != nil {
		return "", err
	}
	var message struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &message); err != nil {
		return "", err
	}
	return strings.TrimSpace(message.Content), nil
}

// ProfileCompletion is an internal, metered model call. Only manager-owned
// conversations supply tools; there is no public forwarding endpoint.
func (g *Gateway) AssistantCompletion(ctx context.Context, u store.User, session, model string, maxTokens int, messages any, tools any) (json.RawMessage, error) {
	return g.assistantCompletion(ctx, u, session, model, maxTokens, messages, tools, nil)
}

func (g *Gateway) assistantCompletion(ctx context.Context, u store.User, session, model string, maxTokens int, messages any, tools any, settings map[string]any) (json.RawMessage, error) {
	raw, err := g.completionResponse(ctx, u, session, model, maxTokens, messages, tools, settings)
	if err != nil {
		return nil, err
	}
	var r struct {
		Choices []struct {
			Message json.RawMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &r); err != nil || len(r.Choices) == 0 {
		return nil, errors.Join(err, errors.New("complete: empty answer"))
	}
	return r.Choices[0].Message, nil
}

func (g *Gateway) completionResponse(ctx context.Context, u store.User, session, model string, maxTokens int, messages any, tools any, settings map[string]any) (json.RawMessage, error) {
	if u.UID != store.ManagerUID {
		release, err := g.Budget.Reserve(ctx, u.UID)
		if err != nil {
			return nil, err
		}
		defer release() // after g.record below
	}
	maxTokens = min(max(maxTokens, 1), g.accounting().MaxOutputTokens)
	request := map[string]any{"model": model, "messages": messages}
	for key, value := range settings {
		if g.Flavor == "openai" && !requestKey(key) {
			continue
		}
		request[key] = value
	}
	request["max_tokens"] = maxTokens
	if tools != nil {
		request["tools"] = tools
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	var sent atomic.Bool
	req, err := http.NewRequestWithContext(dispatched(ctx, &sent), "POST", g.Upstream.JoinPath("/v1/chat/completions").String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	id, _ := uuid.NewV7()
	g.upstreamHeaders(ctx, req.Header, u, session, id.String())
	ev := store.UsageEvent{EventID: id.String(), Start: time.Now(), UID: u.UID, SessionID: session, RootSessionID: session,
		Capability: "llm", Target: model, Status: http.StatusBadGateway}
	resp, err := g.proxy.Transport.RoundTrip(req)
	if err != nil {
		unanswered(&ev, sent.Load(), g.accounting())
		g.record(ctx, ev)
		return nil, err
	}
	defer resp.Body.Close()
	g.responseUsage(&ev, resp)
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	m := &meter{ev: ev, accounting: g.accounting(), flavor: g.Flavor}
	m.apply(raw)
	g.record(ctx, m.ev)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("complete: HTTP %d", resp.StatusCode)
	}
	return raw, nil
}
