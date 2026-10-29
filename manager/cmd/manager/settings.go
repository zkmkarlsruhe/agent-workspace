// SPDX-License-Identifier: MIT

// Glue between internal/settings, the lifecycle hooks and the portal's Settings page.
package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/gateway/llm"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/portal"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/settings"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// portalSettings is set by run(); nil hides /settings.
var portalSettings *settings.Service

// newSettings wires the Settings service and hooks it into the container start
// (after any hook already set, e.g. the catalog) and the full export.
func newSettings(cfg config.Config, st *store.Store, svc *lifecycle.Service, gw *llm.Gateway, log *slog.Logger) *settings.Service {
	p := &settings.Service{Store: st, Log: log, Hidden: []string{cfg.RequiredGroup, portal.UserGroup},
		Shell: func(ctx context.Context, uid, script string, stdout io.Writer, args ...string) error {
			err := svc.Shell(ctx, uid, script, stdout, args...)
			if errors.Is(err, lifecycle.ErrNotRunning) {
				return settings.ErrNotRunning
			}
			return err
		},
	}
	if cfg.LDAPURL != "" {
		p.Dir = settings.LDAP{URL: cfg.LDAPURL, BaseDN: cfg.LDAPBaseDN, BindDN: cfg.LDAPBindDN, Password: cfg.LDAPPassword}
	}
	if gw != nil {
		p.Complete = func(ctx context.Context, uid, system, user string) (string, error) {
			u, err := st.UserByUID(ctx, uid)
			if err != nil {
				return "", err
			}
			return gw.Complete(ctx, u, system, user, 800)
		}
	}
	prev := svc.AfterStart
	svc.AfterStart = func(ctx context.Context, uid string) {
		if prev != nil {
			prev(ctx, uid)
		}
		p.AfterStart(ctx, uid)
	}
	svc.Settings = func(ctx context.Context, uid string) (string, error) {
		d, err := st.LoadSettings(ctx, uid)
		return d.Block(), err
	}
	return p
}

var _ portal.Settings = settingsPortal{}

type settingsPortal struct{ s *settings.Service }

func settingsPerson(id portal.Identity) settings.Person {
	return settings.Person{UID: id.UID, Username: id.Username, Email: id.Email, Name: id.Name, Groups: id.Groups}
}

func (p settingsPortal) Load(ctx context.Context, id portal.Identity) (settings.Record, error) {
	return p.s.Refresh(ctx, settingsPerson(id))
}

func (p settingsPortal) Suggest(ctx context.Context, id portal.Identity, a settings.Answers) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return p.s.Suggest(ctx, id.UID, a)
}

func (p settingsPortal) Save(ctx context.Context, id portal.Identity, a settings.Answers, text string) error {
	return p.s.Save(ctx, id.UID, a, text)
}

// settingsFor avoids a typed nil in the portal config.
func settingsFor(s *settings.Service) portal.Settings {
	if s == nil {
		return nil
	}
	return settingsPortal{s}
}

// prefsFor rewrites the settings block after a language switch: the block is
// written in the portal language. Without settings or prefs it is pr as is.
func prefsFor(pr portal.Prefs, p *settings.Service) portal.Prefs {
	if pr == nil || p == nil {
		return pr
	}
	return settingsPrefs{pr, p}
}

type settingsPrefs struct {
	portal.Prefs
	p *settings.Service
}

func (x settingsPrefs) SetLang(ctx context.Context, uid, l string) error {
	old, _ := x.Prefs.Lang(ctx, uid)
	if err := x.Prefs.SetLang(ctx, uid, l); err != nil {
		return err
	}
	// only a real change: without an account the store keeps "" and nothing is written
	if cur, _ := x.Prefs.Lang(ctx, uid); cur == l && old != l {
		x.p.AfterStart(ctx, uid)
	}
	return nil
}
