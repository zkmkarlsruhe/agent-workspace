// SPDX-License-Identifier: MIT

// Package runtime is the only package that talks to the container engine.
// Lifecycle uses the Runtime interface; Docker implements it via the Engine API.
package runtime

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"time"
)

// LabelManaged marks every container and network the manager owns.
const LabelManaged = "oc.managed"

var ErrNotFound = errors.New("runtime: not found")

type Mount struct {
	Source   string // absolute host path = bind; otherwise a named volume
	Target   string
	ReadOnly bool
}

// Spec describes one person's OpenCode container. The hardening is fixed in
// the implementation, not configurable per spec.
type Spec struct {
	Name    string
	Image   string
	Network string
	IP      netip.Addr
	Labels  map[string]string
	Env     []string
	Cmd     []string
	Mounts  []Mount
	// Tool is a person's tool container (D60): no network at all (Network and
	// IP unset), tmpfs /tmp only, smaller limits. Portable shape: in Kubernetes
	// a second container of the person's pod or an own pod on the same volume.
	Tool    bool
	Office  bool // private HTTP browser worker, no user mounts
	FreeCAD bool // visible CAD session on the person network
}

type Container struct {
	ID      string // immutable, unlike Name: a removed container's name is reused
	Name    string
	Running bool
	Labels  map[string]string
}

type Runtime interface {
	Ping(ctx context.Context) error
	// EnsureNetwork creates an internal bridge network; existing is fine.
	EnsureNetwork(ctx context.Context, name string, subnet netip.Prefix, gatewayMode string) error
	// NetworkGatewayMode returns ErrNotFound for a missing network.
	NetworkGatewayMode(ctx context.Context, name string) (string, error)
	// Connect attaches a container with a fixed address; already attached is fine.
	Connect(ctx context.Context, network, container string, ip netip.Addr, aliases ...string) error
	// RemoveNetwork disconnects every container (manager, egress) and removes
	// the network; missing is fine.
	RemoveNetwork(ctx context.Context, name string) error
	Create(ctx context.Context, s Spec) error
	Start(ctx context.Context, name string) error
	Stop(ctx context.Context, name string, timeout time.Duration) error
	// Remove force-removes by name or ID; missing is fine.
	Remove(ctx context.Context, name string) error
	Inspect(ctx context.Context, name string) (Container, error)
	// List returns all managed containers, running or not.
	List(ctx context.Context) ([]Container, error)
	// Exec runs cmd as uid 1000 inside a running container (file operations,
	// D33: the manager itself never mounts user data). stdin may be nil.
	// It returns the exit code; a failing stdin reader is returned as error.
	// A cancelled ctx closes the attach stream and returns at once; the
	// process itself runs on until it exits or the container is removed.
	Exec(ctx context.Context, name string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) (int, error)
}
