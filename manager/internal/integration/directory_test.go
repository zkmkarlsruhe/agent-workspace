// SPDX-License-Identifier: MIT

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

func TestManifestDirectory(t *testing.T) {
	const valid = `{"schema":1,"name":"hello","version":"0.1.0","runs":"service","kind":"remote","mcp":"/mcp","tools":["hello"]}`
	builtins, err := Builtins()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Inventory(""); err != nil || len(got) != len(builtins) {
		t.Fatalf("default: %v %v", got, err)
	}
	for _, tc := range []struct{ name, data, extra, want string }{
		{name: "valid", data: valid},
		{name: "invalid", data: strings.Replace(valid, `"schema":1`, `"schema":2`, 1), want: "hello.json"},
		{name: "unknown field", data: strings.Replace(valid, `"schema":1`, `"unknown":1`, 1), want: "hello.json"},
		{name: "invalid kind", data: strings.Replace(valid, `"remote"`, `"typo"`, 1), want: "hello.json"},
		{name: "builtin collision", data: strings.Replace(valid, `"name":"hello"`, `"name":"pdf"`, 1), want: "duplicate integration"},
		{name: "directory collision", data: valid, extra: valid, want: "duplicate integration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "hello.json")
			if err := os.WriteFile(file, []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			if tc.extra != "" {
				if err := os.WriteFile(filepath.Join(dir, "other.json"), []byte(tc.extra), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("not JSON"), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := Inventory(dir)
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), dir) {
					t.Fatalf("error = %v", err)
				}
				return
			}
			if err != nil || len(got) != len(builtins)+1 || !slices.ContainsFunc(got, func(m Manifest) bool { return m.Name == "hello" && !m.BuiltinTools() }) {
				t.Fatalf("inventory: %v %v", got, err)
			}
		})
	}
	if _, err := Inventory(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing directory accepted")
	}
}

func TestOperatorManifestBoundaries(t *testing.T) {
	const valid = `{"schema":1,"name":"addon","version":"1.0.0","runs":"service","mcp":"/mcp","tools":["web_read"]}`
	for _, tc := range []struct{ name, data, want string }{
		{"builtin", strings.Replace(valid, `"service"`, `"builtin"`, 1), "runs: builtin"},
		{"default true", strings.Replace(valid, `"schema":1`, `"schema":1,"enabled_by_default":true`, 1), "enabled_by_default"},
		{"mixed case default", strings.Replace(valid, `"schema":1`, `"schema":1,"ENABLED_BY_DEFAULT":true`, 1), "enabled_by_default"},
		{"mixed case builtin", strings.Replace(valid, `"runs":"service"`, `"RUNS":"builtin"`, 1), "runs: builtin"},
		{"default false", strings.Replace(valid, `"schema":1`, `"schema":1,"enabled_by_default":false`, 1), "enabled_by_default"},
		{"oversized", valid + strings.Repeat(" ", 256<<10), "256 KiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "addon.json"), []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Inventory(dir); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatal(err)
			}
		})
	}
	for _, kind := range []string{"symlink", "directory", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "addon.json")
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink("/dev/zero", file)
			case "directory":
				err = os.Mkdir(file, 0700)
			case "fifo":
				err = syscall.Mkfifo(file, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = Inventory(dir); err == nil || !strings.Contains(err.Error(), "regular") {
				t.Fatal(err)
			}
		})
	}
	dir := t.TempDir()
	for i := 0; i < 201; i++ {
		body := strings.Replace(valid, `"addon"`, fmt.Sprintf(`"addon-%d"`, i), 1)
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%03d.json", i)), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Inventory(dir); err == nil || !strings.Contains(err.Error(), "200 manifests") {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "200.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := Inventory(dir); err != nil {
		t.Fatal("200 manifests refused", err)
	}
	boundary := t.TempDir()
	exact := valid + strings.Repeat(" ", (256<<10)-len(valid))
	if err := os.WriteFile(filepath.Join(boundary, "addon.json"), []byte(exact), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Inventory(boundary); err != nil {
		t.Fatal("256 KiB manifest refused", err)
	}

}

func TestLegacyDispatchCollisions(t *testing.T) {
	a, b := builtin("nextcloud"), builtin("other")
	a.Tools, b.Tools = []string{"list"}, []string{"nextcloud_list"}
	if _, err := Resolve([]Manifest{a, b}, Options{}); err == nil || !strings.Contains(err.Error(), "nextcloud_list") {
		t.Fatal(err)
	}
	if a.ShippedBuiltinTools() {
		t.Fatal("unshipped manifest owns legacy tools")
	}
}
