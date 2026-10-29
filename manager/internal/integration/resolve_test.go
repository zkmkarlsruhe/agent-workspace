// SPDX-License-Identifier: MIT

package integration

import (
	"reflect"
	"strings"
	"testing"
)

func builtin(name string) Manifest {
	return Manifest{Schema: 1, Name: name, Version: "1.0.0", Runs: "builtin", EnabledByDefault: true}
}

func TestResolve(t *testing.T) {
	for _, tt := range []struct {
		name  string
		setup func(*[]Manifest, *Options)
		want  []string
		err   string
	}{
		{"defaults", func(m *[]Manifest, o *Options) {
			(*m)[1].EnabledByDefault = false
		}, []string{"alpha"}, ""},
		{"explicit on", func(m *[]Manifest, o *Options) {
			(*m)[1].EnabledByDefault = false
			o.Enabled = map[string]bool{"beta": true}
		}, []string{"alpha", "beta"}, ""},
		{"explicit off", func(m *[]Manifest, o *Options) {
			o.Enabled = map[string]bool{"alpha": false}
		}, []string{"beta"}, ""},
		{"minimal", func(m *[]Manifest, o *Options) {
			*m = nil
		}, []string{}, ""},
		{"all off", func(m *[]Manifest, o *Options) {
			o.Enabled = map[string]bool{"alpha": false, "beta": false}
		}, []string{}, ""},
		{"dependency order", func(m *[]Manifest, o *Options) {
			(*m)[0].Requires = []string{"beta"}
		}, []string{"beta", "alpha"}, ""},
		{"missing dependency", func(m *[]Manifest, o *Options) {
			(*m)[0].Requires = []string{"missing"}
		}, nil, "requires enabled"},
		{"disabled dependency", func(m *[]Manifest, o *Options) {
			(*m)[0].Requires = []string{"beta"}
			o.Enabled = map[string]bool{"beta": false}
		}, nil, "requires enabled"},
		{"disabled dependent", func(m *[]Manifest, o *Options) {
			(*m)[0].Requires = []string{"missing"}
			o.Enabled = map[string]bool{"alpha": false}
		}, []string{"beta"}, ""},
		{"cycle", func(m *[]Manifest, o *Options) {
			(*m)[0].Requires = []string{"beta"}
			(*m)[1].Requires = []string{"alpha"}
		}, nil, "cycle"},
		{"one sided conflict", func(m *[]Manifest, o *Options) {
			(*m)[1].Conflicts = []string{"alpha"}
		}, nil, "conflicts"},
		{"disabled conflict", func(m *[]Manifest, o *Options) {
			(*m)[1].Conflicts = []string{"alpha"}
			o.Enabled = map[string]bool{"alpha": false}
		}, []string{"beta"}, ""},
		{"absent conflict", func(m *[]Manifest, o *Options) {
			(*m)[0].Conflicts = []string{"absent"}
		}, []string{"alpha", "beta"}, ""},
		{"unknown switch", func(m *[]Manifest, o *Options) {
			o.Enabled = map[string]bool{"typo": false}
		}, nil, "unknown integration switch"},
		{"duplicate manifest", func(m *[]Manifest, o *Options) {
			(*m)[1].Name = "alpha"
		}, nil, "duplicate integration"},
		{"invalid disabled manifest", func(m *[]Manifest, o *Options) {
			(*m)[1].Schema = 2
			o.Enabled = map[string]bool{"beta": false}
		}, nil, "schema"},
		{"compatible versions", func(m *[]Manifest, o *Options) {
			(*m)[0].RequiresCore = ">=1.4 <2"
			(*m)[1].RequiresOpenCode = ">=2.2.0 <2.3"
		}, []string{"alpha", "beta"}, ""},
		{"incompatible core", func(m *[]Manifest, o *Options) {
			(*m)[0].RequiresCore = ">=2"
		}, nil, "requires_core"},
		{"incompatible opencode", func(m *[]Manifest, o *Options) {
			(*m)[0].RequiresOpenCode = "<2.2"
		}, nil, "requires_opencode"},
		{"missing runtime version", func(m *[]Manifest, o *Options) {
			(*m)[0].RequiresOpenCode = ">=2.2"
			o.OpenCodeVersion = ""
		}, nil, "requires_opencode"},
		{"invalid runtime version", func(m *[]Manifest, o *Options) {
			(*m)[0].RequiresCore = ">=1"
			o.CoreVersion = "broken"
		}, nil, "requires_core"},
		{"unused runtime version", func(m *[]Manifest, o *Options) {
			o.CoreVersion = ""
			o.OpenCodeVersion = ""
		}, []string{"alpha", "beta"}, ""},
		{"disabled incompatible", func(m *[]Manifest, o *Options) {
			(*m)[0].RequiresCore = ">=99"
			o.Enabled = map[string]bool{"alpha": false}
		}, []string{"beta"}, ""},
		{"scopes missing grants", func(m *[]Manifest, o *Options) {
			(*m)[0].Scopes = []string{"transcripts:read"}
		}, nil, "not granted"},
		{"scopes own grant", func(m *[]Manifest, o *Options) {
			(*m)[0].Scopes = []string{"notify"}
			o.Grants = map[string][]string{"alpha": {"notify"}}
		}, []string{"alpha", "beta"}, ""},
		{"grant isolation", func(m *[]Manifest, o *Options) {
			(*m)[0].Scopes = []string{"notify"}
			o.Grants = map[string][]string{"beta": {"notify"}}
		}, nil, "not granted"},
		{"scope escalation", func(m *[]Manifest, o *Options) {
			(*m)[0].Scopes = []string{"notify", "transcripts:read"}
			o.Grants = map[string][]string{"alpha": {"notify"}}
		}, nil, "not granted"},
		{"extra grant", func(m *[]Manifest, o *Options) {
			o.Grants = map[string][]string{"alpha": {"notify"}}
		}, []string{"alpha", "beta"}, ""},
		{"invalid grant", func(m *[]Manifest, o *Options) {
			o.Grants = map[string][]string{"alpha": {"unknown"}}
		}, nil, "invalid grants"},
		{"duplicate grant", func(m *[]Manifest, o *Options) {
			o.Grants = map[string][]string{"alpha": {"notify", "notify"}}
		}, nil, "invalid grants"},
		{"disabled scopes", func(m *[]Manifest, o *Options) {
			(*m)[0].Scopes = []string{"transcripts:read"}
			o.Enabled = map[string]bool{"alpha": false}
		}, []string{"beta"}, ""},
		{"same local tool distinct namespace", func(m *[]Manifest, o *Options) {
			(*m)[0].Tools = []string{"fetch"}
			(*m)[1].Tools = []string{"fetch"}
		}, nil, "tool collision"},
		{"duplicate tool", func(m *[]Manifest, o *Options) {
			(*m)[0].Tools = []string{"fetch", "fetch"}
		}, nil, "tools"},
		{"portal collision", func(m *[]Manifest, o *Options) {
			for i := range *m {
				(*m)[i].Portal = []PortalPage{{Path: "/report", Roles: []string{"admin"}}}
			}
		}, nil, "portal path collision"},
		{"consumer collision", func(m *[]Manifest, o *Options) {
			for i := range *m {
				(*m)[i].Events = []EventType{ChatFinished}
				(*m)[i].EventConsumer = "report"
			}
		}, nil, "event consumer collision"},
		{"default consumer collision", func(m *[]Manifest, o *Options) {
			for i := range *m {
				(*m)[i].Events = []EventType{ChatFinished}
			}
			(*m)[1].EventConsumer = "alpha"
		}, nil, "event consumer collision"},
		{"distinct consumers same event", func(m *[]Manifest, o *Options) {
			for i := range *m {
				(*m)[i].Events = []EventType{ChatFinished}
			}
		}, []string{"alpha", "beta"}, ""},
		{"disabled collisions", func(m *[]Manifest, o *Options) {
			for i := range *m {
				(*m)[i].Events = []EventType{ChatFinished}
				(*m)[i].EventConsumer = "report"
				(*m)[i].Portal = []PortalPage{{Path: "/report", Roles: []string{"admin"}}}
			}
			o.Enabled = map[string]bool{"alpha": false}
		}, []string{"beta"}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			manifests := []Manifest{builtin("alpha"), builtin("beta")}
			opts := Options{CoreVersion: "1.4.0", OpenCodeVersion: "2.2.0"}
			tt.setup(&manifests, &opts)
			got, err := Resolve(manifests, opts)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) || got != nil {
					t.Fatalf("Resolve = %v, %v; want no partial result and %q", got, err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			names := []string{}
			for _, m := range got {
				names = append(names, m.Name)
			}
			if !reflect.DeepEqual(names, tt.want) {
				t.Fatalf("names = %v, want %v", names, tt.want)
			}
		})
	}
}

func TestResolveDeterministicAndNoMutation(t *testing.T) {
	a, b, c := builtin("alpha"), builtin("beta"), builtin("charlie")
	a.Requires = []string{"charlie", "beta"}
	opts := Options{}
	for _, input := range [][]Manifest{{a, b, c}, {c, a, b}, {b, c, a}} {
		got, err := Resolve(input, opts)
		if err != nil {
			t.Fatal(err)
		}
		if got[0].Name != "beta" || got[1].Name != "charlie" || got[2].Name != "alpha" {
			t.Fatalf("unexpected order: %v", got)
		}
		if !reflect.DeepEqual(a.Requires, []string{"charlie", "beta"}) {
			t.Fatal("Resolve mutated dependency order")
		}
	}
}

func TestResolveUpgradeKeepsApplied(t *testing.T) {
	old := builtin("report")
	old.Scopes = []string{"notify"}
	opts := Options{Grants: map[string][]string{"report": {"notify"}}}
	applied, err := Resolve([]Manifest{old}, opts)
	if err != nil {
		t.Fatal(err)
	}
	upgrade := old
	upgrade.Version = "1.1.0"
	upgrade.Scopes = []string{"notify", "transcripts:read"}
	candidate, err := Resolve([]Manifest{upgrade}, opts)
	if err == nil || candidate != nil || applied[0].Version != "1.0.0" {
		t.Fatalf("failed upgrade must not replace applied: %v, %v", candidate, err)
	}
}
