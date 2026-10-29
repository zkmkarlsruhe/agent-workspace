// SPDX-License-Identifier: MIT

// render-instance writes the selected instance's deterministic runtime assets.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
)

func main() {
	file := flag.String("instance", "", "instance.yaml; required without an embedded default or sole minimal")
	out := flag.String("out", "", "output directory (required)")
	check := flag.Bool("check", false, "check existing output without writing")
	flag.Parse()
	if err := run(*file, *out, *check); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(file, out string, check bool) error {
	if out == "" {
		return fmt.Errorf("-out is required")
	}
	d, err := instance.Load(file)
	if err != nil {
		return err
	}
	assets := d.Render()
	names := make([]string, 0, len(assets))
	for name := range assets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		filename := filepath.Join(out, name)
		if check {
			b, err := os.ReadFile(filename)
			if err != nil {
				return err
			}
			if !bytes.Equal(b, assets[name]) {
				return fmt.Errorf("generated asset differs: %s", filename)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filename, assets[name], 0o644); err != nil {
			return err
		}
	}
	return nil
}
