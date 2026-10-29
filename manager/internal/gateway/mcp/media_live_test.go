// SPDX-License-Identifier: MIT

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/filetools"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/gateway/llm"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// liveDisk keeps the integration test's files on disk for visual inspection.
// Root prevents paths and symlinks from escaping the dedicated test directory.
type liveDisk struct{ root *os.Root }

func (f liveDisk) SessionDir(_ context.Context, _, session string) (string, error) {
	if session == "ses_medialive1234" {
		return "/workspace/bericht", nil
	}
	return "", store.ErrNotFound
}

func (f liveDisk) OpenFile(_ context.Context, _, project, rel string) (io.ReadCloser, lifecycle.File, error) {
	r, err := f.root.Open(filepath.Join(project, rel))
	if err != nil {
		return nil, lifecycle.File{}, err
	}
	info, err := r.Stat()
	if err != nil {
		r.Close()
		return nil, lifecycle.File{}, err
	}
	return r, lifecycle.File{Name: info.Name(), Size: info.Size()}, nil
}

func (f liveDisk) WriteNew(_ context.Context, _, project, rel string, r io.Reader) (string, error) {
	name := filepath.Join(project, rel)
	if err := f.root.MkdirAll(filepath.Dir(name), 0700); err != nil {
		return "", err
	}
	out, err := f.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(out, r)
	closeErr := out.Close()
	if err != nil {
		return "", err
	}
	return rel, closeErr
}

type liveUsers struct{}

func (liveUsers) UserBySlot(context.Context, int) (store.User, error) {
	return store.User{UID: "file-tools-live-test", Username: "file-tools-live-test", Slot: 1}, nil
}

// TestMediaLive opts into billable upstream calls only when all three variables
// are supplied. MEDIA_LIVE_CONFIG is a private JSON object of the deployment's
// provider environment settings; MEDIA_LIVE_DIR holds synthetic fixtures/results.
// FILE_TOOLS_TEST_URL must point to the real, isolated worker.
func TestMediaLive(t *testing.T) {
	configPath, dir, worker := os.Getenv("MEDIA_LIVE_CONFIG"), os.Getenv("MEDIA_LIVE_DIR"), os.Getenv("FILE_TOOLS_TEST_URL")
	if configPath == "" || dir == "" || worker == "" {
		t.Skip("explicit live provider config, artifact directory and worker URL required")
	}
	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]string
	if err := json.Unmarshal(configBytes, &env); err != nil {
		t.Fatal(err)
	}
	value := func(key, fallback string) string {
		if env[key] != "" {
			return env[key]
		}
		return fallback
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	s, _ := mediaServer(t, filetools.New(worker))
	s.Files, s.Users = liveDisk{root}, liveUsers{}
	s.SlotOf = func(ip netip.Addr) (int, bool) { return 1, ip.IsLoopback() }
	s.Lang = func(context.Context, string) (string, error) { return "en", nil }
	logFile, err := os.OpenFile(filepath.Join(dir, "provider.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	s.Log = slog.New(slog.NewJSONHandler(logFile, nil))
	recorder := &rec{}
	s.Usage = recorder
	gateway := llm.New(llm.Gateway{Upstream: mustURL(value("OC_LLM_UPSTREAM_URL", env["LITELLM_BASE_URL"])),
		APIKey: value("OC_LLM_UPSTREAM_KEY", env["LITELLM_API_KEY"]), Flavor: value("OC_LLM_UPSTREAM_FLAVOR", "litellm"),
		Usage: recorder, Log: s.Log, Budget: &llm.Budget{CapEUR: 2, Now: time.Now, Spent: func(context.Context, string, time.Time) (float64, error) {
			recorder.mu.Lock()
			defer recorder.mu.Unlock()
			total := 0.0
			for _, e := range recorder.ev {
				if e.CostEUR != nil {
					total += *e.CostEUR
				}
			}
			return total, nil
		}}})
	s.Generate = func(ctx context.Context, u store.User, session, prompt, size string, source []byte) ([]byte, string, error) {
		models := strings.Split(value("OC_IMAGE_MODELS", "workspace/example-image,workspace/fallback-image"), ",")
		if source != nil {
			models = []string{env["OC_IMAGE_EDIT_MODEL"]}
		}
		return gateway.Image(ctx, u, session, models, prompt, size, source)
	}
	s.CanEdit = env["OC_IMAGE_EDIT_MODEL"] != ""
	s.Look = func(ctx context.Context, u store.User, session, question string, jpeg []byte) (string, error) {
		return gateway.Look(ctx, u, session, env["OC_VISION_MODEL"], question, jpeg, 4096)
	}
	if model := env["OC_HANDWRITING_MODEL"]; model != "" {
		s.Transcribe = func(ctx context.Context, u store.User, session string, png []byte) (llm.Transcription, error) {
			return gateway.Transcribe(ctx, u, session, model, png)
		}
	}
	gateway.Users, gateway.SlotOf = s.Users, s.SlotOf
	mux := http.NewServeMux()
	mux.Handle("/v1/", &llm.Gate{SlotOf: s.SlotOf, Users: s.Users, Budget: gateway.Budget,
		Consented: func(context.Context, string) (bool, error) { return true, nil }, Next: gateway})
	mux.Handle("/", s)
	server := httptest.NewServer(mux)
	defer server.Close()
	// A separately launched runtime can use the same real gateway and file
	// adapter. Only loopback requests and the dedicated session are accepted.
	if os.Getenv("MEDIA_LIVE_SERVE") == "1" {
		ready := filepath.Join(dir, "endpoint")
		if err := os.WriteFile(ready, []byte(server.URL), 0600); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(ready)
		t.Logf("live harness listening at %s", server.URL)
		deadline := time.NewTimer(10 * time.Minute)
		defer deadline.Stop()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-deadline.C:
				t.Fatal("runtime did not finish within ten minutes")
			case <-tick.C:
				if _, err := os.Stat(filepath.Join(dir, "done")); err == nil {
					return
				}
			}
		}
	}
	client := &http.Client{Timeout: 245 * time.Second}
	sequence := 0
	rpcCall := func(t *testing.T, method string, params any) json.RawMessage {
		t.Helper()
		sequence++
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": sequence, "method": method, "params": params})
		req, _ := http.NewRequest("POST", server.URL, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != 200 {
			t.Fatalf("MCP HTTP %d: %.500s", res.StatusCode, raw)
		}
		var envelope struct {
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatal(err)
		}
		if len(envelope.Error) > 0 {
			t.Fatalf("MCP error: %s", envelope.Error)
		}
		return envelope.Result
	}
	call := func(t *testing.T, name string, args map[string]any, wantError bool) fileResult {
		t.Helper()
		started := time.Now()
		raw := rpcCall(t, "tools/call", map[string]any{"name": name, "arguments": args, "_meta": map[string]string{"ai.opencode/sessionID": "ses_medialive1234"}})
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%02d-%s.json", sequence, name)), raw, 0600); err != nil {
			t.Fatal(err)
		}
		var result struct {
			IsError    bool       `json:"isError"`
			Structured fileResult `json:"structuredContent"`
			Content    []content  `json:"content"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatal(err)
		}
		if result.IsError != wantError {
			t.Fatalf("%s error=%t: %.1000s", name, result.IsError, raw)
		}
		for _, output := range result.Structured.Outputs {
			data, err := root.ReadFile(strings.TrimPrefix(output.Path, workspace))
			if err != nil {
				t.Fatal(err)
			}
			if digest(data) != output.SHA256 {
				t.Fatalf("hash mismatch: %s", output.Path)
			}
		}
		t.Logf("%s: %.2fs, status=%s, outputs=%d, expected_error=%t", name, time.Since(started).Seconds(), result.Structured.Status, len(result.Structured.Outputs), wantError)
		return result.Structured
	}
	output := func(t *testing.T, r fileResult, index int) fileArtifact {
		t.Helper()
		if len(r.Outputs) <= index {
			t.Fatal("missing output")
		}
		return r.Outputs[index]
	}
	read := func(t *testing.T, path string) []byte {
		t.Helper()
		data, err := root.ReadFile(strings.TrimPrefix(path, workspace))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	if os.Getenv("HANDWRITING_LIVE") == "1" {
		raw := rpcCall(t, "tools/list", map[string]any{})
		if !bytes.Contains(raw, []byte(`"name":"handwriting_to_text"`)) {
			t.Fatal("handwriting tool not discovered")
		}
		original := read(t, "/workspace/bericht/inbox/notes.pdf")
		call(t, "handwriting_to_text", map[string]any{"path": "inbox/notes.pdf"}, true)
		call(t, "handwriting_to_text", map[string]any{"path": "/workspace/other/notes.pdf", "options": map[string]any{"page": 1}}, true)
		for _, args := range []map[string]any{
			{"path": "inbox/notes.pdf", "options": map[string]any{"page": 1}},
			{"path": "inbox/note.png"},
		} {
			result := call(t, "handwriting_to_text", args, false)
			if result.Status != "complete" {
				t.Fatalf("incomplete transcription: %+v", result)
			}
			text := read(t, output(t, result, 0).Path)
			if !bytes.Contains(text, []byte("MARS")) {
				t.Fatal("fixture heading not transcribed")
			}
			metadata := read(t, result.MetadataPath)
			if !bytes.Contains(metadata, []byte(`"review_status": "unreviewed"`)) || !bytes.Contains(metadata, []byte(`"page": 1`)) {
				t.Fatal("transcription metadata missing")
			}
		}
		if !bytes.Equal(original, read(t, "/workspace/bericht/inbox/notes.pdf")) {
			t.Fatal("source overwritten")
		}
		return
	}
	var created, generated, edited fileArtifact
	t.Run("discovery", func(t *testing.T) {
		raw := rpcCall(t, "tools/list", map[string]any{})
		for _, name := range []string{"image_generate", "image_edit", "image_view", "svg_create", "svg_edit", "image_transform", "raster_to_vector", "background_remove", "pdf_to_text"} {
			if !bytes.Contains(raw, []byte(`"name":"`+name+`"`)) {
				t.Errorf("missing %s", name)
			}
		}
	})
	t.Run("svg_chain", func(t *testing.T) {
		markup := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 400 400"><rect x="80" y="80" width="240" height="240" rx="25" fill="#e52b2b"/><circle cx="200" cy="200" r="65" fill="white"/></svg>`
		r := call(t, "svg_create", map[string]any{"svg": markup}, false)
		created = output(t, r, 0)
		preview := output(t, r, 1)
		resized := call(t, "image_transform", map[string]any{"path": preview.Path, "options": map[string]any{"width": 256, "format": "png"}}, false)
		traced := call(t, "raster_to_vector", map[string]any{"path": output(t, resized, 0).Path}, false)
		if !bytes.Contains(read(t, output(t, traced, 0).Path), []byte("<path")) {
			t.Fatal("no vector paths")
		}
		changed := call(t, "svg_edit", map[string]any{"path": created.Path, "source_sha256": created.SHA256, "svg": strings.ReplaceAll(markup, "#e52b2b", "#2040ee")}, false)
		img, _, err := image.Decode(bytes.NewReader(read(t, output(t, changed, 1).Path)))
		if err != nil {
			t.Fatal(err)
		}
		rc, _, bc, _ := img.At(250, 250).RGBA()
		if bc <= rc {
			t.Fatal("preview did not become blue")
		}
		if digest(read(t, created.Path)) != created.SHA256 {
			t.Fatal("original changed")
		}
		call(t, "svg_edit", map[string]any{"path": created.Path, "source_sha256": "stale", "svg": markup}, true)
		call(t, "svg_create", map[string]any{"svg": `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20"><script>alert(1)</script></svg>`}, true)
		call(t, "image_transform", map[string]any{"path": "/workspace/other/input.png"}, true)
	})
	t.Run("pdf", func(t *testing.T) {
		r := call(t, "pdf_to_text", map[string]any{"path": "inbox/text.pdf"}, false)
		if r.Status != "complete" || !bytes.Contains(read(t, output(t, r, 0).Path), []byte("Invoice 42")) {
			t.Fatal("text missing")
		}
		r = call(t, "pdf_to_text", map[string]any{"path": "inbox/mixed.pdf"}, false)
		if r.Status != "partial" || len(r.Warnings) == 0 {
			t.Fatal("missing mixed PDF warning")
		}
		call(t, "pdf_to_text", map[string]any{"path": "inbox/scan.pdf"}, true)
	})
	t.Run("generate", func(t *testing.T) {
		r := call(t, "image_generate", map[string]any{"prompt": "Studio product photograph: one large solid bright red wooden cube centered on a plain white background, soft shadow, front and top faces visible. No text, no other objects.", "aspect": "square", "filename": "red-cube"}, false)
		generated = output(t, r, 0)
		img, _, err := image.Decode(bytes.NewReader(read(t, generated.Path)))
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("generated dimensions: %v", img.Bounds())
	})
	t.Run("edit", func(t *testing.T) {
		if generated.Path == "" {
			t.Skip("generation failed")
		}
		r := call(t, "image_edit", map[string]any{"source_image": generated.Path, "prompt": "Change only the red wooden cube to vivid royal blue. Keep the cube shape, framing, white background, lighting and shadow unchanged.", "aspect": "square", "filename": "blue-cube"}, false)
		edited = output(t, r, 0)
		img, _, err := image.Decode(bytes.NewReader(read(t, edited.Path)))
		if err != nil {
			t.Fatal(err)
		}
		b := img.Bounds()
		red, _, blue, _ := img.At(b.Dx()/2, b.Dy()/2).RGBA()
		if blue <= red {
			t.Errorf("center pixel not blue: r=%d b=%d", red, blue)
		}
		if digest(read(t, generated.Path)) != generated.SHA256 {
			t.Fatal("source overwritten")
		}
	})
	t.Run("view", func(t *testing.T) {
		if edited.Path == "" {
			t.Skip("edit failed")
		}
		call(t, "image_view", map[string]any{"path": edited.Path, "question": "Describe the object, its color and the background in one short English sentence."}, false)
	})
	t.Run("cutout", func(t *testing.T) {
		if generated.Path == "" {
			t.Skip("generation failed")
		}
		r := call(t, "background_remove", map[string]any{"path": generated.Path}, false)
		if r.Status != "complete" || len(r.Warnings) > 0 {
			t.Fatalf("unexpected foreground result: %+v", r)
		}
		img, _, err := image.Decode(bytes.NewReader(read(t, output(t, r, 0).Path)))
		if err != nil {
			t.Fatal(err)
		}
		b := img.Bounds()
		_, _, _, corner := img.At(0, 0).RGBA()
		_, _, _, center := img.At(b.Dx()/2, b.Dy()/2).RGBA()
		if corner > 1000 || center < 60000 {
			t.Fatalf("bad alpha corner=%d center=%d", corner, center)
		}
		call(t, "image_transform", map[string]any{"path": output(t, r, 0).Path, "options": map[string]any{"width": 320, "format": "webp"}}, false)
	})
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	var summary []map[string]any
	for _, e := range recorder.ev {
		summary = append(summary, map[string]any{"capability": e.Capability, "target": e.Target, "status": e.Status, "cost_eur": e.CostEUR, "cost_source": e.CostSource})
	}
	data, _ := json.MarshalIndent(summary, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "usage.json"), data, 0600); err != nil {
		t.Error(err)
	}
}
