// SPDX-License-Identifier: MIT

// Package lctest holds test fakes for lifecycle.Service: a container runtime
// whose Exec runs the shell scripts on the test host, and a minimal OpenCode API.
package lctest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/runtime"
)

// Runtime records engine calls; Exec runs the command on the test host so the
// shell scripts are exercised for real against a temp workspace.
type Runtime struct {
	Mu           sync.Mutex
	Ctrs         map[string]*runtime.Container
	Specs        []runtime.Spec
	Nets         []string
	Connects     []string
	Removed      []string
	NetsGone     []string // RemoveNetwork calls
	GatewayModes map[string]string
	// Root, if set, replaces the /workspace prefix of every Exec argument, for
	// callers outside package lifecycle that cannot move the workspace root.
	Root string
	// Paths replaces container paths inside every Exec argument, script text
	// included (catalog scripts name /home/user/... and /opt/workspace/... inline).
	Paths map[string]string
	// OpenCode, if set, is the API served for the containers: Stop ends its
	// runs, as a removed container takes the OpenCode process and its runs
	// with it (GET /api/session/active only counts runs of this process).
	OpenCode  *OpenCode
	StartErr  error            // Start fails with this error
	RemoveErr map[string]error // Remove fails for these container names
	// AfterInspect runs after Inspect read the state and before it returns,
	// so a test can change the engine under a caller holding a stale view.
	AfterInspect func(name string)
	// KillOnRemove makes Remove kill the container's exec processes, as the
	// engine does; off, an exec outlives Remove (tests of the drain barrier).
	KillOnRemove bool

	execs sync.WaitGroup
	ids   int
	procs map[string][]int // process groups of execs per container name, killed by Remove
}

func NewRuntime() *Runtime { return &Runtime{Ctrs: map[string]*runtime.Container{}} }

// Close kills and reaps execs left running after their attach context ended.
// Callers must stop submitting execs before Close.
func (f *Runtime) Close() {
	f.Mu.Lock()
	for _, groups := range f.procs {
		for _, pg := range groups {
			syscall.Kill(-pg, syscall.SIGKILL)
		}
	}
	f.Mu.Unlock()
	f.execs.Wait()
}

func (f *Runtime) Ping(context.Context) error { return nil }
func (f *Runtime) EnsureNetwork(_ context.Context, n string, p netip.Prefix, gatewayMode string) error {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.Nets = append(f.Nets, n+"="+p.String())
	if f.GatewayModes == nil {
		f.GatewayModes = map[string]string{}
	}
	if _, exists := f.GatewayModes[n]; !exists {
		f.GatewayModes[n] = gatewayMode
	}
	return nil
}
func (f *Runtime) NetworkGatewayMode(_ context.Context, n string) (string, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	mode, ok := f.GatewayModes[n]
	if !ok {
		return "", runtime.ErrNotFound
	}
	return mode, nil
}
func (f *Runtime) RemoveNetwork(_ context.Context, n string) error {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.NetsGone = append(f.NetsGone, n)
	delete(f.GatewayModes, n)
	return nil
}
func (f *Runtime) Connect(_ context.Context, n, c string, ip netip.Addr, al ...string) error {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.Connects = append(f.Connects, fmt.Sprintf("%s/%s/%s/%v", n, c, ip, al))
	return nil
}
func (f *Runtime) Create(_ context.Context, s runtime.Spec) error {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if _, ok := f.Ctrs[s.Name]; ok {
		return fmt.Errorf("conflict %s", s.Name)
	}
	f.Specs = append(f.Specs, s)
	f.ids++
	f.Ctrs[s.Name] = &runtime.Container{ID: fmt.Sprintf("id%d", f.ids), Name: s.Name, Labels: s.Labels}
	return nil
}
func (f *Runtime) Start(_ context.Context, n string) error {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if f.StartErr != nil {
		return f.StartErr
	}
	f.Ctrs[n].Running = true
	return nil
}
func (f *Runtime) Stop(_ context.Context, n string, _ time.Duration) error {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if c := f.Ctrs[n]; c != nil {
		c.Running = false
	}
	if o := f.OpenCode; o != nil {
		o.Mu.Lock()
		o.Active = nil
		o.Mu.Unlock()
	}
	return nil
}

// Execs counts the exec processes of a container that still run.
func (f *Runtime) Execs(name string) int {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return len(f.procs[name])
}

// Remove takes a name or an ID like Docker; Removed records the name.
func (f *Runtime) Remove(_ context.Context, n string) error {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	for name, c := range f.Ctrs {
		if c.ID == n {
			n = name
		}
	}
	if err := f.RemoveErr[n]; err != nil {
		return err
	}
	delete(f.Ctrs, n) // a stale ID matches nothing
	if f.KillOnRemove {
		for _, pg := range f.procs[n] {
			syscall.Kill(-pg, syscall.SIGKILL)
		}
	}
	f.Removed = append(f.Removed, n)
	return nil
}
func (f *Runtime) Inspect(_ context.Context, n string) (runtime.Container, error) {
	f.Mu.Lock()
	c, err := runtime.Container{}, runtime.ErrNotFound
	if p := f.Ctrs[n]; p != nil {
		c, err = *p, nil
	}
	hook := f.AfterInspect
	f.Mu.Unlock()
	if hook != nil {
		hook(n)
	}
	return c, err
}
func (f *Runtime) List(context.Context) ([]runtime.Container, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	var out []runtime.Container
	for _, c := range f.Ctrs {
		out = append(out, *c)
	}
	return out, nil
}
func (f *Runtime) Exec(ctx context.Context, name string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	f.Mu.Lock()
	_, known := f.Ctrs[name]
	f.Mu.Unlock()
	if !known && len(f.Ctrs) > 0 { // like Docker: exec into a removed container
		return 0, errors.New("Error response from daemon: No such container: " + name)
	}
	if f.Root != "" {
		cmd = append([]string(nil), cmd...)
		for i, a := range cmd {
			if a == "/workspace" || strings.HasPrefix(a, "/workspace/") {
				cmd[i] = f.Root + strings.TrimPrefix(a, "/workspace")
			}
		}
	}
	if len(f.Paths) > 0 {
		cmd = append([]string(nil), cmd...)
		for from, to := range f.Paths {
			for i := range cmd {
				cmd[i] = strings.ReplaceAll(cmd[i], from, to)
			}
		}
	}
	// like a Docker exec: a cancelled ctx ends the attach stream, not the
	// process; Remove of the container kills it (own process group)
	c := exec.Command(cmd[0], cmd[1:]...)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	g := &gate{}
	c.Stdin, c.Stdout, c.Stderr = stdin, g.to(stdout), g.to(stderr)
	if same(stdout, stderr) {
		c.Stderr = c.Stdout // one pipe keeps the order of the two streams
	}
	// du echoes its path arguments; map them back so the caller finds its keys
	var du bytes.Buffer
	isDu := f.Root != "" && slices.ContainsFunc(cmd, func(a string) bool { return strings.Contains(a, "du -s") })
	if isDu {
		c.Stdout = g.to(&du)
	}
	if err := c.Start(); err != nil {
		return 0, err
	}
	pg := c.Process.Pid
	f.Mu.Lock()
	if f.procs == nil {
		f.procs = map[string][]int{}
	}
	f.procs[name] = append(f.procs[name], pg)
	f.Mu.Unlock()
	done := make(chan error, 1)
	f.execs.Add(1)
	go func() {
		defer f.execs.Done()
		err := c.Wait()
		f.Mu.Lock()
		f.procs[name] = slices.DeleteFunc(f.procs[name], func(p int) bool { return p == pg }) // never kill a reused group id
		f.Mu.Unlock()
		done <- err
	}()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		g.close()
		return -1, ctx.Err()
	}
	if isDu {
		io.WriteString(stdout, strings.ReplaceAll(du.String(), "\t"+f.Root, "\t/workspace"))
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	return 0, err
}

// gate passes writes on until close; later output of a detached exec is dropped.
type gate struct {
	mu     sync.Mutex
	closed bool
}

func (g *gate) to(w io.Writer) io.Writer {
	if w == nil {
		return nil
	}
	return gateWriter{g, w}
}

func (g *gate) close() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
}

type gateWriter struct {
	g *gate
	w io.Writer
}

func (w gateWriter) Write(p []byte) (int, error) {
	w.g.mu.Lock()
	defer w.g.mu.Unlock()
	if w.g.closed {
		return len(p), nil
	}
	return w.w.Write(p)
}

// same is a == b without the panic on uncomparable writer types.
func same(a, b io.Writer) (eq bool) {
	defer func() { recover() }()
	return a != nil && a == b
}

// OpenCode is a minimal OpenCode API.
type OpenCode struct {
	InterruptPending bool // accept the interrupt; the test emits settlement later

	events        map[chan []byte]bool
	FailInterrupt bool
	Mu            sync.Mutex
	Active        []string
	ActiveDown    bool // GET /api/session/active answers 500
	Sessions      []ocapi.Session
	Interrupted   []string
	Prompts       []string
	Forms         int
	Deleted       []string         // DELETE /api/session/{id} calls
	Moved         []string         // "<id>=<directory>" per move call
	ArchiveCalls  []string         // "<id>=<bool>" per PATCH call
	FailArchive   string           // this id answers 500 on PATCH
	Now           func() time.Time // clock for archive/unarchive timestamps
	FailDelete    string           // this id answers 500 on DELETE
	Lists         int
	LoadingFor    int // GET /api/model answers 503 this many more times (folder still loading)
	ModelGets     int
	MCPGets       int // GET /api/mcp calls
	MCPPendingFor int // the next n GET /api/mcp report workspace as pending
	Queued        int // GET .../message hides the prompts this many more times (still in the inbox)
	// Messages are raw message objects per session id, served oldest first
	// with limit and cursor (= offset) like GET /api/session/{id}/message.
	Messages    map[string][]string
	PageCalls   int // message pages served from Messages
	Reloads     int // POST /api/location/reload calls
	Locations   []ocapi.Location
	Evicted     []string
	Reloaded    []string
	MissingDirs map[string]bool // simulate DirectoryNotFoundError during reload
	FailReload  bool
}

func (o *OpenCode) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/event" {
		o.serveEvents(w, r)
		return
	}
	o.Mu.Lock()
	defer o.Mu.Unlock()
	switch {
	case r.URL.Path == "/api/info":
		io.WriteString(w, `{"version":"2.2.0"}`)
	case r.URL.Path == "/api/session/active" && o.ActiveDown:
		w.WriteHeader(500)
	case r.URL.Path == "/api/session/active":
		m := map[string]any{}
		for _, id := range o.Active {
			m[id] = map[string]string{"type": "running"}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": m})
	case r.Method == "POST" && r.URL.Path == "/api/session":
		var in struct {
			Location ocapi.Location
			Agent    string
		}
		json.NewDecoder(r.Body).Decode(&in)
		se := ocapi.Session{ID: fmt.Sprintf("ses_%d", len(o.Sessions)+1), Location: in.Location, Agent: in.Agent}
		o.Sessions = append(o.Sessions, se)
		json.NewEncoder(w).Encode(map[string]any{"data": se})
	case r.Method == "GET" && r.URL.Path == "/api/session":
		// cursor = "<offset>:<directory>", standing in for OpenCode's opaque cursor
		q := r.URL.Query()
		dir, off := q.Get("directory"), 0
		if c := q.Get("cursor"); c != "" {
			n, d, _ := strings.Cut(c, ":")
			off, _ = strconv.Atoi(n)
			dir = d
		}
		var out []ocapi.Session
		for _, se := range o.Sessions {
			if se.Location.Directory == dir {
				out = append(out, se)
			}
		}
		out = out[min(off, len(out)):]
		next := ""
		if l, _ := strconv.Atoi(q.Get("limit")); l > 0 && len(out) > l {
			out, next = out[:l], fmt.Sprintf("%d:%s", off+l, dir)
		}
		o.Lists++
		json.NewEncoder(w).Encode(map[string]any{"data": out, "cursor": map[string]string{"next": next}})
	case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, "/api/session/"):
		id := strings.TrimPrefix(r.URL.Path, "/api/session/")
		var in struct {
			Archived *bool `json:"archived"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Archived == nil {
			w.WriteHeader(400)
			return
		}
		o.ArchiveCalls = append(o.ArchiveCalls, fmt.Sprintf("%s=%t", id, *in.Archived))
		if id == o.FailArchive {
			w.WriteHeader(500)
			return
		}
		i := slices.IndexFunc(o.Sessions, func(se ocapi.Session) bool { return se.ID == id })
		if i < 0 {
			w.WriteHeader(404)
			return
		}
		se := &o.Sessions[i]
		if (se.Time.Archived != nil) != *in.Archived {
			now := time.Now()
			if o.Now != nil {
				now = o.Now()
			}
			se.Time.Updated = float64(now.UnixMilli())
			se.Time.Archived = nil
			if *in.Archived {
				ms := se.Time.Updated
				se.Time.Archived = &ms
			}
		}
		w.WriteHeader(204)
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/api/session/"):
		id := strings.TrimPrefix(r.URL.Path, "/api/session/")
		o.Deleted = append(o.Deleted, id)
		if id == o.FailDelete {
			w.WriteHeader(500)
			return
		}
		n := len(o.Sessions)
		o.Sessions = slices.DeleteFunc(o.Sessions, func(se ocapi.Session) bool { return se.ID == id || se.ParentID == id })
		if n == len(o.Sessions) {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(204)
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/move"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/session/"), "/move")
		var in ocapi.Location
		json.NewDecoder(r.Body).Decode(&in)
		o.Moved = append(o.Moved, id+"="+in.Directory)
		i := slices.IndexFunc(o.Sessions, func(se ocapi.Session) bool { return se.ID == id })
		if i < 0 {
			w.WriteHeader(404)
			return
		}
		o.Sessions[i].Location = in
		w.WriteHeader(204)
	case r.URL.Path == "/api/form":
		fmt.Fprintf(w, `{"data":[%s]}`, strings.Repeat(`{}`, o.Forms))
	case r.URL.Path == "/api/permission/request":
		io.WriteString(w, `{"data":[]}`)
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/interrupt"):
		o.Interrupted = append(o.Interrupted, r.URL.Path)
		if o.FailInterrupt {
			w.WriteHeader(500)
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/session/"), "/interrupt")
		if !o.InterruptPending {
			o.emitLocked("session.execution.interrupted", map[string]any{"sessionID": id, "reason": "user"})
		}
		io.WriteString(w, `{"interrupted":true}`)
	case r.Method == "GET" && r.URL.Path == "/api/mcp":
		o.MCPGets++
		if o.MCPPendingFor > 0 {
			o.MCPPendingFor--
			io.WriteString(w, `{"data":[{"name":"workspace","status":{"status":"pending"}}]}`)
			return
		}
		io.WriteString(w, `{"data":[{"name":"workspace","status":{"status":"connected"}},{"name":"workspace-knowledge","status":{"status":"connected"}}]}`)
	case r.Method == "GET" && r.URL.Path == "/api/model":
		o.ModelGets++
		if o.LoadingFor > 0 {
			o.LoadingFor--
			w.WriteHeader(503)
			return
		}
		io.WriteString(w, `{"data":[]}`)
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/message") &&
		o.Messages[strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/session/"), "/message")] != nil:
		q := r.URL.Query()
		if q.Get("cursor") != "" && q.Get("order") != "" {
			w.WriteHeader(400) // InvalidCursorError: the cursor carries its order
			return
		}
		msgs := o.Messages[strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/session/"), "/message")]
		off, _ := strconv.Atoi(q.Get("cursor"))
		limit, _ := strconv.Atoi(q.Get("limit"))
		if q.Get("order") == "desc" { // newest first; callers read one page only
			page := slices.Clone(msgs[max(0, len(msgs)-limit):])
			slices.Reverse(page)
			fmt.Fprintf(w, `{"data":[%s]}`, strings.Join(page, ","))
			return
		}
		page := msgs[min(off, len(msgs)):min(off+limit, len(msgs))]
		o.PageCalls++
		fmt.Fprintf(w, `{"data":[%s],"cursor":{"next":"%d"}}`, strings.Join(page, ","), off+len(page))
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/session/") && strings.Count(r.URL.Path, "/") == 3:
		id := strings.TrimPrefix(r.URL.Path, "/api/session/")
		i := slices.IndexFunc(o.Sessions, func(se ocapi.Session) bool { return se.ID == id })
		if i < 0 {
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": o.Sessions[i]})
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/message"):
		if len(o.Prompts) == 0 || o.Queued > 0 {
			o.Queued = max(o.Queued-1, 0)
			io.WriteString(w, `{"data":[]}`)
			return
		}
		io.WriteString(w, `{"data":[{"type":"assistant"},{"type":"user"}]}`)
	case r.Method == "GET" && r.URL.Path == "/api/debug/location":
		json.NewEncoder(w).Encode(o.Locations)
	case r.Method == "DELETE" && r.URL.Path == "/api/debug/location":
		dir := r.URL.Query().Get("location[directory]")
		o.Evicted = append(o.Evicted, dir)
		o.Locations = slices.DeleteFunc(o.Locations, func(l ocapi.Location) bool { return l.Directory == dir })
		w.WriteHeader(204)
	case r.Method == "POST" && r.URL.Path == "/api/location/reload":
		o.Reloads++
		missing := ""
		for _, l := range o.Locations {
			if o.MissingDirs[l.Directory] {
				missing = l.Directory
			} else {
				o.Reloaded = append(o.Reloaded, l.Directory)
			}
		}
		// Failed builds are evicted; healthy replacements still settle.
		o.Locations = slices.DeleteFunc(o.Locations, func(l ocapi.Location) bool { return o.MissingDirs[l.Directory] })
		if missing != "" || o.FailReload {
			w.WriteHeader(503)
			message := "reload unavailable"
			if missing != "" {
				message = "FileSystem.DirectoryNotFoundError: Directory not found: " + missing
			}
			json.NewEncoder(w).Encode(map[string]string{"_tag": "ServiceUnavailableError", "service": "location", "message": message})
			return
		}
		w.WriteHeader(204)
	case strings.HasSuffix(r.URL.Path, "/prompt"):
		b, _ := io.ReadAll(r.Body)
		o.Prompts = append(o.Prompts, string(b))
	default:
		w.WriteHeader(404)
	}
}

// Emit sends one live event and mirrors execution state in /session/active.
func (o *OpenCode) Emit(kind string, data map[string]any) {
	o.Mu.Lock()
	defer o.Mu.Unlock()
	o.emitLocked(kind, data)
}

func (o *OpenCode) emitLocked(kind string, data map[string]any) {
	id, _ := data["sessionID"].(string)
	switch kind {
	case "session.execution.started":
		if !slices.Contains(o.Active, id) {
			o.Active = append(o.Active, id)
		}
	case "session.execution.interrupted", "session.execution.succeeded", "session.execution.failed":
		o.Active = slices.DeleteFunc(o.Active, func(s string) bool { return s == id })
	}
	b, _ := json.Marshal(map[string]any{"type": kind, "data": data})
	for ch := range o.events {
		select {
		case ch <- b:
		default:
			panic("fake event reader fell behind")
		}
	}
}

func (o *OpenCode) EventReaders() int {
	o.Mu.Lock()
	defer o.Mu.Unlock()
	return len(o.events)
}

func (o *OpenCode) DisconnectEvents() {
	o.Mu.Lock()
	defer o.Mu.Unlock()
	for ch := range o.events {
		close(ch)
		delete(o.events, ch)
	}
}

func (o *OpenCode) serveEvents(w http.ResponseWriter, r *http.Request) {
	ch := make(chan []byte, 256)
	o.Mu.Lock()
	if o.events == nil {
		o.events = map[chan []byte]bool{}
	}
	o.events[ch] = true
	o.Mu.Unlock()
	defer func() {
		o.Mu.Lock()
		delete(o.events, ch)
		o.Mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	io.WriteString(w, "data: {\"type\":\"server.connected\",\"data\":{}}\n\n")
	w.(http.Flusher).Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case b, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", b)
			w.(http.Flusher).Flush()
		}
	}
}
