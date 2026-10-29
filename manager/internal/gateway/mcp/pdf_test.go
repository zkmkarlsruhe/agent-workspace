// SPDX-License-Identifier: MIT

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/gateway/llm"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle"
)

func TestResolveFile(t *testing.T) {
	const sess = "/workspace/bericht"
	for _, c := range []struct {
		file, dir, project, rel string
	}{
		{"results/a.md", sess, "bericht", "results/a.md"},
		{"./results/a.md", sess, "bericht", "results/a.md"},
		{"a.MD", sess, "bericht", "a.MD"},
		{"/workspace/anderes/results/a.md", "", "anderes", "results/a.md"},
		{" /workspace/bericht/b.md ", "", "bericht", "b.md"},
		{"results/../../x/a.md", sess, "", ""},
		{"../a.md", sess, "", ""},
		{"/workspace/bericht/../x/a.md", "", "", ""},
		{"/workspace/bericht//a.md", "", "", ""},
		{"/workspace/.trash/x/a.md", "", "", ""},
		{"/workspace/bericht/.project/brief.md", "", "", ""},
		{".project/brief.md", sess, "", ""},
		{"results/.a.md", sess, "", ""},
		{"/etc/opencode/AGENTS.md", "", "", ""},
		{"/workspace/a.md", "", "", ""},
		{"/workspace/Bericht/a.md", "", "", ""},
		{"results/a.txt", sess, "", ""},
		{"results/a.md", "", "", ""},
		{"results/a.md", "/workspace/a/b", "", ""},
		{"results/a.md", "/home/user", "", ""},
		{"results/a\x00.md", sess, "", ""},
		{"results/", sess, "", ""},
	} {
		p, r, err := resolveFile(c.file, c.dir, ".md")
		if c.project == "" {
			if err == nil {
				t.Errorf("%q: want error, got %q %q", c.file, p, r)
			}
			continue
		}
		if err != nil || p != c.project || r != c.rel {
			t.Errorf("%q: got %q %q %v, want %q %q", c.file, p, r, err, c.project, c.rel)
		}
	}
}

type fakeFiles struct {
	files   map[string]string // "project/rel" -> content
	written map[string][]byte
	dir     string
}

func (f *fakeFiles) OpenFile(_ context.Context, uid, project, rel string) (io.ReadCloser, lifecycle.File, error) {
	c, ok := f.files[project+"/"+rel]
	if uid != "u1" || !ok {
		return nil, lifecycle.File{}, lifecycle.ErrNotFound
	}
	return io.NopCloser(strings.NewReader(c)), lifecycle.File{Size: int64(len(c))}, nil
}

func (f *fakeFiles) WriteNew(_ context.Context, _, project, rel string, r io.Reader) (string, error) {
	if _, ok := f.files[project+"/"+rel]; ok {
		rel = strings.TrimSuffix(rel, ".pdf") + "_v2.pdf"
	}
	b, _ := io.ReadAll(r)
	f.written[project+"/"+rel] = b
	return rel, nil
}

func (f *fakeFiles) SessionDir(_ context.Context, _, id string) (string, error) {
	if id != "ses_abcdefgh12" {
		return "", errors.New("404")
	}
	return f.dir, nil
}

func TestMdToPDF(t *testing.T) {
	r := &rec{}
	ff := &fakeFiles{files: map[string]string{"bericht/results/a.md": "# A", "bericht/results/a.pdf": "alt"}, written: map[string][]byte{}, dir: "/workspace/bericht"}
	var gotMD string
	s := New(Server{SearXNG: &url.URL{}, SlotOf: config.Config{UserNetPrefix: "10.90"}.SlotOf, Users: users{}, Usage: r,
		Log: slog.New(slog.DiscardHandler), Files: ff,
		PDF: func(_ context.Context, md []byte, name string) ([]byte, error) {
			gotMD = string(md) + "|" + name
			return []byte("%PDF-fake"), nil
		}})
	const me = "10.90.1.10:40000"

	w := rpc(s, me, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if !strings.Contains(w.Body.String(), `"md_to_pdf"`) {
		t.Fatalf("tools/list lacks md_to_pdf: %s", w.Body)
	}
	call := func(args string) toolResult {
		w := rpc(s, me, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"md_to_pdf","arguments":`+args+`,"_meta":{"ai.opencode/sessionID":"ses_abcdefgh12"}}}`)
		var resp struct{ Result toolResult }
		json.Unmarshal(w.Body.Bytes(), &resp)
		return resp.Result
	}
	res := call(`{"file":"results/a.md"}`)
	if res.IsError || !strings.Contains(res.Content[0].Text, "/workspace/bericht/results/a_v2.pdf") {
		t.Fatalf("result %+v", res)
	}
	if gotMD != "# A|a.md" || !bytes.Equal(ff.written["bericht/results/a_v2.pdf"], []byte("%PDF-fake")) {
		t.Errorf("md %q written %v", gotMD, ff.written)
	}
	if len(r.ev) != 1 || r.ev[0].Capability != "convert" || r.ev[0].Status != 200 || r.ev[0].SessionID != "ses_abcdefgh12" {
		t.Errorf("usage %+v", r.ev)
	}
	for _, args := range []string{`{"file":"../x.md"}`, `{"file":"/workspace/bericht/fehlt.md"}`, `{"file":"results/a.pdf"}`, `{}`} {
		if res := call(args); !res.IsError {
			t.Errorf("%s: want error, got %+v", args, res)
		}
	}
	if len(ff.written) != 1 {
		t.Errorf("unexpected writes %v", ff.written)
	}

	off := New(Server{SearXNG: &url.URL{}, SlotOf: config.Config{UserNetPrefix: "10.90"}.SlotOf, Users: users{}, Usage: r, Log: slog.New(slog.DiscardHandler)})
	if w := rpc(off, me, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); strings.Contains(w.Body.String(), "md_to_pdf") {
		t.Error("md_to_pdf listed without OC_PDF_URL")
	}
}

func TestPDFBudget(t *testing.T) {
	ctx := context.Background()
	record := &rec{}
	ff := &fakeFiles{files: map[string]string{"bericht/a.md": "# A"}, written: map[string][]byte{}}
	calls, releases := 0, 0
	s := New(Server{Files: ff, Usage: record, Log: slog.New(slog.DiscardHandler), PDF: func(context.Context, []byte, string) ([]byte, error) { calls++; return []byte("pdf"), nil }})
	for _, refusal := range []error{llm.ErrDayCap, llm.ErrBusy, errors.New("unavailable"), nil} {
		s.Reserve = func(_ context.Context, uid string, eur float64) (func(), error) {
			if uid != user.UID || eur != PDFChargeEUR {
				t.Fatalf("reservation: %s %v", uid, eur)
			}
			if refusal != nil {
				return nil, refusal
			}
			return func() {
				if len(record.ev) == 0 || record.ev[len(record.ev)-1].CostEUR == nil || *record.ev[len(record.ev)-1].CostEUR != PDFChargeEUR {
					t.Fatal("released before metering")
				}
				releases++
			}, nil
		}
		res, meta := s.mdToPDF(ctx, user, "", json.RawMessage(`{"file":"/workspace/bericht/a.md"}`))
		if refusal != nil {
			want := http.StatusTooManyRequests
			if !errors.Is(refusal, llm.ErrDayCap) && !errors.Is(refusal, llm.ErrBusy) {
				want = http.StatusInternalServerError
			}
			if !res.IsError || meta.status != want || calls != 0 || len(ff.written) != 0 || meta.release != nil || meta.costEUR != 0 {
				t.Fatalf("refusal: %+v %+v calls=%d", res, meta, calls)
			}
		} else {
			if res.IsError || calls != 1 || meta.costEUR != PDFChargeEUR {
				t.Fatalf("admitted: %+v %+v", res, meta)
			}
			s.meter(ctx, user, "", "md_to_pdf", time.Now(), res, meta)
		}
	}
	if releases != 1 {
		t.Fatalf("releases=%d", releases)
	}
	s.PDFChargeEUR = new(float64(0.01))
	s.Reserve = func(_ context.Context, _ string, eur float64) (func(), error) {
		if eur != 0.01 {
			t.Fatalf("override=%v", eur)
		}
		return func() {}, nil
	}
	res, meta := s.mdToPDF(ctx, user, "", json.RawMessage(`{"file":"/workspace/bericht/a.md"}`))
	if res.IsError || meta.costEUR != 0.01 {
		t.Fatal("charge override failed")
	}
	s.meter(ctx, user, "", "md_to_pdf", time.Now(), res, meta)
}
