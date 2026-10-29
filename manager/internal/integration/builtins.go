// SPDX-License-Identifier: MIT

package integration

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

//go:embed builtins.json
var builtinJSON []byte

// Builtins is the shipped manifest inventory. Tests compare this embedded
// snapshot with deploy/integrations so the scratch image needs no source tree.
func Builtins() ([]Manifest, error) {
	var raw []json.RawMessage
	if err := json.Unmarshal(builtinJSON, &raw); err != nil {
		return nil, err
	}
	out := make([]Manifest, 0, len(raw))
	for _, b := range raw {
		m, err := Load(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		m.shipped = true
		out = append(out, m)
	}
	return out, nil
}

// Inventory adds operator manifests to the embedded inventory. Empty dir disables
// external loading; files are read once when the instance is loaded.
func Inventory(dir string) ([]Manifest, error) {
	out, err := Builtins()
	if err != nil || dir == "" {
		return out, err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("integrations directory %s: %w", dir, err)
	}
	defer directory.Close()
	seen := map[string]bool{}
	for _, m := range out {
		seen[m.Name] = true
	}
	count := 0
	for {
		entries, err := directory.ReadDir(1)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("integrations directory %s: %w", dir, err)
		}
		entry := entries[0]
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		file := filepath.Join(dir, entry.Name())
		count++
		if count > 200 {
			return nil, fmt.Errorf("integrations directory %s: maximum 200 manifests", dir)
		}
		b, err := readManifest(file)
		if err != nil {
			return nil, fmt.Errorf("manifest %s: %w", file, err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(b, &fields); err != nil {
			return nil, fmt.Errorf("manifest %s: %w", file, err)
		}
		for key, value := range fields {
			if strings.EqualFold(key, "enabled_by_default") {
				return nil, fmt.Errorf("manifest %s: enabled_by_default is reserved for shipped integrations; activate in instance integrations", file)
			}
			if strings.EqualFold(key, "runs") {
				var runs string
				if json.Unmarshal(value, &runs) == nil && runs == "builtin" {
					return nil, fmt.Errorf("manifest %s: runs: builtin is reserved for shipped integrations", file)
				}
			}
		}
		m, err := Load(bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("manifest %s: %w", file, err)
		}
		if seen[m.Name] {
			return nil, fmt.Errorf("manifest %s: duplicate integration %q", file, m.Name)
		}
		seen[m.Name] = true
		out = append(out, m)
	}
	return out, nil
}

// LegacyTool keeps the production MCP vocabulary during the instance cutover.
// The separate-server naming contract remains available through ToolName.
func LegacyTool(integration, local string) string {
	if integration == "nextcloud" {
		return "nextcloud_" + local
	}
	return local
}

func readManifest(file string) ([]byte, error) {
	info, err := os.Lstat(file)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("regular manifest file required (no symlinks)")
	}
	f, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("regular manifest file required")
	}
	const maxBytes = 256 << 10
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("manifest exceeds 256 KiB")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxBytes {
		return nil, fmt.Errorf("manifest exceeds 256 KiB")
	}
	return b, nil
}
