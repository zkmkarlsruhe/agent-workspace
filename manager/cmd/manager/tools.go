// SPDX-License-Identifier: MIT

// Glue between internal/catalog and the portal's Tools page.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/catalog"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/portal"
)

// catalogReload serves POST /admin/catalog/reload (admin listener, after a
// catalog rsync): {"reloaded": idle environments done, "pending": busy ones,
// retried every reap tick}.
func catalogReload(cat *catalog.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		now, later := cat.ReloadAll(r.Context())
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]int{"reloaded": now, "pending": later})
	}
}

// portalTools is set by run() when OC_CATALOG_DIR is configured; nil hides /tools.
var portalTools portal.Tools

var _ portal.Tools = tools{}

type tools struct{ c *catalog.Service }

func toolErr(err error) error {
	switch {
	case errors.Is(err, catalog.ErrUnknown), errors.Is(err, catalog.ErrNotFound):
		return portal.ErrNotFound
	case errors.Is(err, catalog.ErrNotRunning):
		return portal.ErrNotRunning
	}
	return mapErr(err)
}

func (t tools) Tools(ctx context.Context, uid string) (portal.ToolsPage, error) {
	d := portal.ToolsPage{ProjectScoped: t.c.ProjectEnabled()}
	if !t.c.Enabled() {
		return d, nil
	}
	on, err := t.c.On(ctx, uid)
	if err != nil {
		return d, err
	}
	std, own, err := t.c.Skills(ctx, uid)
	switch {
	case errors.Is(err, catalog.ErrNotRunning):
	case err != nil:
		return d, toolErr(err)
	default:
		d.Running = true
	}
	for _, s := range std {
		d.Standard = append(d.Standard, portal.Tool{ID: s.ID, Title: s.TitleDE, Description: s.DescriptionDE,
			TitleEN: s.Title, DescriptionEN: s.Description})
	}
	for _, it := range t.c.Items() {
		tl := portal.Tool{Remote: it.Kind == catalog.KindCentral, ID: it.ID, Title: it.TitleDE, Description: it.DescriptionDE, Category: it.Category,
			TitleEN: it.Title, DescriptionEN: it.Description, On: on[it.ID]}
		if it.Tier == catalog.TierStandard {
			d.Standard = append(d.Standard, tl)
		} else {
			d.Catalog = append(d.Catalog, tl)
		}
	}
	slices.SortStableFunc(d.Catalog, func(a, b portal.Tool) int { return cmp.Compare(a.Category, b.Category) })
	for _, s := range own {
		d.Own = append(d.Own, portal.OwnSkill{ID: s.ID, Description: s.DescriptionDE, Changed: s.Changed})
	}
	return d, nil
}

func (t tools) SetTool(ctx context.Context, uid, id string, on bool) (portal.ToolEffect, error) {
	eff, err := t.c.Set(ctx, uid, id, on)
	return portal.ToolEffect(eff), toolErr(err)
}

func (t tools) DeleteSkill(ctx context.Context, uid, id string) (portal.ToolEffect, error) {
	eff, err := t.c.DeleteOwn(ctx, uid, id)
	return portal.ToolEffect(eff), toolErr(err)
}
