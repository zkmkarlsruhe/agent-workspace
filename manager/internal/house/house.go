// SPDX-License-Identifier: MIT

// Package house connects a person to house services (docs/adr/README.md §3.2,
// §3.3.5): Nextcloud Login Flow v2 started from the portal, the app password
// stored encrypted in the manager DB, never in the container, never logged.
//
// shortcut: every configured service is a Nextcloud; give config.HouseService
// a kind and branch in Connect/revoke when a second system type comes.
package house

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/logx"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ncguard"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

var (
	ErrUnknown      = errors.New("house: unknown service")
	ErrNotConnected = errors.New("house: not connected")
	ErrUnreachable  = errors.New("house: service unreachable")
	// errSecret: the stored secret does not open (OC_MASTER_KEY changed)
	errSecret = errors.New("house: stored secret unreadable")
)

// Store is the part of *store.Store the service needs.
type Store interface {
	PutConnection(ctx context.Context, c store.Connection) error
	Connection(ctx context.Context, uid, service string) (store.Connection, error)
	DeleteConnection(ctx context.Context, uid, service string) error
	ConnectionUsed(ctx context.Context, uid, service string, at time.Time, expired bool) error
	AddRevocation(ctx context.Context, uid, service string, secretEnc []byte, at time.Time) error
	Revocations(ctx context.Context, uid string) ([]store.Revocation, error)
	DeleteRevocation(ctx context.Context, id int64) error
}

type Service struct {
	Services []config.HouseService
	Store    Store
	Log      *slog.Logger
	// LoginFor maps an Authentik username to the Nextcloud account the person
	// must connect; nil = the same name. Compared case-insensitively.
	LoginFor func(username string) string

	// PollEvery is the poll interval of Login Flow v2 (2 s, docs/adr/README.md §3.3.5).
	PollEvery time.Duration

	key     []byte
	http    *http.Client
	flowTTL time.Duration
	now     func() time.Time

	mu    sync.Mutex
	flows map[string]*Flow       // uid|service
	ops   map[string]*sync.Mutex // uid|service: a grant's finish and Disconnect never interleave
}

// FlowState is the state of the last Login Flow v2 of a person and service.
type FlowState int

const (
	FlowNone     FlowState = iota
	FlowPending            // waiting for the grant in Nextcloud
	FlowDone               // connected
	FlowMismatch           // granted with another account, revoked again
	FlowFailed             // Nextcloud answered unexpectedly
	FlowTimeout            // not granted within the token lifetime
)

type Flow struct {
	State    FlowState
	LoginURL string // the Nextcloud page the person opens; same host as the service
	Ended    time.Time
	cancel   context.CancelFunc
}

// Status is one service as the portal shows it.
type Status struct {
	Service   config.HouseService
	Connected bool
	Login     string // Nextcloud user id
	Since     time.Time
	Expired   bool
	Flow      Flow
}

// New derives the encryption key from OC_MASTER_KEY.
func New(services []config.HouseService, st Store, masterKey []byte, log *slog.Logger) (*Service, error) {
	key, err := deriveKey(masterKey)
	if err != nil {
		return nil, err
	}
	return &Service{
		Services: services, Store: st, Log: log, key: key,
		// never follow redirects: the Basic header must only go to the configured host
		http:      &http.Client{Timeout: 20 * time.Second, Transport: logx.Transport{Upstream: "nextcloud", Propagate: true, Body: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		PollEvery: 2 * time.Second,
		flowTTL:   20 * time.Minute, // lifetime of the poll token in Nextcloud
		now:       time.Now,
		flows:     map[string]*Flow{},
		ops:       map[string]*sync.Mutex{},
	}, nil
}

func (s *Service) service(id string) (config.HouseService, bool) {
	for _, h := range s.Services {
		if h.ID == id {
			return h, true
		}
	}
	return config.HouseService{}, false
}

// secret is the sealed part of a connection: the login name Nextcloud wants
// in Basic auth (may differ from the user id, e.g. an e-mail login).
type secret struct {
	LoginName   string `json:"l"`
	AppPassword string `json:"p"`
}

func aad(uid, service string) []byte { return []byte(uid + "|" + service) }

func (s *Service) load(ctx context.Context, uid, id string) (store.Connection, secret, error) {
	var sec secret
	c, err := s.Store.Connection(ctx, uid, id)
	if errors.Is(err, store.ErrNotFound) {
		return c, sec, ErrNotConnected
	}
	if err != nil {
		return c, sec, err
	}
	plain, err := open(s.key, aad(uid, id), c.SecretEnc)
	if err != nil {
		return c, sec, errSecret
	}
	if json.Unmarshal(plain, &sec) != nil {
		return c, sec, errSecret
	}
	return c, sec, nil
}

// Creds returns the Nextcloud user id (for WebDAV paths), the login name and
// the app password for a tool call. An expired connection is ErrNotConnected.
func (s *Service) Creds(ctx context.Context, uid, id string) (user, login, password string, err error) {
	c, sec, err := s.load(ctx, uid, id)
	if err != nil {
		return "", "", "", err
	}
	if c.Expired {
		return "", "", "", ErrNotConnected
	}
	if err := s.Store.ConnectionUsed(ctx, uid, id, s.now(), false); err != nil {
		s.Log.WarnContext(ctx, "house: last used", "uid", uid, "service", id, "err", err)
	}
	return c.Login, sec.LoginName, sec.AppPassword, nil
}

// MarkExpired records a 401 from the service; the portal then shows
// "Verbindung abgelaufen" until the person reconnects.
func (s *Service) MarkExpired(ctx context.Context, uid, id string) {
	if err := s.Store.ConnectionUsed(ctx, uid, id, s.now(), true); err != nil {
		s.Log.WarnContext(ctx, "house: mark expired", "uid", uid, "service", id, "err", err)
	}
}

// Status lists every configured service for the person.
func (s *Service) Status(ctx context.Context, uid string) ([]Status, error) {
	var out []Status
	for _, h := range s.Services {
		st := Status{Service: h, Flow: s.flow(uid, h.ID)}
		c, err := s.Store.Connection(ctx, uid, h.ID)
		switch {
		case err == nil:
			st.Connected, st.Login, st.Since, st.Expired = true, c.Login, c.ConnectedAt, c.Expired
		case !errors.Is(err, store.ErrNotFound):
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

func (s *Service) flow(uid, id string) Flow {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f := s.flows[uid+"|"+id]; f != nil {
		return *f
	}
	return Flow{}
}

func (s *Service) opLock(key string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.ops[key]
	if l == nil {
		l = &sync.Mutex{}
		s.ops[key] = l
	}
	return l
}

// current reports whether f is still the flow of key: Disconnect drops it,
// a newer Connect replaces it. The pointer is the flow's generation.
func (s *Service) current(key string, f *Flow) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flows[key] == f
}

func (s *Service) setState(f *Flow, st FlowState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f.State, f.Ended = st, s.now()
}

// Connect starts Login Flow v2 and returns the login URL to open. A second
// Connect replaces a pending flow. The poll runs in the manager (not in the
// browser), so closing the portal tab does not lose the grant.
func (s *Service) Connect(ctx context.Context, uid, username, id string) (string, error) {
	h, ok := s.service(id)
	if !ok {
		return "", ErrUnknown
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.URL.String()+"/index.php/login/v2", nil)
	req.Header.Set("User-Agent", instance.Value("name")+" ("+username+")") // the device name Nextcloud shows
	resp, err := s.http.Do(req)
	if err != nil {
		s.Log.WarnContext(ctx, "house: login flow start", "service", id, "err", err)
		return "", ErrUnreachable
	}
	defer resp.Body.Close()
	var start struct {
		Poll struct {
			Token    string `json:"token"`
			Endpoint string `json:"endpoint"`
		} `json:"poll"`
		Login string `json:"login"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&start) != nil ||
		start.Poll.Token == "" || !sameHost(h.URL, start.Poll.Endpoint) || !sameHost(h.URL, start.Login) {
		s.Log.WarnContext(ctx, "house: login flow start", "service", id, "status", resp.StatusCode)
		return "", ErrUnreachable
	}
	pctx, cancel := context.WithTimeout(context.Background(), s.flowTTL)
	f := &Flow{State: FlowPending, LoginURL: start.Login, cancel: cancel}
	key := uid + "|" + id
	s.mu.Lock()
	if old := s.flows[key]; old != nil && old.cancel != nil {
		old.cancel()
	}
	s.flows[key] = f
	s.mu.Unlock()
	go s.poll(pctx, cancel, f, h, uid, username, start.Poll.Endpoint, start.Poll.Token)
	return start.Login, nil
}

// sameHost: Nextcloud's answer may only send the person (and the poll token)
// to the configured instance.
func sameHost(base *url.URL, raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == base.Scheme && strings.EqualFold(u.Host, base.Host)
}

type grant struct {
	Server      string `json:"server"`
	LoginName   string `json:"loginName"`
	AppPassword string `json:"appPassword"`
}

func (s *Service) poll(ctx context.Context, cancel context.CancelFunc, f *Flow, h config.HouseService, uid, username, endpoint, token string) {
	defer cancel()
	t := time.NewTicker(s.PollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				s.setState(f, FlowTimeout)
				s.Log.InfoContext(ctx, "house: login flow timeout", "uid", uid, "service", h.ID)
			}
			return // canceled: a newer flow replaced this one
		case <-t.C:
		}
		g, status, err := s.pollOnce(ctx, endpoint, token)
		switch {
		case err != nil:
			continue // network hiccup: try again until the token expires
		case status == http.StatusNotFound:
			continue // not granted yet
		case status != http.StatusOK:
			s.Log.WarnContext(ctx, "house: login flow poll", "uid", uid, "service", h.ID, "status", status)
			s.setState(f, FlowFailed)
			return
		}
		s.setState(f, s.finish(context.WithoutCancel(ctx), f, h, uid, username, g))
		return
	}
}

func (s *Service) pollOnce(ctx context.Context, endpoint, token string) (grant, int, error) {
	var g grant
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(url.Values{"token": {token}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.http.Do(req)
	if err != nil {
		return g, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&g); err != nil || g.LoginName == "" || g.AppPassword == "" {
			return g, http.StatusBadGateway, nil
		}
	}
	return g, resp.StatusCode, nil
}

// finish checks a grant and stores it. A grant for another server or
// another account is revoked at once: a shared or a colleague's account
// must never act for this person. So is a grant whose flow was disconnected
// or replaced meanwhile.
func (s *Service) finish(ctx context.Context, f *Flow, h config.HouseService, uid, username string, g grant) FlowState {
	sec := secret{LoginName: g.LoginName, AppPassword: g.AppPassword}
	l := s.opLock(uid + "|" + h.ID)
	l.Lock()
	defer l.Unlock()
	if !s.current(uid+"|"+h.ID, f) {
		s.Log.InfoContext(ctx, "house: grant of an ended flow revoked", "uid", uid, "service", h.ID)
		s.revokeOrQueue(ctx, h, uid, sec)
		return FlowFailed
	}
	if !strings.EqualFold(strings.TrimRight(g.Server, "/"), h.URL.String()) {
		s.Log.WarnContext(ctx, "house: grant for another server", "uid", uid, "service", h.ID)
		s.revokeOrQueue(ctx, h, uid, sec)
		return FlowMismatch
	}
	user, err := s.userID(ctx, h, sec)
	if err != nil {
		s.Log.WarnContext(ctx, "house: user id", "uid", uid, "service", h.ID, "err", err)
		s.revokeOrQueue(ctx, h, uid, sec)
		return FlowFailed
	}
	want := username
	if s.LoginFor != nil {
		want = s.LoginFor(username)
	}
	if want == "" || !strings.EqualFold(user, want) && !strings.EqualFold(g.LoginName, want) {
		s.Log.WarnContext(ctx, "house: grant for another account", "uid", uid, "service", h.ID)
		s.revokeOrQueue(ctx, h, uid, sec)
		return FlowMismatch
	}
	if _, old, err := s.load(ctx, uid, h.ID); err == nil {
		// reconnect: the previous app password must not stay valid
		s.revokeOrQueue(ctx, h, uid, old)
	}
	plain, _ := json.Marshal(sec)
	blob, err := seal(s.key, aad(uid, h.ID), plain)
	if err == nil {
		err = s.Store.PutConnection(ctx, store.Connection{UID: uid, Service: h.ID, Login: user, SecretEnc: blob, ConnectedAt: s.now()})
	}
	if err != nil {
		s.Log.ErrorContext(ctx, "house: store connection", "uid", uid, "service", h.ID, "err", err)
		s.revokeOrQueue(ctx, h, uid, sec)
		return FlowFailed
	}
	s.Log.InfoContext(ctx, "house: connected", "uid", uid, "service", h.ID)
	return FlowDone
}

func (s *Service) ocs(ctx context.Context, method string, h config.HouseService, sec secret, path string) (*http.Response, error) {
	req, _ := http.NewRequestWithContext(ctx, method, h.URL.String()+path, nil)
	req.SetBasicAuth(sec.LoginName, sec.AppPassword)
	req.Header.Set("OCS-APIRequest", "true")
	req.Header.Set("Accept", "application/json")
	if err := ncguard.Check(h.URL, req); err != nil {
		return nil, err
	}
	return s.http.Do(req)
}

// userID asks Nextcloud whose app password this is (the WebDAV path needs
// the user id, the login name may be an e-mail address).
func (s *Service) userID(ctx context.Context, h config.HouseService, sec secret) (string, error) {
	resp, err := s.ocs(ctx, http.MethodGet, h, sec, "/ocs/v2.php/cloud/user?format=json")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var r struct {
		OCS struct {
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		} `json:"ocs"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r) != nil || r.OCS.Data.ID == "" {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	return r.OCS.Data.ID, nil
}

// revoke deletes the app password in Nextcloud. 401 means it is already
// invalid (revoked under Sicherheit, or expired), which is the goal too.
func (s *Service) revoke(ctx context.Context, h config.HouseService, sec secret) error {
	resp, err := s.ocs(ctx, http.MethodDelete, h, sec, "/ocs/v2.php/core/apppassword?format=json")
	if err != nil {
		return ErrUnreachable
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("%w: revoke status %d", ErrUnreachable, resp.StatusCode)
	}
	return nil
}

// revokeOrQueue revokes sec; if Nextcloud does not confirm, the sealed
// secret goes to the retry list (RetryRevocations), never just dropped.
func (s *Service) revokeOrQueue(ctx context.Context, h config.HouseService, uid string, sec secret) {
	err := s.revoke(ctx, h, sec)
	if err == nil {
		return
	}
	plain, _ := json.Marshal(sec)
	blob, serr := seal(s.key, aad(uid, h.ID), plain)
	if serr == nil {
		serr = s.Store.AddRevocation(ctx, uid, h.ID, blob, s.now())
	}
	if serr != nil {
		s.Log.ErrorContext(ctx, "house: revocation lost", "uid", uid, "service", h.ID, "err", errors.Join(err, serr))
		return
	}
	s.Log.WarnContext(ctx, "house: revocation queued", "uid", uid, "service", h.ID, "err", err)
}

// RetryRevocations revokes the queued app passwords of uid ("" = everyone)
// and reports the ones Nextcloud still did not confirm.
func (s *Service) RetryRevocations(ctx context.Context, uid string) error {
	list, err := s.Store.Revocations(ctx, uid)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range list {
		h, ok := s.service(r.Service)
		if !ok {
			errs = append(errs, fmt.Errorf("%w: %s", ErrUnknown, r.Service))
			continue
		}
		var sec secret
		plain, err := open(s.key, aad(r.UID, r.Service), r.SecretEnc)
		if err == nil && json.Unmarshal(plain, &sec) == nil {
			if err := s.revoke(ctx, h, sec); err != nil {
				errs = append(errs, err)
				continue
			}
		} else {
			s.Log.WarnContext(ctx, "house: unreadable queued revocation dropped", "uid", r.UID, "service", r.Service)
		}
		if err := s.Store.DeleteRevocation(ctx, r.ID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Loop retries queued revocations every interval until ctx ends.
func (s *Service) Loop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := s.RetryRevocations(ctx, ""); err != nil {
			s.Log.WarnContext(ctx, "house: revocation retry", "err", err)
		}
	}
}

// Disconnect revokes the app password, then deletes the row. If Nextcloud
// cannot be reached the row stays, so a later Disconnect can still revoke.
func (s *Service) Disconnect(ctx context.Context, uid, id string) error {
	h, ok := s.service(id)
	if !ok {
		return ErrUnknown
	}
	l := s.opLock(uid + "|" + id)
	l.Lock()
	defer l.Unlock()
	s.mu.Lock()
	if f := s.flows[uid+"|"+id]; f != nil && f.cancel != nil {
		f.cancel()
	}
	delete(s.flows, uid+"|"+id)
	s.mu.Unlock()
	_, sec, err := s.load(ctx, uid, id)
	switch {
	case errors.Is(err, ErrNotConnected):
		return nil
	case errors.Is(err, errSecret):
		// cannot revoke without the password; the person can still revoke it in Nextcloud
		s.Log.WarnContext(ctx, "house: unreadable secret dropped", "uid", uid, "service", id)
		err = nil
	case err == nil:
		err = s.revoke(ctx, h, sec)
	}
	if err != nil {
		return err
	}
	if err := s.Store.DeleteConnection(ctx, uid, id); err != nil {
		return err
	}
	s.Log.InfoContext(ctx, "house: disconnected", "uid", uid, "service", id)
	return nil
}

// DisconnectAll runs before account deletion; an error keeps the account.
// Queued revocations count: their passwords would outlive the account.
func (s *Service) DisconnectAll(ctx context.Context, uid string) error {
	errs := []error{s.RetryRevocations(ctx, uid)}
	for _, h := range s.Services {
		errs = append(errs, s.Disconnect(ctx, uid, h.ID))
	}
	return errors.Join(errs...)
}
