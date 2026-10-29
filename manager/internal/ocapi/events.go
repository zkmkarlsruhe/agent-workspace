// SPDX-License-Identifier: MIT

package ocapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	sse "github.com/tmaxmax/go-sse"
)

// Event is one frame of GET /api/event: `data: {"id","type","location"?,"data"}`.
// Location is optional; it names the folder the event belongs to.
// Sync uses the envelope; the watchdog decodes only session activity metadata.
type Event struct {
	Type     string          `json:"type"`
	Data     json.RawMessage `json:"data"`
	Location struct {
		Directory string `json:"directory"`
	} `json:"location"`
}

// Events follows the event stream of all locations until ctx ends or the
// stream breaks and calls fn for every frame. Heartbeats (": heartbeat") and
// frames that are not JSON are skipped. The stream has no timeout, so the
// client's own Timeout is not used.
func (c *Client) Events(ctx context.Context, fn func(Event)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.BaseURL, "/")+"/api/event", nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth("opencode", c.Password)
	req.Header.Set("Accept", "text/event-stream")
	hc := c.client()
	hc.Timeout = 0
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &StatusError{Code: resp.StatusCode, Body: string(b)}
	}
	return readEvents(resp.Body, fn)
}

// maxFrame bounds one event across its data lines; the stream is untrusted.
const maxFrame = 16 << 20

var errFrameTooLarge = errors.New("opencode: event frame too large")

func readEvents(r io.Reader, fn func(Event)) error {
	for e, err := range sse.Read(r, &sse.ReadConfig{MaxEventSize: maxFrame}) {
		if errors.Is(err, bufio.ErrTooLong) {
			return errFrameTooLarge
		} else if err != nil {
			return err
		}
		var ev Event
		if json.Unmarshal([]byte(e.Data), &ev) == nil && ev.Type != "" {
			fn(ev)
		}
	}
	return io.ErrUnexpectedEOF // sse.Read ends without error at EOF; the stream has no end
}
