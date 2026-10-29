// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/runtime"
)

// Tool containers (D60, Demo/Test): one per person and switched-on catalog
// toolbox, created on first use, removed with the person's container (stop,
// idle stop, disappearance, account deletion), by the reaper's sweep when that
// failed, and by Reconcile after a manager restart. They
// only exist through runtime.Runtime (Spec.Tool, Exec), so the shape maps to
// a second container in the person's pod or an own pod on the same volume.

// ToolboxTimeout bounds one command; ToolboxOutput bounds what comes back.
const (
	ToolboxTimeout = 120 * time.Second
	ToolboxOutput  = 64 << 10
)

const (
	labelToolbox  = "oc.toolbox"
	toolWorkspace = "/workspace" // mount target in the tool container
	// oldLabelToolbox marks containers (oc-user<n>-w-<id>) started before the
	// English rename; stop and sweep still remove them. Compatibility for one
	// release, together with dockerproxy.oldToolCtrRE.
	oldLabelToolbox = "oc.werkzeug"
)

// isToolbox reports a toolbox container under the current or the old label.
func isToolbox(labels map[string]string) bool {
	return labels[labelToolbox] != "" || labels[oldLabelToolbox] != ""
}

// RunToolbox runs command with sh -c as uid 1000 in the person's tool
// container for toolbox (image from the catalog), in dir below /workspace.
// out gets stdout and stderr, cut at ToolboxOutput (truncated reports it).
// Exit code 124 means the timeout hit. The person's environment must run.
//
// The timeout inside the container shares uid 1000 with the command, which
// can stop it (kill -STOP $PPID). So the manager enforces the end itself: at
// toolboxKill, or when ctx ends, it removes the tool container, which kills
// every process in it; the next run creates it again. Runs in one tool
// container take turns, so that removal never hits another run.
func (s *Service) RunToolbox(ctx context.Context, uid, toolbox, image, dir, command string, out io.Writer) (code int, truncated bool, err error) {
	if dir != toolWorkspace && !isBelow(dir, toolWorkspace) {
		return 0, false, fmt.Errorf("toolbox: dir %q outside the workspace", dir)
	}
	done, err := s.toolTurn(ctx, uid+"/"+toolbox)
	if err != nil {
		return 0, false, err
	}
	defer done()
	slot, err := s.ensureTool(ctx, uid, toolbox, image)
	if err != nil {
		return 0, false, err
	}
	// same lease as sh: DeleteAccount waits for this exec before the slot is reused
	op := s.opLock(uid)
	op.RLock()
	defer op.RUnlock()
	name := config.ToolContainer(slot, toolbox)
	r := s.get(uid)
	if r == nil || r.slot != slot {
		return 0, false, ErrNotRunning
	}
	// the generation lease: a stop or account deletion cancels the exec
	ctx, release, ok := r.lease.bind(ctx)
	if !ok {
		return 0, false, ErrNotRunning
	}
	defer release()
	c, err := s.rt.Inspect(ctx, name)
	if err != nil || c.Labels["oc.uid"] != uid {
		return 0, false, ErrNotRunning
	}
	ectx, cancel := context.WithTimeout(ctx, s.toolboxKill)
	defer cancel()
	lw := &capWriter{w: out, n: ToolboxOutput}
	// the usual end: timeout inside the container gives exit code 124 with the output so far
	cmd := []string{"sh", "-c", `cd -- "$1" || exit 126; exec timeout -k 5 ` + strconv.Itoa(int(ToolboxTimeout.Seconds())) + ` sh -c "$2"`,
		"sh", dir, command}
	code, err = s.rt.Exec(ectx, name, cmd, nil, lw, lw)
	if ectx.Err() == nil {
		return code, lw.cut, err
	}
	// by ID: the name may already be a recreated container
	if rerr := s.rt.Remove(context.WithoutCancel(ctx), c.ID); rerr != nil {
		s.log.ErrorContext(ctx, "tool container not removed after its deadline", "toolbox", toolbox, "err", rerr)
		return code, lw.cut, errors.Join(ectx.Err(), rerr)
	}
	s.log.WarnContext(ctx, "tool container removed under a run", "toolbox", toolbox, "cause", ectx.Err())
	if ctx.Err() != nil {
		return code, lw.cut, ctx.Err()
	}
	return 124, lw.cut, nil
}

// toolTurn waits for the tool container's single place; done gives it back.
func (s *Service) toolTurn(ctx context.Context, key string) (done func(), err error) {
	s.mu.Lock()
	ch := s.toolRuns[key]
	if ch == nil {
		ch = make(chan struct{}, 1)
		s.toolRuns[key] = ch
	}
	s.mu.Unlock()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func isBelow(p, root string) bool {
	return path.Clean(p) == p && len(p) > len(root) && p[:len(root)+1] == root+"/"
}

// ensureTool starts the tool container if it is missing, stopped or on
// another image. Runs under the person's lock, so a concurrent stop cannot
// leave one behind.
func (s *Service) ensureTool(ctx context.Context, uid, toolbox, image string) (int, error) {
	l := s.lock(uid)
	l.Lock()
	defer l.Unlock()
	r := s.get(uid)
	if r == nil {
		return 0, ErrNotRunning
	}
	name := config.ToolContainer(r.slot, toolbox)
	c, err := s.rt.Inspect(ctx, name)
	switch {
	case err == nil && c.Running && c.Labels["oc.uid"] == uid && c.Labels["oc.image"] == image:
		return r.slot, nil
	case err == nil:
		if err := s.rt.Remove(ctx, name); err != nil {
			return 0, err
		}
	case !errors.Is(err, runtime.ErrNotFound):
		return 0, err
	}
	spec := runtime.Spec{
		Name:  name,
		Image: image,
		Tool:  true,
		Labels: map[string]string{
			runtime.LabelManaged: "1",
			"oc.uid":             uid,
			"oc.slot":            strconv.Itoa(r.slot),
			labelToolbox:         toolbox,
			"oc.image":           image,
		},
		Cmd:    []string{"sleep", "infinity"},
		Mounts: []runtime.Mount{{Source: path.Join(s.cfg.UsersRoot, uid, "workspace"), Target: toolWorkspace}},
	}
	if err := s.rt.Create(ctx, spec); err != nil {
		return 0, fmt.Errorf("create %s: %w", name, err)
	}
	if err := s.rt.Start(ctx, name); err != nil {
		s.rt.Remove(context.WithoutCancel(ctx), name)
		return 0, fmt.Errorf("start %s: %w", name, err)
	}
	s.log.InfoContext(ctx, "tool container started", "slot", r.slot, "toolbox", toolbox)
	return r.slot, nil
}

// removeTools removes the tool containers of uid on slot; a reused slot's next
// tenant keeps theirs.
func (s *Service) removeTools(ctx context.Context, uid string, slot int) error {
	list, err := s.rt.List(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range list {
		if isToolbox(c.Labels) && c.Labels["oc.uid"] == uid && c.Labels["oc.slot"] == strconv.Itoa(slot) {
			errs = append(errs, s.rt.Remove(ctx, c.ID))
		}
	}
	return errors.Join(errs...)
}

// sweepTools removes tool containers whose person has no tracked main
// container on that slot: left by a teardown that failed or a main that
// vanished unnoticed. A person whose lock is busy waits for the next tick.
func (s *Service) sweepTools(ctx context.Context) {
	list, err := s.rt.List(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "tool sweep: list", "err", err)
		return
	}
	for _, c := range list {
		if !isToolbox(c.Labels) {
			continue
		}
		uid := c.Labels["oc.uid"]
		l := s.lock(uid)
		if !l.TryLock() {
			continue
		}
		if r := s.get(uid); r == nil || strconv.Itoa(r.slot) != c.Labels["oc.slot"] {
			if err := s.rt.Remove(ctx, c.ID); err != nil {
				s.log.WarnContext(ctx, "tool sweep: remove", "name", c.Name, "err", err)
			} else {
				s.log.InfoContext(ctx, "orphan tool container removed", "name", c.Name)
			}
		}
		l.Unlock()
	}
}

// capWriter keeps the first n bytes and swallows the rest.
type capWriter struct {
	w   io.Writer
	n   int
	cut bool
}

func (c *capWriter) Write(p []byte) (int, error) {
	if len(p) > c.n {
		c.cut = true
	}
	if k := min(len(p), c.n); k > 0 {
		if _, err := c.w.Write(p[:k]); err != nil {
			return 0, err
		}
		c.n -= k
	}
	return len(p), nil
}
