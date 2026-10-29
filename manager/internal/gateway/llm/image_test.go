// SPDX-License-Identifier: MIT

package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// Image falls through the models in order, records every attempt with its
// own cost and edits through multipart with the template as field "image".
func TestImageFallbackAndEdit(t *testing.T) {
	var paths, models []string
	var editImage []byte
	g, rec := gateway(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-litellm-end-user-id") != "alice" || r.Header.Get("Authorization") != "Bearer sk-unified" {
			w.WriteHeader(401)
			return
		}
		paths = append(paths, r.URL.Path)
		var model string
		if r.URL.Path == "/v1/images/edits" {
			model = r.FormValue("model")
			f, _, err := r.FormFile("image")
			if err != nil {
				t.Error(err)
			} else {
				editImage, _ = io.ReadAll(f)
			}
		} else {
			var b struct {
				Model, Size    string
				ResponseFormat string `json:"response_format"`
			}
			json.NewDecoder(r.Body).Decode(&b)
			model = b.Model
			if b.Size == "" || b.ResponseFormat != "b64_json" {
				t.Errorf("size %q", b.Size)
			}
		}
		models = append(models, model)
		switch model {
		case "workspace/eu-kaputt":
			w.WriteHeader(403)
			io.WriteString(w, `{"error":{"message":"not available for this API key"}}`)
		case "workspace/eu-bild":
			w.Header().Set("x-litellm-response-cost", "0.04")
			io.WriteString(w, `{"data":[{"b64_json":"`+base64.StdEncoding.EncodeToString([]byte("PNGDATA"))+`"}]}`)
		default: // no cost reported
			io.WriteString(w, `{"data":[{"b64_json":"`+base64.StdEncoding.EncodeToString([]byte("EDIT"))+`"}]}`)
		}
	})
	u := store.User{UID: "u1", Username: "alice"}
	img, model, err := g.Image(context.Background(), u, "ses_abcdefgh12", []string{"workspace/eu-kaputt", "workspace/eu-bild", "workspace/us-bild"}, "ein Fahrrad", "1536x1024", nil)
	if err != nil || string(img) != "PNGDATA" || model != "workspace/eu-bild" {
		t.Fatal(string(img), model, err)
	}
	if len(models) != 2 || len(rec.ev) != 2 {
		t.Fatalf("models %v events %+v", models, rec.ev)
	}
	if e := rec.ev[0]; e.Status != 403 || e.CostEUR != nil || e.Capability != "image" {
		t.Errorf("refused attempt %+v", e)
	}
	if e := rec.ev[1]; e.Status != 200 || *e.CostEUR != 0.04 || e.CostSource != "header" || e.Target != "workspace/eu-bild" || e.SessionID != "ses_abcdefgh12" {
		t.Errorf("answered attempt %+v", e)
	}

	img, _, err = g.Image(context.Background(), u, "", []string{"workspace/edit"}, "rot statt blau", "1536x1024", []byte("VORLAGE"))
	if err != nil || string(img) != "EDIT" || paths[len(paths)-1] != "/v1/images/edits" || string(editImage) != "VORLAGE" {
		t.Fatal(string(img), paths, string(editImage), err)
	}
	if e := rec.ev[2]; *e.CostEUR != ImageReserveEUR || e.CostSource != "estimate" {
		t.Errorf("edit without reported cost %+v", e)
	}

	if _, _, err := g.Image(context.Background(), u, "", []string{"workspace/eu-kaputt"}, "x", "1024x1024", nil); err == nil {
		t.Error("all models failed: want error")
	}
	n := len(rec.ev)
	g.Budget = &Budget{Spent: func(context.Context, string, time.Time) (float64, error) { return 5, nil }, CapEUR: 5, Now: time.Now}
	if _, _, err := g.Image(context.Background(), u, "", []string{"workspace/eu-bild", "workspace/us-bild"}, "x", "1024x1024", nil); !errors.Is(err, ErrDayCap) {
		t.Fatalf("over the cap: %v", err)
	}
	if len(rec.ev) != n {
		t.Errorf("refused calls reached LiteLLM: %+v", rec.ev[n:])
	}
}
