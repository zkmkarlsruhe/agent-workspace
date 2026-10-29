// SPDX-License-Identifier: MIT

package dockerproxy

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/runtime"
)

// TestRuntimeToolSpec sends the real runtime create of a tool container
// (runtime.dockerSpec, Spec.Tool) through the proxy to the fake engine: the
// shape the manager sends must pass the tool rules.
func TestRuntimeToolSpec(t *testing.T) {
	f, px := setup(t)
	px.(*Proxy).ToolImages = []string{toolImg}
	dir, _ := os.MkdirTemp("", "dp")
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	go http.Serve(ln, px)
	t.Setenv("DOCKER_HOST", "unix://"+sock)
	d, err := runtime.NewDocker()
	if err != nil {
		t.Fatal(err)
	}
	uid := strings.Repeat("a", 64)
	err = d.Create(context.Background(), runtime.Spec{Name: "oc-user4-tb-media", Image: toolImg, Tool: true,
		Labels: map[string]string{runtime.LabelManaged: "1", "oc.uid": uid, "oc.slot": "4", "oc.toolbox": "media"},
		Cmd:    []string{"sleep", "infinity"}, Mounts: []runtime.Mount{{Source: "/srv/opencode/users/" + uid + "/workspace", Target: "/workspace"}}})
	if err != nil || !strings.HasSuffix(f.got[len(f.got)-1], "/containers/create") {
		t.Fatalf("tool create through the proxy: %v %v", err, f.got)
	}
}

func TestRuntimeOfficeSpec(t *testing.T) {
	f, px := setup(t)
	c := officeCreate()
	px.(*Proxy).OfficeImage = c.Image
	dir, err := os.MkdirTemp("", "dp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: px}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	t.Setenv("DOCKER_HOST", "unix://"+sock)
	d, err := runtime.NewDocker()
	if err != nil {
		t.Fatal(err)
	}
	// Exercise the real Office dockerSpec, not just the hand-built policy fixture.
	err = d.Create(context.Background(), runtime.Spec{
		Name: "oc-user4-office", Image: c.Image, Office: true,
		Network: "oc-u4", IP: netip.MustParseAddr("10.90.4.11"),
		Labels: c.Labels, Cmd: c.Cmd, Env: c.Env,
	})
	if err != nil || len(f.got) == 0 || !strings.HasSuffix(f.got[len(f.got)-1], "/containers/create") {
		t.Fatalf("office create through the proxy: %v %v", err, f.got)
	}
}

// TestLive drives the real runtime code through the proxy against the local
// engine, only with OC_DOCKER_IT=1 (socket at /var/run/docker.sock).
// OC_DOCKER_IT_PROXY=<socket> uses a running oc-dockerproxy instead (the
// compose service, started with OC_RUNTIME_IMAGE=golang:1.27-alpine).
func TestLive(t *testing.T) {
	if os.Getenv("OC_DOCKER_IT") != "1" {
		t.Skip("set OC_DOCKER_IT=1 to run against the local docker engine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	img := "golang:1.27-alpine"
	sock := os.Getenv("OC_DOCKER_IT_PROXY")
	if sock == "" {
		dir, _ := os.MkdirTemp("", "dp")
		defer os.RemoveAll(dir)
		sock = filepath.Join(dir, "s")
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		px := New(Policy{ImageRepo: ImageRepo(img), RuntimeImage: img, UserNetPrefix: "10.199", UsersRoot: "/srv/opencode/users", Joinable: []string{"oc-manager"}},
			"/var/run/docker.sock", slog.Default())
		go http.Serve(ln, px)
	}
	t.Setenv("DOCKER_HOST", "unix://"+sock)
	d, err := runtime.NewDocker()
	if err != nil {
		t.Fatal(err)
	}
	direct, err := client.New(client.WithHost("unix:///var/run/docker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	nw, name, vol, uid := "oc-u249", "oc-user249", "oc-it-proxy-ws", strings.Repeat("0", 64)
	defer direct.VolumeRemove(context.Background(), vol, client.VolumeRemoveOptions{Force: true})
	defer d.RemoveNetwork(context.Background(), nw)
	defer d.Remove(context.Background(), name)
	if err := d.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureNetwork(ctx, nw, netip.MustParsePrefix("10.199.249.0/24"), "isolated"); err != nil {
		t.Fatal(err)
	}
	err = d.Create(ctx, runtime.Spec{Name: name, Image: img, Network: nw, IP: netip.MustParseAddr("10.199.249.10"),
		Labels: map[string]string{runtime.LabelManaged: "1", "oc.uid": uid, "oc.slot": "249"}, Cmd: []string{"sleep", "300"},
		Mounts: []runtime.Mount{{Source: vol, Target: "/workspace"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Start(ctx, name); err != nil {
		t.Fatal(err)
	}
	if c, err := d.Inspect(ctx, name); err != nil || !c.Running {
		t.Fatal(c, err)
	}
	if l, err := d.List(ctx); err != nil || len(l) == 0 {
		t.Fatal(l, err)
	}
	// stdin through the hijacked exec stream, half-close included
	var out bytes.Buffer
	code, err := d.Exec(ctx, name, []string{"sh", "-c", "cat; id -u"}, strings.NewReader("hallo\n"), &out, nil)
	if err != nil || code != 0 || out.String() != "hallo\n1000\n" {
		t.Fatalf("exec %d %v %q", code, err, out.String())
	}

	// forbidden through the proxy: privileged create, root exec, unfiltered list
	cl, _ := client.New(client.WithHost("unix://" + sock))
	_, err = cl.ContainerCreate(ctx, client.ContainerCreateOptions{Name: "oc-user248",
		Config:     &container.Config{Image: img, User: "1000:1000", Labels: map[string]string{"oc.managed": "1", "oc.uid": uid, "oc.slot": "248"}},
		HostConfig: &container.HostConfig{Privileged: true, NetworkMode: container.NetworkMode(nw)}})
	if err == nil || !strings.Contains(err.Error(), "Privileged not allowed") {
		t.Fatalf("privileged create: %v", err)
	}
	_, err = cl.ExecCreate(ctx, name, client.ExecCreateOptions{User: "root", Cmd: []string{"id"}})
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("root exec: %v", err)
	}
	if _, err := cl.ContainerList(ctx, client.ContainerListOptions{All: true}); err == nil {
		t.Fatal("unfiltered list passed")
	}

	if err := d.Stop(ctx, name, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := d.Remove(ctx, name); err != nil {
		t.Fatal(err)
	}
	if err := d.RemoveNetwork(ctx, nw); err != nil {
		t.Fatal(err)
	}
}
