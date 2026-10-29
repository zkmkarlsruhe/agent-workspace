// SPDX-License-Identifier: MIT

package integration

import (
	"fmt"
	"slices"
)

// Options is an instance snapshot. Enabled overrides manifest defaults; Grants
// comes from the admin-managed store, never from a manifest or service token.
type Options struct {
	CoreVersion     string
	OpenCodeVersion string
	Enabled         map[string]bool
	Grants          map[string][]string
}

// Resolve returns the entire selected set in dependency order, or nil on error.
// It never activates dependencies implicitly or mutates the last applied set.
func Resolve(manifests []Manifest, opts Options) ([]Manifest, error) {
	all := map[string]Manifest{}
	active := map[string]Manifest{}
	var names []string
	for _, m := range manifests {
		if err := m.Validate(); err != nil {
			return nil, err
		}
		if _, exists := all[m.Name]; exists {
			return nil, fmt.Errorf("duplicate integration %q", m.Name)
		}
		all[m.Name] = m
		on := m.EnabledByDefault
		if override, ok := opts.Enabled[m.Name]; ok {
			on = override
		}
		if on {
			active[m.Name] = m
			names = append(names, m.Name)
		}
	}
	for name := range opts.Enabled {
		if _, ok := all[name]; !ok {
			return nil, fmt.Errorf("unknown integration switch %q", name)
		}
	}
	slices.Sort(names)
	tools, paths, consumers := map[string]string{}, map[string]string{}, map[string]string{}
	claim := func(registry map[string]string, kind, key, owner string) error {
		if old, ok := registry[key]; ok {
			return fmt.Errorf("%s collision %q: %s and %s", kind, key, old, owner)
		}
		registry[key] = owner
		return nil
	}
	for _, name := range names {
		m := active[name]
		for _, r := range []struct{ label, version, requirement string }{
			{"core", opts.CoreVersion, m.RequiresCore},
			{"opencode", opts.OpenCodeVersion, m.RequiresOpenCode},
		} {
			if r.requirement == "" {
				continue
			}
			ok, err := satisfies(r.version, r.requirement)
			if err != nil || !ok {
				return nil, fmt.Errorf("%s: requires_%s %q not satisfied by %q", name, r.label, r.requirement, r.version)
			}
		}
		for _, dep := range m.Requires {
			if _, ok := active[dep]; !ok {
				return nil, fmt.Errorf("%s requires enabled integration %s", name, dep)
			}
		}
		for _, conflict := range m.Conflicts {
			if _, ok := active[conflict]; ok {
				return nil, fmt.Errorf("%s conflicts with %s", name, conflict)
			}
		}
		grants := opts.Grants[name]
		if !unique(grants, knownScope) {
			return nil, fmt.Errorf("%s: invalid grants", name)
		}
		for _, scope := range m.Scopes {
			if !slices.Contains(grants, scope) {
				return nil, fmt.Errorf("%s: scope %s is not granted", name, scope)
			}
		}
		for _, tool := range m.Tools {
			full, _ := ToolName(name, tool) // Validate checked the reference
			if m.BuiltinTools() {
				full = LegacyTool(name, tool)
			}
			if err := claim(tools, "tool", full, name); err != nil {
				return nil, err
			}
		}
		for _, page := range m.Portal {
			if err := claim(paths, "portal path", page.Path, name); err != nil {
				return nil, err
			}
		}
		if len(m.Events) > 0 {
			consumer := m.EventConsumer
			if consumer == "" {
				consumer = name
			}
			if err := claim(consumers, "event consumer", consumer, name); err != nil {
				return nil, err
			}
		}
	}
	state := map[string]int{}
	out := []Manifest{}
	var visit func(string) error
	visit = func(name string) error {
		switch state[name] {
		case 1:
			return fmt.Errorf("dependency cycle at %s", name)
		case 2:
			return nil
		}
		state[name] = 1
		m := active[name]
		deps := slices.Clone(m.Requires)
		slices.Sort(deps)
		for _, dep := range deps {
			if err := visit(dep); err != nil {
				return err
			}
		}
		state[name] = 2
		out = append(out, m)
		return nil
	}
	for _, name := range names {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	return out, nil
}
