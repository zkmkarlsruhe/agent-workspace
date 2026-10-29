// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/consent"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/gateway/llm"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/portal"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// portalConsent is set by run(); the router refuses everything without it.
var portalConsent portal.Consent

// portalPrefs keeps the portal language per person (users.lang); set by run().
var portalPrefs portal.Prefs

// portalSpent is the budget gate's sum (store.SpentSince) for the Usage page; set by run().
var portalSpent func(ctx context.Context, uid string, since time.Time) (float64, error)

var _ portal.Consent = consentStore{}

type consentStore struct{ st *store.Store }

func (c consentStore) Accepted(ctx context.Context, uid string) (time.Time, bool, error) {
	v, at, err := c.st.Consent(ctx, uid)
	if errors.Is(err, store.ErrNotFound) {
		return time.Time{}, false, nil
	}
	return at, err == nil && v == consent.Version(), err
}

func (c consentStore) Accept(ctx context.Context, uid string) error {
	slog.InfoContext(ctx, "consent accepted", "uid", uid, "version", consent.Version())
	return c.st.AcceptConsent(ctx, uid, consent.Version(), time.Now())
}

// gwGate guards the whole llm-gw listener (LLM routes and Workspace-MCP).
func gwGate(cfg config.Config, st *store.Store, b *llm.Budget, next http.Handler) http.Handler {
	return &llm.Gate{RuntimeAccess: st.SharedRuntimeAccess, SlotOf: cfg.SlotOf, Users: st, Consented: consented(st), Budget: b, Portal: "https://" + cfg.Domain, Next: next}
}

// budget is the one daily cap of a person: llm-gw and the manager's calls on their behalf.
func budget(cfg config.Config, st *store.Store) *llm.Budget {
	return &llm.Budget{Accounting: &cfg.Accounting, Spent: st.SpentSince, CapEUR: cfg.CostCapDayEUR, Now: time.Now}
}

// consented is the check of router and llm-gw: accepted the current version.
func consented(st *store.Store) func(ctx context.Context, uid string) (bool, error) {
	return func(ctx context.Context, uid string) (bool, error) {
		ok, err := st.HasConsent(ctx, uid, consent.Version())
		slog.DebugContext(ctx, "consent check", "uid", uid, "version", consent.Version(), "consented", ok, "err", err)
		return ok, err
	}
}
