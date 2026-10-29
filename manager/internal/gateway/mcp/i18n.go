// SPDX-License-Identifier: MIT

package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
)

// English is the source language of every text the agent sees; it doubles as
// the key of the German catalog (i18n_de.go). The language is the one the
// person chose in the portal (users.lang); anything but "en" gets German.
type langKey struct{}

func withLang(ctx context.Context, l string) context.Context {
	return context.WithValue(ctx, langKey{}, l)
}

func langOf(ctx context.Context) string { l, _ := ctx.Value(langKey{}).(string); return l }

func english(ctx context.Context) bool { return langOf(ctx) == "en" }

// msg translates an English text for the person, then fills in args (if any).
// A missing entry stays English; TestCatalogGo keeps that from happening.
func msg(ctx context.Context, en string, args ...any) string {
	s := en
	if !english(ctx) {
		if d := de[en]; d != "" {
			s = d
		}
	}
	if len(args) > 0 {
		return fmt.Sprintf(instance.Current().Text(s, false, true), args...)
	}
	return instance.Text(s)
}

// localize copies tool definitions with title and description texts in the
// person's language; names, argument names and enum values stay as they are.
func localize(ctx context.Context, tools []map[string]any) []map[string]any {
	out := make([]map[string]any, len(tools))
	for i, t := range tools {
		out[i] = localizeMap(ctx, t)
	}
	return out
}

func localizeMap(ctx context.Context, m map[string]any) map[string]any {
	c := make(map[string]any, len(m))
	for k, v := range m {
		switch x := v.(type) {
		case string:
			if k == "title" || k == "description" {
				x = msg(ctx, x)
			}
			c[k] = x
		case map[string]any:
			c[k] = localizeMap(ctx, x)
		default:
			c[k] = v
		}
	}
	return c
}

// humanBytes is "1,5 MB" in German and "1.5 MB" in English.
func humanBytes(ctx context.Context, n int64) string {
	var s string
	switch {
	case n >= 1<<30:
		s = fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		s = fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	default:
		return msg(ctx, "%d bytes", n)
	}
	if english(ctx) {
		return s
	}
	return strings.Replace(s, ".", ",", 1)
}
