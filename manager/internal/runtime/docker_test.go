// SPDX-License-Identifier: MIT

package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

func TestDockerSpecHardening(t *testing.T) {
	cfg, hc, nc := dockerSpec(Spec{
		Name: "oc-user1", Image: "img", Network: "oc-u1", IP: netip.MustParseAddr("10.90.1.10"),
		Mounts: []Mount{{Source: "/srv/x/workspace", Target: "/workspace"}, {Source: "opencode-config", Target: "/etc/opencode", ReadOnly: true}},
	})
	if cfg.User != "1000:1000" || !hc.ReadonlyRootfs || !slices.Equal(hc.CapDrop, []string{"ALL"}) ||
		!slices.Contains(hc.SecurityOpt, "no-new-privileges:true") {
		t.Fatal("hardening missing")
	}
	if hc.Memory != 2<<30 || hc.MemorySwap != hc.Memory || hc.NanoCPUs != 2e9 || *hc.PidsLimit != 512 {
		t.Fatal("limits")
	}
	if hc.Tmpfs["/home/user"] == "" || !strings.HasSuffix(hc.Tmpfs["/tmp"], ",exec") {
		t.Fatal("tmpfs")
	}
	if hc.Mounts[0].Type != mount.TypeBind || hc.Mounts[1].Type != mount.TypeVolume || !hc.Mounts[1].ReadOnly {
		t.Fatalf("mounts %+v", hc.Mounts)
	}
	if nc.EndpointsConfig["oc-u1"].IPAMConfig.IPv4Address.String() != "10.90.1.10" || string(hc.NetworkMode) != "oc-u1" {
		t.Fatal("network")
	}
}

func TestDockerSpecTool(t *testing.T) {
	cfg, hc, nc := dockerSpec(Spec{Name: "oc-user1-w-medien", Image: "img", Tool: true,
		Mounts: []Mount{{Source: "/srv/x/workspace", Target: "/workspace"}}})
	if cfg.User != "1000:1000" || !hc.ReadonlyRootfs || !slices.Equal(hc.CapDrop, []string{"ALL"}) ||
		!slices.Contains(hc.SecurityOpt, "no-new-privileges:true") {
		t.Fatal("hardening missing")
	}
	if string(hc.NetworkMode) != "none" || nc != nil || len(hc.Tmpfs) != 1 || hc.Tmpfs["/tmp"] == "" {
		t.Fatalf("network %q %v tmpfs %v", hc.NetworkMode, nc, hc.Tmpfs)
	}
	if hc.Memory != 1<<30 || hc.MemorySwap != hc.Memory || hc.NanoCPUs != 1e9 || *hc.PidsLimit != 256 {
		t.Fatal("limits")
	}
}

// TestDockerLocalTool runs a tool container (Spec.Tool) against the local
// engine when OC_DOCKER_IT_TOOL_IMAGE names a built tool image.
func TestDockerLocalTool(t *testing.T) {
	img := os.Getenv("OC_DOCKER_IT_TOOL_IMAGE")
	if img == "" {
		t.Skip("set OC_DOCKER_IT_TOOL_IMAGE to run a tool container on the local docker engine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	d, err := NewDocker()
	if err != nil {
		t.Fatal(err)
	}
	name := "oc-it-tool"
	d.Remove(ctx, name)
	defer d.Remove(context.Background(), name)
	var mounts []Mount
	if ws := os.Getenv("OC_DOCKER_IT_TOOL_WS"); ws != "" { // host dir writable by uid 1000
		mounts = []Mount{{Source: ws, Target: "/workspace"}}
	}
	if err := d.Create(ctx, Spec{Name: name, Image: img, Tool: true, Labels: map[string]string{LabelManaged: "1"},
		Cmd: []string{"sleep", "infinity"}, Mounts: mounts}); err != nil {
		t.Fatal(err)
	}
	if err := d.Start(ctx, name); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	script := `id -u; touch /etc/x 2>/dev/null && echo rootfs-writable; ls /sys/class/net; ` +
		`magick -size 8x8 xc:red /tmp/a.png && exiftool -s3 -ImageSize /tmp/a.png; ` +
		`[ ! -d /workspace ] || (cd /workspace && ffmpeg -hide_banner -loglevel error -f lavfi -i testsrc=d=1:s=64x48 -y it.mp4 && ls it.mp4)`
	code, err := d.Exec(ctx, name, []string{"sh", "-c", script}, nil, &out, &out)
	got := out.String()
	if err != nil || code != 0 || !strings.HasPrefix(got, "1000\n") || strings.Contains(got, "rootfs-writable") ||
		strings.Contains(got, "eth0") || !strings.Contains(got, "8x8") {
		t.Fatalf("exec %d %v: %q", code, err, got)
	}
	t.Logf("%s", got)
}

// TestDockerLocal runs against the local engine only when OC_DOCKER_IT=1.
func TestDockerLocal(t *testing.T) {
	if os.Getenv("OC_DOCKER_IT") != "1" {
		t.Skip("set OC_DOCKER_IT=1 to run against the local docker engine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	d, err := NewDocker()
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	net, name := "oc-it-net", "oc-it-ctr"
	sub := netip.MustParsePrefix("10.199.7.0/24")
	d.Remove(ctx, name)
	defer d.c.NetworkRemove(context.Background(), net, client.NetworkRemoveOptions{})
	defer d.Remove(context.Background(), name)
	for range 2 {
		if err := d.EnsureNetwork(ctx, net, sub, "isolated"); err != nil {
			t.Fatal(err)
		}
	}
	img := os.Getenv("OC_DOCKER_IT_IMAGE")
	if img == "" {
		img = "golang:1.27-alpine"
	}
	err = d.Create(ctx, Spec{Name: name, Image: img, Network: net, IP: netip.MustParseAddr("10.199.7.10"),
		Labels: map[string]string{LabelManaged: "1", "oc.uid": "x"}, Cmd: []string{"sleep", "300"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Start(ctx, name); err != nil {
		t.Fatal(err)
	}
	c, err := d.Inspect(ctx, name)
	if err != nil || !c.Running || c.Labels["oc.uid"] != "x" {
		t.Fatal(c, err)
	}
	if err := d.Connect(ctx, net, name, netip.Addr{}); err != nil { // already attached: no-op
		t.Fatal(err)
	}
	l, err := d.List(ctx)
	if err != nil || !slices.ContainsFunc(l, func(c Container) bool { return c.Name == name && c.Running }) {
		t.Fatal(l, err)
	}
	for range 2 { // first with the running container attached, then missing
		if err := d.RemoveNetwork(ctx, net); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Stop(ctx, name, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := d.Remove(ctx, name); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Inspect(ctx, name); err != ErrNotFound {
		t.Fatal(err)
	}
}

func TestEnsureNetworkGatewayMode(t *testing.T) {
	for _, mode := range []string{"isolated", ""} {
		t.Run(mode, func(t *testing.T) {
			var created network.CreateRequest
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == "GET" {
					w.WriteHeader(http.StatusNotFound)
					fmt.Fprint(w, `{"message":"network missing"}`)
					return
				}
				if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
					t.Error(err)
				}
				w.WriteHeader(http.StatusCreated)
				fmt.Fprint(w, `{"Id":"network-id"}`)
			}))
			defer up.Close()
			c, err := client.New(client.WithHost(up.URL), client.WithAPIVersion("1.56"))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			d := &Docker{c: c}
			subnet := netip.MustParsePrefix("10.90.4.0/24")
			if err := d.EnsureNetwork(context.Background(), "oc-u4", subnet, mode); err != nil {
				t.Fatal(err)
			}
			if !created.Internal || created.EnableIPv6 == nil || *created.EnableIPv6 || created.Options["com.docker.network.bridge.gateway_mode_ipv4"] != mode ||
				created.IPAM == nil || len(created.IPAM.Config) != 1 || created.IPAM.Config[0].Subnet != subnet {
				t.Fatalf("network create: %+v", created)
			}
			if mode == "" && len(created.Options) != 0 {
				t.Fatalf("legacy options: %v", created.Options)
			}
		})
	}
}
