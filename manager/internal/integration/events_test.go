// SPDX-License-Identifier: MIT

package integration

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestEventWireContract(t *testing.T) {
	for _, tt := range []struct {
		typ     EventType
		name    string
		payload any
		wire    string
	}{
		{ChatFinished, "chat.finished", ChatFinishedPayload{"chat-1", "project-1", "session-1"}, `{"chat_id":"chat-1","project_id":"project-1","session_id":"session-1"}`},
		{AccountDeleting, "account.deleting", AccountDeletingPayload{"delete-1"}, `{"operation_id":"delete-1"}`},
		{ConsentChanged, "consent.changed", ConsentChangedPayload{"evaluation", 2, false}, `{"scope":"evaluation","version":2,"accepted":false}`},
		{UsageRecorded, "usage.recorded", UsageRecordedPayload{"usage-1"}, `{"usage_id":"usage-1"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if string(tt.typ) != tt.name || !knownEvent(tt.typ) {
				t.Fatalf("event constant = %q, want %q", tt.typ, tt.name)
			}
			data, err := json.Marshal(tt.payload)
			if err != nil || string(data) != tt.wire {
				t.Fatalf("payload = %s, %v; want %s", data, err, tt.wire)
			}
			event := Event{Schema: 1, ID: "event-1", Type: tt.typ, UID: "person-1", Generation: 7,
				OccurredAt: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC), Data: data}
			b, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			want := `{"schema":1,"id":"event-1","type":"` + tt.name + `","uid":"person-1","generation":7,"occurred_at":"2026-10-27T12:00:00Z","data":` + tt.wire + `}`
			if string(b) != want {
				t.Fatalf("envelope = %s, want %s", b, want)
			}
			var got Event
			if err := json.Unmarshal(b, &got); err != nil || !reflect.DeepEqual(event, got) {
				t.Fatalf("round trip = %+v, %v", got, err)
			}
		})
	}
	if knownEvent("session.compacted") {
		t.Fatal("upstream runtime event accepted as a service event")
	}
}

func TestEventReadersIgnoreAdditions(t *testing.T) {
	var event Event
	if err := json.Unmarshal([]byte(`{"schema":1,"id":"e","type":"future.event","uid":"u","generation":0,"occurred_at":"2026-10-27T12:00:00Z","data":{"future":true},"new_field":42}`), &event); err != nil {
		t.Fatal(err)
	}
	if event.Type != "future.event" || event.ID != "e" {
		t.Fatalf("envelope lost known fields: %+v", event)
	}
	var payload ChatFinishedPayload
	if err := json.Unmarshal([]byte(`{"chat_id":"c","project_id":"p","session_id":"s","new_field":42}`), &payload); err != nil || payload.ChatID != "c" {
		t.Fatalf("additive payload = %+v, %v", payload, err)
	}
}
