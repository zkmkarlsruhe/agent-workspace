// SPDX-License-Identifier: MIT

// Command oc-dockerproxy is the only holder of /var/run/docker.sock in the
// stack: it serves the filtered Engine API (internal/dockerproxy) on a unix
// socket owned by the manager's remapped root. Configuration: env vars, see
// docs/INSTALL.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/dockerproxy"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/logx"
)

func main() {
	log, err := logx.Setup(os.Stdout)
	if err == nil {
		err = run(log)
	}
	if err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func run(log *slog.Logger) error {
	img := env("OC_RUNTIME_IMAGE", "")
	if img == "" {
		return errors.New("OC_RUNTIME_IMAGE is required")
	}
	var uid, gid int
	// 100000 = container root of the manager under userns-remap (D44)
	if _, err := fmt.Sscanf(env("OC_PROXY_SOCKET_OWNER", "100000:100000"), "%d:%d", &uid, &gid); err != nil {
		return fmt.Errorf("OC_PROXY_SOCKET_OWNER: %w", err)
	}
	pol := dockerproxy.Policy{
		ImageRepo:     dockerproxy.ImageRepo(img),
		RuntimeImage:  img,
		UserNetPrefix: env("OC_USER_NET_PREFIX", "10.90"),
		UsersRoot:     env("OC_USERS_ROOT", "/srv/opencode/users"),
		ReadOnlyRoots: strings.Split(env("OC_PROXY_RO_ROOTS", "/srv/opencode/base"), ","),
		Volumes:       strings.FieldsFunc(env("OC_PROXY_VOLUMES", ""), func(r rune) bool { return r == ',' }),
		Joinable:      []string{env("OC_MANAGER_CONTAINER", "oc-manager"), env("OC_EGRESS_CONTAINER", "egress")},
		// D60: exact digest-pinned tool images (catalog kind werkzeug), comma-separated
		ToolImages: strings.FieldsFunc(env("OC_PROXY_TOOL_IMAGES", ""), func(r rune) bool { return r == ',' }),
	}
	sock := env("OC_PROXY_SOCKET", "/run/oc-docker/sock")
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	syscall.Umask(0o177) // socket is created 0600, then handed to the manager
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	if err := os.Chown(sock, uid, gid); err != nil {
		return err
	}
	px := dockerproxy.New(pol, env("OC_PROXY_ENGINE", "/var/run/docker.sock"), log)
	// the socket's only peer is the manager: its request id is kept
	srv := &http.Server{Handler: logx.Middleware(log, "dockerproxy", func(*http.Request) bool { return true }, px), ReadHeaderTimeout: 10 * time.Second} // no WriteTimeout: exec streams

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	log.Info("listening", "socket", sock, "image_repo", pol.ImageRepo, "users_root", pol.UsersRoot, "ro_roots", pol.ReadOnlyRoots, "joinable", pol.Joinable, "tool_images", pol.ToolImages)
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
