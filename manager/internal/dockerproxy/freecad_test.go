// SPDX-License-Identifier: MIT

package dockerproxy

import (
	"bytes"
	"encoding/json"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func cadCreate() container.CreateRequest {
	c := valid()
	c.Image = "cad@sha256:" + strings.Repeat("b", 64)
	c.WorkingDir = "/config"
	c.Labels["oc.freecad"] = "1"
	c.Labels["oc.generation"] = "1"
	c.Labels["oc.session"] = strings.Repeat("a", 32)
	c.Labels["oc.project"] = "11111111-1111-4111-8111-111111111111"
	c.Env = []string{"WORKSPACE_FREECAD_SESSION=" + c.Labels["oc.session"], "WORKSPACE_FREECAD_PROJECT_ID=" + c.Labels["oc.project"], "WORKSPACE_FREECAD_PROJECT=/workspace/demo"}
	c.HostConfig.Mounts = c.HostConfig.Mounts[:1]
	c.HostConfig.Tmpfs = map[string]string{"/tmp": "size=512m,mode=1777", "/run": "size=256m,uid=1000,gid=1000,mode=0755,exec", "/config": "size=256m,uid=1000,gid=1000,mode=0750"}
	c.HostConfig.Init = new(true)
	c.HostConfig.ShmSize = 1 << 30
	c.HostConfig.Memory = 4 << 30
	c.HostConfig.MemorySwap = 4 << 30
	c.NetworkingConfig = &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{"oc-u4": {IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: netip.MustParseAddr("10.90.4.12")}}}}
	return c
}
func TestFreeCADPolicy(t *testing.T) {
	for name, mutate := range map[string]func(*container.CreateRequest){
		"valid": func(c *container.CreateRequest) {},
		"foreign workspace": func(c *container.CreateRequest) {
			c.HostConfig.Mounts[0].Source = "/srv/opencode/users/" + strings.Repeat("b", 64) + "/workspace"
		},
		"nested bind":     func(c *container.CreateRequest) { c.HostConfig.Mounts[0].Source += "/demo" },
		"mutable image":   func(c *container.CreateRequest) { c.Image = "cad:latest" },
		"privileged":      func(c *container.CreateRequest) { c.HostConfig.Privileged = true },
		"root":            func(c *container.CreateRequest) { c.User = "0" },
		"command":         func(c *container.CreateRequest) { c.Cmd = []string{"sh"} },
		"entrypoint":      func(c *container.CreateRequest) { c.Entrypoint = []string{"sh"} },
		"preload":         func(c *container.CreateRequest) { c.Env = append(c.Env, "PYTHONPATH=/workspace") },
		"escape path":     func(c *container.CreateRequest) { c.Env[2] = "WORKSPACE_FREECAD_PROJECT=/workspace/../demo" },
		"foreign session": func(c *container.CreateRequest) { c.Labels["oc.session"] = strings.Repeat("b", 32) },
		"duplicate env":   func(c *container.CreateRequest) { c.Env = append(c.Env, c.Env[0]) },
		"kill":            func(c *container.CreateRequest) { c.StopSignal = "SIGKILL" },
		"host network":    func(c *container.CreateRequest) { c.HostConfig.NetworkMode = "host" },
		"foreign address": func(c *container.CreateRequest) {
			c.NetworkingConfig.EndpointsConfig["oc-u4"].IPAMConfig.IPv4Address = netip.MustParseAddr("10.90.5.12")
		},
		"code tmpfs": func(c *container.CreateRequest) { c.HostConfig.Tmpfs["/opt"] = "size=1m" },
	} {
		t.Run(name, func(t *testing.T) {
			c := cadCreate()
			engine, h := setup(t)
			h.(*Proxy).FreeCADImage = c.Image
			mutate(&c)
			b, _ := json.Marshal(c)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("POST", "/containers/create?name=oc-user4-freecad", bytes.NewReader(b)))
			if name == "valid" {
				if w.Code >= 400 || len(engine.got) != 1 {
					t.Fatalf("rejected: %d %s", w.Code, w.Body)
				}
			} else if w.Code != 403 || len(engine.got) != 0 {
				t.Fatalf("escaped: %d %v", w.Code, engine.got)
			}
		})
	}
}
