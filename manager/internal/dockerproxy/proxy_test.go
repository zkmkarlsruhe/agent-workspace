// SPDX-License-Identifier: MIT

package dockerproxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
)

const img = "registry.example.org/operations/opencode-runtime:2026.10.16.1@sha256:c3c17c3ddcee149dc9fc04692e77837dc256d0265a160e1d21e0c752b45785ae"

// fakeEngine answers every call with 200 and records what reached it.
type fakeEngine struct {
	got   []string
	body  string
	execs map[string]string // exec id -> inspect JSON
	names map[string]string // container id -> name
}

func (f *fakeEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.got, f.body = append(f.got, r.Method+" "+r.URL.Path), string(b)
	p := strings.Split(r.URL.Path, "/")
	switch {
	case len(p) == 4 && p[1] == "exec" && p[3] == "json" && f.execs[p[2]] != "":
		io.WriteString(w, f.execs[p[2]])
	case len(p) == 4 && p[1] == "containers" && f.names[p[2]] != "":
		io.WriteString(w, `{"Name":"/`+f.names[p[2]]+`"}`)
	case len(p) == 4 && (p[1] == "exec" || p[1] == "containers") && p[3] == "json":
		http.NotFound(w, r)
	default:
		io.WriteString(w, "{}")
	}
}

func setup(t *testing.T) (*fakeEngine, http.Handler) {
	dir, err := os.MkdirTemp("", "dp") // short: unix socket paths are limited to 108 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "e")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeEngine{
		execs: map[string]string{
			"e1": `{"ContainerID":"c1","ProcessConfig":{"user":"1000:1000"}}`,
			"e2": `{"ContainerID":"c2","ProcessConfig":{"user":"1000:1000"}}`,
			"e3": `{"ContainerID":"c1","ProcessConfig":{"user":"root","privileged":true}}`,
			"e4": `{"ContainerID":"c3","ProcessConfig":{"user":"1000:1000"}}`,
		},
		names: map[string]string{"c1": "oc-user4", "c2": "traefik", "c3": "oc-user4-tb-media",
			"4179ce2b3591": "oc-user4-tb-media", "aaaabbbbcccc": "traefik"},
	}
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: f}}
	srv.Start()
	t.Cleanup(srv.Close)
	px := New(Policy{ImageRepo: ImageRepo(img), UsersRoot: "/srv/opencode/users", ReadOnlyRoots: []string{"/srv/opencode/base"}, Volumes: []string{"opencode-config"},
		Joinable: []string{"oc-manager", "egress"}}, sock, slog.New(slog.DiscardHandler))
	return f, px
}

type engineTransport struct{ http.Handler }

func (tr engineTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	tr.ServeHTTP(w, r)
	return w.Result(), nil
}

func TestConnectRequiresMatchingSlot(t *testing.T) {
	for _, tc := range []struct {
		container string
		status    int
	}{
		{"oc-user5", http.StatusForbidden},
		{"oc-user7", http.StatusOK},
		{"oc-manager", http.StatusOK},
		{"egress", http.StatusOK},
	} {
		t.Run(tc.container, func(t *testing.T) {
			engine := &fakeEngine{}
			proxy := New(Policy{Joinable: []string{"oc-manager", "egress"}}, "", slog.New(slog.DiscardHandler))
			proxy.rp.Transport = engineTransport{engine}
			r := httptest.NewRequest("POST", "/networks/oc-u7/connect", strings.NewReader(`{"Container":"`+tc.container+`"}`))
			w := httptest.NewRecorder()
			proxy.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if tc.status == http.StatusForbidden && len(engine.got) != 0 {
				t.Fatal("forbidden connect reached engine", engine.got)
			}
		})
	}
}

// valid is what runtime.dockerSpec sends (TestDockerSpecHardening) for a slot.
func valid() container.CreateRequest {
	return container.CreateRequest{
		Config: &container.Config{Image: img, User: "1000:1000", Labels: map[string]string{"oc.managed": "1", "oc.uid": strings.Repeat("a", 64), "oc.slot": "4"}},
		HostConfig: &container.HostConfig{
			ReadonlyRootfs: true, CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"},
			NetworkMode: "oc-u4",
			Tmpfs: map[string]string{
				"/tmp":       "size=1g,mode=1777,exec",
				"/home/user": "size=256m,uid=1000,gid=1000,mode=0750",
			},
			Resources: container.Resources{
				Memory: 2 << 30, MemorySwap: 2 << 30, MemoryReservation: 256 << 20,
				NanoCPUs: 2e9, PidsLimit: new(int64(512)),
			},
			Mounts: []mount.Mount{
				{Type: mount.TypeBind, Source: "/srv/opencode/users/" + strings.Repeat("a", 64) + "/workspace", Target: "/workspace"},
				{Type: mount.TypeBind, Source: "/srv/opencode/base/etc-opencode", Target: "/etc/opencode", ReadOnly: true},
				{Type: mount.TypeVolume, Source: "opencode-config", Target: "/x", ReadOnly: true},
			},
		},
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{"oc-u4": {}}},
	}
}

func TestCreateRules(t *testing.T) {
	cases := map[string]func(c *container.CreateRequest){
		"writable rootfs": func(c *container.CreateRequest) { c.HostConfig.ReadonlyRootfs = false },
		"caps kept":       func(c *container.CreateRequest) { c.HostConfig.CapDrop = nil },
		"partial cap drop": func(c *container.CreateRequest) {
			c.HostConfig.CapDrop = []string{"NET_RAW"}
		},
		"new privileges": func(c *container.CreateRequest) { c.HostConfig.SecurityOpt = nil },
		"no memory limit": func(c *container.CreateRequest) {
			c.HostConfig.Memory, c.HostConfig.MemorySwap = 0, 0
		},
		"negative memory": func(c *container.CreateRequest) {
			c.HostConfig.Memory, c.HostConfig.MemorySwap = -1, -1
		},
		"too much memory": func(c *container.CreateRequest) {
			c.HostConfig.Memory, c.HostConfig.MemorySwap = (2<<30)+1, (2<<30)+1
		},
		"no swap limit": func(c *container.CreateRequest) { c.HostConfig.MemorySwap = 0 },
		"swap":          func(c *container.CreateRequest) { c.HostConfig.MemorySwap = -1 },
		"unequal swap":  func(c *container.CreateRequest) { c.HostConfig.MemorySwap++ },
		"negative reservation": func(c *container.CreateRequest) {
			c.HostConfig.MemoryReservation = -1
		},
		"no pids limit": func(c *container.CreateRequest) { c.HostConfig.PidsLimit = nil },
		"zero pids":     func(c *container.CreateRequest) { c.HostConfig.PidsLimit = new(int64(0)) },
		"negative pids": func(c *container.CreateRequest) { c.HostConfig.PidsLimit = new(int64(-1)) },
		"too many pids": func(c *container.CreateRequest) { c.HostConfig.PidsLimit = new(int64(513)) },
		"no cpu limit":  func(c *container.CreateRequest) { c.HostConfig.NanoCPUs = 0 },
		"negative cpus": func(c *container.CreateRequest) { c.HostConfig.NanoCPUs = -1 },
		"too many cpus": func(c *container.CreateRequest) { c.HostConfig.NanoCPUs = 2e9 + 1 },
		"group add":     func(c *container.CreateRequest) { c.HostConfig.GroupAdd = []string{"0"} },
		"negative shm":  func(c *container.CreateRequest) { c.HostConfig.ShmSize = -1 },
		"oversized shm": func(c *container.CreateRequest) { c.HostConfig.ShmSize = (2 << 30) + 1 },
		"ulimit": func(c *container.CreateRequest) {
			c.HostConfig.Resources.Ulimits = []*container.Ulimit{{Name: "nofile", Soft: 65536, Hard: 65536}}
		},
		"privileged":       func(c *container.CreateRequest) { c.HostConfig.Privileged = true },
		"no label":         func(c *container.CreateRequest) { delete(c.Labels, "oc.managed") },
		"other slot label": func(c *container.CreateRequest) { c.Labels["oc.slot"] = "5" },
		"bad uid label":    func(c *container.CreateRequest) { c.Labels["oc.uid"] = "../x" },
		"other person":     func(c *container.CreateRequest) { c.Labels["oc.uid"] = strings.Repeat("b", 64) },
		"other slot net":   func(c *container.CreateRequest) { c.HostConfig.NetworkMode = "oc-u5" },
		"writable base": func(c *container.CreateRequest) {
			c.HostConfig.Mounts = append(c.HostConfig.Mounts, mount.Mount{Type: mount.TypeBind, Source: "/srv/opencode/base/catalog", Target: "/c"})
		},
		"unpinned tag":     func(c *container.CreateRequest) { c.Image = ImageRepo(img) + ":latest" },
		"unpinned repo":    func(c *container.CreateRequest) { c.Image = ImageRepo(img) },
		"short digest":     func(c *container.CreateRequest) { c.Image = ImageRepo(img) + "@sha256:abc" },
		"other image":      func(c *container.CreateRequest) { c.Image = "alpine:3" },
		"look-alike image": func(c *container.CreateRequest) { c.Image = "registry.example.org/operations/opencode-runtime-evil:1" },
		"root user":        func(c *container.CreateRequest) { c.User = "0" },
		"cap add":          func(c *container.CreateRequest) { c.HostConfig.CapAdd = []string{"SYS_ADMIN"} },
		"host network":     func(c *container.CreateRequest) { c.HostConfig.NetworkMode = "host" },
		"proxy network": func(c *container.CreateRequest) {
			c.NetworkingConfig.EndpointsConfig["proxy"] = &network.EndpointSettings{}
		},
		"host pid":     func(c *container.CreateRequest) { c.HostConfig.PidMode = "host" },
		"host ipc":     func(c *container.CreateRequest) { c.HostConfig.IpcMode = "host" },
		"host uts":     func(c *container.CreateRequest) { c.HostConfig.UTSMode = "host" },
		"userns host":  func(c *container.CreateRequest) { c.HostConfig.UsernsMode = "host" },
		"legacy binds": func(c *container.CreateRequest) { c.HostConfig.Binds = []string{"/:/host"} },
		"device": func(c *container.CreateRequest) {
			c.HostConfig.Devices = []container.DeviceMapping{{PathOnHost: "/dev/sda"}}
		},
		"seccomp unconfined": func(c *container.CreateRequest) {
			c.HostConfig.SecurityOpt = append(c.HostConfig.SecurityOpt, "seccomp=unconfined")
		},
		"unmasked proc":  func(c *container.CreateRequest) { c.HostConfig.MaskedPaths = []string{} },
		"published port": func(c *container.CreateRequest) { c.HostConfig.PublishAllPorts = true },
		"bind docker sock": func(c *container.CreateRequest) {
			c.HostConfig.Mounts = append(c.HostConfig.Mounts, mount.Mount{Type: mount.TypeBind, Source: "/var/run/docker.sock", Target: "/s"})
		},
		"bind provision socket": func(c *container.CreateRequest) {
			c.HostConfig.Mounts = append(c.HostConfig.Mounts, mount.Mount{Type: mount.TypeBind, Source: "/run/oc-provision", Target: "/p"})
		},
		"bind all users": func(c *container.CreateRequest) {
			c.HostConfig.Mounts = append(c.HostConfig.Mounts, mount.Mount{Type: mount.TypeBind, Source: "/srv/opencode/users", Target: "/u"})
		},
		"bind dotdot": func(c *container.CreateRequest) {
			c.HostConfig.Mounts = append(c.HostConfig.Mounts, mount.Mount{Type: mount.TypeBind, Source: "/srv/opencode/base/../manager", Target: "/m"})
		},
		"bind prefix trick": func(c *container.CreateRequest) {
			c.HostConfig.Mounts = append(c.HostConfig.Mounts, mount.Mount{Type: mount.TypeBind, Source: "/srv/opencode/base-x/y", Target: "/m"})
		},
		"volume as host bind": func(c *container.CreateRequest) {
			c.HostConfig.Mounts = append(c.HostConfig.Mounts, mount.Mount{Type: mount.TypeVolume, Source: "v", Target: "/v",
				VolumeOptions: &mount.VolumeOptions{DriverConfig: &mount.Driver{Name: "local", Options: map[string]string{"o": "bind", "device": "/"}}}})
		},
		"unlisted volume": func(c *container.CreateRequest) {
			c.HostConfig.Mounts = append(c.HostConfig.Mounts, mount.Mount{Type: mount.TypeVolume, Source: "portainer_data", Target: "/v", ReadOnly: true})
		},
		"writable volume": func(c *container.CreateRequest) { c.HostConfig.Mounts[2].ReadOnly = false },
		"bind below a user dir (symlink)": func(c *container.CreateRequest) {
			c.HostConfig.Mounts = append(c.HostConfig.Mounts, mount.Mount{Type: mount.TypeBind,
				Source: "/srv/opencode/users/" + strings.Repeat("a", 64) + "/workspace/link", Target: "/l"})
		},
		"bind person dir": func(c *container.CreateRequest) {
			c.HostConfig.Mounts = append(c.HostConfig.Mounts, mount.Mount{Type: mount.TypeBind,
				Source: "/srv/opencode/users/" + strings.Repeat("a", 64) + "/trash", Target: "/t"})
		},
		"image mount": func(c *container.CreateRequest) {
			c.HostConfig.Mounts = append(c.HostConfig.Mounts, mount.Mount{Type: mount.TypeImage, Source: "alpine", Target: "/i"})
		},
	}
	f := &fakeEngine{}
	px := New(Policy{ImageRepo: ImageRepo(img), UsersRoot: "/srv/opencode/users",
		ReadOnlyRoots: []string{"/srv/opencode/base"}, Volumes: []string{"opencode-config"}}, "", slog.New(slog.DiscardHandler))
	px.rp.Transport = engineTransport{f}
	do := func(name string, c container.CreateRequest) int {
		b, _ := json.Marshal(c)
		w := httptest.NewRecorder()
		px.ServeHTTP(w, httptest.NewRequest("POST", "/v1.52/containers/create?name="+name, bytes.NewReader(b)))
		return w.Code
	}
	if code := do("oc-user4", valid()); code != 200 || len(f.got) != 1 || f.got[0] != "POST /v1.52/containers/create" {
		t.Fatalf("valid create: %d %v", code, f.got)
	}
	var fwd container.CreateRequest
	// D54: the exec option of /tmp passes unchanged
	if json.Unmarshal([]byte(f.body), &fwd) != nil || fwd.Image != img || len(fwd.HostConfig.Mounts) != 3 ||
		fwd.HostConfig.Tmpfs["/tmp"] != "size=1g,mode=1777,exec" ||
		fwd.HostConfig.Tmpfs["/home/user"] != "size=256m,uid=1000,gid=1000,mode=0750" {
		t.Fatalf("forwarded body %s", f.body)
	}
	if code := do("traefik", valid()); code != 403 {
		t.Fatalf("foreign name: %d", code)
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := valid()
			mutate(&c)
			if code := do("oc-user4", c); code != 403 {
				t.Errorf("%s: %d", name, code)
			}
		})
	}
	if len(f.got) != 1 {
		t.Fatalf("denied creates reached the engine: %v", f.got)
	}
	for name, mutate := range map[string]func(c *container.CreateRequest){
		"runtime host config": func(c *container.CreateRequest) {},
		"no reservation":      func(c *container.CreateRequest) { c.HostConfig.MemoryReservation = 0 },
		"large reservation":   func(c *container.CreateRequest) { c.HostConfig.MemoryReservation = 3 << 30 },
		"shm at cap":          func(c *container.CreateRequest) { c.HostConfig.ShmSize = 2 << 30 },
		"lower limits": func(c *container.CreateRequest) {
			c.HostConfig.Resources = container.Resources{Memory: 1 << 30, MemorySwap: 1 << 30, NanoCPUs: 1e9, PidsLimit: new(int64(1))}
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := valid()
			mutate(&c)
			before := len(f.got)
			if code := do("oc-user4", c); code != 200 || len(f.got) != before+1 {
				t.Fatalf("valid create: %d %v", code, f.got)
			}
		})
	}
	// a field the engine knows but this API version does not is dropped
	w := httptest.NewRecorder()
	b, _ := json.Marshal(valid())
	b = bytes.Replace(b, []byte(`"HostConfig":{`), []byte(`"HostConfig":{"FutureHostAccess":true,`), 1)
	px.ServeHTTP(w, httptest.NewRequest("POST", "/v1.52/containers/create?name=oc-user4", bytes.NewReader(b)))
	if w.Code != 200 || strings.Contains(f.body, "FutureHostAccess") {
		t.Fatalf("%d %s", w.Code, f.body)
	}
}

const toolImg = "registry.example.org/operations/opencode-tool-medien:2026.10.17.1@sha256:f6511c1c2ea3b3e7b1fecbffd5176a932fcb36ec1ea3ce6ed1dbae5afc9eec87"

// validTool is what runtime.dockerSpec sends for a tool container (TestDockerSpecTool).
func validTool() container.CreateRequest {
	pids := int64(256)
	return container.CreateRequest{
		Config: &container.Config{Image: toolImg, User: "1000:1000", Cmd: []string{"sleep", "infinity"},
			Labels: map[string]string{"oc.managed": "1", "oc.uid": strings.Repeat("a", 64), "oc.slot": "4", "oc.toolbox": "media"}},
		HostConfig: &container.HostConfig{
			ReadonlyRootfs: true, CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"},
			NetworkMode: "none", Tmpfs: map[string]string{"/tmp": "size=1g,mode=1777"},
			Resources: container.Resources{Memory: 1 << 30, MemorySwap: 1 << 30, NanoCPUs: 1e9, PidsLimit: &pids},
			Mounts:    []mount.Mount{{Type: mount.TypeBind, Source: "/srv/opencode/users/" + strings.Repeat("a", 64) + "/workspace", Target: "/workspace"}},
		},
	}
}

func TestToolCreateRules(t *testing.T) {
	cases := map[string]func(c *container.CreateRequest){
		"runtime image": func(c *container.CreateRequest) { c.Image = img },
		"tag only": func(c *container.CreateRequest) {
			c.Image = "registry.example.org/operations/opencode-tool-medien:2026.10.17.1"
		},
		"other digest":      func(c *container.CreateRequest) { c.Image = toolImg[:len(toolImg)-1] + "0" },
		"other toolbox":     func(c *container.CreateRequest) { c.Labels["oc.toolbox"] = "andere" },
		"other slot":        func(c *container.CreateRequest) { c.Labels["oc.slot"] = "5" },
		"no managed label":  func(c *container.CreateRequest) { delete(c.Labels, "oc.managed") },
		"root":              func(c *container.CreateRequest) { c.User = "0:0" },
		"writable rootfs":   func(c *container.CreateRequest) { c.HostConfig.ReadonlyRootfs = false },
		"caps kept":         func(c *container.CreateRequest) { c.HostConfig.CapDrop = nil },
		"new privileges":    func(c *container.CreateRequest) { c.HostConfig.SecurityOpt = nil },
		"no memory limit":   func(c *container.CreateRequest) { c.HostConfig.Memory, c.HostConfig.MemorySwap = 0, 0 },
		"swap":              func(c *container.CreateRequest) { c.HostConfig.MemorySwap = -1 },
		"no pids limit":     func(c *container.CreateRequest) { c.HostConfig.PidsLimit = nil },
		"too many cpus":     func(c *container.CreateRequest) { c.HostConfig.NanoCPUs = 8e9 },
		"slot network":      func(c *container.CreateRequest) { c.HostConfig.NetworkMode = "oc-u4" },
		"host network":      func(c *container.CreateRequest) { c.HostConfig.NetworkMode = "host" },
		"default bridge":    func(c *container.CreateRequest) { c.HostConfig.NetworkMode = "" },
		"share main netns":  func(c *container.CreateRequest) { c.HostConfig.NetworkMode = "container:oc-user4" },
		"privileged":        func(c *container.CreateRequest) { c.HostConfig.Privileged = true },
		"pid of main":       func(c *container.CreateRequest) { c.HostConfig.PidMode = "container:oc-user4" },
		"volumes from main": func(c *container.CreateRequest) { c.HostConfig.VolumesFrom = []string{"oc-user4"} },
		"endpoint": func(c *container.CreateRequest) {
			c.NetworkingConfig = &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{"oc-u4": {}}}
		},
		"config dir": func(c *container.CreateRequest) {
			c.HostConfig.Mounts = append(c.HostConfig.Mounts, mount.Mount{Type: mount.TypeBind, Source: "/srv/opencode/users/" + strings.Repeat("a", 64) + "/config", Target: "/c"})
		},
		"base ro": func(c *container.CreateRequest) {
			c.HostConfig.Mounts = append(c.HostConfig.Mounts, mount.Mount{Type: mount.TypeBind, Source: "/srv/opencode/base/catalog", Target: "/c", ReadOnly: true})
		},
		"other person": func(c *container.CreateRequest) {
			c.HostConfig.Mounts[0].Source = "/srv/opencode/users/" + strings.Repeat("b", 64) + "/workspace"
		},
		"workspace elsewhere": func(c *container.CreateRequest) { c.HostConfig.Mounts[0].Target = "/etc" },
		"volume": func(c *container.CreateRequest) {
			c.HostConfig.Mounts = append(c.HostConfig.Mounts, mount.Mount{Type: mount.TypeVolume, Source: "opencode-config", Target: "/x", ReadOnly: true})
		},
		"docker sock": func(c *container.CreateRequest) {
			c.HostConfig.Mounts = append(c.HostConfig.Mounts, mount.Mount{Type: mount.TypeBind, Source: "/var/run/docker.sock", Target: "/s"})
		},
	}
	f, px := setup(t)
	px.(*Proxy).ToolImages = []string{toolImg, "registry.example.org/operations/opencode-tool-medien:latest"}
	do := func(name string, c container.CreateRequest) int {
		b, _ := json.Marshal(c)
		w := httptest.NewRecorder()
		px.ServeHTTP(w, httptest.NewRequest("POST", "/v1.52/containers/create?name="+name, bytes.NewReader(b)))
		return w.Code
	}
	if code := do("oc-user4-tb-media", validTool()); code != 200 || len(f.got) != 1 {
		t.Fatalf("valid tool create: %d %v", code, f.got)
	}
	for _, name := range []string{"oc-user5-tb-media", "oc-user4-tb-andere", "oc-user4", "oc-user4-tb-", "oc-user4-tb-Media", "oc-user4-w-media"} {
		if code := do(name, validTool()); code != 403 {
			t.Errorf("name %s: %d", name, code)
		}
	}
	// a listed image without a digest is still refused
	c := validTool()
	c.Image = "registry.example.org/operations/opencode-tool-medien:latest"
	if code := do("oc-user4-tb-media", c); code != 403 {
		t.Errorf("listed tag without digest: %d", code)
	}
	for name, mutate := range cases {
		c := validTool()
		mutate(&c)
		if code := do("oc-user4-tb-media", c); code != 403 {
			t.Errorf("%s: %d", name, code)
		}
	}
	// the main container shape stays refused under a tool name and vice versa
	if code := do("oc-user4-tb-media", valid()); code != 403 {
		t.Errorf("runtime shape as tool: %d", code)
	}
	if len(f.got) != 1 {
		t.Fatalf("denied creates reached the engine: %v", f.got)
	}
	px.(*Proxy).ToolImages = nil
	if code := do("oc-user4-tb-media", validTool()); code != 403 {
		t.Errorf("no tool images configured: %d", code)
	}
}

func TestAllowlist(t *testing.T) {
	label := `?filters=` + `%7B%22label%22%3A%7B%22oc.managed%3D1%22%3Atrue%7D%7D`
	cases := []struct {
		method, path, body string
		want               int
	}{
		{"GET", "/_ping", "", 200},
		{"HEAD", "/_ping", "", 200},
		{"GET", "/v1.52/version", "", 200},
		{"GET", "/v1.52/info", "", 403},
		{"GET", "/v1.52/containers/json" + label, "", 200},
		{"GET", "/v1.52/containers/json?all=1", "", 403},
		{"GET", "/v1.52/containers/oc-user4/json", "", 200},
		{"GET", "/v1.52/containers/oc-manager/json", "", 200},
		{"GET", "/v1.52/containers/traefik/json", "", 403},
		{"POST", "/v1.52/containers/oc-user4/start", "", 200},
		{"POST", "/v1.52/containers/oc-user4/stop?t=10", "", 200},
		{"POST", "/v1.52/containers/egress/stop", "", 403},
		{"POST", "/v1.52/containers/oc-user4/kill", "", 403},
		{"POST", "/v1.52/containers/oc-user4/update", "{}", 403},
		{"GET", "/v1.52/containers/oc-user4/archive?path=/", "", 403},
		{"DELETE", "/v1.52/containers/oc-user4?force=1&v=1", "", 200},
		{"DELETE", "/v1.52/containers/oc-manager?force=1", "", 403},
		{"POST", "/v1.52/containers/oc-user4/exec", `{"User":"1000:1000","Cmd":["ls"]}`, 200},
		{"POST", "/v1.52/containers/oc-user4/exec", `{"User":"root","Cmd":["sh"]}`, 403},
		{"POST", "/v1.52/containers/oc-user4/exec", `{"User":"1000:1000","Privileged":true}`, 403},
		{"POST", "/v1.52/containers/oc-manager/exec", `{"User":"1000:1000"}`, 403},
		{"POST", "/v1.52/containers/oc-user4-tb-media/start", "", 200},
		{"POST", "/v1.52/containers/oc-user4-tb-media/exec", `{"User":"1000:1000","Cmd":["sh"]}`, 200},
		{"POST", "/v1.52/containers/oc-user4-tb-media/exec", `{"User":"root","Cmd":["sh"]}`, 403},
		{"GET", "/v1.52/containers/oc-user4-tb-media/json", "", 200},
		{"DELETE", "/v1.52/containers/oc-user4-tb-media?force=1", "", 200},
		{"POST", "/v1.52/networks/oc-u4/connect", `{"Container":"oc-user4-tb-media"}`, 403},
		{"POST", "/v1.52/containers/oc-user4-tb-x/start", "", 403},   // toolbox id too short
		{"GET", "/v1.52/containers/oc-user4-w-medien/json", "", 200}, // name before the rename, one release
		{"DELETE", "/v1.52/containers/oc-user4-w-medien?force=1", "", 200},
		{"DELETE", "/v1.52/containers/4179ce2b3591", "", 200}, // tool container by ID (D60 removes by ID)
		{"POST", "/v1.52/containers/4179ce2b3591/stop", "", 200},
		{"DELETE", "/v1.52/containers/aaaabbbbcccc", "", 403}, // foreign container by ID
		{"DELETE", "/v1.52/containers/ffffffffffff", "", 403}, // unknown ID
		{"POST", "/v1.52/exec/e4/start", "{}", 200},           // exec in a tool container
		{"POST", "/v1.52/exec/e1/start", "{}", 200},
		{"GET", "/v1.52/exec/e1/json", "", 200},
		{"POST", "/v1.52/exec/e2/start", "{}", 403}, // exec in a foreign container
		{"POST", "/v1.52/exec/e3/start", "{}", 403}, // privileged root exec
		{"POST", "/v1.52/exec/nope/start", "{}", 403},
		{"POST", "/v1.52/networks/create", `{"Name":"oc-u4","Driver":"bridge","Internal":true,"Labels":{"oc.managed":"1"},"IPAM":{"Config":[{"Subnet":"10.90.4.0/24"}]}}`, 200},
		{"POST", "/v1.52/networks/create", `{"Name":"oc-u4","Driver":"bridge","Internal":true,"Labels":{"oc.managed":"1"}}`, 403},
		{"POST", "/v1.52/networks/create", `{"Name":"oc-u4","Driver":"bridge","Internal":true,"Labels":{"oc.managed":"1"},"IPAM":{"Config":[{"Subnet":"10.90.4.0/24"}]},"Options":{"com.docker.network.bridge.gateway_mode_ipv4":"isolated"}}`, 200},
		{"POST", "/v1.52/networks/create", `{"Name":"oc-u4","Driver":"bridge","Internal":true,"Labels":{"oc.managed":"1"},"IPAM":{"Config":[{"Subnet":"10.90.4.0/24"}]},"Options":{"foreign":"isolated"}}`, 403},
		{"POST", "/v1.52/networks/create", `{"Name":"oc-u4","Driver":"bridge","Internal":true,"Labels":{"oc.managed":"1"},"IPAM":{"Config":[{"Subnet":"10.90.4.0/24"}]},"Options":{"com.docker.network.bridge.gateway_mode_ipv4":"nat"}}`, 403},
		{"POST", "/v1.52/networks/create", `{"Name":"oc-u4","Driver":"bridge","Internal":true,"Labels":{"oc.managed":"1"},"IPAM":{"Config":[{"Subnet":"10.90.4.0/24"}]},"Options":{"com.docker.network.bridge.gateway_mode_ipv4":"isolated","foreign":"value"}}`, 403},
		{"POST", "/v1.52/networks/create", `{"Name":"oc-u4","Driver":"bridge","Internal":true,"Labels":{"oc.managed":"1"},"IPAM":{"Config":[{"Subnet":"10.90.5.0/24"}]}}`, 403},
		{"POST", "/v1.52/networks/create", `{"Name":"oc-u4","Driver":"bridge","Internal":true,"Labels":{"oc.managed":"1"},"IPAM":{"Config":[{"Subnet":"10.91.4.0/24"}]}}`, 403},
		{"POST", "/v1.52/networks/create", `{"Name":"oc-u4","Driver":"bridge","Internal":true,"Labels":{"oc.managed":"1"},"IPAM":{"Config":[{"Subnet":"10.90.4.0/16"}]}}`, 403},
		{"POST", "/v1.52/networks/create", `{"Name":"oc-u4","Driver":"bridge","Internal":true,"Labels":{"oc.managed":"1"},"IPAM":{"Config":[]}}`, 403},
		{"POST", "/v1.52/networks/create", `{"Name":"oc-u4","Driver":"bridge","Internal":true,"Labels":{"oc.managed":"1"},"IPAM":{"Config":[{"Subnet":"10.90.4.0/24"},{"Subnet":"10.90.5.0/24"}]}}`, 403},
		{"POST", "/v1.52/networks/create", `{"Name":"oc-u4","Driver":"bridge","Internal":true,"Labels":{"oc.managed":"1"},"IPAM":{"Config":[{"Subnet":"10.90.4.0/24"}]},"EnableIPv6":true}`, 403},
		{"POST", "/v1.52/networks/create", `{"Name":"oc-u4","Driver":"bridge","Internal":true,"Labels":{"oc.managed":"1"},"IPAM":{"Config":[{"Subnet":"10.90.4.0/24"}]},"EnableIPv6":false}`, 200},
		{"POST", "/v1.52/networks/create", `{"Name":"oc-u4","Driver":"bridge","Internal":true,"Labels":{"oc.managed":"1"},"IPAM":{"Driver":"foreign","Config":[{"Subnet":"10.90.4.0/24"}]}}`, 403},
		{"POST", "/v1.52/networks/create", `{"Name":"oc-u4","Driver":"bridge","Internal":true,"Labels":{"oc.managed":"1"},"IPAM":{"Options":{"foreign":"value"},"Config":[{"Subnet":"10.90.4.0/24"}]}}`, 403},
		{"POST", "/v1.52/networks/create", `{"Name":"oc-u4","Driver":"bridge","Internal":true,"Labels":{"oc.managed":"1"},"IPAM":{"Config":[{"Subnet":"10.90.4.0/24","Gateway":"10.90.4.1"}]}}`, 403},
		{"POST", "/v1.52/networks/create", `{"Name":"oc-u4","Driver":"bridge","Labels":{"oc.managed":"1"}}`, 403},
		{"POST", "/v1.52/networks/create", `{"Name":"proxy","Driver":"bridge","Internal":true,"Labels":{"oc.managed":"1"}}`, 403},
		{"POST", "/v1.52/networks/create", `{"Name":"oc-u4","Driver":"macvlan","Internal":true,"Labels":{"oc.managed":"1"}}`, 403},
		{"GET", "/v1.52/networks/oc-u4", "", 200},
		{"GET", "/v1.52/networks/proxy", "", 403},
		{"DELETE", "/v1.52/networks/oc-u4", "", 200},
		{"DELETE", "/v1.52/networks/proxy", "", 403},
		{"POST", "/v1.52/networks/oc-u4/connect", `{"Container":"oc-manager"}`, 200},
		{"POST", "/v1.52/networks/oc-u4/connect", `{"Container":"oc-user4"}`, 200},
		{"POST", "/v1.52/networks/oc-u4/connect", `{"Container":"authentik-outpost"}`, 403},
		{"POST", "/v1.52/networks/proxy/connect", `{"Container":"oc-user4"}`, 403},
		{"POST", "/v1.52/networks/oc-u4/disconnect", `{"Container":"0123abc","Force":true}`, 200},
		{"POST", "/v1.52/images/create?fromImage=alpine", "", 403},
		{"POST", "/v1.52/volumes/create", `{}`, 403},
		{"GET", "/v1.52/containers/oc-user4%2F..%2Ftraefik/json", "", 403},
		{"GET", "/v1.52/containers/../secrets", "", 403},
	}
	for _, c := range cases {
		f, px := setup(t)
		w := httptest.NewRecorder()
		r := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		own := c.method + " " + r.URL.Path // the proxy's own ownership lookups are GETs of other paths
		px.ServeHTTP(w, r)
		if code := w.Code; code != c.want || (c.want == 403 && slices.Contains(f.got, own)) {
			t.Errorf("%s %s: %d (engine saw %v)", c.method, c.path+" "+c.body, w.Code, f.got)
		}
		if c.path == "/v1.52/networks/create" && c.want == 200 {
			var forwarded network.CreateRequest
			if err := json.Unmarshal([]byte(f.body), &forwarded); err != nil || forwarded.EnableIPv6 == nil || *forwarded.EnableIPv6 {
				t.Fatalf("IPv6 not pinned: %s (%v)", f.body, err)
			}
		}
	}
}

func TestImageRepo(t *testing.T) {
	for ref, want := range map[string]string{
		img:                               "registry.example.org/operations/opencode-runtime",
		"registry.example.org:5000/a/b:1": "registry.example.org:5000/a/b",
		"registry.example.org:5000/a/b":   "registry.example.org:5000/a/b",
		"alpine":                          "alpine",
	} {
		if got := ImageRepo(ref); got != want {
			t.Errorf("%s: %s", ref, got)
		}
	}
}

func TestNetworkPrefix(t *testing.T) {
	_, handler := setup(t)
	handler.(*Proxy).UserNetPrefix = "10.199"
	for subnet, want := range map[string]int{"10.199.4.0/24": 200, "10.90.4.0/24": 403} {
		body := `{"Name":"oc-u4","Driver":"bridge","Internal":true,"Labels":{"oc.managed":"1"},"IPAM":{"Config":[{"Subnet":"` + subnet + `"}]}}`
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("POST", "/networks/create", strings.NewReader(body)))
		if w.Code != want {
			t.Fatalf("%s: %d, want %d", subnet, w.Code, want)
		}
	}
}

func TestCreateConfiguredImage(t *testing.T) {
	px := New(Policy{ImageRepo: ImageRepo(img), RuntimeImage: ImageRepo(img) + ":configured", UsersRoot: "/srv/opencode/users", ReadOnlyRoots: []string{"/srv/opencode/base"}, Volumes: []string{"opencode-config"}}, "", slog.New(slog.DiscardHandler))
	for _, ref := range []string{px.RuntimeImage, img, ImageRepo(img) + "@sha256:" + strings.Repeat("a", 64)} {
		c := valid()
		c.Image = ref
		if err := px.checkCreate("oc-user4", &c); err != nil {
			t.Fatalf("%s: %v", ref, err)
		}
	}
	c := valid()
	c.Image = px.RuntimeImage + "-other"
	if err := px.checkCreate("oc-user4", &c); err == nil {
		t.Fatal("unconfigured tag accepted")
	}
}
