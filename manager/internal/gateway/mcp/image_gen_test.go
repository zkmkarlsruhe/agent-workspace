// SPDX-License-Identifier: MIT

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/jpeg"
	"image/png"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/gateway/llm"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

func TestAILabelName(t *testing.T) {
	now := time.Date(2026, 10, 18, 9, 5, 0, 0, time.UTC)
	for in, want := range map[string]string{
		"":                       "bild-2026-10-18-0905-ki.png",
		"Plakat Sommerfest.png":  "plakat-sommerfest-ki.png",
		"plakat-ki":              "plakat-ki.png",
		"../../etc/Grüße":        "etc-gruesse-ki.png",
		"///":                    "bild-2026-10-18-0905-ki.png",
		strings.Repeat("a", 100): strings.Repeat("a", 60) + "-ki.png",
	} {
		if got := aiFileName(in, now); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestImageGenerate(t *testing.T) {
	r := &rec{}
	ff := &fakeFiles{dir: "/workspace/bericht", written: map[string][]byte{}, files: map[string]string{"bericht/inbox/foto.png": pngOf(20, 10)}}
	var gotPrompt, gotSize string
	var gotSourceImage []byte
	answer := []byte(pngOf(8, 8))
	var genErr error
	gen := func(_ context.Context, u store.User, session, prompt, size string, sourceImage []byte) ([]byte, string, error) {
		gotPrompt, gotSize, gotSourceImage = prompt, size, sourceImage
		return answer, "workspace/gwdg-image", genErr
	}
	srv := func(canEdit bool) *Server {
		return New(Server{SearXNG: &url.URL{}, SlotOf: config.Config{UserNetPrefix: "10.90"}.SlotOf, Users: users{}, Usage: r,
			Log: slog.New(slog.DiscardHandler), Files: ff, Generate: gen, CanEdit: canEdit})
	}
	s := srv(false)
	const me = "10.90.1.10:40000"
	list := rpc(s, me, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`).Body.String()
	if !strings.Contains(list, `"image_generate"`) || strings.Contains(list, `"source_image"`) {
		t.Fatalf("tools/list without edit model: %s", list)
	}
	if l := rpc(srv(true), me, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`).Body.String(); !strings.Contains(l, `"source_image"`) {
		t.Error("source_image not listed with edit model")
	}
	call := func(s *Server, args string) toolResult {
		w := rpc(s, me, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"image_generate","arguments":`+args+`,"_meta":{"ai.opencode/sessionID":"ses_abcdefgh12"}}}`)
		var resp struct{ Result toolResult }
		json.Unmarshal(w.Body.Bytes(), &resp)
		return resp.Result
	}

	res := call(s, `{"prompt":"Ein rotes Fahrrad vor dem Workspace","filename":"Fahrrad"}`)
	if res.IsError || !strings.Contains(res.Content[0].Text, "/workspace/bericht/results/fahrrad-ki.png") || !strings.Contains(res.Content[0].Text, "KI-generiert mit workspace/gwdg-image") {
		t.Fatalf("result %+v", res)
	}
	if gotPrompt != "Ein rotes Fahrrad vor dem Workspace" || gotSize != "1536x1024" || gotSourceImage != nil {
		t.Errorf("generate got %q %q %v", gotPrompt, gotSize, gotSourceImage)
	}
	out := ff.written["bericht/results/fahrrad-ki.png"]
	if _, err := png.Decode(bytes.NewReader(out)); err != nil {
		t.Fatalf("written file is no valid PNG: %v", err)
	}
	for _, want := range []string{"Software\x00Agent Workspace", "Comment\x00\x00\x00\x00\x00KI-generiert mit workspace/gwdg-image, Agent Workspace, 20",
		"digitalsourcetype/trainedAlgorithmicMedia"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("label %q missing", want)
		}
	}
	if e := r.ev[len(r.ev)-1]; e.Capability != "image" || e.Target != "workspace/gwdg-image" || e.Status != 200 {
		t.Errorf("usage %+v", e)
	}

	// a JPEG answer is converted, the format reaches the model as size
	var j bytes.Buffer
	jpeg.Encode(&j, image.NewRGBA(image.Rect(0, 0, 4, 4)), nil)
	answer = j.Bytes()
	if res := call(s, `{"prompt":"x","aspect":"portrait"}`); res.IsError || gotSize != "1024x1536" {
		t.Fatalf("jpeg answer: %+v %q", res, gotSize)
	}
	answer = []byte(pngOf(8, 8))

	for args, want := range map[string]string{
		`{}`:                             "prompt fehlt",
		`{"prompt":"x","aspect":"rund"}`: "square, landscape oder portrait",
		`{"prompt":"x","source_image":"inbox/foto.png"}`: "nur neue Bilder",
	} {
		if res := call(s, args); !res.IsError || !strings.Contains(res.Content[0].Text, want) {
			t.Errorf("%s: want error %q, got %+v", args, want, res)
		}
	}
	answer = []byte("kein Bild")
	if res := call(s, `{"prompt":"x"}`); !res.IsError || !strings.Contains(res.Content[0].Text, "kein brauchbares Bild") {
		t.Errorf("broken answer: %+v", res)
	}
	answer = []byte(pngOf(8, 8))
	genErr = llm.ErrDayCap
	if res := call(s, `{"prompt":"x"}`); !res.IsError || !strings.Contains(res.Content[0].Text, "Tagesbudget") {
		t.Errorf("budget: %+v", res)
	}
	genErr = nil

	edit := srv(true)
	if res := call(edit, `{"prompt":"rot statt blau","source_image":"inbox/foto.png","filename":"rad"}`); res.IsError {
		t.Fatalf("edit: %+v", res)
	}
	if cfg, err := png.DecodeConfig(bytes.NewReader(gotSourceImage)); err != nil || cfg.Width != 20 {
		t.Errorf("source_image sent: %v %+v", err, cfg)
	}
	if out := ff.written["bericht/results/rad-ki.png"]; !bytes.Contains(out, []byte("compositeWithTrainedAlgorithmicMedia")) {
		t.Errorf("edit not labelled as composite: %d bytes, files %v", len(out), len(ff.written))
	}
}
