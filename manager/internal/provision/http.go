// SPDX-License-Identifier: MIT

package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/logx"
)

type request struct {
	UID  string `json:"uid"`
	Slot int    `json:"slot"`
}

type usage struct {
	Used  int64 `json:"used"`
	Limit int64 `json:"limit"`
}

// Handler serves POST /ensure and POST /purge {"uid","slot"}: 204 done, 400
// invalid, 409 conflict; and GET /usage?slot=<n>: 200 {"used","limit"} in bytes, 400 invalid.
func Handler(p *Provisioner, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ensure", write(log, "ensured", p.Ensure))
	mux.HandleFunc("POST /purge", write(log, "purged", p.Purge))
	mux.HandleFunc("GET /usage", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()["slot"]
		slot, err := 0, error(ErrInvalid)
		if len(q) == 1 && len(q[0]) <= 3 {
			slot, err = strconv.Atoi(q[0])
		}
		if err != nil || strconv.Itoa(slot) != q[0] { // plain decimal only: no sign, no leading zero
			http.Error(w, "slot must be a decimal integer", http.StatusBadRequest)
			return
		}
		used, limit, err := p.Usage(slot)
		switch {
		case err == nil:
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(usage{used, limit})
			log.DebugContext(r.Context(), "usage", "op", "usage", "slot", slot, "used", used, "limit", limit)
			return
		case errors.Is(err, ErrInvalid):
			w.WriteHeader(http.StatusBadRequest)
		default:
			w.WriteHeader(http.StatusInternalServerError)
			log.ErrorContext(r.Context(), "usage failed", "op", "usage", "slot", slot, "err", err)
		}
		io.WriteString(w, err.Error())
	})
	return mux
}

// write serves one of the root operations on {"uid","slot"}.
func write(log *slog.Logger, done string, op func(uid string, slot int) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req request
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		err := op(req.UID, req.Slot)
		switch {
		case err == nil:
			log.InfoContext(r.Context(), done, "op", done, "uid", req.UID, "slot", req.Slot)
			w.WriteHeader(http.StatusNoContent)
			return
		case errors.Is(err, ErrInvalid):
			w.WriteHeader(http.StatusBadRequest)
		case errors.Is(err, ErrConflict):
			w.WriteHeader(http.StatusConflict)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
		log.ErrorContext(r.Context(), done+" failed", "op", done, "uid", req.UID, "slot", req.Slot, "err", err)
		io.WriteString(w, err.Error())
	}
}

// Client calls oc-provision over its unix socket.
type Client struct{ h *http.Client }

func NewClient(socket string) *Client {
	return &Client{h: &http.Client{
		Timeout: 10 * time.Minute, // purge of a full 20 GiB tree; callers bound ensure/usage by ctx
		Transport: logx.Transport{Upstream: "oc-provision", Propagate: true, Body: true,
			Next: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			}}},
	}}
}

func (c *Client) Ensure(ctx context.Context, uid string, slot int) error {
	return c.post(ctx, "ensure", uid, slot)
}

// Purge deletes the person's data dir and clears the slot's quota limit.
func (c *Client) Purge(ctx context.Context, uid string, slot int) error {
	return c.post(ctx, "purge", uid, slot)
}

func (c *Client) post(ctx context.Context, op, uid string, slot int) error {
	body, _ := json.Marshal(request{UID: uid, Slot: slot})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://oc-provision/"+op, bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp, err := c.h.Do(req)
	if err != nil {
		return fmt.Errorf("provision: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("provision %s: %s: %s", op, resp.Status, bytes.TrimSpace(msg))
}

// Ping checks that oc-provision answers on its socket: any HTTP answer
// (here 404) proves socket permissions and the service.
func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://oc-provision/", nil)
	if err != nil {
		return err
	}
	resp, err := c.h.Do(req)
	if err != nil {
		return fmt.Errorf("provision: %w", err)
	}
	return resp.Body.Close()
}

// Usage returns bytes used and the hard limit of slot's project.
func (c *Client) Usage(ctx context.Context, slot int) (used, limit int64, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://oc-provision/usage?slot="+strconv.Itoa(slot), nil)
	if err != nil {
		return 0, 0, err
	}
	resp, err := c.h.Do(req)
	if err != nil {
		return 0, 0, fmt.Errorf("provision: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return 0, 0, fmt.Errorf("provision: %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	var u usage
	err = json.NewDecoder(io.LimitReader(resp.Body, 1024)).Decode(&u)
	return u.Used, u.Limit, err
}
