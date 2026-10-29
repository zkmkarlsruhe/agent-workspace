// SPDX-License-Identifier: MIT

package runtime

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/logx"
)

// Docker talks to the Docker Engine API (DOCKER_HOST, default /var/run/docker.sock).
// Under userns-remap all user containers share one remapped range; uid 1000
// inside is host uid 101000, the owner of /srv/opencode/users/<uid>/*.
type Docker struct{ c *client.Client }

func NewDocker() (*Docker, error) {
	c, err := client.New(client.FromEnv,
		// oc-dockerproxy logs its allow/deny decisions under the caller's request id
		client.WithHTTPRequestHook(func(r *http.Request) error {
			if id := logx.ReqID(r.Context()); id != "" {
				r.Header.Set(logx.HeaderReqID, id)
			}
			return nil
		}),
		client.WithHTTPResponseHook(func(resp *http.Response) {
			if resp.Request != nil {
				slog.DebugContext(resp.Request.Context(), "upstream", "upstream", "docker", "method", resp.Request.Method,
					"path", resp.Request.URL.Path, "status", resp.StatusCode)
			}
		}))
	if err != nil {
		return nil, err
	}
	return &Docker{c: c}, nil
}

func (d *Docker) Ping(ctx context.Context) error {
	_, err := d.c.Ping(ctx, client.PingOptions{})
	return err
}

func (d *Docker) EnsureNetwork(ctx context.Context, name string, subnet netip.Prefix, gatewayMode string) error {
	_, err := d.c.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	if err == nil {
		return nil
	}
	if !cerrdefs.IsNotFound(err) {
		return err
	}
	var options map[string]string
	if gatewayMode != "" {
		options = map[string]string{"com.docker.network.bridge.gateway_mode_ipv4": gatewayMode}
	}
	_, err = d.c.NetworkCreate(ctx, name, client.NetworkCreateOptions{
		Options:    options,
		EnableIPv6: new(false),
		Driver:     "bridge",
		Internal:   true, // no route out; egress only via squid on the same network
		IPAM:       &network.IPAM{Config: []network.IPAMConfig{{Subnet: subnet}}},
		Labels:     map[string]string{LabelManaged: "1"},
	})
	if cerrdefs.IsConflict(err) {
		return nil
	}
	return err
}

func (d *Docker) NetworkGatewayMode(ctx context.Context, name string) (string, error) {
	r, err := d.c.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return r.Network.Options["com.docker.network.bridge.gateway_mode_ipv4"], nil
}

func (d *Docker) RemoveNetwork(ctx context.Context, name string) error {
	r, err := d.c.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for id := range r.Network.Containers {
		_, err := d.c.NetworkDisconnect(ctx, name, client.NetworkDisconnectOptions{Container: id, Force: true})
		if err != nil && !cerrdefs.IsNotFound(err) {
			return err
		}
	}
	_, err = d.c.NetworkRemove(ctx, name, client.NetworkRemoveOptions{})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func (d *Docker) Connect(ctx context.Context, net, ctr string, ip netip.Addr, aliases ...string) error {
	r, err := d.c.ContainerInspect(ctx, ctr, client.ContainerInspectOptions{})
	if err != nil {
		return err
	}
	if ns := r.Container.NetworkSettings; ns != nil && ns.Networks[net] != nil {
		return nil
	}
	_, err = d.c.NetworkConnect(ctx, net, client.NetworkConnectOptions{
		Container: ctr,
		EndpointConfig: &network.EndpointSettings{
			IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: ip},
			Aliases:    aliases,
		},
	})
	return err
}

func (d *Docker) Create(ctx context.Context, s Spec) error {
	cfg, hc, nc := dockerSpec(s)
	_, err := d.c.ContainerCreate(ctx, client.ContainerCreateOptions{Name: s.Name, Config: cfg, HostConfig: hc, NetworkingConfig: nc})
	return err
}

// dockerSpec carries the hardening of the phase-1 compose (gitops oc-user1).
func dockerSpec(s Spec) (*container.Config, *container.HostConfig, *network.NetworkingConfig) {
	stop := 10
	pids := int64(512)
	cfg := &container.Config{
		Image:       s.Image,
		User:        "1000:1000",
		Env:         s.Env,
		Cmd:         s.Cmd,
		Labels:      s.Labels,
		StopTimeout: &stop,
	}
	hc := &container.HostConfig{
		ReadonlyRootfs: true,
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges:true"},
		Tmpfs: map[string]string{
			"/tmp":       "size=1g,mode=1777,exec", // D54: Docker's tmpfs default is noexec; the file watcher needs exec
			"/home/user": "size=256m,uid=1000,gid=1000,mode=0750",
		},
		NetworkMode: container.NetworkMode(s.Network),
		Resources: container.Resources{
			Memory:            2 << 30,
			MemorySwap:        2 << 30, // no swap
			MemoryReservation: 256 << 20,
			NanoCPUs:          2e9,
			PidsLimit:         &pids,
		},
	}
	for _, m := range s.Mounts {
		t := mount.TypeVolume
		if strings.HasPrefix(m.Source, "/") {
			t = mount.TypeBind // a missing host dir fails the create instead of being created root-owned
		}
		hc.Mounts = append(hc.Mounts, mount.Mount{Type: t, Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
	}
	nc := &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
		s.Network: {IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: s.IP}},
	}}
	if s.Tool {
		toolPids := int64(256)
		hc.NetworkMode, nc = "none", nil
		hc.Tmpfs = map[string]string{"/tmp": "size=1g,mode=1777"}
		hc.Resources = container.Resources{Memory: 1 << 30, MemorySwap: 1 << 30, NanoCPUs: 1e9, PidsLimit: &toolPids}
	}
	return cfg, hc, nc
}

func (d *Docker) Start(ctx context.Context, name string) error {
	_, err := d.c.ContainerStart(ctx, name, client.ContainerStartOptions{})
	return err
}

func (d *Docker) Stop(ctx context.Context, name string, timeout time.Duration) error {
	secs := int(timeout.Seconds())
	_, err := d.c.ContainerStop(ctx, name, client.ContainerStopOptions{Timeout: &secs})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func (d *Docker) Remove(ctx context.Context, name string) error {
	_, err := d.c.ContainerRemove(ctx, name, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func (d *Docker) Inspect(ctx context.Context, name string) (Container, error) {
	r, err := d.c.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return Container{}, ErrNotFound
	}
	if err != nil {
		return Container{}, err
	}
	c := Container{ID: r.Container.ID, Name: strings.TrimPrefix(r.Container.Name, "/")}
	if r.Container.State != nil {
		c.Running = r.Container.State.Running
	}
	if r.Container.Config != nil {
		c.Labels = r.Container.Config.Labels
	}
	return c, nil
}

func (d *Docker) Exec(ctx context.Context, name string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	cr, err := d.c.ExecCreate(ctx, name, client.ExecCreateOptions{
		User: "1000:1000", Cmd: cmd, WorkingDir: "/workspace",
		AttachStdin: stdin != nil, AttachStdout: true, AttachStderr: true,
	})
	if err != nil {
		return -1, err
	}
	ar, err := d.c.ExecAttach(ctx, cr.ID, client.ExecAttachOptions{})
	if err != nil {
		return -1, err
	}
	defer ar.Close()
	// the hijacked stream ignores ctx: closing it is what ends StdCopy
	defer context.AfterFunc(ctx, func() { ar.Close() })()
	in := make(chan error, 1)
	if stdin != nil {
		go func() {
			_, err := io.Copy(ar.Conn, stdin)
			ar.CloseWrite()
			in <- err
		}()
	} else {
		in <- nil
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if _, err := stdcopy.StdCopy(stdout, stderr, ar.Reader); err != nil {
		if ctx.Err() != nil {
			return -1, ctx.Err()
		}
		return -1, err
	}
	ar.Close() // unblocks a stdin copy if the process exited early
	stdinErr := <-in
	ins, err := d.c.ExecInspect(ctx, cr.ID, client.ExecInspectOptions{})
	if err != nil {
		return -1, err
	}
	if ins.ExitCode == 0 && stdinErr != nil {
		return -1, stdinErr
	}
	return ins.ExitCode, nil
}

func (d *Docker) List(ctx context.Context) ([]Container, error) {
	r, err := d.c.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", LabelManaged+"=1"),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(r.Items))
	for _, it := range r.Items {
		name := ""
		if len(it.Names) > 0 {
			name = strings.TrimPrefix(it.Names[0], "/")
		}
		out = append(out, Container{ID: it.ID, Name: name, Running: it.State == container.StateRunning, Labels: it.Labels})
	}
	return out, nil
}
