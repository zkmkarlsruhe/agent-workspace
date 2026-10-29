// SPDX-License-Identifier: MIT

package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// ImageReserveEUR is the legacy default; runtime charges use accounting.image_eur.
const ImageReserveEUR = 0.10

const maxImageResponse = 48 << 20

// Image generates one picture with the first of models that answers, or
// edits src when it is set (/v1/images/edits). Every attempt is reserved
// and recorded on its own; a refused budget stops the round.
func (g *Gateway) Image(ctx context.Context, u store.User, session string, models []string, prompt, size string, src []byte) ([]byte, string, error) {
	err := errors.New("image: no model configured")
	for _, m := range models {
		var img []byte
		if img, err = g.image(ctx, u, session, m, prompt, size, src); err == nil {
			return img, m, nil
		}
		if errors.Is(err, ErrDayCap) || errors.Is(err, ErrBusy) || ctx.Err() != nil {
			break
		}
		g.Log.WarnContext(ctx, "image: model failed", "upstream", "llm", "model", m, "err", err)
	}
	return nil, "", err
}

func (g *Gateway) image(ctx context.Context, u store.User, session, model, prompt, size string, src []byte) ([]byte, error) {
	if u.UID != store.ManagerUID {
		release, err := g.Budget.ReserveEUR(ctx, u.UID, g.accounting().ImageEUR)
		if err != nil {
			return nil, err
		}
		defer release() // after g.record below
	}
	route, ctype := "/v1/images/generations", "application/json"
	var body bytes.Buffer
	if src == nil {
		json.NewEncoder(&body).Encode(map[string]any{"model": model, "prompt": prompt, "size": size, "n": 1, "response_format": "b64_json"})
	} else {
		route = "/v1/images/edits"
		mw := multipart.NewWriter(&body)
		for k, v := range map[string]string{"model": model, "prompt": prompt, "size": size, "n": "1", "response_format": "b64_json"} {
			mw.WriteField(k, v)
		}
		fw, _ := mw.CreateFormFile("image", "vorlage.png")
		fw.Write(src)
		mw.Close()
		ctype = mw.FormDataContentType()
	}
	var sent atomic.Bool
	req, err := http.NewRequestWithContext(dispatched(ctx, &sent), "POST", g.Upstream.JoinPath(route).String(), &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", ctype)
	id, _ := uuid.NewV7()
	g.upstreamHeaders(ctx, req.Header, u, session, id.String())
	ev := store.UsageEvent{EventID: id.String(), Start: time.Now(), UID: u.UID, SessionID: session, RootSessionID: session,
		Capability: "image", Target: model, Status: http.StatusBadGateway}
	estimate := func() {
		c := g.accounting().ImageEUR
		ev.CostEUR, ev.CostSource = &c, "estimate"
	}
	resp, err := g.proxy.Transport.RoundTrip(req)
	if err != nil {
		if sent.Load() { // LiteLLM may have run (and billed) it
			estimate()
		}
		g.record(ctx, ev)
		return nil, err
	}
	defer resp.Body.Close()
	g.responseUsage(&ev, resp)
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxImageResponse))
	m := &meter{ev: ev, accounting: g.accounting()}
	m.apply(raw)
	ev = m.ev
	if ev.CostEUR == nil && resp.StatusCode/100 == 2 {
		estimate()
	}
	g.record(ctx, ev)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("image: HTTP %d: %.300s", resp.StatusCode, raw)
	}
	var r struct {
		Data []struct {
			B64 string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &r); err != nil || len(r.Data) == 0 || r.Data[0].B64 == "" {
		return nil, errors.Join(err, errors.New("image: no b64_json in the answer"))
	}
	return base64.StdEncoding.DecodeString(r.Data[0].B64)
}
