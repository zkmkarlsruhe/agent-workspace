// SPDX-License-Identifier: MIT

package dockerproxy

import (
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/moby/moby/api/types/container"
)

func (px *Proxy) checkOfficeCreate(slot string, c *container.CreateRequest) error {
	if px.OfficeImage == "" || c.Config == nil || c.HostConfig == nil || c.Image != px.OfficeImage || !pinnedRE.MatchString(c.Image) {
		return errors.New("office image not allowed")
	}
	if c.Labels["oc.office"] != "nextcloud" || c.Labels["oc.generation"] == "" {
		return errors.New("office labels required")
	}
	if _, err := strconv.ParseUint(c.Labels["oc.generation"], 10, 64); err != nil {
		return errors.New("invalid generation")
	}
	if len(c.HostConfig.Mounts) != 0 || len(c.Volumes) != 0 {
		return errors.New("office workers cannot mount user data")
	}
	if len(c.HostConfig.Tmpfs) != 1 || c.HostConfig.Tmpfs["/tmp"] != "size=1g,mode=1777" {
		return errors.New("office workers require the private /tmp tmpfs")
	}
	if c.WorkingDir != "/tmp" || len(c.Cmd) != 2 || c.Cmd[0] != "node" || c.Cmd[1] != "/app/src/worker.mjs" || len(c.Entrypoint) != 0 || c.Healthcheck != nil {
		return errors.New("invalid office command")
	}
	if c.StopSignal != "" && c.StopSignal != "SIGTERM" {
		return errors.New("office workers require graceful shutdown")
	}
	// Preload variables and duplicate keys can replace the checked command or its configuration.
	expected := map[string]string{
		"BROWSER_BRIDGE_NEXTCLOUD_URL":      "",
		"BROWSER_BRIDGE_PROXY_URL":          "http://egress:3128",
		"BROWSER_BRIDGE_AUTH_FILE":          "/tmp/bridge-secrets/nextcloud.json",
		"BROWSER_BRIDGE_HTTP_TOKEN_FILE":    "/tmp/bridge-secrets/service.json",
		"BROWSER_BRIDGE_HTTP_HOST":          "0.0.0.0",
		"BROWSER_BRIDGE_HTTP_PORT":          "8080",
		"BROWSER_BRIDGE_HTTP_ALLOWED_HOSTS": px.UserNetPrefix + "." + slot + ".11",
	}
	for _, entry := range c.Env {
		key, value, ok := strings.Cut(entry, "=")
		want, allowed := expected[key]
		if !ok || !allowed {
			return errors.New("unexpected or duplicate office environment variable")
		}
		if key == "BROWSER_BRIDGE_NEXTCLOUD_URL" {
			u, err := url.Parse(value)
			if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
				return errors.New("invalid office Nextcloud URL")
			}
		} else if value != want {
			return errors.New("invalid office environment value")
		}
		delete(expected, key)
	}
	if len(expected) != 0 {
		return errors.New("office environment incomplete")
	}
	// Reuse the user-container hardening without permitting its image or mounts.
	policy := *px
	policy.ImageRepo, policy.RuntimeImage = ImageRepo(px.OfficeImage), px.OfficeImage
	if err := policy.checkCreate("oc-user"+slot, c); err != nil {
		return err
	}
	if c.NetworkingConfig == nil || len(c.NetworkingConfig.EndpointsConfig) != 1 {
		return errors.New("office network required")
	}
	endpoint := c.NetworkingConfig.EndpointsConfig["oc-u"+slot]
	if endpoint == nil || endpoint.IPAMConfig == nil || endpoint.IPAMConfig.IPv4Address.String() != px.UserNetPrefix+"."+slot+".11" {
		return errors.New("invalid office address")
	}
	return nil
}
