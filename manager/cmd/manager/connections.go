// SPDX-License-Identifier: MIT

// Glue between internal/house and the portal's Connections and Onboarding.
package main

import (
	"context"
	"errors"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/gateway/mcp"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/house"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/portal"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// portalConns is set by run() when a house service is configured; nil hides /connections.
var portalConns portal.Connections

// portalOnboarding is set by run(); nil = no Onboarding.
var portalOnboarding portal.Onboarding

var (
	_ portal.Connections = conns{}
	_ portal.Onboarding  = onboarding{}
)

type conns struct{ h *house.Service }

func houseErr(err error) error {
	switch {
	case errors.Is(err, house.ErrUnknown):
		return portal.ErrNotFound
	case errors.Is(err, house.ErrUnreachable):
		return portal.ErrUnavailable
	}
	return err
}

var flowStates = map[house.FlowState]portal.FlowState{
	house.FlowPending: portal.FlowPending, house.FlowDone: portal.FlowDone, house.FlowMismatch: portal.FlowMismatch,
	house.FlowFailed: portal.FlowFailed, house.FlowTimeout: portal.FlowTimeout,
}

func (c conns) List(ctx context.Context, uid string) ([]portal.Connection, error) {
	sts, err := c.h.Status(ctx, uid)
	out := make([]portal.Connection, len(sts))
	for i, s := range sts {
		out[i] = portal.Connection{ID: s.Service.ID, Title: s.Service.Title, Connected: s.Connected, Expired: s.Expired,
			Login: s.Login, Since: s.Since,
			Flow: portal.Flow{State: flowStates[s.Flow.State], LoginURL: s.Flow.LoginURL, Ended: s.Flow.Ended}}
	}
	return out, err
}

func (c conns) Connect(ctx context.Context, uid, username, id string) (string, error) {
	link, err := c.h.Connect(ctx, uid, username, id)
	return link, houseErr(err)
}

func (c conns) Disconnect(ctx context.Context, uid, id string) error {
	return houseErr(c.h.Disconnect(ctx, uid, id))
}

type onboarding struct{ st *store.Store }

func (o onboarding) Seen(ctx context.Context, uid string) (bool, error) {
	return o.st.OnboardingSeen(ctx, uid)
}
func (o onboarding) MarkSeen(ctx context.Context, uid string) error {
	return o.st.MarkOnboardingSeen(ctx, uid, time.Now())
}

// ncTools serves the Workspace-MCP Nextcloud tools from the person's stored login.
func ncTools(hs *house.Service, nc config.HouseService, files mcp.NCFiles) *mcp.Nextcloud {
	return &mcp.Nextcloud{Base: nc.URL, Files: files,
		Creds: func(ctx context.Context, uid string) (string, string, string, error) {
			return hs.Creds(ctx, uid, nc.ID)
		},
		Expired: func(ctx context.Context, uid string) { hs.MarkExpired(ctx, uid, nc.ID) },
	}
}
