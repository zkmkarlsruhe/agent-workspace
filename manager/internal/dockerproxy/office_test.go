// SPDX-License-Identifier: MIT

package dockerproxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
)

func officeCreate() container.CreateRequest {
	c := valid()
	c.Image = "bridge@sha256:" + strings.Repeat("a", 64)
	c.WorkingDir = "/tmp"
	c.Cmd = []string{"node", "/app/src/worker.mjs"}
	c.Env = []string{
		"BROWSER_BRIDGE_NEXTCLOUD_URL=https://cloud.example/nextcloud",
		"BROWSER_BRIDGE_PROXY_URL=http://egress:3128",
		"BROWSER_BRIDGE_AUTH_FILE=/tmp/bridge-secrets/nextcloud.json",
		"BROWSER_BRIDGE_HTTP_TOKEN_FILE=/tmp/bridge-secrets/service.json",
		"BROWSER_BRIDGE_HTTP_HOST=0.0.0.0", "BROWSER_BRIDGE_HTTP_PORT=8080",
		"BROWSER_BRIDGE_HTTP_ALLOWED_HOSTS=10.90.4.11",
	}
	c.Labels["oc.office"] = "nextcloud"
	c.Labels["oc.generation"] = "1"
	c.HostConfig.Mounts = nil
	c.HostConfig.Tmpfs = map[string]string{"/tmp": "size=1g,mode=1777"}
	c.NetworkingConfig = &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{"oc-u4": {IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: netip.MustParseAddr("10.90.4.11")}}}}
	return c
}
func TestOfficePolicyAllowsOnlyPrivateWorkerSpec(t *testing.T) {
	c := officeCreate()
	p := &Proxy{Policy: Policy{OfficeImage: c.Image, UserNetPrefix: "10.90"}}
	if err := p.checkOfficeCreate("4", &c); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*container.CreateRequest){
		"image": func(c *container.CreateRequest) { c.Image = "bridge:latest" },
		"mount": func(c *container.CreateRequest) {
			c.HostConfig.Mounts = []mount.Mount{{Type: mount.TypeBind, Source: "/tmp", Target: "/data"}}
		},
		"network": func(c *container.CreateRequest) { c.HostConfig.NetworkMode = "host" },
		"address": func(c *container.CreateRequest) {
			c.NetworkingConfig.EndpointsConfig["oc-u4"].IPAMConfig.IPv4Address = netip.MustParseAddr("10.90.4.10")
		},
		"command":    func(c *container.CreateRequest) { c.Cmd = []string{"sh"} },
		"entrypoint": func(c *container.CreateRequest) { c.Entrypoint = []string{"sh", "-c", "echo marker; exit 0", "--"} },
		"preload": func(c *container.CreateRequest) {
			c.Env = append(c.Env, "NODE_OPTIONS=--import=data:text/javascript,process.exit(0)")
		},
		"healthcheck command": func(c *container.CreateRequest) {
			c.Healthcheck = &container.HealthConfig{Test: []string{"CMD-SHELL", "echo marker"}}
		},
		"code tmpfs":                func(c *container.CreateRequest) { c.HostConfig.Tmpfs["/app/src"] = "size=1m" },
		"missing tmpfs":             func(c *container.CreateRequest) { c.HostConfig.Tmpfs = nil },
		"tmpfs options":             func(c *container.CreateRequest) { c.HostConfig.Tmpfs["/tmp"] = "size=1g,mode=0777,exec" },
		"anonymous volume":          func(c *container.CreateRequest) { c.Volumes = map[string]struct{}{"/tmp/bridge-secrets": {}} },
		"kill signal":               func(c *container.CreateRequest) { c.StopSignal = "SIGKILL" },
		"duplicate environment":     func(c *container.CreateRequest) { c.Env = append(c.Env, c.Env[0]) },
		"missing environment":       func(c *container.CreateRequest) { c.Env = c.Env[1:] },
		"malformed environment":     func(c *container.CreateRequest) { c.Env[0] = "BROWSER_BRIDGE_NEXTCLOUD_URL" },
		"alternate proxy":           func(c *container.CreateRequest) { c.Env[1] = "BROWSER_BRIDGE_PROXY_URL=http://attacker.example:3128" },
		"alternate credential path": func(c *container.CreateRequest) { c.Env[2] = "BROWSER_BRIDGE_AUTH_FILE=/app/secrets.json" },
		"foreign allowed host":      func(c *container.CreateRequest) { c.Env[6] = "BROWSER_BRIDGE_HTTP_ALLOWED_HOSTS=10.90.5.11" },
		"generation":                func(c *container.CreateRequest) { delete(c.Labels, "oc.generation") },
		"privilege":                 func(c *container.CreateRequest) { c.HostConfig.Privileged = true },
	} {
		t.Run(name, func(t *testing.T) {
			c := officeCreate()
			mutate(&c)
			engine, handler := setup(t)
			handler.(*Proxy).OfficeImage = p.OfficeImage
			body, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest("POST", "/containers/create?name=oc-user4-office", bytes.NewReader(body)))
			if w.Code != http.StatusForbidden || len(engine.got) != 0 {
				t.Fatalf("unsafe worker reached engine: status=%d calls=%v", w.Code, engine.got)
			}
		})
	}
	p.OfficeImage = ""
	if p.checkOfficeCreate("4", &c) == nil {
		t.Fatal("unconfigured worker enabled")
	}
}

func TestOfficeNextcloudURLPolicy(t *testing.T) {
	for _, raw := range []string{"http://cloud.example", "https://user:password@cloud.example", "https://cloud.example/?token=value", "https://cloud.example/?", "https://cloud.example/#fragment", "https:///path", "https:cloud.example"} {
		t.Run(raw, func(t *testing.T) {
			c := officeCreate()
			c.Env[0] = "BROWSER_BRIDGE_NEXTCLOUD_URL=" + raw
			p := &Proxy{Policy: Policy{OfficeImage: c.Image, UserNetPrefix: "10.90"}}
			if p.checkOfficeCreate("4", &c) == nil {
				t.Fatal("unsafe Nextcloud URL accepted")
			}
		})
	}
	c := officeCreate()
	c.StopSignal = "SIGTERM"
	p := &Proxy{Policy: Policy{OfficeImage: c.Image, UserNetPrefix: "10.90"}}
	if err := p.checkOfficeCreate("4", &c); err != nil {
		t.Fatal("explicit graceful stop rejected", err)
	}
}
