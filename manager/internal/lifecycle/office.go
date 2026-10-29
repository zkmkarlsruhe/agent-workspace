// SPDX-License-Identifier: MIT

package lifecycle

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/runtime"
)

const officeIdle = time.Hour

const officeCleanupTimeout = 25 * time.Second

var ErrOfficeUnavailable = errors.New("office unavailable; check the Nextcloud connection")

type officeSlot struct {
	sync.Mutex
	worker *officeWorker
}
type officeWorker struct {
	id         string
	slot       int
	generation int64
	credential [32]byte
	session    *sdk.ClientSession
	transport  *http.Transport
	failed     bool
	lease      *lease
	lastUsed   time.Time
	expires    time.Time
	active     int
}

func (s *Service) officeSlot(uid string) *officeSlot {
	slot, _ := s.office.LoadOrStore(uid, &officeSlot{})
	return slot.(*officeSlot)
}

// CloseOffice uses its own lock: account deletion already holds the person's lifecycle lock.
func (s *Service) CloseOffice(ctx context.Context, uid string) error {
	slot := s.officeSlot(uid)
	slot.Lock()
	defer slot.Unlock()
	return s.closeOffice(ctx, slot)
}
func (s *Service) closeOffice(ctx context.Context, slot *officeSlot) error {
	w := slot.worker
	if w == nil {
		return nil
	}
	w.failed = true
	w.lease.end()
	_ = w.session.Close()
	w.transport.CloseIdleConnections()
	if err := s.removeOffice(context.WithoutCancel(ctx), w.id); err != nil {
		return err
	}
	slot.worker = nil
	return nil
}

// Reserve time for forced removal if the graceful stop request itself times out.
func (s *Service) removeOffice(ctx context.Context, id string) error {
	cleanup, cancel := context.WithTimeout(ctx, officeCleanupTimeout)
	defer cancel()
	stopCtx, stop := context.WithTimeout(cleanup, 17*time.Second)
	stopErr := s.rt.Stop(stopCtx, id, 16*time.Second)
	stop()
	if err := s.rt.Remove(cleanup, id); err != nil {
		return errors.Join(stopErr, err)
	}
	return nil
}

func officeResult(value any) *sdk.CallToolResult {
	data, _ := json.Marshal(value)
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: string(data)}}, StructuredContent: value}
}

// CallOffice never retries a tool call. The two leases prevent slot reuse and connection changes
// from sending an old call to a new browser or person.
func (s *Service) CallOffice(ctx context.Context, uid, name string, arguments json.RawMessage) (*sdk.CallToolResult, error) {
	ctx, finished, admitted := s.beginWork(ctx)
	if !admitted {
		return nil, ErrNotRunning
	}
	defer finished()
	if s.cfg.OfficeImage == "" || s.OfficeCreds == nil {
		return nil, ErrOfficeUnavailable
	}
	r := s.get(uid)
	if r == nil {
		return nil, ErrNotRunning
	}
	ctx, release, ok := r.lease.bind(ctx)
	if !ok {
		return nil, ErrNotRunning
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	slot := s.officeSlot(uid)
	slot.Lock()
	if ctx.Err() != nil {
		slot.Unlock()
		return nil, ErrOfficeUnavailable
	}
	login, password, err := s.OfficeCreds(ctx, uid)
	if err != nil {
		_ = s.closeOffice(ctx, slot)
		slot.Unlock()
		return nil, ErrOfficeUnavailable
	}
	fingerprint := sha256.Sum256([]byte(login + "\x00" + password))
	if w := slot.worker; w != nil && (w.failed || w.generation != r.gen || w.credential != fingerprint || !s.now().Before(w.expires)) {
		if err := s.closeOffice(ctx, slot); err != nil {
			slot.Unlock()
			return nil, ErrOfficeUnavailable
		}
	}
	if ctx.Err() != nil || s.get(uid) != r {
		slot.Unlock()
		return nil, ErrNotRunning
	}
	if slot.worker == nil {
		switch name {
		case "office_status", "office_documents":
			slot.Unlock()
			return officeResult(map[string]any{"configured": true, "connected": false, "documents": []any{},
				"capabilities": map[string]any{"remoteSelection": false}, "documentIdleMs": 900000, "browserIdleMs": 3600000}), nil
		case "office_open":
		default:
			slot.Unlock()
			return nil, ErrOfficeUnavailable
		}
		slot.worker, err = s.startOffice(ctx, uid, r, login, password, fingerprint)
		if err != nil {
			slot.Unlock()
			return nil, ErrOfficeUnavailable
		}
	}
	w := slot.worker
	callCtx, done, ok := w.lease.bind(ctx)
	if !ok {
		slot.Unlock()
		return nil, ErrOfficeUnavailable
	}
	w.active++
	touch := name != "office_status" && name != "office_documents"
	if touch {
		w.lastUsed = s.now()
	}
	slot.Unlock()
	failed := false
	defer func() {
		done() // CloseOffice may hold slot.Lock while waiting on this lease.
		slot.Lock()
		w.active--
		w.failed = w.failed || failed
		if touch {
			w.lastUsed = s.now()
		}
		slot.Unlock()
	}()
	result, err := w.session.CallTool(callCtx, &sdk.CallToolParams{Name: name, Arguments: arguments})
	failed = err != nil
	if err != nil {
		return nil, ErrOfficeUnavailable
	}
	return result, nil
}

type officeBearer struct {
	token     string
	transport http.RoundTripper
}

func (b officeBearer) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.token)
	return b.transport.RoundTrip(req)
}

func (s *Service) startOffice(ctx context.Context, uid string, r *running, login, password string, fingerprint [32]byte) (_ *officeWorker, err error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	name := config.OfficeContainer(r.slot)
	existing, inspectErr := s.rt.Inspect(ctx, name)
	if inspectErr == nil {
		if existing.Labels["oc.uid"] != uid {
			return nil, ErrNotRunning
		}
		if err := s.removeOffice(ctx, existing.ID); err != nil {
			return nil, err
		}
	} else if !errors.Is(inspectErr, runtime.ErrNotFound) {
		return nil, inspectErr
	}
	endpoint := "http://" + s.cfg.SlotAddr(r.slot, config.HostOffice).String() + ":8080/mcp"
	if s.officeURL != nil {
		endpoint = s.officeURL(r.slot)
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return nil, err
	}
	token := base64.RawURLEncoding.EncodeToString(random)
	expires := s.now().Add(s.cfg.MaxLifetime + 24*time.Hour)
	spec := runtime.Spec{Name: name, Image: s.cfg.OfficeImage, Network: config.SlotNetwork(r.slot),
		IP: s.cfg.SlotAddr(r.slot, config.HostOffice), Office: true,
		Labels: map[string]string{runtime.LabelManaged: "1", "oc.uid": uid, "oc.slot": strconv.Itoa(r.slot),
			"oc.generation": strconv.FormatInt(r.gen, 10), "oc.office": "nextcloud", "oc.image": s.cfg.OfficeImage},
		Cmd: []string{"node", "/app/src/worker.mjs"}, Env: []string{
			"BROWSER_BRIDGE_NEXTCLOUD_URL=" + s.cfg.House[0].URL.String(),
			"BROWSER_BRIDGE_PROXY_URL=http://egress:3128",
			"BROWSER_BRIDGE_AUTH_FILE=/tmp/bridge-secrets/nextcloud.json",
			"BROWSER_BRIDGE_HTTP_TOKEN_FILE=/tmp/bridge-secrets/service.json",
			"BROWSER_BRIDGE_HTTP_HOST=0.0.0.0", "BROWSER_BRIDGE_HTTP_PORT=8080",
			"BROWSER_BRIDGE_HTTP_ALLOWED_HOSTS=" + s.cfg.SlotAddr(r.slot, config.HostOffice).String(),
		},
	}
	if err := s.rt.Create(ctx, spec); err != nil {
		return nil, err
	}
	created, err := s.rt.Inspect(ctx, name)
	if err != nil {
		_ = s.removeOffice(context.WithoutCancel(ctx), name)
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = s.removeOffice(context.WithoutCancel(ctx), created.ID)
		}
	}()
	if err = s.rt.Start(ctx, name); err != nil {
		return nil, err
	}
	data, _ := json.Marshal(map[string]any{"loginName": login, "appPassword": password, "token": token, "expiresAt": expires.Unix()})
	code, err := s.rt.Exec(ctx, name, []string{"node", "/app/src/configure-worker.mjs"}, bytes.NewReader(data), io.Discard, io.Discard)
	if err != nil || code != 0 {
		return nil, ErrOfficeUnavailable
	}
	transport := &http.Transport{}
	httpClient := &http.Client{Transport: officeBearer{token, transport},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	health := endpoint[:len(endpoint)-len("/mcp")] + "/healthz"
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		probeCtx, stop := context.WithTimeout(ctx, time.Second)
		request, _ := http.NewRequestWithContext(probeCtx, http.MethodGet, health, nil)
		response, requestErr := httpClient.Do(request)
		ready := requestErr == nil && response.StatusCode == http.StatusOK
		if response != nil {
			response.Body.Close()
		}
		stop()
		if ready {
			break
		}
		select {
		case <-ctx.Done():
			transport.CloseIdleConnections()
			return nil, ctx.Err()
		case <-tick.C:
		}
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "workspace-office", Version: "1"}, nil)
	session, err := client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: endpoint, HTTPClient: httpClient, MaxRetries: -1, DisableStandaloneSSE: true}, nil)
	if err != nil {
		transport.CloseIdleConnections()
		return nil, err
	}
	return &officeWorker{id: created.ID, slot: r.slot, generation: r.gen, credential: fingerprint,
		session: session, transport: transport, lease: newLease(), lastUsed: s.now(), expires: expires}, nil
}

func (s *Service) sweepOffice(ctx context.Context) {
	var cleanup sync.WaitGroup
	s.office.Range(func(key, value any) bool {
		if ctx.Err() != nil {
			return false
		}
		uid, slot := key.(string), value.(*officeSlot)
		if !slot.TryLock() {
			return true
		}
		w := slot.worker
		if w != nil {
			r := s.get(uid)
			if w.failed || r == nil || r.gen != w.generation || w.active == 0 && s.now().Sub(w.lastUsed) >= officeIdle {
				cleanup.Go(func() {
					defer slot.Unlock()
					if err := s.closeOffice(ctx, slot); err != nil {
						s.log.WarnContext(ctx, "office cleanup failed", "uid", uid)
					}
				})
				return true
			}
		}
		slot.Unlock()
		return true
	})
	cleanup.Wait()
	// Also catch resources whose removal failed after their in-memory entry was lost.
	list, err := s.rt.List(ctx)
	if err != nil {
		return
	}
	for _, c := range list {
		if ctx.Err() != nil {
			break
		}
		if c.Labels["oc.office"] == "" {
			continue
		}
		uid := c.Labels["oc.uid"]
		slot := s.officeSlot(uid)
		if !slot.TryLock() {
			continue
		}
		if slot.worker == nil || slot.worker.id != c.ID {
			cleanup.Go(func() {
				defer slot.Unlock()
				if err := s.removeOffice(ctx, c.ID); err != nil {
					s.log.WarnContext(ctx, "orphan office cleanup failed", "uid", uid)
				}
			})
			continue
		}
		slot.Unlock()
	}
	cleanup.Wait()
}
