// SPDX-License-Identifier: MIT

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/catalog"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/gateway/llm"
)

func TestToolboxRun(t *testing.T) {
	charge := .07
	var on []catalog.Item
	type run struct{ toolbox, image, dir, command string }
	var runs []run
	r := &rec{}
	s := New(Server{SearXNG: mustURL("http://127.0.0.1:1"), SlotOf: config.Config{UserNetPrefix: "10.90"}.SlotOf,
		Users: users{}, Usage: r, Log: slog.New(slog.DiscardHandler),
		Toolboxes: &Toolboxes{ChargeEUR: &charge,
			On: func(_ context.Context, uid string) []catalog.Item {
				if uid != "u1" {
					return nil
				}
				return on
			},
			Run: func(_ context.Context, uid, w, image, dir, command string, out io.Writer) (int, bool, error) {
				runs = append(runs, run{w, image, dir, command})
				if command == "kaputt" {
					return 0, false, errors.New("engine down")
				}
				io.WriteString(out, "ffmpeg version 7.1")
				return 0, command == "viel", nil
			},
			SessionDir: func(_ context.Context, uid, session string) (string, error) { return "/workspace/bericht", nil },
		}})
	const me = "10.90.1.10:40000"
	list := func() string { return rpc(s, me, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`).Body.String() }
	call := func(args string) (string, bool) {
		w := rpc(s, me, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"toolbox_run","arguments":`+args+
			`,"_meta":{"ai.opencode/sessionID":"ses_abcdefgh12"}}}`)
		var res struct {
			Result toolResult `json:"result"`
		}
		json.Unmarshal(w.Body.Bytes(), &res)
		if len(res.Result.Content) == 0 {
			t.Fatalf("%s: %s", args, w.Body)
		}
		return res.Result.Content[0].Text, res.Result.IsError
	}
	if strings.Contains(list(), "toolbox_run") {
		t.Fatal("offered without a switched-on toolbox")
	}
	if text, isErr := call(`{"toolbox":"media","command":"ffmpeg -version"}`); !isErr || !strings.Contains(text, "nicht eingeschaltet") || len(runs) != 0 {
		t.Fatalf("off: %q", text)
	}
	on = []catalog.Item{{ID: "media", TitleDE: "Medien umwandeln", Image: "img@sha256:x", Programs: []string{"ffmpeg", "magick"}}}
	if l := list(); !strings.Contains(l, "toolbox_run") || !strings.Contains(l, "- media (Medien umwandeln): ffmpeg, magick") {
		t.Fatalf("on: %s", l)
	}
	text, isErr := call(`{"toolbox":"media","command":"ffmpeg -version","workdir":"medien/"}`)
	if isErr || text != "Exit-Code 0\nffmpeg version 7.1" || runs[0] != (run{"media", "img@sha256:x", "/workspace/bericht/medien", "ffmpeg -version"}) {
		t.Fatalf("run: %q %+v", text, runs)
	}
	if text, _ := call(`{"toolbox":"media","command":"viel"}`); !strings.Contains(text, "gekürzt") || runs[1].dir != "/workspace/bericht" {
		t.Fatalf("cut: %q %+v", text, runs[1])
	}
	for _, dir := range []string{"../anderes", "/etc", ".git", "a/../../x"} {
		if text, isErr := call(`{"toolbox":"media","command":"ls","workdir":"` + dir + `"}`); !isErr || !strings.Contains(text, "workdir") {
			t.Errorf("%s: %q", dir, text)
		}
	}
	if text, isErr := call(`{"toolbox":"andere","command":"ls"}`); !isErr || !strings.Contains(text, "Eingeschaltet: media") {
		t.Fatalf("other toolbox: %q", text)
	}
	if text, isErr := call(`{"toolbox":"media","command":"kaputt"}`); !isErr || !strings.Contains(text, "nicht starten") {
		t.Fatalf("engine error: %q", text)
	}
	if len(runs) != 3 {
		t.Fatalf("runs %+v", runs)
	}
	if len(r.ev) == 0 || r.ev[0].Capability != "toolbox" || r.ev[0].Target != "media" {
		t.Fatalf("usage %+v", r.ev)
	}

	// the daily cap and the concurrency limit hold for tool runs too
	refuse := llm.ErrDayCap
	released := 0
	s.Toolboxes.Reserve = func(_ context.Context, uid string, eur float64) (func(), error) {
		if refuse != nil {
			return nil, refuse
		}
		if uid != "u1" || eur != charge {
			t.Errorf("reserve %s %v", uid, eur)
		}
		return func() {
			if last := r.ev[len(r.ev)-1]; last.CostEUR == nil || *last.CostEUR != charge || last.CostSource != "estimate" {
				t.Errorf("released before the charge was recorded: %+v", last)
			}
			released++
		}, nil
	}
	if text, isErr := call(`{"toolbox":"media","command":"ls"}`); !isErr || !strings.Contains(text, "Tagesbudget") || len(runs) != 3 {
		t.Fatalf("over the cap: %q, %d runs", text, len(runs))
	}
	refuse = llm.ErrBusy
	if text, _ := call(`{"toolbox":"media","command":"ls"}`); !strings.Contains(text, "gleichzeitige") || len(runs) != 3 {
		t.Fatalf("busy: %q", text)
	}
	refuse = nil
	if _, isErr := call(`{"toolbox":"media","command":"ls"}`); isErr || len(runs) != 4 || released != 1 {
		t.Fatalf("admitted: %d runs, %d released", len(runs), released)
	}
}
