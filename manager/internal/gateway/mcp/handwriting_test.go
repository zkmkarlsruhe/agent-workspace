// SPDX-License-Identifier: MIT

package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/filetools"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/gateway/llm"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

func TestHandwritingProjectScopeStatusAndMetadata(t *testing.T) {
	prepCalls, modelCalls := 0, 0
	s, files := mediaServer(t, func(_ context.Context, op string, data []byte, options map[string]any) (filetools.Result, error) {
		prepCalls++
		if op != "handwriting_prepare" || options["source_type"] != "pdf" || options["page"] != 2 {
			t.Fatalf("bad preparation: %s %v", op, options)
		}
		return filetools.Result{Status: "complete", Outputs: []filetools.Output{{Data: []byte(pngOf(20, 20)), Extension: ".png", MediaType: "image/png", Role: "result"}}, Metadata: map[string]any{"page": 2, "document_pages": 3, "render_dpi": 192}}, nil
	})
	files.files["bericht/inbox/notes.pdf"] = "%PDF fixture"
	held := false
	finish, text := "stop", "Änne\n12,50\n[unlesbar]"
	s.Transcribe = func(context.Context, store.User, string, []byte) (llm.Transcription, error) {
		if held {
			t.Fatal("nested budget reservation")
		}
		modelCalls++
		return llm.Transcription{Text: text, FinishReason: finish, Model: "transcription-route"}, nil
	}
	// Preparation admission must end before the model transport reserves its cost.
	s.Reserve = func(context.Context, string, float64) (func(), error) {
		if held {
			t.Fatal("preparation reservation leaked")
		}
		held = true
		return func() { held = false }, nil
	}
	call := func(args string) toolResult {
		r, _ := s.fileTool(context.Background(), store.User{UID: "u1"}, "ses_abcdefgh12", "handwriting_to_text", json.RawMessage(args))
		return r
	}
	for _, bad := range []string{`{"path":"/workspace/other/notes.pdf","options":{"page":2}}`, `{"path":"inbox/notes.pdf"}`, `{"path":"inbox/notes.pdf","options":{"page":1.5}}`, `{"path":"inbox/notes.pdf","options":{"model":"other"}}`, `{"path":"inbox/notes.pdf","question":"rewrite"}`} {
		if !call(bad).IsError {
			t.Fatalf("accepted %s", bad)
		}
	}
	if modelCalls != 0 || prepCalls != 0 {
		t.Fatal("invalid source reached processing")
	}
	const args = `{"path":"inbox/notes.pdf","options":{"page":2}}`
	res := call(args)
	if res.IsError {
		t.Fatalf("%+v", res)
	}
	got := res.Structured.(fileResult)
	if got.Status != "complete" || len(got.Outputs) != 1 || len(got.Warnings) == 0 {
		t.Fatalf("%+v", got)
	}
	saved := files.files[strings.TrimPrefix(got.Outputs[0].Path, workspace)]
	if saved != text+"\n" {
		t.Fatalf("transcript changed: %q", saved)
	}
	meta := files.files[strings.TrimPrefix(got.MetadataPath, workspace)]
	for _, want := range []string{`"review_status": "unreviewed"`, `"page": 2`, `"document_pages": 3`, `"source_sha256"`, `"prepared_sha256"`} {
		if !strings.Contains(meta, want) {
			t.Fatalf("missing %s: %s", want, meta)
		}
	}
	for _, scenario := range []struct{ text, finish string }{{"unfinished", "length"}, {"", "stop"}, {strings.Repeat("A repeated line with many letters\n", 4), "stop"}} {
		text, finish = scenario.text, scenario.finish
		res = call(args)
		if res.IsError || res.Structured.(fileResult).Status != "partial" {
			t.Fatalf("lost partial: %+v", res)
		}
	}
	finish, text = "stop", "valid text"
	files.failAt = files.writes + 2
	res = call(args)
	if res.IsError || res.Structured.(fileResult).Status != "partial" || len(res.Structured.(fileResult).Outputs) != 1 {
		t.Fatal("saved transcript lost after metadata failure")
	}
}

func TestHandwritingDiscoveryAndDirectGate(t *testing.T) {
	s, _ := mediaServer(t, fakeProcess)
	list := func() string {
		return rpc(s, "10.90.1.10:40000", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`).Body.String()
	}
	if strings.Contains(list(), `"name":"handwriting_to_text"`) {
		t.Fatal("unconfigured OCR listed")
	}
	s.Transcribe = func(context.Context, store.User, string, []byte) (llm.Transcription, error) {
		t.Fatal("disabled OCR called")
		return llm.Transcription{}, nil
	}
	if !strings.Contains(list(), `"name":"handwriting_to_text"`) {
		t.Fatal("configured OCR missing")
	}
	s.Grants = func(context.Context, string, string) (map[string]bool, map[string]string) {
		return map[string]bool{}, map[string]string{"handwriting_to_text": "File tools"}
	}
	if strings.Contains(list(), `"name":"handwriting_to_text"`) {
		t.Fatal("disabled OCR listed")
	}
	out := rpc(s, "10.90.1.10:40000", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"handwriting_to_text","arguments":{"path":"inbox/notes.pdf","options":{"page":1}},"_meta":{"ai.opencode/sessionID":"ses_abcdefgh12"}}}`).Body.String()
	if !strings.Contains(out, `"isError":true`) {
		t.Fatalf("catalog bypass: %s", out)
	}
}
