// SPDX-License-Identifier: MIT

package ocapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, _ := r.BasicAuth(); u != "opencode" || p != "pw" || r.URL.Path != "/api/event" {
			http.Error(w, "no", 401)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"id":"e1","type":"server.connected","data":{}}`+"\n\n")
		io.WriteString(w, ": heartbeat\n\n")
		io.WriteString(w, `data: {"id":"e2","type":"session.tool.success","location":{"directory":"/workspace/bericht"},"data":{"sessionID":"ses_1","assistantMessageID":"msg_1","ordinal":0,"content":[{"type":"text","text":"done"}],"executed":true,"native":{"provider":"replay"}}}`+"\n\n")
		io.WriteString(w, "data: not json\n\n")
		io.WriteString(w, `data: {"id":"e3","type":"filesystem.changed",`+"\n"+`data: "data":{"file":"a","event":"add"}}`+"\n\n")
		io.WriteString(w, `data: {"id":"e4","type":"session.idle","location":{"directory":"/workspace/bericht"},"data":{"sessionID":"ses_1"}}`+"\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Password: "pw", HTTP: &http.Client{Timeout: time.Millisecond}} // Timeout must not apply
	var got []Event
	err := c.Events(context.Background(), func(e Event) { got = append(got, e) })
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("end of stream: %v", err)
	}
	if len(got) != 4 || got[1].Type != "session.tool.success" || got[1].Location.Directory != "/workspace/bericht" ||
		got[2].Type != "filesystem.changed" || got[2].Location.Directory != "" || got[3].Type != "session.idle" ||
		got[3].Location.Directory != "/workspace/bericht" {
		t.Fatalf("%+v", got)
	}
	var data struct {
		SessionID string `json:"sessionID"`
	}
	if err := json.Unmarshal(got[1].Data, &data); err != nil || data.SessionID != "ses_1" {
		t.Fatalf("watchdog session metadata: %s, %v", got[1].Data, err)
	}
	c.Password = "bad"
	if err := c.Events(context.Background(), func(Event) {}); !IsStatus(err, 401) {
		t.Fatalf("401: %v", err)
	}
}
