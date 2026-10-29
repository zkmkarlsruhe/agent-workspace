// SPDX-License-Identifier: MIT

package mcp

import (
	"encoding/json"
	"testing"
)

func TestTimeAlias(t *testing.T) {
	for in, want := range map[string]string{
		`{"from":"2026-10-19T10:00","to":"2026-10-19T12:00"}`: `{"from":"2026-10-19T10:00","to":"2026-10-19T12:00"}`,
		`{"start":"2026-10-19","instance":"administration"}`:  `{"from":"2026-10-19","instance":"administration"}`,
		`{"von":"2026-10-19","bis":"2026-10-20"}`:             `{"from":"2026-10-19","to":"2026-10-20"}`, // pre-rename names, one release
		`{"from":"2026-10-19","von":"2026-10-20"}`:            `{"from":"2026-10-19"}`,                   // from wins
		`not json`: `not json`,
	} {
		got := string(timeAlias(json.RawMessage(in)))
		if got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
}
