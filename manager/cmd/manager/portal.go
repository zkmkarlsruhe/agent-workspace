// SPDX-License-Identifier: MIT

// Glue between lifecycle.Service and the portal package.
package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/portal"
	"github.com/zkmkarlsruhe/agent-workspace/manager/web"
)

var portalIdentityGuard portal.IdentityGuard

func init() {
	newPortal = func(svc *lifecycle.Service, cfg config.Config, pdf pdfFunc) (http.Handler, error) {
		pc := portal.Config{Capabilities: map[string]bool{
			"web": cfg.SearXNGURL != "", "rooms": cfg.RoomsURL != nil && cfg.BikesURL != nil,
			"image_view": cfg.VisionModel != "", "image_generate": len(cfg.ImageModels) > 0,
			"image_edit":  cfg.ImageEditModel != "",
			"mcp-central": cfg.MCPUpstreamURL != "",
		}, LogoutURL: cfg.LogoutURL, UploadMaxBytes: cfg.UploadMaxBytes, TaskAssistants: portalTaskAssistants, Backend: backend{svc}, Assets: web.FS, Origin: "https://" + cfg.Domain, PDF: pdf, Tools: portalTools,
			Connections: portalConns, Onboarding: portalOnboarding, Sync: portalSync, Consent: portalConsent, Group: cfg.RequiredGroup,
			DayCapEUR: cfg.CostCapDayEUR, IdleTimeout: cfg.IdleTimeout, MaxRunning: cfg.MaxRunning, DayStart: config.DayStart, Spent: portalSpent, Settings: settingsFor(portalSettings),
			IdentityGuard: portalIdentityGuard, Admin: portalAdmin, AdminGroup: cfg.AdminGroup, Prefs: prefsFor(portalPrefs, portalSettings), Feedback: portalFeedback}
		pc.Knowledge = portalKnowledge
		pc.KnowledgeReadOnly = !instance.Current().Enabled("knowledge")
		if !instance.Current().Enabled("feedback") {
			pc.Feedback = nil
		}
		if !instance.Current().Enabled("evaluation") {
			pc.Admin = nil
		}
		if !instance.Current().Enabled("pdf") {
			pc.PDF = nil
		}
		if !instance.Current().Enabled("nextcloud") {
			pc.Connections, pc.Sync = nil, nil
		}
		h, err := portal.New(pc)
		if err != nil {
			return nil, err
		}
		// one header parser and trust check (identity); the portal reads its own context key
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if id, ok := identity.FromContext(r.Context()); ok {
				pid := portal.Identity{Slug: id.Slug, Blocked: id.Blocked, UID: id.UID, Username: id.Username, Email: id.Email, Name: id.Name, Groups: id.Groups}
				r = r.WithContext(portal.WithIdentity(r.Context(), pid))
				if !id.Blocked && portalSettings != nil && (cfg.RequiredGroup == "" || slices.Contains(id.Groups, cfg.RequiredGroup)) {
					portalSettings.Seen(settingsPerson(pid)) // facts refresh on login, at most daily
				}
			}
			h.ServeHTTP(w, r)
		}), nil
	}
}

var _ portal.Backend = backend{}

// backend adapts lifecycle.Service (same method set, own types) to portal.Backend.
type backend struct{ s *lifecycle.Service }

func mapErr(err error) error {
	for _, m := range []struct{ from, to error }{
		{lifecycle.ErrNotFound, portal.ErrNotFound}, {lifecycle.ErrInvalid, portal.ErrNotFound},
		{lifecycle.ErrExists, portal.ErrExists}, {lifecycle.ErrQuota, portal.ErrQuota},
		{lifecycle.ErrBusy, portal.ErrBusy}, {lifecycle.ErrNotRunning, portal.ErrNotRunning},
		{lifecycle.ErrUnavailable, portal.ErrUnavailable}, {lifecycle.ErrFull, portal.ErrFull},
	} {
		if errors.Is(err, m.from) {
			return m.to
		}
	}
	return err
}

func project(p lifecycle.Project) portal.Project {
	out := portal.Project{Isolated: p.Isolated, Assistant: p.Assistant, Name: p.Name, Description: p.Description, Created: p.Created, LastActivity: p.LastActivity,
		Status: p.Status, Chats: p.Chats, SizeBytes: p.SizeBytes, CostEUR: p.CostEUR, CO2MinG: p.CO2MinG, CO2MaxG: p.CO2MaxG,
		TrashAt: p.TrashAt}
	for _, c := range p.Recent {
		out.Recent = append(out.Recent, portal.Chat(c))
	}
	return out
}

func (b backend) ListProjects(ctx context.Context, uid string) ([]portal.Project, error) {
	ps, err := b.s.ListProjects(ctx, uid)
	out := make([]portal.Project, len(ps))
	for i, p := range ps {
		out[i] = project(p)
	}
	return out, mapErr(err)
}

func (b backend) Project(ctx context.Context, uid, name string) (portal.ProjectDetail, error) {
	d, err := b.s.Project(ctx, uid, name)
	out := portal.ProjectDetail{Project: project(d.Project)}
	for _, c := range d.ChatList {
		out.ChatList = append(out.ChatList, portal.Chat(c))
	}
	for _, c := range d.ArchivedChats {
		out.ArchivedChats = append(out.ArchivedChats, portal.Chat(c))
	}
	return out, mapErr(err)
}

func (b backend) RestoreSession(ctx context.Context, uid, project, session string) error {
	return mapErr(b.s.RestoreSession(ctx, uid, project, session))
}

func (b backend) ProjectExists(ctx context.Context, uid, name string) (bool, error) {
	ok, err := b.s.ProjectExists(ctx, uid, name)
	return ok, mapErr(err)
}

func (b backend) SuggestName(ctx context.Context, uid, description string) (string, error) {
	return b.s.SuggestName(ctx, uid, description)
}

func (b backend) CreateProject(ctx context.Context, uid string, p portal.NewProject) error {
	return mapErr(b.s.CreateProject(ctx, uid, lifecycle.NewProject(p)))
}

func (b backend) EnvStatus(ctx context.Context, uid string) (portal.EnvStatus, error) {
	st, err := b.s.EnvStatus(ctx, uid)
	return portal.EnvStatus{State: portal.EnvState(st.State), Since: st.Since, DiskUsed: st.DiskUsed,
		DiskQuota: st.DiskQuota, ErrorID: st.ErrorID, ActiveRuns: st.ActiveRuns}, mapErr(err)
}

func (b backend) EnvStart(ctx context.Context, uid string) error {
	return mapErr(b.s.EnvStart(ctx, uid))
}
func (b backend) EnvStop(ctx context.Context, uid string) error { return mapErr(b.s.EnvStop(ctx, uid)) }
func (b backend) EnvRestart(ctx context.Context, uid string) error {
	return mapErr(b.s.EnvRestart(ctx, uid))
}
func (b backend) EnvReset(ctx context.Context, uid string) error {
	return mapErr(b.s.EnvReset(ctx, uid))
}

func (b backend) CreateSession(ctx context.Context, uid, project, prompt string) (string, error) {
	link, err := b.s.CreateSession(ctx, uid, project, prompt)
	return link, mapErr(err)
}

func (b backend) Tree(ctx context.Context, uid, project string) ([]portal.File, bool, error) {
	fs, truncated, err := b.s.Tree(ctx, uid, project)
	out := make([]portal.File, len(fs))
	for i, f := range fs {
		out[i] = portal.File(f)
	}
	return out, truncated, mapErr(err)
}

func (b backend) OpenFile(ctx context.Context, uid, project, path string) (io.ReadCloser, portal.File, error) {
	rc, f, err := b.s.OpenFile(ctx, uid, project, path)
	return rc, portal.File(f), mapErr(err)
}

func (b backend) WriteZip(ctx context.Context, uid, project, part string, w io.Writer) error {
	return mapErr(b.s.WriteZip(ctx, uid, project, part, w))
}

func (b backend) Upload(ctx context.Context, uid, project, filename string, r io.Reader) (string, error) {
	name, err := b.s.Upload(ctx, uid, project, filename, r)
	return name, mapErr(err)
}

func (b backend) MoveToTrash(ctx context.Context, uid, project string) error {
	return mapErr(b.s.MoveToTrash(ctx, uid, project))
}

func (b backend) RenameProject(ctx context.Context, uid, project, newName string) error {
	return mapErr(b.s.RenameProject(ctx, uid, project, newName))
}

func (b backend) ListTrash(ctx context.Context, uid string) ([]portal.TrashItem, error) {
	ts, err := b.s.ListTrash(ctx, uid)
	out := make([]portal.TrashItem, len(ts))
	for i, t := range ts {
		out[i] = portal.TrashItem(t)
	}
	return out, mapErr(err)
}

func (b backend) Restore(ctx context.Context, uid, id, newName string) error {
	return mapErr(b.s.Restore(ctx, uid, id, newName))
}

func (b backend) Purge(ctx context.Context, uid, id string) error {
	return mapErr(b.s.Purge(ctx, uid, id))
}

func (b backend) Export(ctx context.Context, uid, project string, w io.Writer) error {
	return mapErr(b.s.Export(ctx, uid, project, w))
}

func (b backend) DeleteAccount(ctx context.Context, uid string) (string, error) {
	slug, err := b.s.DeleteAccount(ctx, uid)
	return slug, mapErr(err)
}

func (b backend) Usage(ctx context.Context, uid string, since time.Time) ([]portal.UsageRow, error) {
	rows, err := b.s.Usage(ctx, uid, since)
	out := make([]portal.UsageRow, len(rows))
	for i, r := range rows {
		out[i] = portal.UsageRow(r)
	}
	return out, mapErr(err)
}

func (b backend) Watchdog(ctx context.Context, uid, project string) ([]portal.WatchdogNotice, error) {
	notices, err := b.s.Watchdog(ctx, uid, project)
	out := make([]portal.WatchdogNotice, len(notices))
	for i, n := range notices {
		out[i] = portal.WatchdogNotice(n)
	}
	return out, mapErr(err)
}

func (b backend) StopSilentSession(ctx context.Context, uid, project, session string) error {
	return mapErr(b.s.StopSilentSession(ctx, uid, project, session))
}

func (b backend) PrepareAssistant(ctx context.Context, uid, initPrompt, question, projectMode string) (string, string, error) {
	return b.s.PrepareAssistant(ctx, uid, initPrompt, question, projectMode)
}
func (b backend) CreateAssistantProject(ctx context.Context, uid string, groups []string, p portal.NewProject, assistant, hash string) error {
	return mapErr(b.s.CreateAssistantProject(ctx, uid, groups, lifecycle.NewProject(p), assistant, hash))
}
