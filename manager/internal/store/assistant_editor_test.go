// SPDX-License-Identifier: MIT

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/consent"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/testinstance"
)

func assistantTestDefinition(t *testing.T) *instance.Definition {
	t.Helper()
	d, err := testinstance.Load("../../../instance/minimal/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(instance.Select(d))
	return d
}
func TestAssistantDatabaseAuthorityAndCAS(t *testing.T) {
	d := assistantTestDefinition(t)
	path := filepath.Join(t.TempDir(), "manager.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err = s.LoadAssistants(ctx); err != nil {
		t.Fatal(err)
	}
	p := d.Assistants[0]
	base := d.AssistantPin(p).Hash
	p.Title = "Saved <script>"
	p, err = s.SaveAssistant(ctx, p, base, uid(1))
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := instance.Current().Assistant(p.ID)
	if saved.Title != p.Title || saved.Version != 3 {
		t.Fatal(saved)
	}
	if d.Assistants[0].Title == p.Title {
		t.Fatal("mutated old snapshot")
	}
	if _, err = s.SaveAssistant(ctx, p, base, uid(2)); !errors.Is(err, ErrAssistantConflict) {
		t.Fatal("stale save", err)
	}
	base = instance.Current().AssistantPin(p).Hash
	invalid := p
	invalid.KnowledgeSources = []string{"revoked"}
	if _, err = s.SaveAssistant(ctx, invalid, base, uid(1)); err == nil {
		t.Fatal("revoked source accepted")
	}
	fresh := p
	fresh.ID = "second"
	if _, err = s.SaveAssistant(ctx, fresh, "", uid(1)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveAssistant(ctx, fresh, "", uid(1)); !errors.Is(err, ErrAssistantConflict) {
		t.Fatal("duplicate create", err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	// Simulate a deployment with changed, missing and newly added file defaults.
	replacement := *d
	replacement.Assistants = []assistants.Definition{d.Assistants[0]}
	replacement.Assistants[0].ID = "new-image-default"
	restore := instance.Select(&replacement)
	defer restore()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.LoadAssistants(ctx); err != nil {
		t.Fatal(err)
	}
	saved, ok := instance.Current().Assistant(p.ID)
	if !ok || saved.Title != p.Title {
		t.Fatal("deployment overwrote database", saved)
	}
	if _, ok := instance.Current().Assistant("second"); !ok {
		t.Fatal("new assistant lost on restart")
	}
	if _, ok := instance.Current().Assistant("new-image-default"); ok {
		t.Fatal("deployment added unrequested assistant")
	}
	audit, _ := s.Audit(ctx, 10)
	if len(audit) != 2 {
		t.Fatal("missing audit", audit)
	}
}

func TestAssistantGapOwnershipConsentRetention(t *testing.T) {
	d := assistantTestDefinition(t)
	d.Assistants[0].Enabled = true
	d.Assistants[0].ConsentMode = "support"
	d.Assistants[0].GapCollection = true
	s := open(t)
	ctx := context.Background()
	for _, id := range []string{uid(1), uid(2)} {
		if _, err := s.EnsureUser(ctx, identity.Identity{UID: id, Username: id}); err != nil {
			t.Fatal(err)
		}
		if err := s.AcceptConsent(ctx, id, consent.Version(), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	p := Project{ID: "project-gap", UID: uid(1), Folder: "support", Assistant: d.AssistantPin(d.Assistants[0]), CreatedAt: time.Now()}
	if err := s.CreateOpenCodeAssistantProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	g := AssistantGap{ID: uuid.NewString(), UID: uid(1), ProjectID: p.ID, Summary: "Missing reset instructions", Sources: "https://example.test/guide", Category: "missing"}
	foreign := g
	foreign.UID = uid(2)
	if err := s.AddAssistantGap(ctx, foreign); err == nil {
		t.Fatal("foreign gap accepted")
	}
	if err := s.AddAssistantGap(ctx, g); err != nil {
		t.Fatal(err)
	}
	if err := s.AddAssistantGap(ctx, g); err != nil {
		t.Fatal("repeat failed", err)
	}
	rows, _ := s.AssistantGaps(ctx, g.UID, "")
	if len(rows) != 1 || rows[0].Assistant != p.Assistant.ID {
		t.Fatal(rows)
	}
	if err := s.DeleteAssistantGap(ctx, uid(2), g.ID); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.AssistantGaps(ctx, uid(2), "")
	if len(rows) != 0 {
		t.Fatal("cross owner leak")
	}
	if err := s.AnswerAssistantGap(ctx, uid(2), g.ID, 1, "javascript:alert(1)"); err == nil {
		t.Fatal("unsafe link")
	}
	if err := s.AnswerAssistantGap(ctx, uid(2), g.ID, 1, "https://example.test/new-guide"); err != nil {
		t.Fatal(err)
	}
	if err := s.AnswerAssistantGap(ctx, uid(2), g.ID, 1, ""); !errors.Is(err, ErrAssistantConflict) {
		t.Fatal("stale answer", err)
	}
	d.Assistants[0].GapCollection = false
	g.ID = uuid.NewString()
	if err := s.AddAssistantGap(ctx, g); err == nil {
		t.Fatal("disabled gap collection accepted")
	}
	d.Assistants[0].GapCollection = true
	if err := s.AcceptConsent(ctx, uid(1), "old", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.AddAssistantGap(ctx, g); err == nil {
		t.Fatal("revoked consent accepted")
	}
	if err := s.AcceptConsent(ctx, uid(1), consent.Version(), time.Now()); err != nil {
		t.Fatal(err)
	}
	// Deletion works with collection disabled and removes entries with the account.
	if err := s.PurgeAssistantGaps(ctx, time.Now().Add(AssistantGapRetention+time.Second)); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.AssistantGaps(ctx, "", "")
	if len(rows) != 0 {
		t.Fatal("expired data survived")
	}
	if err := s.AddAssistantGap(ctx, g); err != nil {
		t.Fatal(err)
	}
	if err := s.MoveToTrash(ctx, p, "trash-gap", time.Now(), time.Now(), nil, false, false); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTrash(ctx, uid(1), "trash-gap"); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.AssistantGaps(ctx, "", "")
	if len(rows) != 0 {
		t.Fatal("project deletion left a gap")
	}
	p.ID = "new-project"
	if err := s.CreateOpenCodeAssistantProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	g.ProjectID = p.ID
	g.ID = uuid.NewString()
	if err := s.AddAssistantGap(ctx, g); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteUser(ctx, uid(1), "deleted-one"); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.AssistantGaps(ctx, "", "")
	if len(rows) != 0 {
		t.Fatal("account deletion left a gap")
	}
}

func TestAssistantSaveValidatesNewDefinitions(t *testing.T) {
	assistantTestDefinition(t)
	s := open(t)
	if err := s.LoadAssistants(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := s.SaveAssistant(context.Background(), assistants.Definition{ID: "../escape"}, "", uid(1))
	if err == nil {
		t.Fatal("invalid definition accepted")
	}
}

func TestAssistantConcurrentSave(t *testing.T) {
	d := assistantTestDefinition(t)
	s := open(t)
	if err := s.LoadAssistants(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := d.Assistants[0]
	base := d.AssistantPin(p).Hash
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := s.SaveAssistant(context.Background(), p, base, uid(1))
			results <- err
		}()
	}
	close(start)
	success, conflict := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			success++
		} else if errors.Is(err, ErrAssistantConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatal("lost update", success, conflict)
	}
}

func TestAssistantBootQuarantinesInvalidDefinition(t *testing.T) {
	d := assistantTestDefinition(t)
	s := open(t)
	ctx := context.Background()
	broken := d.Assistants[0]
	broken.Enabled = true
	broken.ConsentMode = "support"
	broken.KnowledgeSources = []string{"removed-source"}
	good := d.Assistants[0]
	good.ID = "healthy"
	raw, err := json.Marshal([]assistants.Definition{broken, good})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO assistant_config VALUES(1,?)`, string(raw)); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	for range 2 {
		if err := s.LoadAssistants(ctx); err != nil {
			t.Fatal(err)
		}
		if _, ok := instance.Current().Assistant(broken.ID); ok {
			t.Fatal("broken assistant active")
		}
		if _, ok := instance.Current().Assistant(good.ID); !ok {
			t.Fatal("healthy assistant lost")
		}
		stored, err := s.StoredAssistants(ctx)
		if err != nil || len(stored) != 2 {
			t.Fatal(stored, err)
		}
		if stored[0].Enabled || !strings.Contains(stored[0].ValidationError, "removed-source") {
			t.Fatal(stored[0])
		}
		if !strings.Contains(logs.String(), broken.ID) || !strings.Contains(logs.String(), "removed-source") {
			t.Fatal(logs.String())
		}
	}
	stored, _ := s.StoredAssistants(ctx)
	fixed := d.Assistants[0]
	if _, err := s.SaveAssistant(ctx, fixed, instance.Current().AssistantPin(stored[0].Definition).Hash, "admin"); err != nil {
		t.Fatal(err)
	}
	stored, _ = s.StoredAssistants(ctx)
	if stored[0].ValidationError != "" {
		t.Fatal(stored[0])
	}
	if _, ok := instance.Current().Assistant(fixed.ID); !ok {
		t.Fatal("repair not activated")
	}
}
