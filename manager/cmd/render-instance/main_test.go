// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRenderAndCheck(t *testing.T) {
	dir := t.TempDir()
	if err := run("../../../instance/minimal/instance.yaml", dir, false); err != nil {
		t.Fatal(err)
	}
	if err := run("../../../instance/minimal/instance.yaml", dir, true); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(file, []byte("hand edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run("../../../instance/minimal/instance.yaml", dir, true); err == nil {
		t.Fatal("check accepted a hand edit")
	}
	if b, err := os.ReadFile(file); err != nil || string(b) != "hand edit" {
		t.Fatal("check modified output")
	}
}
