// SPDX-License-Identifier: MIT

package settings

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// readAgents runs in the container on a path the agent controls.
func TestReadAgentsBounded(t *testing.T) {
	run := func(p string) (string, error) {
		var out bytes.Buffer
		cmd := exec.Command("sh", "-c", readAgents, "sh", p)
		cmd.Stdout = &capped{&out, MaxAgents + 1}
		err := cmd.Run()
		return out.String(), err
	}
	if _, err := run("/dev/zero"); err == nil {
		t.Error("/dev/zero read")
	}
	d := t.TempDir()
	big := filepath.Join(d, "big")
	os.WriteFile(big, []byte(strings.Repeat("x", 3*MaxAgents)), 0o644)
	if out, err := run(big); err != nil || len(out) != MaxAgents+1 {
		t.Errorf("big file: %d bytes, %v", len(out), err)
	}
	if out, err := run(filepath.Join(d, "missing")); err != nil || out != "" {
		t.Errorf("missing: %q %v", out, err)
	}
	if MaxAgents+1 != 8193 {
		t.Error("readAgents head -c must be MaxAgents+1")
	}
}
