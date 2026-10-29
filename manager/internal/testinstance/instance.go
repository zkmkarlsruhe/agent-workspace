// SPDX-License-Identifier: MIT

// Package testinstance supplies explicit remote fixtures for integration tests.
// Tests of the shipped inventory must use instance.Load directly.
package testinstance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/integration"
)

// Load enables the example catalog's remote transports for tests, including
// transports intentionally absent from the public builtin inventory.
func Load(filename string) (*instance.Definition, error) {
	d, err := instance.Load(filename)
	if err != nil {
		return nil, err
	}
	builtins, err := integration.Builtins()
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, m := range builtins {
		known[m.Name] = true
	}
	dir, err := os.MkdirTemp("", "remote-test-fixtures-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	var added []string
	for _, entry := range d.Catalog {
		if entry.Kind != "mcp-central" || known[entry.ID] {
			continue
		}
		m := integration.Manifest{Schema: 1, Name: entry.ID, Version: "0.1.0", Runs: "builtin", Kind: "remote", Tools: []string{"lookup"}}
		b, err := json.Marshal(m)
		if err != nil {
			return nil, err
		}
		// The filename is independent of the catalog ID; manifest validation owns names.
		if err := os.WriteFile(filepath.Join(dir, "fixture-"+strconv.Itoa(len(added))+".json"), b, 0600); err != nil {
			return nil, err
		}
		added = append(added, entry.ID)
	}
	d, err = instance.LoadWithIntegrations(filename, dir)
	if err != nil {
		return nil, err
	}
	for _, name := range added {
		d.Integrations[name] = true
	}
	return d, d.Validate()
}
