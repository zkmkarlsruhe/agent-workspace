// SPDX-License-Identifier: MIT

package integration

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

func TestExampleManifests(t *testing.T) {
	files, err := filepath.Glob("../../../deploy/integrations/*/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	embedded, err := Builtins()
	if err != nil {
		t.Fatal(err)
	}
	var want, wantDefaults []string
	for _, m := range embedded {
		want = append(want, m.Name)
		if m.EnabledByDefault {
			wantDefaults = append(wantDefaults, m.Name)
		}
	}
	slices.Sort(want)
	slices.Sort(wantDefaults)
	var names []string
	var manifests []Manifest
	opts := Options{CoreVersion: "2026.10.27.1", OpenCodeVersion: "2.2.0", Enabled: map[string]bool{}, Grants: map[string][]string{}}
	for _, file := range files {
		f, err := os.Open(file)
		if err != nil {
			t.Fatal(err)
		}
		m, err := Load(f)
		f.Close()
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if m.Name != filepath.Base(filepath.Dir(file)) {
			t.Fatalf("%s: directory does not match name %s", file, m.Name)
		}
		names = append(names, m.Name)
		m.shipped = true
		manifests = append(manifests, m)
		opts.Enabled[m.Name] = true
		opts.Grants[m.Name] = slices.Clone(m.Scopes) // explicit admin grants in this fixture
	}
	slices.Sort(names)
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("examples = %v, want %v", names, want)
	}
	if _, err := Resolve(manifests, opts); err != nil {
		t.Fatalf("all examples: %v", err)
	}
	if !reflect.DeepEqual(embedded, manifests) {
		t.Fatalf("embedded manifests differ from deploy: %v", err)
	}
	opts.Enabled = nil
	defaults, err := Resolve(manifests, opts)
	if err != nil {
		t.Fatal(err)
	}
	names = nil
	for _, m := range defaults {
		names = append(names, m.Name)
	}
	slices.Sort(names)
	if !reflect.DeepEqual(names, wantDefaults) {
		t.Fatalf("default integrations = %v", names)
	}
	opts.Enabled = map[string]bool{}
	for _, m := range manifests {
		opts.Enabled[m.Name] = false
	}
	if got, err := Resolve(manifests, opts); err != nil || len(got) != 0 {
		t.Fatalf("minimal = %v, %v", got, err)
	}
}
