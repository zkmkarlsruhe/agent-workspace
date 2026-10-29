// SPDX-License-Identifier: MIT

// Command oc-provision is the root helper of D33: it creates a person's data
// directory with owner and xfs project quota. It listens only on a unix socket.
// With -sync-from it first installs the admin config baked into the image.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/logx"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/provision"
)

func main() {
	socket := flag.String("socket", "/run/oc-provision/sock", "unix socket path")
	gid := flag.Int("gid", 0, "group owning the socket (mode 0660)")
	base := flag.String("base", "/srv/opencode/users", "directory holding <uid64>/")
	mount := flag.String("mount", "/srv/opencode", "mount point of the prjquota xfs")
	syncFrom := flag.String("sync-from", "", "baked config dirs to install at start, e.g. /opt/workspace; empty = no sync")
	syncTo := flag.String("sync-to", "/srv/opencode/base", "host directory receiving each dir of -sync-from")
	flag.Parse()
	log, err := logx.Setup(os.Stdout)
	if err != nil {
		log.Error("oc-provision", "err", err)
		os.Exit(1)
	}
	if *syncFrom != "" {
		// no network, the manager is not told: OpenCode's watchers pick up etc-opencode, catalog
		// links in running containers need POST /admin/catalog/reload from the host
		changed, err := provision.SyncConfig(*syncFrom, *syncTo)
		if err != nil {
			log.Error("config sync", "err", err)
			os.Exit(1)
		}
		log.Info("config synced", "from", *syncFrom, "to", *syncTo, "replaced", changed)
	}
	if err := run(log, *socket, *gid, *base, *mount); err != nil {
		log.Error("oc-provision", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, socket string, gid int, base, mount string) error {
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	syscall.Umask(0o117) // socket is created 0660, no window with wider mode
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	if err := os.Chown(socket, 0, gid); err != nil {
		return err
	}
	p := &provision.Provisioner{Base: base, Q: provision.XFS{Mount: mount}}
	// the socket's only peer is the manager (mode 0660, its group): its request id is kept
	srv := &http.Server{Handler: logx.Middleware(log, "provision", func(*http.Request) bool { return true }, provision.Handler(p, log)), ReadHeaderTimeout: 5 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	log.Info("listening", "socket", socket, "base", base, "mount", mount)
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
