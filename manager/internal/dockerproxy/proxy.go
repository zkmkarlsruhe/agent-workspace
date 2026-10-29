// SPDX-License-Identifier: MIT

// Package dockerproxy is the filtering Docker Engine API proxy between the
// manager and /var/run/docker.sock: only the calls of internal/runtime pass,
// only on the manager's own objects, and every create body is checked and
// re-encoded, so a compromised manager cannot start a privileged or
// host-mounting container (blueprint §11 #1).
package dockerproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/logx"
)

// Names as in config.SlotContainer / config.SlotNetwork; neither can be read
// as a hex id prefix, so the engine resolves them by name only.
var (
	userCtrRE = regexp.MustCompile(`^oc-user[0-9]{1,3}$`)
	// config.ToolContainer: oc-user<slot>-tb-<toolbox> (D60)
	ctrIDRE   = regexp.MustCompile(`^[0-9a-f]{12,64}$`)
	toolCtrRE = regexp.MustCompile(`^oc-user([0-9]{1,3})-tb-([a-z0-9-]{2,40})$`)
	// oldToolCtrRE is the name before the English rename (oc-user<slot>-w-<id>):
	// such containers may still be inspected, started and removed, never created.
	// Compatibility for one release; remove together with lifecycle.oldLabelToolbox.
	oldToolCtrRE = regexp.MustCompile(`^oc-user([0-9]{1,3})-w-([a-z0-9-]{2,40})$`)
	pinnedRE     = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
	userNetRE    = regexp.MustCompile(`^oc-u[0-9]{1,3}$`)
	versionRE    = regexp.MustCompile(`^/v[0-9]+\.[0-9]+`)
	uidRE        = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

const labelManaged = "oc.managed" // runtime.LabelManaged

const (
	maxMem  = 2 << 30
	maxCPU  = 2e9
	maxPids = 512
)

type Policy struct {
	UserNetPrefix string // first two octets, default 10.90
	ImageRepo     string // the runtime image repository
	RuntimeImage  string // exact OC_RUNTIME_IMAGE; other refs require a digest
	// UsersRoot: writable binds only <UsersRoot>/<oc.uid>/<one of userDirs>,
	// the container's own person (label oc.uid).
	UsersRoot string
	// Volumes are the named volumes a container may mount, read-only; none by default.
	Volumes []string
	// ReadOnlyRoots: any other bind is read-only and below one of these
	// (admin config, catalog).
	ReadOnlyRoots []string
	// Joinable are the non-user containers that may be inspected and joined to
	// user networks (the manager itself as llm-gw, egress).
	Joinable []string
	// ToolImages are the exact digest-pinned images a tool container may run
	// (catalog kind toolbox, D60); none by default.
	ToolImages []string
}

type Proxy struct {
	Policy
	Log    *slog.Logger
	mux    *http.ServeMux
	rp     *httputil.ReverseProxy
	engine *http.Client
}

// New proxies to the engine socket at sock.
func New(p Policy, sock string, log *slog.Logger) *Proxy {
	if p.UserNetPrefix == "" {
		p.UserNetPrefix = "10.90"
	}
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}
	px := &Proxy{Policy: p, Log: log, mux: http.NewServeMux(), engine: &http.Client{Transport: tr}}
	px.rp = &httputil.ReverseProxy{
		Rewrite:       func(r *httputil.ProxyRequest) { r.Out.URL.Scheme, r.Out.URL.Host = "http", "docker" },
		Transport:     tr,
		FlushInterval: -1,
	}
	ok := func(*http.Request) error { return nil }
	user := px.ownRef
	unet := func(r *http.Request) error { return match(userNetRE, r.PathValue("net"), "network") }
	for pattern, check := range map[string]func(*http.Request) error{
		"GET /_ping":                    ok, // GET patterns match HEAD too
		"GET /version":                  ok,
		"GET /containers/json":          listFilter,
		"POST /containers/create":       px.create,
		"GET /containers/{name}/json":   px.inspectable,
		"POST /containers/{name}/start": user,
		"POST /containers/{name}/stop":  user,
		"DELETE /containers/{name}":     user,
		"POST /containers/{name}/exec": func(r *http.Request) error {
			if err := user(r); err != nil {
				return err
			}
			return rewrite(r, checkExec)
		},
		"POST /exec/{id}/start":           px.execOwned,
		"GET /exec/{id}/json":             px.execOwned,
		"POST /networks/create":           func(r *http.Request) error { return rewrite(r, px.checkNetwork) },
		"GET /networks/{net}":             unet,
		"DELETE /networks/{net}":          unet,
		"POST /networks/{net}/disconnect": unet, // the body names container ids; leaving a user network is harmless
		"POST /networks/{net}/connect": func(r *http.Request) error {
			if err := unet(r); err != nil {
				return err
			}
			return rewrite(r, func(c *network.ConnectRequest) error {
				if userCtrRE.MatchString(c.Container) && r.PathValue("net") != "oc-u"+strings.TrimPrefix(c.Container, "oc-user") {
					return errors.New("container and network slots must match")
				}
				if !userCtrRE.MatchString(c.Container) && !slices.Contains(px.Joinable, c.Container) {
					return fmt.Errorf("container %q may not join", c.Container)
				}
				return nil
			})
		},
	} {
		px.mux.HandleFunc(pattern, px.guard(check))
	}
	px.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { px.deny(w, r, errors.New("endpoint not allowed")) })
	return px
}

type origPath struct{}

// ServeHTTP routes on the path without the API version prefix and forwards
// the original path, so the negotiated API version stays in effect.
func (px *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// none of the allowed names has escaped bytes; unclean paths would get a mux redirect
	if r.URL.RawPath != "" || path.Clean(r.URL.Path) != r.URL.Path {
		px.deny(w, r, errors.New("unclean path"))
		return
	}
	orig := r.URL.Path
	r = r.WithContext(context.WithValue(r.Context(), origPath{}, orig))
	r.URL.Path = versionRE.ReplaceAllString(orig, "")
	logx.Route(px.mux).ServeHTTP(w, r)
}

func (px *Proxy) guard(check func(*http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := check(r); err != nil {
			px.deny(w, r, err)
			return
		}
		px.Log.DebugContext(r.Context(), "docker call allowed", "rule", r.Pattern, "method", r.Method, "path", r.URL.Path)
		r.URL.Path = r.Context().Value(origPath{}).(string)
		px.rp.ServeHTTP(w, r)
	}
}

func (px *Proxy) deny(w http.ResponseWriter, r *http.Request, err error) {
	px.Log.WarnContext(r.Context(), "docker call denied", "rule", r.Pattern, "method", r.Method, "path", r.URL.Path, "err", err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	json.NewEncoder(w).Encode(map[string]string{"message": "oc-dockerproxy: " + err.Error()})
}

func match(re *regexp.Regexp, s, what string) error {
	if !re.MatchString(s) {
		return fmt.Errorf("%s %q not managed", what, s)
	}
	return nil
}

// ownCtr: a person's OpenCode container or one of its tool containers.
func ownCtr(name string) error {
	if toolCtrRE.MatchString(name) || oldToolCtrRE.MatchString(name) {
		return nil
	}
	return match(userCtrRE, name, "container")
}

func (px *Proxy) inspectable(r *http.Request) error {
	if n := r.PathValue("name"); slices.Contains(px.Joinable, n) {
		return nil
	}
	return px.ownRef(r)
}

// ownRef: the path names a managed container by name, or by ID (the manager
// removes by immutable ID, D60); an ID is resolved at the engine and its name checked.
func (px *Proxy) ownRef(r *http.Request) error {
	ref := r.PathValue("name")
	if !ctrIDRE.MatchString(ref) {
		return ownCtr(ref)
	}
	var c container.InspectResponse
	if err := px.get(r.Context(), "/containers/"+ref+"/json", &c); err != nil {
		return err
	}
	return ownCtr(strings.TrimPrefix(c.Name, "/"))
}

// listFilter: a list must be restricted to managed containers.
func listFilter(r *http.Request) error {
	var f map[string]map[string]bool
	if json.Unmarshal([]byte(r.URL.Query().Get("filters")), &f) != nil || !f["label"][labelManaged+"=1"] {
		return errors.New("list needs the label filter " + labelManaged + "=1")
	}
	return nil
}

// rewrite decodes the body into the engine's own request type, checks it and
// forwards the re-encoded value: fields this API version does not know never
// reach the engine.
func rewrite[T any](r *http.Request, check func(*T) error) error {
	var v T
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&v); err != nil {
		return fmt.Errorf("body: %w", err)
	}
	if err := check(&v); err != nil {
		return err
	}
	b, err := json.Marshal(&v)
	if err != nil {
		return err
	}
	r.Body, r.ContentLength, r.TransferEncoding = io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
	r.Header.Set("Content-Type", "application/json")
	return nil
}

func (px *Proxy) create(r *http.Request) error {
	name := r.URL.Query().Get("name")
	if m := toolCtrRE.FindStringSubmatch(name); m != nil {
		return rewrite(r, func(c *container.CreateRequest) error { return px.checkToolCreate(m[1], m[2], c) })
	}
	if err := match(userCtrRE, name, "container"); err != nil {
		return err
	}
	return rewrite(r, func(c *container.CreateRequest) error { return px.checkCreate(name, c) })
}

// checkCreate holds the create rules, including the hardening floor
// (read-only root, cap drop, limits) of runtime.dockerSpec.
func (px *Proxy) checkCreate(name string, c *container.CreateRequest) error {
	if c.Config == nil || c.HostConfig == nil {
		return errors.New("Config and HostConfig required")
	}
	if ImageRepo(c.Image) != px.ImageRepo || (c.Image != px.RuntimeImage && !pinnedRE.MatchString(c.Image)) {
		return fmt.Errorf("image %q not allowed", c.Image)
	}
	if c.Labels[labelManaged] != "1" {
		return errors.New("label " + labelManaged + "=1 required")
	}
	if c.User != "1000:1000" {
		return fmt.Errorf("user %q not allowed", c.User)
	}
	// name, network and data dirs must all belong to one slot and one person
	slot, uid := strings.TrimPrefix(name, "oc-user"), c.Labels["oc.uid"]
	if c.Labels["oc.slot"] != slot || !uidRE.MatchString(uid) {
		return errors.New("labels oc.slot and oc.uid must match the container")
	}
	h := c.HostConfig
	if err := checkHost(h); err != nil {
		return err
	}
	if !h.ReadonlyRootfs || !slices.Contains(h.CapDrop, "ALL") || len(h.SecurityOpt) == 0 {
		return errors.New("user container needs read-only rootfs, CapDrop ALL and no-new-privileges")
	}
	r := h.Resources
	if r.Memory <= 0 || r.Memory > maxMem || r.MemorySwap != r.Memory || r.NanoCPUs <= 0 || r.NanoCPUs > maxCPU ||
		r.PidsLimit == nil || *r.PidsLimit <= 0 || *r.PidsLimit > maxPids {
		return errors.New("user container needs memory, cpu and pids limits")
	}
	if string(h.NetworkMode) != "oc-u"+slot {
		return fmt.Errorf("NetworkMode %q not allowed", h.NetworkMode)
	}
	if c.NetworkingConfig != nil {
		for n := range c.NetworkingConfig.EndpointsConfig {
			if n != "oc-u"+slot {
				return fmt.Errorf("network %q not allowed", n)
			}
		}
	}
	for _, m := range h.Mounts {
		if err := px.checkMount(m, uid); err != nil {
			return err
		}
	}
	return nil
}

// checkToolCreate: a tool container may only run a catalog image, without
// network, with the hardening of runtime.dockerSpec (Tool) and nothing but
// the person's workspace and tmpfs mounted.
func (px *Proxy) checkToolCreate(slot, toolbox string, c *container.CreateRequest) error {
	if c.Config == nil || c.HostConfig == nil {
		return errors.New("Config and HostConfig required")
	}
	if !slices.Contains(px.ToolImages, c.Image) || !pinnedRE.MatchString(c.Image) {
		return fmt.Errorf("tool image %q not allowed", c.Image)
	}
	uid := c.Labels["oc.uid"]
	if c.Labels[labelManaged] != "1" || c.Labels["oc.slot"] != slot || c.Labels["oc.toolbox"] != toolbox || !uidRE.MatchString(uid) {
		return errors.New("labels oc.managed, oc.slot, oc.uid and oc.toolbox must match the container")
	}
	if c.User != "1000:1000" {
		return fmt.Errorf("user %q not allowed", c.User)
	}
	h := c.HostConfig
	if err := checkHost(h); err != nil {
		return err
	}
	if !h.ReadonlyRootfs || !slices.Contains(h.CapDrop, "ALL") || len(h.SecurityOpt) == 0 {
		return errors.New("tool container needs read-only rootfs, CapDrop ALL and no-new-privileges")
	}
	r := h.Resources
	if r.Memory <= 0 || r.Memory > maxMem || r.MemorySwap != r.Memory || r.NanoCPUs <= 0 || r.NanoCPUs > maxCPU ||
		r.PidsLimit == nil || *r.PidsLimit <= 0 || *r.PidsLimit > maxPids {
		return errors.New("tool container needs memory, cpu and pids limits")
	}
	if h.NetworkMode != "none" {
		return fmt.Errorf("NetworkMode %q not allowed", h.NetworkMode)
	}
	if c.NetworkingConfig != nil {
		for n := range c.NetworkingConfig.EndpointsConfig {
			if n != "none" {
				return fmt.Errorf("network %q not allowed", n)
			}
		}
	}
	for _, m := range h.Mounts {
		if m.Type != mount.TypeTmpfs && (m.Type != mount.TypeBind || m.BindOptions != nil || m.Target != "/workspace" ||
			m.Source != px.UsersRoot+"/"+uid+"/workspace") {
			return fmt.Errorf("tool mount %q not allowed", m.Source)
		}
	}
	return nil
}

// checkHost refuses every HostConfig field that reaches the host or another
// namespace; SecurityOpt may only carry no-new-privileges.
func checkHost(h *container.HostConfig) error {
	for name, bad := range map[string]bool{
		"Privileged": h.Privileged, "CapAdd": len(h.CapAdd) > 0, "Binds": len(h.Binds) > 0,
		// GroupAdd and Ulimits widen privileges and are not needed by the runtime.
		"GroupAdd": len(h.GroupAdd) > 0, "Ulimits": len(h.Resources.Ulimits) > 0,
		// Tmpfs and shm are charged to the memory cgroup, bounded by the required Memory limit.
		"ShmSize": h.ShmSize < 0 || h.ShmSize > maxMem, "MemoryReservation": h.Resources.MemoryReservation < 0,
		"VolumesFrom": len(h.VolumesFrom) > 0, "VolumeDriver": h.VolumeDriver != "",
		"PidMode": h.PidMode != "", "IpcMode": h.IpcMode != "", "UTSMode": h.UTSMode != "",
		"UsernsMode": h.UsernsMode != "", "CgroupnsMode": h.CgroupnsMode != "", "Cgroup": h.Cgroup != "",
		"CgroupParent": h.CgroupParent != "", "Devices": len(h.Devices) > 0, "DeviceRequests": len(h.DeviceRequests) > 0,
		"DeviceCgroupRules": len(h.DeviceCgroupRules) > 0, "PortBindings": len(h.PortBindings) > 0,
		"PublishAllPorts": h.PublishAllPorts, "Sysctls": len(h.Sysctls) > 0, "Links": len(h.Links) > 0,
		"Runtime": h.Runtime != "", "MaskedPaths": h.MaskedPaths != nil, "ReadonlyPaths": h.ReadonlyPaths != nil,
		"Annotations": len(h.Annotations) > 0, "StorageOpt": len(h.StorageOpt) > 0, "LogConfig": h.LogConfig.Type != "",
		"OomScoreAdj": h.OomScoreAdj < 0, "OomKillDisable": h.OomKillDisable != nil && *h.OomKillDisable,
	} {
		if bad {
			return fmt.Errorf("HostConfig.%s not allowed", name)
		}
	}
	for _, o := range h.SecurityOpt {
		if o != "no-new-privileges" && o != "no-new-privileges:true" && o != "no-new-privileges=true" {
			return fmt.Errorf("SecurityOpt %q not allowed", o)
		}
	}
	return nil
}

func (px *Proxy) checkMount(m mount.Mount, uid string) error {
	switch m.Type {
	case mount.TypeTmpfs:
		return nil
	case mount.TypeVolume:
		// a driver config can turn a new named volume into a bind of any host path
		if m.VolumeOptions != nil && m.VolumeOptions.DriverConfig != nil || !m.ReadOnly || !slices.Contains(px.Volumes, m.Source) {
			return fmt.Errorf("volume mount %q not allowed", m.Source)
		}
		return nil
	case mount.TypeBind:
		if m.BindOptions == nil && path.IsAbs(m.Source) && path.Clean(m.Source) == m.Source {
			// exact paths only: below them the agent can plant symlinks, and the
			// engine follows a symlinked bind source
			if d, ok := strings.CutPrefix(m.Source, px.UsersRoot+"/"+uid+"/"); ok && slices.Contains(userDirs, d) {
				return nil
			}
			for _, root := range px.ReadOnlyRoots {
				if m.ReadOnly && strings.HasPrefix(m.Source, root+"/") {
					return nil
				}
			}
		}
		return fmt.Errorf("bind mount %q not allowed", m.Source)
	}
	return fmt.Errorf("mount type %q not allowed", m.Type)
}

// userDirs are the bind sources of runtime spec (lifecycle.spec) below the
// person's dir; the agent sees only their contents, never the dir holding them.
var userDirs = []string{"workspace", "config", "data", "state"}

func checkExec(e *container.ExecCreateRequest) error {
	if e.Privileged || e.User != "1000:1000" {
		return fmt.Errorf("exec as %q privileged=%v not allowed", e.User, e.Privileged)
	}
	return nil
}

func (px *Proxy) checkNetwork(n *network.CreateRequest) error {
	if !userNetRE.MatchString(n.Name) || n.Driver != "bridge" || !n.Internal || n.Labels[labelManaged] != "1" ||
		n.ConfigFrom != nil || n.ConfigOnly || n.Ingress || n.Attachable || n.EnableIPv6 != nil && *n.EnableIPv6 {
		return fmt.Errorf("network create %q: only internal managed bridges oc-u<n>", n.Name)
	}
	n.EnableIPv6 = new(false) // pin omitted values too, independent of daemon defaults
	for k, v := range n.Options {
		if k != "com.docker.network.bridge.gateway_mode_ipv4" || v != "isolated" {
			return errors.New("network options: only isolated IPv4 gateway mode allowed")
		}
	}
	slot, err := strconv.Atoi(strings.TrimPrefix(n.Name, "oc-u"))
	if err != nil || slot < 1 || slot > 254 {
		return errors.New("network slot must be 1..254")
	}
	subnet, err := netip.ParsePrefix(fmt.Sprintf("%s.%d.0/24", px.UserNetPrefix, slot))
	if err != nil || n.IPAM == nil || len(n.IPAM.Config) != 1 ||
		(n.IPAM.Driver != "" && n.IPAM.Driver != "default") || len(n.IPAM.Options) != 0 {
		return errors.New("network IPAM must use the slot subnet and default driver")
	}
	ipam := n.IPAM.Config[0]
	if ipam.Subnet != subnet || ipam.IPRange.IsValid() || ipam.Gateway.IsValid() || len(ipam.AuxAddress) != 0 {
		return errors.New("network IPAM must contain only the slot subnet")
	}
	return nil
}

// execOwned asks the engine whose exec the id is: it must be an unprivileged
// uid 1000 exec in a user container. Stateless, so it survives a proxy restart.
func (px *Proxy) execOwned(r *http.Request) error {
	var e container.ExecInspectResponse
	if err := px.get(r.Context(), "/exec/"+r.PathValue("id")+"/json", &e); err != nil {
		return err
	}
	if e.ProcessConfig == nil || e.ProcessConfig.User != "1000:1000" || e.ProcessConfig.Privileged != nil && *e.ProcessConfig.Privileged {
		return errors.New("exec not created by the manager")
	}
	var c container.InspectResponse
	if err := px.get(r.Context(), "/containers/"+e.ContainerID+"/json", &c); err != nil {
		return err
	}
	return ownCtr(strings.TrimPrefix(c.Name, "/"))
}

func (px *Proxy) get(ctx context.Context, p string, v any) error {
	if strings.ContainsAny(p[1:], "?#%") || strings.Count(p, "/") != 3 {
		return errors.New("bad id")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+p, nil)
	if err != nil {
		return err
	}
	resp, err := px.engine.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("engine %s: %s", p, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}

// ImageRepo strips digest and tag: "h:5000/a/b:t@sha256:x" is "h:5000/a/b".
func ImageRepo(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref = ref[:i]
	}
	return ref
}
