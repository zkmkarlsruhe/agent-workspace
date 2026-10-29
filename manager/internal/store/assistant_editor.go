// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
)

// StoredAssistant retains invalid definitions for administrator repair.
type StoredAssistant struct {
	assistants.Definition
	ValidationError string `json:"validation_error,omitempty"`
}

func (s *Store) StoredAssistants(ctx context.Context) ([]StoredAssistant, error) {
	var raw string
	if err := s.db.QueryRowContext(ctx, `SELECT definitions FROM assistant_config WHERE id=1`).Scan(&raw); err != nil {
		return nil, err
	}
	var definitions []StoredAssistant
	err := json.Unmarshal([]byte(raw), &definitions)
	return definitions, err
}

// assistantSnapshot excludes quarantined definitions from runtime validation and dispatch.
func assistantSnapshot(d *instance.Definition, definitions []StoredAssistant) (*instance.Definition, error) {
	var active []assistants.Definition
	for _, p := range definitions {
		if p.ValidationError == "" {
			active = append(active, p.Definition)
		}
	}
	return d.WithAssistants(active)
}

var ErrAssistantConflict = errors.New("assistant changed; reload before saving")

// LoadAssistants imports the installation defaults once. Later restarts and
// deployments retain database settings; invalid definitions are quarantined and
// old editor drafts are not published.
func (s *Store) LoadAssistants(ctx context.Context) error {
	s.assistantMu.Lock()
	defer s.assistantMu.Unlock()
	d := instance.Current()
	defaults, err := json.Marshal(d.Assistants)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO assistant_config(id,definitions) VALUES (1,?)`, string(defaults)); err != nil {
		return err
	}
	var raw string
	if err = tx.QueryRowContext(ctx, `SELECT definitions FROM assistant_config WHERE id=1`).Scan(&raw); err != nil {
		return err
	}
	var definitions []StoredAssistant
	if err = json.Unmarshal([]byte(raw), &definitions); err != nil {
		return err
	}
	var valid []assistants.Definition
	for i := range definitions {
		p := &definitions[i]
		if p.ValidationError == "" {
			candidate := append(append([]assistants.Definition{}, valid...), p.Definition)
			if _, e := d.WithAssistants(candidate); e != nil {
				p.ValidationError = e.Error()
			} else {
				valid = candidate
			}
		}
		if p.ValidationError != "" {
			p.Enabled = false
			slog.ErrorContext(ctx, "stored assistant disabled", "assistant", p.ID, "err", p.ValidationError)
		}
	}
	next, err := assistantSnapshot(d, definitions)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(definitions)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE assistant_config SET definitions=? WHERE id=1`, string(encoded)); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	instance.ActivateAssistants(next)
	return nil
}

// SaveAssistant atomically replaces the complete validated object and runtime
// snapshot. The hash is a concurrency guard, not a user-visible version workflow.
func (s *Store) SaveAssistant(ctx context.Context, p assistants.Definition, expectedHash, actor string) (assistants.Definition, error) {
	s.assistantMu.Lock()
	defer s.assistantMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return p, err
	}
	defer tx.Rollback()
	var raw string
	if err = tx.QueryRowContext(ctx, `SELECT definitions FROM assistant_config WHERE id=1`).Scan(&raw); err != nil {
		return p, err
	}
	var definitions []StoredAssistant
	if err = json.Unmarshal([]byte(raw), &definitions); err != nil {
		return p, err
	}
	d := instance.Current()
	found := -1
	for i, old := range definitions {
		if old.ID == p.ID {
			if d.AssistantPin(old.Definition).Hash != expectedHash {
				return p, ErrAssistantConflict
			}
			found = i
			p.Version = old.Version + 1
			break
		}
	}
	if found < 0 {
		if expectedHash != "" {
			return p, ErrAssistantConflict
		}
		p.Version = 1
		definitions = append(definitions, StoredAssistant{Definition: p})
	} else {
		definitions[found] = StoredAssistant{Definition: p}
	}
	next, err := assistantSnapshot(d, definitions)
	if err != nil {
		return p, err
	}
	encoded, err := json.Marshal(definitions)
	if err != nil {
		return p, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE assistant_config SET definitions=? WHERE id=1`, string(encoded)); err != nil {
		return p, err
	}
	pin := next.AssistantPin(p)
	if _, err = tx.ExecContext(ctx, `INSERT INTO admin_audit (ts,admin_uid,admin_name,session_id,action) VALUES (?,?,'',?,'assistant_save')`, time.Now().UnixMilli(), actor, fmt.Sprintf("%s/%d/%s", p.ID, p.Version, pin.Hash)); err != nil {
		return p, err
	}
	if err = tx.Commit(); err != nil {
		return p, err
	}
	instance.ActivateAssistants(next)
	return p, nil
}

func (s *Store) AuditAssistant(ctx context.Context, actor, id string, revision int, hash, action string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO admin_audit (ts, admin_uid, admin_name, session_id, action) VALUES (?,?,'',?,?)`, time.Now().UnixMilli(), actor, fmt.Sprintf("%s/%d/%s", id, revision, hash), action)
	return err
}

func (s *Store) AssistantProjectCount(ctx context.Context, id string) (projects, people int, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT count(*), count(DISTINCT uid) FROM projects WHERE profile_id=?`, id).Scan(&projects, &people)
	return
}
