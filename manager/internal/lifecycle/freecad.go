// SPDX-License-Identifier: MIT

package lifecycle

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/runtime"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

var ErrFreeCAD = errors.New("FreeCAD unavailable or session changed; reopen from the current project")

type cadSlot struct {
	sync.Mutex
	worker *cadWorker
}
type cadWorker struct {
	id, session, token, endpoint, project, folder string
	generation                                    int64
	slot                                          int
	lease                                         *lease
	done                                          chan struct{}
	err                                           error // published by closing done
	active                                        bool  // guarded by cadSlot; only one tool round trip per worker
}

func (s *Service) cadSlot(uid string) *cadSlot {
	v, _ := s.cad.LoadOrStore(uid, &cadSlot{})
	return v.(*cadSlot)
}
func cadReady(w *cadWorker) bool {
	select {
	case <-w.done:
		return w.err == nil
	default:
		return false
	}
}
func cadID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func (s *Service) cadProject(ctx context.Context, uid, session string) (store.Project, error) {
	dir, err := s.SessionDir(ctx, uid, session)
	if err != nil || path.Dir(dir) != s.ws || !ValidProject(path.Base(dir)) {
		return store.Project{}, ErrFreeCAD
	}
	p, err := s.st.ProjectByFolder(ctx, uid, path.Base(dir))
	if err != nil || p.Isolated || p.Assistant.ID != "" || s.FreeCADAllowed == nil || !s.FreeCADAllowed(ctx, uid, p.ID) {
		return store.Project{}, ErrFreeCAD
	}
	return p, nil
}

// The chat session determines the project; model-supplied paths never select a tenant.
func (s *Service) CallFreeCAD(ctx context.Context, uid, chat, name string, args json.RawMessage) (*sdk.CallToolResult, error) {
	ctx, finished, admitted := s.beginWork(ctx)
	if !admitted {
		return nil, ErrNotRunning
	}
	defer finished()
	if s.cfg.FreeCADImage == "" {
		return nil, ErrFreeCAD
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
	// Serialize selection with stop/project changes without holding the person lock over I/O.
	lock := s.lock(uid)
	lock.Lock()
	slot := s.cadSlot(uid)
	locked := slot.TryLock()
	lock.Unlock()
	if !locked {
		return nil, ErrBusy
	}
	defer slot.Unlock()
	if ctx.Err() != nil || s.get(uid) != r {
		return nil, ErrNotRunning
	}
	p, err := s.cadProject(ctx, uid, chat)
	if err != nil {
		return nil, err
	}
	if w := slot.worker; name == "freecad_open" && w != nil && cadReady(w) {
		c, inspectErr := s.rt.Inspect(ctx, w.id)
		if inspectErr != nil && !errors.Is(inspectErr, runtime.ErrNotFound) {
			return nil, ErrFreeCAD
		}
		if errors.Is(inspectErr, runtime.ErrNotFound) || !c.Running {
			if err := s.closeCAD(ctx, slot, true); err != nil {
				return nil, err
			}
		}
	}
	if w := slot.worker; w != nil && (w.project != p.ID || w.generation != r.gen) {
		if name != "freecad_open" {
			return nil, ErrFreeCAD
		}
		if err := s.closeCAD(ctx, slot, false); err != nil {
			return nil, err
		}
	}
	if name == "freecad_open" {
		if slot.worker == nil {
			w := &cadWorker{session: cadID(), token: cadID() + cadID(), project: p.ID, folder: p.Folder, generation: r.gen, slot: r.slot, lease: newLease(), done: make(chan struct{})}
			w.endpoint = "http://" + s.cfg.SlotAddr(r.slot, config.HostFreeCAD).String() + ":8080/mcp"
			if s.cadURL != nil {
				w.endpoint = s.cadURL(r.slot)
			}
			s.mu.Lock()
			if s.background.Err() != nil {
				s.mu.Unlock()
				return nil, ErrFreeCAD
			}
			slot.worker = w
			s.workers.Go(func() {
				startCtx, done, valid := w.lease.bind(s.background)
				if !valid {
					w.err = ErrFreeCAD
					close(w.done)
					return
				}
				defer done()
				w.err = s.startCAD(startCtx, uid, r, w)
				close(w.done)
			})
			s.mu.Unlock()
		}
		w := slot.worker
		select {
		case <-w.done:
			if w.err != nil {
				_ = s.closeCAD(ctx, slot, true)
				return nil, ErrFreeCAD
			}
		default:
			return officeResult(map[string]any{"status": "starting", "session_id": w.session, "retry_after_seconds": 2}), nil
		}
		s.Touch(uid)
		return officeResult(map[string]any{"status": "ready", "session_id": w.session, "project": p.Folder,
			"browser_url": "https://cad-" + w.session + "." + s.cfg.Domain + "/", "next": "Open browser_url in a new tab, then use freecad_state. All dimensions use mm."}), nil
	}
	w := slot.worker
	var a map[string]json.RawMessage
	if json.Unmarshal(args, &a) != nil || w == nil || !cadReady(w) {
		return nil, ErrFreeCAD
	}
	var session string
	if json.Unmarshal(a["session_id"], &session) != nil || session != w.session {
		return nil, ErrFreeCAD
	}
	if name == "freecad_close" {
		if err := s.closeCAD(ctx, slot, false); err != nil {
			return nil, err
		}
		return officeResult(map[string]any{"status": "closed", "saved": true}), nil
	}
	if w.active {
		return nil, ErrBusy
	}
	callCtx, done, ok := w.lease.bind(ctx)
	if !ok {
		return nil, ErrFreeCAD
	}
	w.active = true
	slot.Unlock()
	defer func() {
		done() // Teardown holds the slot while draining the lease.
		slot.Lock()
		w.active = false
	}()
	s.Touch(uid)
	s.touchProject(ctx, uid, p.Folder, s.now())
	return s.cadCall(callCtx, w, name, args)
}

func (s *Service) startCAD(ctx context.Context, uid string, r *running, w *cadWorker) (err error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	ctx, release, ok := r.lease.bind(ctx)
	if !ok {
		return ErrFreeCAD
	}
	defer release()
	// Resolve inside the main runtime before binding its fixed, provisioned workspace root.
	if err = s.sh(ctx, uid, checkPath+`[ -d "$1" ] || exit 3`, nil, nil, s.projectDir(w.folder)); err != nil {
		return err
	}
	name := config.FreeCADContainer(r.slot)
	if old, e := s.rt.Inspect(ctx, name); e == nil {
		if old.Labels["oc.uid"] != uid {
			return ErrFreeCAD
		}
		if e = s.rt.Remove(ctx, old.ID); e != nil {
			return e
		}
	} else if !errors.Is(e, runtime.ErrNotFound) {
		return e
	}
	spec := runtime.Spec{Name: name, Image: s.cfg.FreeCADImage, Network: config.SlotNetwork(r.slot), IP: s.cfg.SlotAddr(r.slot, config.HostFreeCAD), FreeCAD: true,
		Labels: map[string]string{runtime.LabelManaged: "1", "oc.uid": uid, "oc.slot": strconv.Itoa(r.slot), "oc.generation": strconv.FormatInt(r.gen, 10), "oc.freecad": "1", "oc.session": w.session, "oc.project": w.project, "oc.image": s.cfg.FreeCADImage},
		Env:    []string{"WORKSPACE_FREECAD_SESSION=" + w.session, "WORKSPACE_FREECAD_PROJECT_ID=" + w.project, "WORKSPACE_FREECAD_PROJECT=/workspace/" + w.folder},
		Mounts: []runtime.Mount{{Source: path.Join(s.cfg.UsersRoot, uid, "workspace"), Target: "/workspace"}},
	}
	if err = s.rt.Create(ctx, spec); err != nil {
		return err
	}
	c, err := s.rt.Inspect(ctx, name)
	if err != nil {
		_ = s.rt.Remove(context.WithoutCancel(ctx), name)
		return err
	}
	w.id = c.ID
	defer func() {
		if err != nil {
			cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
			defer stop()
			_ = s.rt.Remove(cleanup, c.ID)
		}
	}()
	if err = s.rt.Start(ctx, c.ID); err != nil {
		return err
	}
	data, _ := json.Marshal(map[string]string{"token": w.token, "listen": s.cfg.SlotAddr(r.slot, config.HostFreeCAD).String(), "manager": s.cfg.SlotAddr(r.slot, config.HostManager).String()})
	code, err := s.rt.Exec(ctx, c.ID, []string{"python3", "/opt/workspace-freecad/configure.py"}, bytes.NewReader(data), io.Discard, io.Discard)
	if err != nil || code != 0 {
		return ErrFreeCAD
	}
	for {
		probe, stop := context.WithTimeout(ctx, 3*time.Second)
		result, e := s.cadCall(probe, w, "freecad_state", json.RawMessage(fmt.Sprintf(`{"session_id":%q}`, w.session)))
		stop()
		if e == nil && !result.IsError {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (s *Service) cadCall(ctx context.Context, w *cadWorker, name string, args json.RawMessage) (*sdk.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	req, err := http.NewRequestWithContext(ctx, "POST", w.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+w.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	client := &http.Client{Transport: s.cadTransport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, ErrFreeCAD
	}
	var reply struct {
		Result *sdk.CallToolResult `json:"result"`
		Error  json.RawMessage     `json:"error"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&reply); err != nil || reply.Result == nil || len(reply.Error) > 0 {
		return nil, ErrFreeCAD
	}
	return reply.Result, nil
}

func (s *Service) closeCAD(ctx context.Context, slot *cadSlot, force bool) error {
	w := slot.worker
	if w == nil {
		return nil
	}
	// Stop browser input before saving, but allow the control call to finish independently.
	w.lease.end()
	<-w.done // Startup publishes id and err before closing done.
	if cadReady(w) {
		saveCtx := ctx
		cancel := func() {}
		if force {
			saveCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		}
		result, err := s.cadCall(saveCtx, w, "freecad_save_all", json.RawMessage(fmt.Sprintf(`{"session_id":%q}`, w.session)))
		cancel()
		if err != nil || result.IsError {
			if !force {
				w.lease = newLease()
				return errors.New("FreeCAD could not save; finish the current edit and retry closing")
			}
			s.log.WarnContext(ctx, "FreeCAD save failed; attempting recovery before forced removal", "project", w.project, "session", w.session, "err", err)
			s.recoverCAD(ctx, w)
		}
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 25*time.Second)
	defer cancel()
	if w.id != "" {
		if err := s.removeOffice(cleanup, w.id); err != nil {
			return err
		}
	}
	slot.worker = nil
	return nil
}

// Try a live snapshot even during an edit; a hung GUI falls back to durable documents.
func (s *Service) recoverCAD(ctx context.Context, w *cadWorker) {
	recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	result, err := s.cadCall(recovery, w, "freecad_recover_all", json.RawMessage(fmt.Sprintf(`{"session_id":%q}`, w.session)))
	cancel()
	if err == nil && !result.IsError {
		s.log.WarnContext(ctx, "FreeCAD live recovery saved", "project", w.project)
		return
	}
	recovery, cancel = context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	code, err := s.rt.Exec(recovery, w.id, []string{"python3", "/opt/workspace-freecad/recovery.py"}, nil, io.Discard, io.Discard)
	s.log.WarnContext(ctx, "FreeCAD durable recovery attempted; unsaved GUI changes may be lost", "project", w.project, "exit_code", code, "err", err)
}

func (s *Service) CloseFreeCAD(ctx context.Context, uid string) error {
	return s.closePersonCAD(ctx, uid, false)
}

func (s *Service) closePersonCAD(ctx context.Context, uid string, force bool) error {
	slot := s.cadSlot(uid)
	slot.Lock()
	defer slot.Unlock()
	return s.closeCAD(ctx, slot, force)
}

func (s *Service) closeProjectCAD(ctx context.Context, uid, project string) error {
	slot := s.cadSlot(uid)
	slot.Lock()
	defer slot.Unlock()
	if slot.worker == nil || slot.worker.project != project {
		return nil
	}
	return s.closeCAD(ctx, slot, false)
}

// Browser routes carry only a random session identifier; ownership is checked on every request.
func (s *Service) FreeCADTarget(ctx context.Context, uid, session string) (Target, bool) {
	slot := s.cadSlot(uid)
	slot.Lock()
	defer slot.Unlock()
	w := slot.worker
	r := s.get(uid)
	if s.background.Err() != nil || w == nil || w.session != session || !cadReady(w) || r == nil || r.gen != w.generation || s.FreeCADAllowed == nil || !s.FreeCADAllowed(ctx, uid, w.project) {
		return Target{}, false
	}
	p, err := s.st.ProjectByID(ctx, uid, w.project)
	if err != nil || p.Folder != w.folder {
		return Target{}, false
	}
	return Target{BaseURL: "http://" + s.cfg.SlotAddr(w.slot, config.HostFreeCAD).String() + ":3000", Password: w.token, lease: w.lease}, true
}

func (s *Service) sweepCAD(ctx context.Context) {
	s.cad.Range(func(key, value any) bool {
		uid, slot := key.(string), value.(*cadSlot)
		if !slot.TryLock() {
			return true
		}
		defer slot.Unlock()
		if w := slot.worker; w != nil {
			r := s.get(uid)
			p, err := s.st.ProjectByID(ctx, uid, w.project)
			if err != nil && !errors.Is(err, store.ErrNotFound) && r != nil && r.gen == w.generation {
				s.log.WarnContext(ctx, "FreeCAD project lookup failed; cleanup deferred", "uid", uid, "err", err)
				return true
			}
			if r == nil || r.gen != w.generation || errors.Is(err, store.ErrNotFound) || (err == nil && p.Folder != w.folder) || s.FreeCADAllowed == nil || !s.FreeCADAllowed(ctx, uid, w.project) {
				if err := s.closeCAD(ctx, slot, true); err != nil {
					s.log.WarnContext(ctx, "FreeCAD cleanup failed", "uid", uid)
				}
			}
		}
		return true
	})
	list, err := s.rt.List(ctx)
	if err != nil {
		return
	}
	for _, c := range list {
		if c.Labels["oc.freecad"] != "1" {
			continue
		}
		slot := s.cadSlot(c.Labels["oc.uid"])
		if !slot.TryLock() {
			continue
		}
		if slot.worker == nil {
			_ = s.removeOffice(ctx, c.ID)
		}
		slot.Unlock()
	}
}

func (s *Service) closeAllCAD() {
	var cleanup sync.WaitGroup
	s.cad.Range(func(_, value any) bool {
		slot := value.(*cadSlot)
		cleanup.Go(func() {
			slot.Lock()
			defer slot.Unlock()
			if err := s.closeCAD(context.Background(), slot, true); err != nil {
				s.log.Warn("FreeCAD shutdown failed", "err", err)
			}
		})
		return true
	})
	cleanup.Wait()
}
