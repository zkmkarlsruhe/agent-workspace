// SPDX-License-Identifier: MIT

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/filetools"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/testinstance"
)

const sampleSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 50"><rect width="100" height="50" fill="#345678"/></svg>`

type mediaFiles struct {
	*fakeFiles
	failAt, writes int
}

func (f *mediaFiles) WriteNew(ctx context.Context, uid, project, rel string, r io.Reader) (string, error) {
	f.writes++
	if f.failAt == f.writes {
		return "", fmt.Errorf("disk full")
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	if _, exists := f.files[project+"/"+rel]; exists {
		return "", fmt.Errorf("would overwrite")
	}
	f.files[project+"/"+rel] = string(data)
	f.written[project+"/"+rel] = data
	return rel, nil
}

func mediaServer(t *testing.T, process filetools.Process) (*Server, *mediaFiles) {
	t.Helper()
	d, err := testinstance.Load("../../../../instance/example/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	d.Integrations["file-tools"] = true
	d.IntegrationGrants["file-tools"] = []string{"workspace:read", "workspace:write"}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(instance.Select(d))
	f := &mediaFiles{fakeFiles: &fakeFiles{dir: "/workspace/bericht", files: map[string]string{"bericht/inbox/logo.svg": sampleSVG, "other/logo.svg": sampleSVG}, written: map[string][]byte{}}}
	s := New(Server{SearXNG: mustURL("http://127.0.0.1:1"), SlotOf: config.Config{UserNetPrefix: "10.90"}.SlotOf, Users: users{}, Usage: &rec{}, Log: slog.New(slog.DiscardHandler), Files: f, ProcessFiles: process})
	return s, f
}

func fakeProcess(context.Context, string, []byte, map[string]any) (filetools.Result, error) {
	return filetools.Result{Status: "complete", Outputs: []filetools.Output{
		{Data: []byte(sampleSVG), Extension: ".svg", MediaType: "image/svg+xml", Role: "result"},
		{Data: []byte(pngOf(100, 50)), Extension: ".png", MediaType: "image/png", Role: "preview"},
	}, Metadata: map[string]any{"engine": "fixture"}}, nil
}

func TestFileToolBoundariesAndPartialWrite(t *testing.T) {
	calls := 0
	s, files := mediaServer(t, func(ctx context.Context, op string, data []byte, opts map[string]any) (filetools.Result, error) {
		calls++
		return fakeProcess(ctx, op, data, opts)
	})
	u := store.User{UID: "u1"}
	for _, args := range []map[string]any{
		{"path": "/workspace/other/logo.svg", "svg": sampleSVG, "source_sha256": digest([]byte(sampleSVG))},
		{"path": "../logo.svg", "svg": sampleSVG},
		{"path": "inbox/logo.svg", "svg": sampleSVG, "source_sha256": "stale"},
	} {
		b, _ := json.Marshal(args)
		res, _ := s.fileTool(context.Background(), u, "ses_abcdefgh12", "svg_edit", b)
		if !res.IsError {
			t.Fatalf("accepted invalid source: %s", b)
		}
	}
	if calls != 0 {
		t.Fatal("processor called for refused source")
	}
	args, _ := json.Marshal(map[string]any{"svg": sampleSVG})
	res, _ := s.fileTool(context.Background(), u, "", "svg_create", args)
	if !res.IsError {
		t.Fatal("accepted missing session")
	}
	res, _ = s.fileTool(context.Background(), u, "ses_abcdefgh12", "svg_create", args)
	result := res.Structured.(fileResult)
	if result.Status != "complete" || len(result.Outputs) != 2 || result.MetadataPath == "" {
		t.Fatalf("result: %+v", result)
	}
	if result.Outputs[0].SHA256 != digest([]byte(sampleSVG)) {
		t.Fatal("bad output hash")
	}
	if string(files.written[strings.TrimPrefix(result.MetadataPath, workspace)]) == "" {
		t.Fatal("metadata missing")
	}
	files.failAt = files.writes + 2
	res, _ = s.fileTool(context.Background(), u, "ses_abcdefgh12", "svg_create", args)
	partial := res.Structured.(fileResult)
	if partial.Status != "partial" || len(partial.Outputs) != 1 || len(partial.Warnings) == 0 {
		t.Fatalf("lost partial output: %+v", partial)
	}
}

func TestFileToolCatalogGate(t *testing.T) {
	s, _ := mediaServer(t, fakeProcess)
	s.Grants = func(context.Context, string, string) (map[string]bool, map[string]string) {
		return map[string]bool{}, map[string]string{"svg_create": "File tools"}
	}
	list := rpc(s, "10.90.1.10:40000", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`).Body.String()
	if strings.Contains(list, `"name":"svg_create"`) {
		t.Fatal("disabled tool listed")
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "svg_create", "arguments": map[string]any{"svg": sampleSVG}, "_meta": map[string]any{"ai.opencode/sessionID": "ses_abcdefgh12"}}})
	res := rpc(s, "10.90.1.10:40000", string(body)).Body.String()
	if !strings.Contains(res, `"isError":true`) {
		t.Fatalf("catalog bypass: %s", res)
	}
	s.Grants = nil
	list = rpc(s, "10.90.1.10:40000", `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`).Body.String()
	for _, tool := range fileTools {
		if !strings.Contains(list, `"name":"`+tool["name"].(string)+`"`) {
			t.Fatalf("missing %v", tool["name"])
		}
	}
}

func TestImageEditPreservesAlphaResolutionAndProject(t *testing.T) {
	s, files := mediaServer(t, fakeProcess)
	s.CanEdit = true
	img := image.NewNRGBA(image.Rect(0, 0, 2000, 20))
	img.SetNRGBA(10, 10, color.NRGBA{R: 255, A: 128})
	var src bytes.Buffer
	png.Encode(&src, img)
	files.files["bericht/inbox/source.png"] = src.String()
	calls := 0
	s.Generate = func(_ context.Context, _ store.User, _, _, _ string, data []byte) ([]byte, string, error) {
		calls++
		decoded, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if decoded.Bounds().Dx() != 2000 {
			t.Fatal("edit source resized")
		}
		_, _, _, alpha := decoded.At(10, 10).RGBA()
		if alpha != 128*257 {
			t.Fatalf("alpha lost: %d", alpha)
		}
		return []byte(pngOf(10, 10)), "fixture", nil
	}
	args := json.RawMessage(`{"source_image":"inbox/source.png","prompt":"change red to blue"}`)
	res, _ := s.imageGenerate(context.Background(), store.User{UID: "u1"}, "ses_abcdefgh12", args)
	if res.IsError || calls != 1 {
		t.Fatalf("edit failed: %+v", res)
	}
	args = json.RawMessage(`{"source_image":"/workspace/other/source.png","prompt":"change color"}`)
	res, _ = s.imageGenerate(context.Background(), store.User{UID: "u1"}, "ses_abcdefgh12", args)
	if !res.IsError || calls != 1 {
		t.Fatal("cross-project image read")
	}
}

// Run against the built worker with FILE_TOOLS_TEST_URL to verify real byte/file chaining.
func TestFileToolsLiveChain(t *testing.T) {
	base := os.Getenv("FILE_TOOLS_TEST_URL")
	if base == "" {
		t.Skip("set FILE_TOOLS_TEST_URL for the local worker integration test")
	}
	s, files := mediaServer(t, filetools.New(base))
	call := func(op string, args map[string]any) fileResult {
		t.Helper()
		raw, _ := json.Marshal(args)
		res, _ := s.fileTool(context.Background(), store.User{UID: "u1"}, "ses_abcdefgh12", op, raw)
		if res.IsError {
			t.Fatalf("%s: %+v", op, res)
		}
		r := res.Structured.(fileResult)
		if r.Status != "complete" {
			t.Fatalf("%s: %+v", op, r)
		}
		return r
	}
	created := call("svg_create", map[string]any{"svg": sampleSVG})
	resized := call("image_transform", map[string]any{"path": created.Outputs[1].Path, "options": map[string]any{"width": 100}})
	traced := call("raster_to_vector", map[string]any{"path": resized.Outputs[0].Path})
	data := files.files[strings.TrimPrefix(traced.Outputs[0].Path, workspace)]
	call("svg_edit", map[string]any{"path": traced.Outputs[0].Path, "svg": data, "source_sha256": traced.Outputs[0].SHA256})
	cutout := call("background_remove", map[string]any{"path": created.Outputs[1].Path})
	decoded, err := png.Decode(strings.NewReader(files.files[strings.TrimPrefix(cutout.Outputs[0].Path, workspace)]))
	if err != nil || decoded.Bounds().Dx() != 1024 {
		t.Fatalf("bad cutout: %v", err)
	}
}
