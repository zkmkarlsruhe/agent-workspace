// SPDX-License-Identifier: MIT

package integration

import (
	"encoding/json"
	"time"
)

type EventType string

const (
	ChatFinished    EventType = "chat.finished"
	AccountDeleting EventType = "account.deleting"
	ConsentChanged  EventType = "consent.changed"
	UsageRecorded   EventType = "usage.recorded"
)

// Event is the v1 wire envelope. Data contains the payload for Type; API readers
// ignore unknown fields and unknown event types. This is not a delivery engine.
type Event struct {
	Schema     int             `json:"schema"`
	ID         string          `json:"id"`
	Type       EventType       `json:"type"`
	UID        string          `json:"uid"`
	Generation int64           `json:"generation"`
	OccurredAt time.Time       `json:"occurred_at"`
	Data       json.RawMessage `json:"data"`
}

type ChatFinishedPayload struct {
	ChatID    string `json:"chat_id"`
	ProjectID string `json:"project_id"`
	SessionID string `json:"session_id"`
}

type AccountDeletingPayload struct {
	OperationID string `json:"operation_id"`
}

type ConsentChangedPayload struct {
	Scope    string `json:"scope"`
	Version  int    `json:"version"`
	Accepted bool   `json:"accepted"`
}

// UsageRecorded references a scoped usage record rather than copying model
// prompts, responses or provider-specific fields into the event log.
type UsageRecordedPayload struct {
	UsageID string `json:"usage_id"`
}

func knownEvent(t EventType) bool {
	switch t {
	case ChatFinished, AccountDeleting, ConsentChanged, UsageRecorded:
		return true
	}
	return false
}
