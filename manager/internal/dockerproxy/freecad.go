// SPDX-License-Identifier: MIT

package dockerproxy

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
)

func (px *Proxy) checkFreeCADCreate(slot string, c *container.CreateRequest) error {
	if c.Config == nil || c.HostConfig == nil || px.FreeCADImage == "" || c.Image != px.FreeCADImage || !pinnedRE.MatchString(c.Image) {
		return errors.New("FreeCAD image not allowed")
	}
	if c.Labels["oc.freecad"] != "1" || !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(c.Labels["oc.session"]) || !regexp.MustCompile(`^[a-f0-9-]{36}$`).MatchString(c.Labels["oc.project"]) {
		return errors.New("FreeCAD session and project labels required")
	}
	if _, err := strconv.ParseUint(c.Labels["oc.generation"], 10, 64); err != nil {
		return errors.New("invalid generation")
	}
	if len(c.Cmd) != 0 || len(c.Entrypoint) != 0 || c.Healthcheck != nil || c.WorkingDir != "/config" || len(c.Volumes) != 0 || (c.StopSignal != "" && c.StopSignal != "SIGTERM") {
		return errors.New("invalid FreeCAD command")
	}
	h := c.HostConfig
	if len(h.Mounts) != 1 {
		return errors.New("FreeCAD requires the person's workspace")
	}
	m := h.Mounts[0]
	if m.Type != mount.TypeBind || m.Source != px.UsersRoot+"/"+c.Labels["oc.uid"]+"/workspace" || m.Target != "/workspace" || m.ReadOnly || m.BindOptions != nil {
		return errors.New("invalid FreeCAD mount")
	}
	want := map[string]string{"/tmp": "size=512m,mode=1777", "/run": "size=256m,uid=1000,gid=1000,mode=0755,exec", "/config": "size=256m,uid=1000,gid=1000,mode=0750"}
	if len(h.Tmpfs) != len(want) {
		return errors.New("invalid FreeCAD temporary paths")
	}
	for k, v := range want {
		if h.Tmpfs[k] != v {
			return errors.New("invalid FreeCAD temporary path")
		}
	}
	if h.Init == nil || !*h.Init || h.ShmSize != 1<<30 || h.Resources.Memory != 4<<30 || h.Resources.MemorySwap != 4<<30 {
		return errors.New("invalid FreeCAD limits")
	}
	env := map[string]string{}
	for _, pair := range c.Env {
		k, v, ok := strings.Cut(pair, "=")
		if !ok || env[k] != "" {
			return errors.New("invalid FreeCAD environment")
		}
		env[k] = v
	}
	if len(env) != 3 || env["WORKSPACE_FREECAD_SESSION"] != c.Labels["oc.session"] || env["WORKSPACE_FREECAD_PROJECT_ID"] != c.Labels["oc.project"] || !regexp.MustCompile(`^/workspace/[a-z0-9]+(-[a-z0-9]+)*$`).MatchString(env["WORKSPACE_FREECAD_PROJECT"]) || len(env["WORKSPACE_FREECAD_PROJECT"]) > 61 {
		return errors.New("invalid FreeCAD project environment")
	}
	if c.NetworkingConfig == nil || len(c.NetworkingConfig.EndpointsConfig) != 1 {
		return errors.New("FreeCAD network required")
	}
	ep := c.NetworkingConfig.EndpointsConfig["oc-u"+slot]
	if ep == nil || ep.IPAMConfig == nil || ep.IPAMConfig.IPv4Address.String() != px.UserNetPrefix+"."+slot+".12" {
		return errors.New("invalid FreeCAD address")
	}
	// Apply the shared floor after checking the one intentional larger memory limit.
	copyRequest, copyHost := *c, *h
	copyHost.Resources.Memory, copyHost.Resources.MemorySwap = maxMem, maxMem
	copyRequest.HostConfig = &copyHost
	policy := *px
	policy.ImageRepo, policy.RuntimeImage = ImageRepo(px.FreeCADImage), px.FreeCADImage
	return policy.checkCreate("oc-user"+slot, &copyRequest)
}
