// SPDX-License-Identifier: MIT

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/gateway/llm"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

func pngOf(w, h int) string {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.NRGBA{255, 0, 0, 255}) // rest transparent: must come out white
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.String()
}

func TestImageView(t *testing.T) {
	r := &rec{}
	ff := &fakeFiles{dir: "/workspace/bericht", files: map[string]string{
		"bericht/inbox/gross.png":  pngOf(4000, 1000),
		"bericht/inbox/kaputt.jpg": "kein Bild",
		"bericht/inbox/riesig.png": strings.Repeat("x", maxImageBytes+1),
	}}
	var gotQ string
	var gotImg []byte
	var lookErr error
	s := New(Server{SearXNG: &url.URL{}, SlotOf: config.Config{UserNetPrefix: "10.90"}.SlotOf, Users: users{}, Usage: r,
		Log: slog.New(slog.DiscardHandler), Files: ff,
		Look: func(_ context.Context, u store.User, session, q string, img []byte) (string, error) {
			gotQ, gotImg = q, img
			return "Ein roter Punkt auf Weiß.", lookErr
		}})
	const me = "10.90.1.10:40000"
	if w := rpc(s, me, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); !strings.Contains(w.Body.String(), `"image_view"`) {
		t.Fatalf("tools/list lacks image_view: %s", w.Body)
	}
	call := func(args string) toolResult {
		w := rpc(s, me, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"image_view","arguments":`+args+`,"_meta":{"ai.opencode/sessionID":"ses_abcdefgh12"}}}`)
		var resp struct{ Result toolResult }
		json.Unmarshal(w.Body.Bytes(), &resp)
		return resp.Result
	}

	res := call(`{"path":"inbox/gross.png"}`)
	if res.IsError || !strings.Contains(res.Content[0].Text, "/workspace/bericht/inbox/gross.png") || !strings.HasSuffix(res.Content[0].Text, "Ein roter Punkt auf Weiß.") {
		t.Fatalf("result %+v", res)
	}
	if gotQ != de[defaultQuestion] {
		t.Errorf("default question %q", gotQ)
	}
	img, err := jpeg.Decode(bytes.NewReader(gotImg))
	if err != nil || img.Bounds().Dx() != imageSide || img.Bounds().Dy() != imageSide/4 {
		t.Fatalf("sent image: %v %v", err, img)
	}
	if c := color.GrayModel.Convert(img.At(imageSide/2, 100)).(color.Gray); c.Y < 240 {
		t.Errorf("transparency not on white: %v", c)
	}
	if len(r.ev) != 1 || r.ev[0].Capability != "image" || r.ev[0].Status != 200 {
		t.Errorf("usage %+v", r.ev)
	}
	if call(`{"path":"inbox/gross.png","question":"Welche Farbe?"}`); gotQ != "Welche Farbe?" {
		t.Errorf("question %q", gotQ)
	}

	gotImg = nil
	for args, want := range map[string]string{
		`{"path":"inbox/riesig.png"}`: "zu groß",
		`{"path":"inbox/kaputt.jpg"}`: "kein lesbares",
		`{"path":"inbox/foto.heic"}`:  "HEIC",
		`{"path":"inbox/foto.bmp"}`:   "PNG, JPG, WebP oder GIF",
		`{"path":"../x.png"}`:         "keine Punkt-Ordner",
		`{"path":"inbox/fehlt.png"}`:  "gibt es nicht",
		`{}`:                          "path fehlt",
		`{"path":"inbox/gross.png","question":"` + strings.Repeat("a", maxQuestion+1) + `"}`: "2000 Zeichen",
	} {
		if res := call(args); !res.IsError || !strings.Contains(res.Content[0].Text, want) {
			t.Errorf("%.60s: want error %q, got %+v", args, want, res)
		}
	}
	if gotImg != nil {
		t.Error("refused file reached the model")
	}

	lookErr = llm.ErrDayCap
	if res := call(`{"path":"inbox/gross.png"}`); !res.IsError || !strings.Contains(res.Content[0].Text, "Tagesbudget") {
		t.Errorf("budget: %+v", res)
	}
	if last := r.ev[len(r.ev)-1]; last.Status != 429 {
		t.Errorf("budget status %d", last.Status)
	}

	off := New(Server{SearXNG: &url.URL{}, SlotOf: config.Config{UserNetPrefix: "10.90"}.SlotOf, Users: users{}, Usage: r, Log: slog.New(slog.DiscardHandler)})
	if w := rpc(off, me, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); strings.Contains(w.Body.String(), "image_view") {
		t.Error("image_view listed without Look")
	}
}
