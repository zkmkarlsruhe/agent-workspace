// SPDX-License-Identifier: MIT

// Glue between internal/evaluation, llm-gw and the portal's /admin area.
package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/evaluation"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/gateway/llm"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/portal"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// portalAdmin is set by run(); nil = no /admin.
var portalAdmin portal.Admin

// newEvaluation meters the model calls as manager usage: no person pays for
// them and they do not count against anyone's daily cap.
func newEvaluation(st *store.Store, gw *llm.Gateway, log *slog.Logger) *evaluation.Service {
	a := &evaluation.Service{Store: st, Log: log, Now: time.Now, DayStart: config.DayStart}
	if gw != nil {
		manager := store.User{UID: store.ManagerUID, Username: "agents-evaluation"}
		a.Complete = func(ctx context.Context, system, user string) (string, error) {
			return gw.Complete(ctx, manager, system, user, 700)
		}
	}
	return a
}
