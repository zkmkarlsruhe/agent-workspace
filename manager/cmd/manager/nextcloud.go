// SPDX-License-Identifier: MIT

// Glue between internal/ncsync, the portal's project page and the Workspace-MCP.
package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/house"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ncsync"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/portal"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// portalSync is set by run() when Nextcloud is configured; nil hides the project section.
var portalSync portal.Sync

var _ portal.Sync = syncPortal{}

func newSync(hs *house.Service, nc config.HouseService, st *store.Store, svc *lifecycle.Service, log *slog.Logger) *ncsync.Service {
	return &ncsync.Service{DAV: ncsync.NewDAV(nc.URL), Store: st, Files: svc, Log: log,
		Creds: func(ctx context.Context, uid string) (ncsync.Creds, error) {
			user, login, pw, err := hs.Creds(ctx, uid, nc.ID)
			return ncsync.Creds{User: user, Login: login, Password: pw}, err
		},
		Expired:   func(ctx context.Context, uid string) { hs.MarkExpired(ctx, uid, nc.ID) },
		Consented: consented(st),
	}
}

type syncPortal struct{ s *ncsync.Service }

func syncErr(err error) error {
	switch {
	case errors.Is(err, ncsync.ErrNotConnected):
		return portal.ErrNotConnected
	case errors.Is(err, ncsync.ErrNotFolder), errors.Is(err, lifecycle.ErrNotFound):
		return portal.ErrNotFound
	}
	return mapErr(err)
}

func (a syncPortal) Connected(ctx context.Context, uid string) bool {
	_, err := a.s.Creds(ctx, uid)
	return err == nil
}

func (a syncPortal) Status(ctx context.Context, uid, project string) (portal.SyncStatus, error) {
	st, err := a.s.Status(ctx, uid, project)
	out := portal.SyncStatus{Connected: st.Connected, Linked: st.Linked, All: st.All, Folder: st.Folder,
		URL: st.URL, Last: st.LastSync, Notice: st.Note}
	for _, c := range st.Collections {
		out.Collections = append(out.Collections, portal.Collection{Path: c.Path, URL: c.URL})
	}
	return out, syncErr(err)
}

func (a syncPortal) Link(ctx context.Context, uid, project string, all bool) error {
	return syncErr(a.s.Link(ctx, uid, project, all))
}

func (a syncPortal) Unlink(ctx context.Context, uid, project string) error {
	return syncErr(a.s.Unlink(ctx, uid, project))
}

func (a syncPortal) Pull(ctx context.Context, uid, project string) (portal.PullResult, error) {
	r, err := a.s.Pull(ctx, uid, project, "")
	return portal.PullResult{Updated: r.Updated, New: r.New, Conflicts: r.Conflicts}, syncErr(err)
}

func (a syncPortal) AddCollection(ctx context.Context, uid, project, path string) error {
	return syncErr(a.s.AddCollection(ctx, uid, project, path))
}

func (a syncPortal) RemoveCollection(ctx context.Context, uid, project, path string) error {
	return syncErr(a.s.RemoveCollection(ctx, uid, project, path))
}
