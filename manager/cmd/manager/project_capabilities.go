// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"slices"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/catalog"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/portal"
)

func (t tools) ProjectCapabilities(ctx context.Context, uid, folder string) ([]portal.Tool, error) {
	if !t.c.ProjectEnabled() {
		return nil, nil
	}
	available, err := t.c.Available(ctx, uid)
	if err != nil {
		return nil, err
	}
	var ids []string
	if folder == "" {
		ids, err = t.c.Selection(ctx, uid, nil)
	} else {
		p, e := t.c.Projects.ProjectByFolder(ctx, uid, folder)
		if e != nil {
			return nil, toolErr(e)
		}
		if p.Assistant.ID != "" {
			return nil, nil
		}
		var active []catalog.Item
		active, err = t.c.ProjectItems(ctx, uid, p)
		for _, it := range active {
			ids = append(ids, it.ID)
		}
	}
	if err != nil {
		return nil, toolErr(err)
	}
	out := []portal.Tool{}
	for _, it := range available {
		out = append(out, portal.Tool{ID: it.ID, Title: it.TitleDE, TitleEN: it.Title,
			Description: it.DescriptionDE, DescriptionEN: it.Description, On: slices.Contains(ids, it.ID)})
	}
	return out, nil
}

func (t tools) SelectCapabilities(ctx context.Context, uid string, ids []string) ([]string, error) {
	if !t.c.ProjectEnabled() {
		return nil, nil
	}
	selected, err := t.c.Selection(ctx, uid, ids)
	return selected, toolErr(err)
}

func (t tools) SetProjectCapability(ctx context.Context, uid, folder, id string, on bool) (portal.ToolEffect, error) {
	choices, err := t.ProjectCapabilities(ctx, uid, folder)
	if err != nil {
		return 0, err
	}
	found := false
	ids := []string{}
	for _, it := range choices {
		if it.ID == id {
			it.On, found = on, true
		}
		if it.On {
			ids = append(ids, it.ID)
		}
	}
	if !found {
		return 0, portal.ErrNotFound
	}
	effect, err := t.c.SetProject(ctx, uid, folder, ids)
	return portal.ToolEffect(effect), toolErr(err)
}
