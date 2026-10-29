// SPDX-License-Identifier: MIT

package integration

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
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
