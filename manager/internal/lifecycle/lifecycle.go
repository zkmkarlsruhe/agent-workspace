// SPDX-License-Identifier: MIT

// Package lifecycle starts, stops, drains and reconciles one disposable
// OpenCode container per person (docs/adr/README.md §3.6-3.12, Docker per D44).
package lifecycle

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"path"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/catalog"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/logx"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/runtime"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

const (
	StateAbsent   = "absent"
	StateReady    = "ready"
	StateDraining = "draining"
)

type Status struct {
	State     string
	StartedAt time.Time
}

// Target is where the router sends a person's requests.
type Target struct {
	BaseURL  string
	Password string
	lease    *lease // Bind
}

type running struct {
	watch           *watchdog
	slot            int
	gen             int64
	password        string
	lease           *lease // every HTTP call to this generation (lease.go)
	started         time.Time
	lastRequest     time.Time
	streams         int
	draining        bool
	drainSince      time.Time
	lastArchiveScan time.Time // successful archive sweep in this running generation
	collected       time.Time // last chat collection (collect.go)
	busySeen        bool      // an agent ran at the last collection attempt
}

type Service struct {
	background context.Context
	cancel     context.CancelFunc
	workers    sync.WaitGroup

	networkDeferred sync.Map // log a pending migration once per network

	cfg  config.Config
	rt   runtime.Runtime
	st   *store.Store
	log  *slog.Logger
	http *http.Client
	now  func() time.Time
	// baseURL is the OpenCode address of a slot; tests replace it.
	baseURL func(slot int) string
	// ws is the workspace root inside the container; tests replace it.
	ws string
	// interruptWait gives interrupted sessions time to settle before a forced stop.
	interruptWait time.Duration
	// callTimeout bounds each container API call of the reaper (and all
	// interrupts of one forced stop together): one tenant cannot hold up the others.
	callTimeout time.Duration
	// toolboxKill is when the manager removes a tool container under a run
	// that did not end (toolbox.go); tests shorten it.
	toolboxKill time.Duration
	// Suggest asks the fast model for a project folder name; nil = unavailable.
	Suggest func(ctx context.Context, u store.User, description string) (string, error)
	// Complete uses the same metered fast-model gateway as Suggest.
	Complete func(context.Context, store.User, string, string, int) (string, error)
	// Provision creates the person's data dir and quota (oc-provision); nil = dir must exist.
	Provision func(ctx context.Context, uid string, slot int) error
	// DiskUsage reports bytes used and quota of a slot (oc-provision); nil = unknown.
	DiskUsage func(ctx context.Context, slot int) (used, limit int64, err error)
	// PurgeData removes the person's data dir and quota limit (oc-provision);
	// nil = account deletion is unavailable.
	PurgeData func(ctx context.Context, uid string, slot int) error
	// Disconnect revokes the person's house service logins before account
	// deletion (internal/house); nil = none configured.
	Disconnect func(ctx context.Context, uid string) error
	// CollectChats: the evaluation is on (OC_ADMIN_GROUP set), so chats are
	// snapshotted into the store (collect.go); false = no transcript is kept.
	CollectChats bool // at most 500 newest chats per project per collection, store.ChatQuota per person
	// Settings returns the person's settings as Markdown for the full export (internal/settings); nil = none.
	Settings func(ctx context.Context, uid string) (string, error)

	lastSweep time.Time // last inactivity sweep, Reap goroutine only
	// AfterStart runs once a new container is ready, with the start lock held (catalog symlinks); nil = nothing.
	AfterStart func(ctx context.Context, uid string)
	// PrepareCapabilities renders project configuration before opening a session.
	PrepareCapabilities func(ctx context.Context, uid, projectID string) error
	// AfterReap runs at the end of every reap tick (deferred catalog reloads); nil = nothing.
	AfterReap func(ctx context.Context)

	mu     sync.Mutex
	locks  map[string]*sync.Mutex   // per-person start/stop lock
	ops    map[string]*sync.RWMutex // per-person exec lease (opLock)
	run    map[string]*running      // guarded by mu
	starts map[string]*startState   // background starts from the portal, guarded by mu
	// toolRuns holds one place per tool container (uid/toolbox): runs take turns; guarded by mu
	toolRuns       map[string]chan struct{}
	cad            sync.Map
	cadURL         func(int) string
	cadTransport   http.RoundTripper
	FreeCADAllowed func(context.Context, string, string) bool
	office         sync.Map         // uid -> *officeSlot, independent of the account lock
	officeURL      func(int) string // tests override the private worker address
	OfficeCreds    func(context.Context, string) (login, password string, err error)
	// creating counts container creates in flight, so concurrent starts of
	// different people cannot overshoot MaxRunning; guarded by mu
	creating int
}

func New(cfg config.Config, rt runtime.Runtime, st *store.Store, log *slog.Logger) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{
		background: ctx, cancel: cancel,
		cfg: cfg, rt: rt, st: st, log: log,
		cadTransport:  &http.Transport{ResponseHeaderTimeout: 40 * time.Second},
		http:          &http.Client{Timeout: 30 * time.Second},
		now:           time.Now,
		ws:            "/workspace",
		interruptWait: 5 * time.Second,
		callTimeout:   10 * time.Second,
		toolboxKill:   ToolboxTimeout + 15*time.Second,
		toolRuns:      map[string]chan struct{}{},
		locks:         map[string]*sync.Mutex{},
		ops:           map[string]*sync.RWMutex{},
		run:           map[string]*running{},
		starts:        map[string]*startState{},
		baseURL: func(slot int) string {
			return "http://" + cfg.SlotAddr(slot, config.HostUser).String() + ":4096"
		},
	}
}

// Close cancels and joins admitted lifecycle work before removing application workers.
// Cancelled starts may skip writes; no admitted write may outlive Close.
// Callers must stop serving requests and join externally started loops before Close.
func (s *Service) Close() {
	s.mu.Lock()
	s.cancel()
	s.mu.Unlock()
	s.workers.Wait()
	s.closeAllCAD()
	var cleanup sync.WaitGroup
	s.office.Range(func(key, value any) bool {
		slot := value.(*officeSlot)
		slot.Lock()
		if slot.worker == nil {
			slot.Unlock()
			return true
		}
		cleanup.Go(func() {
			defer slot.Unlock()
			if err := s.closeOffice(context.Background(), slot); err != nil {
				s.log.Warn("office shutdown failed", "uid", key.(string), "err", err)
			}
		})
		return true
	})
	cleanup.Wait()
}

// beginWork serializes admission with Close, including handlers that outlive HTTP shutdown.
func (s *Service) beginWork(ctx context.Context) (context.Context, func(), bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.background.Err() != nil || ctx.Err() != nil {
		return ctx, nil, false
	}
	s.workers.Add(1)
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.background, cancel)
	return ctx, func() { stop(); cancel(); s.workers.Done() }, true
}

func (s *Service) suspectAsync(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.background.Err() == nil {
		s.workers.Go(func() { s.Suspect(uid) })
	}
}

// Password derives the OpenCode password of one container generation; it is
// never stored, so a restarted manager recomputes it from the oc.generation label.
func Password(key []byte, uid string, gen int64) string {
	m := hmac.New(sha256.New, key)
	fmt.Fprintf(m, "oc-pw/v1|%s|%d", uid, gen)
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *Service) lock(uid string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.locks[uid]
	if l == nil {
		l = &sync.Mutex{}
		s.locks[uid] = l
	}
	return l
}

// opLock is the person's operation lease: every exec holds it shared,
// DeleteAccount takes it exclusively before the slot can be reused.
func (s *Service) opLock(uid string) *sync.RWMutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.ops[uid]
	if l == nil {
		l = &sync.RWMutex{}
		s.ops[uid] = l
	}
	return l
}

func (s *Service) get(uid string) *running {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.run[uid]
}

func (s *Service) set(uid string, r *running) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r == nil {
		delete(s.run, uid)
	} else {
		if r.watch == nil {
			r.watch = newWatchdog(s.cfg.WatchdogSilent)
		}
		s.run[uid] = r
	}
}

func (s *Service) client(r *running) *ocapi.Client {
	hc := *s.http
	if hc.Transport == nil {
		hc.Transport = http.DefaultTransport
	}
	hc.Transport = leaseTransport{r.lease, hc.Transport}
	return &ocapi.Client{BaseURL: s.baseURL(r.slot), Password: r.password, HTTP: &hc}
}

// Target returns the proxy target if the person's container is ready.
func (s *Service) Target(uid string) (Target, bool) {
	r := s.get(uid)
	if r == nil {
		return Target{}, false
	}
	return Target{BaseURL: s.baseURL(r.slot), Password: r.password, lease: r.lease}, true
}

// Client returns an OpenCode API client for a running container.
func (s *Service) Client(uid string) (*ocapi.Client, bool) {
	r := s.get(uid)
	if r == nil {
		return nil, false
	}
	return s.client(r), true
}

// Touch records user activity (proxied request or portal action).
func (s *Service) Touch(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.run[uid]; r != nil {
		r.lastRequest = s.now()
	}
}

// StreamOpened counts an open proxied SSE/WS stream until done is called.
func (s *Service) StreamOpened(uid string) (done func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.run[uid]
	if r == nil {
		return func() {}
	}
	r.streams++
	r.lastRequest = s.now()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			r.streams--
			s.mu.Unlock()
		})
	}
}

// Suspect re-inspects a container after a proxy dial error and forgets it if
// gone. The lock comes first: a view taken before it may already belong to a
// restarted generation or, after an account deletion, to the slot's next tenant.
func (s *Service) Suspect(uid string) {
	ctx, done, ok := s.beginWork(context.Background())
	if !ok {
		return
	}
	defer done()
	l := s.lock(uid)
	l.Lock()
	defer l.Unlock()
	r := s.get(uid)
	if r == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if ctx.Err() != nil {
		return
	}
	s.dropIfGone(ctx, uid, r)
}

// ours reports whether c is the container of r's generation of uid.
func ours(c runtime.Container, uid string, r *running) bool {
	return c.Labels["oc.uid"] == uid && c.Labels["oc.generation"] == strconv.FormatInt(r.gen, 10)
}

// dropIfGone forgets a tracked container that was removed or stopped from
// outside (docker rm, engine restart), so EnsureRunning starts a fresh one
// instead of the next exec failing with "No such container". A stopped one
// of r's generation is removed by its ID; one of another person or
// generation is forgotten but left alone. Other inspect errors keep the
// state. Runs with the person's lock held.
func (s *Service) dropIfGone(ctx context.Context, uid string, r *running) {
	c, err := s.rt.Inspect(ctx, config.SlotContainer(r.slot))
	switch {
	case errors.Is(err, runtime.ErrNotFound):
	case err != nil || (c.Running && ours(c, uid, r)):
		return
	case ours(c, uid, r):
		if err := s.rt.Remove(ctx, c.ID); err != nil {
			s.log.WarnContext(ctx, "stopped container not removed", "uid", uid, "slot", r.slot, "err", err) // start removes it by name
		}
	}
	s.log.WarnContext(ctx, "container gone", "uid", uid, "slot", r.slot, "reason", "removed or stopped from outside", "inspect_err", err)
	s.forgetLocked(ctx, uid, r.slot, r)
}

// forgetLocked is the common end of a main container (stop, idle, drain,
// restart, disappearance): the generation's lease ends and its tool
// containers go. A failed tool removal stays visible in the engine; the
// reaper's sweep (sweepTools) retries it. r may be nil (nothing tracked).
// Runs with the person's lock held.
func (s *Service) forgetLocked(ctx context.Context, uid string, slot int, r *running) {
	s.set(uid, nil)
	if r != nil {
		r.lease.stop()
	}
	if err := s.removeTools(ctx, uid, slot); err != nil {
		s.log.WarnContext(ctx, "tool containers not removed", "op", "remove tools", "uid", uid, "slot", slot, "err", err)
	}
}

func (s *Service) Status(uid string) Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.run[uid]
	switch {
	case r == nil:
		return Status{State: StateAbsent}
	case r.draining:
		return Status{State: StateDraining, StartedAt: r.started}
	default:
		return Status{State: StateReady, StartedAt: r.started}
	}
}

// Running is the number of containers the manager tracks as running.
func (s *Service) Running() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.run)
}

// RunningUIDs lists the people whose environment runs.
func (s *Service) RunningUIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Collect(maps.Keys(s.run))
}

// EnsureRunning starts the person's container if needed and waits for readiness.
// Concurrent callers for the same person share one start via the per-person lock.
func (s *Service) EnsureRunning(ctx context.Context, uid string) (Status, error) {
	ctx, done, ok := s.beginWork(ctx)
	if !ok {
		return s.Status(uid), ErrNotRunning
	}
	defer done()
	l := s.lock(uid)
	l.Lock()
	defer l.Unlock()
	return s.ensureLocked(ctx, uid)
}

func (s *Service) ensureLocked(ctx context.Context, uid string) (Status, error) {
	if err := s.st.SharedRuntimeAccess(ctx, uid); err != nil {
		return Status{State: StateAbsent}, err
	}
	if r := s.get(uid); r != nil {
		s.dropIfGone(ctx, uid, r)
	}
	if s.get(uid) == nil {
		u, err := s.st.UserByUID(ctx, uid)
		if err != nil {
			return Status{State: StateAbsent}, err
		}
		if err := s.start(ctx, u); err != nil {
			return Status{State: StateAbsent}, err
		}
	}
	s.Touch(uid)
	return s.Status(uid), nil
}

func (s *Service) attachNetwork(ctx context.Context, slot int) error {
	if _, err := s.reconcileNetwork(ctx, slot); err != nil {
		return err
	}
	net := config.SlotNetwork(slot)
	if err := s.rt.EnsureNetwork(ctx, net, s.cfg.SlotSubnet(slot), s.cfg.NetGatewayMode); err != nil {
		return fmt.Errorf("network %s: %w", net, err)
	}
	// The manager serves llm-gw on every user network and proxies to the container from there.
	if err := s.rt.Connect(ctx, net, s.cfg.ManagerContainer, s.cfg.SlotAddr(slot, config.HostManager), "llm-gw"); err != nil {
		return fmt.Errorf("connect manager to %s: %w", net, err)
	}
	if s.cfg.EgressContainer != "" {
		if err := s.rt.Connect(ctx, net, s.cfg.EgressContainer, s.cfg.SlotAddr(slot, config.HostEgress), "egress"); err != nil {
			return fmt.Errorf("connect egress to %s: %w", net, err)
		}
	}
	return nil
}

func (s *Service) spec(u store.User, gen int64, pw string) runtime.Spec {
	home := path.Join(s.cfg.UsersRoot, u.UID)
	mounts := []runtime.Mount{
		{Source: home + "/workspace", Target: "/workspace"},
		{Source: home + "/config", Target: "/home/user/.config"},
		{Source: home + "/data", Target: "/home/user/.local/share"},
		{Source: home + "/state", Target: "/home/user/.local/state"},
	}
	if s.cfg.AdminConfig != "" {
		mounts = append(mounts, runtime.Mount{Source: s.cfg.AdminConfig, Target: "/etc/opencode", ReadOnly: true})
	}
	if s.cfg.CatalogDir != "" {
		// a directory, never single files: rsync --delay-updates replaces files by rename
		mounts = append(mounts, runtime.Mount{Source: s.cfg.CatalogDir, Target: catalog.MountDir(), ReadOnly: true})
	}
	env := []string{
		"OPENCODE_PASSWORD=" + pw,
		"HTTPS_PROXY=http://egress:3128",
		"HTTP_PROXY=http://egress:3128",
		"NO_PROXY=llm-gw,localhost,127.0.0.1",
	}
	if s.cfg.CatalogDir != "" {
		env = append(env, "OPENCODE_CONFIG="+catalog.ConfigFile()) // opted-in central MCPs (D59), written by catalog.Apply
	}
	return runtime.Spec{
		Name:    config.SlotContainer(u.Slot),
		Image:   s.cfg.RuntimeImage,
		Network: config.SlotNetwork(u.Slot),
		IP:      s.cfg.SlotAddr(u.Slot, config.HostUser),
		Labels: map[string]string{
			runtime.LabelManaged: "1",
			"oc.uid":             u.UID,
			"oc.slot":            strconv.Itoa(u.Slot),
			"oc.generation":      strconv.FormatInt(gen, 10),
			"oc.image":           s.cfg.RuntimeImage,
		},
		Env:    env,
		Cmd:    openCodeArgs("https://"+u.Slug+"."+s.cfg.Domain, s.ocLogLevel()),
		Mounts: mounts,
	}
}

// openCodeArgs follow the image's "opencode serve" entrypoint. CI starts the
// real runtime image with testdata/opencode-args.txt (runtime:smoke), and
// TestOpenCodeArgsGolden keeps that file equal to this function.
func openCodeArgs(cors, level string) []string {
	return []string{"--cors", cors, "--print-logs", "--log-level", level}
}

// ocLogLevel is OpenCode's --log-level: debug when the manager logs debug.
// Lowercase: the Effect CLI log-level flag used by 2.2.0 is case-sensitive.
func (s *Service) ocLogLevel() string {
	if s.log.Enabled(context.Background(), slog.LevelDebug) {
		return "debug"
	}
	return "info"
}

// start runs with the person's lock held.
func (s *Service) start(ctx context.Context, u store.User) error {
	ctx = logx.With(ctx, logx.Fields{UID: u.UID, Slot: u.Slot})
	t0 := time.Now()
	if err := s.attachNetwork(ctx, u.Slot); err != nil {
		s.log.ErrorContext(ctx, "container start failed", "op", "network", "err", err)
		return err
	}
	name := config.SlotContainer(u.Slot)
	if c, err := s.rt.Inspect(ctx, name); err == nil {
		if r, err := s.adoptable(ctx, u, c); err != nil {
			s.log.WarnContext(ctx, "container probe failed, keeping container", "err", err)
			return err
		} else if r != nil {
			s.set(u.UID, r)
			s.log.InfoContext(ctx, "container adopted", "reason", "start found it running", "generation", r.gen)
			return nil
		}
		s.log.InfoContext(ctx, "container remove", "reason", "stale before start", "running", c.Running)
		if err := s.rt.Remove(ctx, name); err != nil {
			s.log.ErrorContext(ctx, "container start failed", "op", "remove stale", "err", err)
			return err
		}
	} else if !errors.Is(err, runtime.ErrNotFound) {
		s.log.ErrorContext(ctx, "container start failed", "op", "inspect", "err", err)
		return err
	}
	if !s.reservePlace() {
		s.log.InfoContext(ctx, "container start refused", "reason", "all places taken", "max_running", s.cfg.MaxRunning)
		return ErrFull
	}
	releasePlace := sync.OnceFunc(s.releasePlace)
	defer releasePlace()
	if s.Provision != nil {
		pt := time.Now()
		if err := s.Provision(ctx, u.UID, u.Slot); err != nil {
			s.log.ErrorContext(ctx, "provision failed", "op", "ensure", "uid", u.UID, "slot", u.Slot, "duration_ms", time.Since(pt).Milliseconds(), "err", err)
			return fmt.Errorf("provision slot %d: %w", u.Slot, err)
		}
		s.log.InfoContext(ctx, "provisioned", "op", "ensure", "duration_ms", time.Since(pt).Milliseconds())
	}
	gen, err := s.st.NextGeneration(ctx, u.UID)
	if err != nil {
		return err
	}
	pw := Password(s.cfg.MasterKey, u.UID, gen)
	ct := time.Now()
	if err := s.rt.Create(ctx, s.spec(u, gen, pw)); err != nil {
		s.log.ErrorContext(ctx, "container start failed", "op", "create", "generation", gen, "err", err)
		return fmt.Errorf("create %s: %w", name, err)
	}
	s.log.DebugContext(ctx, "container created", "generation", gen, "image", s.cfg.RuntimeImage, "duration_ms", time.Since(ct).Milliseconds())
	r := &running{slot: u.Slot, gen: gen, password: pw, lease: newLease()}
	op := "start"
	st := time.Now()
	if err = s.rt.Start(ctx, name); err == nil {
		s.log.DebugContext(ctx, "container running, waiting for opencode", "duration_ms", time.Since(st).Milliseconds())
		op, st = "wait ready", time.Now()
		err = s.waitReady(ctx, s.client(r))
	}
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if rerr := s.rt.Remove(cleanup, name); rerr != nil {
			err = errors.Join(err, fmt.Errorf("remove: %w", rerr))
		}
		s.log.ErrorContext(ctx, "container start failed", "op", op, "generation", gen, "duration_ms", time.Since(st).Milliseconds(), "err", err)
		return fmt.Errorf("start %s: %w", name, err)
	}
	r.started, r.lastRequest = s.now(), s.now()
	s.set(u.UID, r)
	releasePlace()
	s.log.InfoContext(ctx, "container started", "slot", u.Slot, "generation", gen, "ready_ms", time.Since(st).Milliseconds(),
		"duration_ms", time.Since(t0).Milliseconds())
	s.finishPendingLocked(ctx, u.UID)
	if s.AfterStart != nil {
		s.AfterStart(ctx, u.UID)
	}
	return nil
}

// reservePlace takes one of the MaxRunning places for a new container.
func (s *Service) reservePlace() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.MaxRunning > 0 && len(s.run)+s.creating >= s.cfg.MaxRunning {
		return false
	}
	s.creating++
	return true
}

func (s *Service) releasePlace() {
	s.mu.Lock()
	s.creating--
	s.mu.Unlock()
}

// adoptable accepts an existing running container of this person whose
// password still matches (manager restart, or forgotten after a dial error).
// An inconclusive probe returns an error so callers retain the container.
func (s *Service) adoptable(ctx context.Context, u store.User, c runtime.Container) (*running, error) {
	if err := s.st.SharedRuntimeAccess(ctx, u.UID); err != nil {
		if errors.Is(err, assistants.ErrIsolationRequired) {
			return nil, nil
		}
		return nil, err
	}
	gen, err := strconv.ParseInt(c.Labels["oc.generation"], 10, 64)
	if !c.Running || c.Labels["oc.uid"] != u.UID || c.Labels["oc.slot"] != strconv.Itoa(u.Slot) || err != nil {
		return nil, nil
	}
	r := &running{slot: u.Slot, gen: gen, password: Password(s.cfg.MasterKey, u.UID, gen), lease: newLease()}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := s.client(r).Info(cctx); err != nil && !ocapi.IsStatus(err, http.StatusServiceUnavailable) {
		if ocapi.IsStatus(err, http.StatusUnauthorized) {
			return nil, nil
		}
		return nil, err
	}
	// grace period: counters are empty after a restart (docs/adr/README.md §3.11)
	r.started, r.lastRequest = s.now(), s.now()
	return r, nil
}

// waitReady polls GET /api/info every 100 ms until 200 or StartTimeout.
func (s *Service) waitReady(ctx context.Context, c *ocapi.Client) error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.StartTimeout)
	defer cancel()
	for {
		pctx, pcancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := c.Info(pctx)
		pcancel()
		if err == nil {
			return nil
		}
		if ocapi.IsStatus(err, http.StatusUnauthorized) {
			return err // wrong password never heals
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("not ready: %w", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Stop stops and removes the container. Data lives in the bind mounts only.
func (s *Service) Stop(ctx context.Context, uid string) error {
	ctx, done, ok := s.beginWork(ctx)
	if !ok {
		return ErrNotRunning
	}
	defer done()
	l := s.lock(uid)
	l.Lock()
	defer l.Unlock()
	return s.stopLocked(ctx, uid, "user")
}

func (s *Service) stopLocked(ctx context.Context, uid, reason string) error {
	if err := s.closePersonCAD(ctx, uid, true); err != nil {
		return err
	}
	r := s.get(uid)
	slot := 0
	if r != nil {
		slot = r.slot
	} else if u, err := s.st.UserByUID(ctx, uid); err == nil {
		slot = u.Slot
	} else {
		return err
	}
	name := config.SlotContainer(slot)
	ctx = logx.With(ctx, logx.Fields{UID: uid, Slot: slot})
	t0 := time.Now()
	if err := s.rt.Stop(ctx, name, 10*time.Second); err != nil {
		s.log.ErrorContext(ctx, "container stop failed", "op", "stop", "reason", reason, "err", err)
		return err
	}
	if err := s.rt.Remove(ctx, name); err != nil {
		s.log.ErrorContext(ctx, "container stop failed", "op", "remove", "reason", reason, "err", err)
		return err // still tracked: the reaper's next stop retries
	}
	s.forgetLocked(ctx, uid, slot, r)
	attrs := []any{"slot", slot, "reason", reason, "duration_ms", time.Since(t0).Milliseconds()}
	if r != nil {
		attrs = append(attrs, "generation", r.gen, "uptime_s", int(s.now().Sub(r.started).Seconds()))
	}
	s.log.InfoContext(ctx, "container stopped", attrs...)
	return nil
}

// Restart stops and starts with a new generation (new password, current
// image), under one hold of the person's lock: nothing sees the gap.
func (s *Service) Restart(ctx context.Context, uid string) (Status, error) {
	ctx, done, ok := s.beginWork(ctx)
	if !ok {
		return s.Status(uid), ErrNotRunning
	}
	defer done()
	l := s.lock(uid)
	l.Lock()
	defer l.Unlock()
	if err := s.stopLocked(ctx, uid, "restart"); err != nil {
		return s.Status(uid), err
	}
	return s.ensureLocked(ctx, uid)
}

// Reset is Restart: root is read-only and /home/user, /tmp are tmpfs, so a
// new container already drops installed packages and caches while workspace,
// config, data and state stay.
func (s *Service) Reset(ctx context.Context, uid string) (Status, error) { return s.Restart(ctx, uid) }

// Drain marks the container for replacement once no agent runs (or DrainCap passes).
func (s *Service) Drain(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.run[uid]; r != nil && !r.draining {
		r.draining, r.drainSince = true, s.now()
		s.log.Info("container draining", "uid", uid, "slot", r.slot, "generation", r.gen, "reason", "max lifetime")
	}
}
