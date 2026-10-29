// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/runtime"
)

type officeRuntime struct {
	runtime.Runtime
	configure func(io.Reader) error
}

func (r officeRuntime) Exec(ctx context.Context, name string, cmd []string, in io.Reader, out, stderr io.Writer) (int, error) {
	if len(cmd) != 2 || cmd[1] != "/app/src/configure-worker.mjs" {
		return 1, errors.New("unexpected exec")
	}
	return 0, r.configure(in)
}

type officeFixture struct {
	env
	mu      sync.Mutex
	tokens  map[string]bool
	writes  atomic.Int32
	entered chan struct{}
}

func newOfficeFixture(t *testing.T) *officeFixture {
	t.Helper()
	f := &officeFixture{env: setup(t), tokens: map[string]bool{}}
	f.s.cfg.OfficeImage = "bridge@sha256:" + strings.Repeat("a", 64)
	base, _ := url.Parse("https://cloud.example")
	f.s.cfg.House = []config.HouseService{{ID: "nextcloud", URL: base}}
	f.s.OfficeCreds = func(ctx context.Context, uid string) (string, string, error) {
		return "login-" + uid, "private-password", nil
	}
	f.s.set(uid1, &running{slot: 1, gen: 1, lease: newLease(), started: time.Now()})
	f.s.rt = officeRuntime{Runtime: f.rt, configure: func(in io.Reader) error {
		var secret struct{ Token, LoginName, AppPassword string }
		if err := json.NewDecoder(in).Decode(&secret); err != nil {
			return err
		}
		if secret.AppPassword != "private-password" || secret.Token == "" {
			return errors.New("missing secret")
		}
		f.mu.Lock()
		f.tokens[secret.Token] = true
		f.mu.Unlock()
		return nil
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/healthz") {
			w.WriteHeader(200)
			return
		}
		f.mu.Lock()
		allowed := f.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		f.mu.Unlock()
		if !allowed {
			w.WriteHeader(401)
			return
		}
		worker := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")[0]
		handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server {
			server := sdk.NewServer(&sdk.Implementation{Name: "fixture", Version: "1"}, nil)
			for _, name := range []string{"office_open", "office_read", "office_status", "office_replace"} {
				name := name
				server.AddTool(&sdk.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
					var args map[string]any
					if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
						return nil, err
					}
					switch name {
					case "office_open":
						return officeResult(map[string]any{"documentId": worker + ":" + args["fileId"].(string)}), nil
					case "office_read":
						if !strings.HasPrefix(args["documentId"].(string), worker+":") {
							return &sdk.CallToolResult{IsError: true}, nil
						}
						return officeResult(map[string]any{"text": "content"}), nil
					case "office_replace":
						f.writes.Add(1)
						if f.entered != nil {
							close(f.entered)
							select {
							case <-ctx.Done():
							case <-time.After(time.Second):
							}
						}
						return officeResult(map[string]any{"persisted": false}), nil
					default:
						return officeResult(map[string]any{"documents": []any{}}), nil
					}
				})
			}
			return server
		}, &sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
		handler.ServeHTTP(w, r)
	}))
	f.s.officeURL = func(slot int) string { return server.URL + "/" + strconv.Itoa(slot) + "/mcp" }
	t.Cleanup(server.Close)
	t.Cleanup(f.s.Close)
	return f
}
func officeCall(t *testing.T, s *Service, uid, name, args string) *sdk.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := s.CallOffice(ctx, uid, name, json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func document(t *testing.T, result *sdk.CallToolResult) string {
	t.Helper()
	data, _ := json.Marshal(result.StructuredContent)
	var v struct{ DocumentID string }
	_ = json.Unmarshal(data, &v)
	return v.DocumentID
}
func TestOfficeReusesWorkerAndSeparatesPeople(t *testing.T) {
	f := newOfficeFixture(t)
	officeCall(t, f.s, uid1, "office_status", `{}`)
	if len(f.rt.Specs) != 0 {
		t.Fatal("status started a worker")
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { officeCall(t, f.s, uid1, "office_open", `{"fileId":"1"}`) })
	}
	wg.Wait()
	if len(f.rt.Specs) != 1 {
		t.Fatalf("created %d workers", len(f.rt.Specs))
	}
	one := document(t, officeCall(t, f.s, uid1, "office_open", `{"fileId":"1"}`))
	officeCall(t, f.s, uid1, "office_read", `{"documentId":"`+one+`"}`)
	uid2 := strings.Repeat("b", 64)
	f.s.set(uid2, &running{slot: 2, gen: 1, lease: newLease(), started: time.Now()})
	two := document(t, officeCall(t, f.s, uid2, "office_open", `{"fileId":"1"}`))
	if one == two {
		t.Fatal("people shared document identity")
	}
	if !officeCall(t, f.s, uid2, "office_read", `{"documentId":"`+one+`"}`).IsError {
		t.Fatal("foreign document accepted")
	}
	if len(f.rt.Specs) != 2 {
		t.Fatal("worker not per person")
	}
	for _, spec := range f.rt.Specs {
		text, _ := json.Marshal(spec)
		if strings.Contains(string(text), "private-password") || len(spec.Mounts) != 0 || !spec.Office {
			t.Fatal("unsafe worker spec")
		}
	}
}
func TestOfficeIdleAndGenerationCleanup(t *testing.T) {
	f := newOfficeFixture(t)
	officeCall(t, f.s, uid1, "office_open", `{"fileId":"1"}`)
	clock := time.Now()
	f.s.now = func() time.Time { return clock }
	slot := f.s.officeSlot(uid1)
	slot.Lock()
	slot.worker.lastUsed = clock
	slot.Unlock()
	clock = clock.Add(officeIdle + time.Second)
	officeCall(t, f.s, uid1, "office_status", `{}`)
	f.s.sweepOffice(context.Background())
	if slot.worker != nil {
		t.Fatal("status kept idle worker alive")
	}
	officeCall(t, f.s, uid1, "office_open", `{"fileId":"1"}`)
	previous := slot.worker.id
	f.s.set(uid1, &running{slot: 1, gen: 2, lease: newLease(), started: clock})
	officeCall(t, f.s, uid1, "office_open", `{"fileId":"1"}`)
	if slot.worker.id == previous || slot.worker.generation != 2 {
		t.Fatal("old generation reused")
	}
	f.s.set(uid1, nil)
	f.s.sweepOffice(context.Background())
	if slot.worker != nil {
		t.Fatal("orphan survived")
	}
}
func TestOfficeDisconnectCancelsWriteWithoutReplay(t *testing.T) {
	f := newOfficeFixture(t)
	officeCall(t, f.s, uid1, "office_open", `{"fileId":"1"}`)
	f.entered = make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := f.s.CallOffice(context.Background(), uid1, "office_replace", json.RawMessage(`{"documentId":"1:1"}`))
		done <- err
	}()
	<-f.entered
	closed := make(chan error, 1)
	go func() { closed <- f.s.CloseOffice(context.Background(), uid1) }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("disconnect deadlocked")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("call did not cancel")
	}
	if f.writes.Load() != 1 || f.s.officeSlot(uid1).worker != nil {
		t.Fatal("write replay or stale worker")
	}
}
func TestOfficeBlockedCredentialsPreventRestart(t *testing.T) {
	f := newOfficeFixture(t)
	officeCall(t, f.s, uid1, "office_open", `{"fileId":"1"}`)
	f.s.OfficeCreds = func(context.Context, string) (string, string, error) { return "", "", errors.New("revoking") }
	_, err := f.s.CallOffice(context.Background(), uid1, "office_open", json.RawMessage(`{"fileId":"1"}`))
	if err == nil || f.s.officeSlot(uid1).worker != nil || len(f.rt.Specs) != 1 {
		t.Fatal("revoking connection reused")
	}
}

func TestOfficeBrokenTransportRequiresExplicitOpen(t *testing.T) {
	f := newOfficeFixture(t)
	officeCall(t, f.s, uid1, "office_open", `{"fileId":"1"}`)
	worker := f.s.officeSlot(uid1).worker
	_ = worker.session.Close()
	_, err := f.s.CallOffice(context.Background(), uid1, "office_replace", json.RawMessage(`{"documentId":"1:1"}`))
	if err == nil || !worker.failed || f.writes.Load() != 0 {
		t.Fatal("closed transport not invalidated or write replayed")
	}
	officeCall(t, f.s, uid1, "office_status", `{}`)
	if f.s.officeSlot(uid1).worker != nil || len(f.rt.Specs) != 1 {
		t.Fatal("status recreated failed worker")
	}
	officeCall(t, f.s, uid1, "office_open", `{"fileId":"1"}`)
	if len(f.rt.Specs) != 2 || f.s.officeSlot(uid1).worker.id == worker.id {
		t.Fatal("explicit open did not replace failed worker")
	}
}
