// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/testinstance"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/consent"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle/lctest"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

func TestAssistantCardOpenCodeSession(t *testing.T) {
	d, err := testinstance.Load("../../../instance/example/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Select(d)()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.LoadAssistants(ctx); err != nil {
		t.Fatal(err)
	}
	d = instance.Current()
	if _, err := st.EnsureUser(ctx, identity.Identity{UID: uid1, Username: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AcceptConsent(ctx, uid1, consent.Version(), time.Now()); err != nil {
		t.Fatal(err)
	}
	oc := &lctest.OpenCode{LoadingFor: 2, Queued: 2}
	rt := lctest.NewRuntime()
	s := New(config.Config{Domain: "apps.example.org", UserNetPrefix: "10.90", StartTimeout: time.Second}, rt, st, slog.New(slog.DiscardHandler))
	s.ws = t.TempDir()
	s.http.Transport = assistantTransport{oc}
	oldPoll, oldGrace := settlePoll, mcpToolsGrace
	settlePoll, mcpToolsGrace = time.Millisecond, 0
	defer func() { settlePoll, mcpToolsGrace = oldPoll, oldGrace }()
	question := "  Drucker sagt: \"E42\"\nWas tun? äöü  "
	def, _ := instance.Current().Assistant("it-hilfe")
	calls := 0
	s.Complete = func(_ context.Context, u store.User, system, user string, maxTokens int) (string, error) {
		calls++
		var input map[string]string
		if err := json.Unmarshal([]byte(user), &input); err != nil {
			t.Fatal(err)
		}
		if u.UID != uid1 || input["question"] != question || input["init_prompt"] != def.InitPrompt || !strings.Contains(system, "VERBATIM") {
			t.Fatal("wrong preparation input", input)
		}
		if strings.Contains(system, "folder") {
			t.Fatal("shared preparation asked for a folder", system)
		}
		return `{"context":"IT-Hilfe aus dem Beispiel-Wiki. Keine Änderungen."}`, nil
	}
	_, prompt, err := s.PrepareAssistant(ctx, uid1, def.InitPrompt, question, def.ProjectMode)
	name := def.SharedFolder()
	if err != nil || calls != 1 || prompt != "IT-Hilfe aus dem Beispiel-Wiki. Keine Änderungen.\n\n"+question {
		t.Fatal(name, prompt, calls, err)
	}
	hash := instance.Current().AssistantPin(def).Hash
	if err := s.CreateAssistantProject(ctx, uid1, nil, NewProject{Name: name, Description: question}, def.ID, "stale"); err == nil {
		t.Fatal("accepted stale card")
	}
	var submissions sync.WaitGroup
	for range 4 {
		submissions.Go(func() {
			if err := s.CreateAssistantProject(ctx, uid1, nil, NewProject{Name: name, Description: question}, def.ID, hash); err != nil {
				t.Error(err)
			}
		})
	}
	submissions.Wait()
	p, err := st.ProjectByFolder(ctx, uid1, name)
	if err != nil || p.Isolated || p.Assistant != instance.Current().AssistantPin(def) || p.Description != question {
		t.Fatal(p, err)
	}
	if err := st.SharedRuntimeAccess(ctx, uid1); err != nil {
		t.Fatal("OpenCode launch blocked", err)
	}
	link, err := s.CreateSession(ctx, uid1, name, prompt)
	if err != nil || link != s.DeepLink("alice", "ses_1") {
		t.Fatal(link, err)
	}
	if len(oc.Sessions) != 1 || oc.Sessions[0].Agent != def.AgentName() || len(oc.Prompts) != 1 {
		t.Fatal(oc.Sessions, oc.Prompts)
	}
	config, err := os.ReadFile(filepath.Join(s.ws, name, "opencode.json"))
	if err != nil || !strings.Contains(string(config), "/mcp/assistant/"+p.ID+"/workspace-knowledge") {
		t.Fatal("assistant source config missing", string(config), err)
	}
	if ids, err := st.OptIns(ctx, uid1); err != nil || len(ids) != 0 {
		t.Fatal("assistant changed opt-ins", ids, err)
	}
	if pin, err := s.AuthorizeAssistantSession(ctx, uid1, p.ID, oc.Sessions[0].ID); err != nil || pin != p.Assistant {
		t.Fatal("bound session denied", pin, err)
	}
	original := oc.Sessions[0]
	for _, change := range []func(*ocapi.Session){
		func(se *ocapi.Session) { se.Agent = "helper" },
		func(se *ocapi.Session) { se.ParentID = "ses_parent" },
		func(se *ocapi.Session) { se.Location.Directory = "/workspace/other" },
	} {
		change(&oc.Sessions[0])
		if _, err := s.AuthorizeAssistantSession(ctx, uid1, p.ID, original.ID); err == nil {
			t.Fatal("unbound session granted")
		}
		oc.Sessions[0] = original
	}
	for _, args := range [][3]string{{uid2, p.ID, original.ID}, {uid1, "missing", original.ID}, {uid1, p.ID, "ses_missing"}} {
		if _, err := s.AuthorizeAssistantSession(ctx, args[0], args[1], args[2]); err == nil {
			t.Fatal("invalid binding granted", args)
		}
	}
	var sent struct{ Text string }
	if err := json.Unmarshal([]byte(oc.Prompts[0]), &sent); err != nil || sent.Text != prompt {
		t.Fatal("first message differs", sent, err)
	}
	if oc.LoadingFor != 0 || oc.Queued != 0 {
		t.Fatal("redirect before settle")
	}
	// A second card submission reuses the folder and creates a fresh session.
	if err := s.CreateAssistantProject(ctx, uid1, nil, NewProject{Name: name, Description: "Second task"}, def.ID, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSession(ctx, uid1, name, "Second task"); err != nil {
		t.Fatal(err)
	}
	projects, err := st.Projects(ctx, uid1)
	if err != nil || len(projects) != 1 || len(oc.Sessions) != 2 || len(oc.Prompts) != 2 || oc.Sessions[0].ID == oc.Sessions[1].ID {
		t.Fatal(projects, oc.Sessions, err)
	}
	if projects[0].Description != question {
		t.Fatal("second submit changed original project brief")
	}
	if err := s.CreateAssistantProject(ctx, uid1, nil, NewProject{Name: "another-folder"}, def.ID, hash); err != ErrInvalid {
		t.Fatal("accepted noncanonical shared folder", err)
	}
	// Restart still admits this project, while new sessions retain the assistant agent.
	if err := s.Stop(ctx, uid1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureRunning(ctx, uid1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSession(ctx, uid1, name, ""); err != nil {
		t.Fatal(err)
	}
	if len(oc.Prompts) != 2 || oc.Sessions[2].Agent != def.AgentName() {
		t.Fatal("repeated initialization or lost agent")
	}
	if err := s.RenameProject(ctx, uid1, name, "renamed"); err != ErrInvalid {
		t.Fatal("shared project renamed", err)
	}
	// A saved definition is used after a fresh authorized card start; busy chats stay intact.
	changed := def
	changed.Prompt.Role = "Use the database-owned instructions."
	changed.ModelRole = "small"
	changed, err = st.SaveAssistant(ctx, changed, hash, uid1)
	if err != nil {
		t.Fatal(err)
	}
	hash = instance.Current().AssistantPin(changed).Hash
	if _, err := s.AuthorizeAssistantSession(ctx, uid1, p.ID, original.ID); err == nil {
		t.Fatal("stale project authorized")
	}
	oc.Active = []string{original.ID}
	if err := s.CreateAssistantProject(ctx, uid1, nil, NewProject{Name: name}, def.ID, hash); err != ErrBusy {
		t.Fatal("busy project changed", err)
	}
	oc.Active = nil
	if err := s.CreateAssistantProject(ctx, uid1, nil, NewProject{Name: name}, def.ID, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSession(ctx, uid1, name, ""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(s.ws, name, "opencode.json"))
	if err != nil || !strings.Contains(string(data), changed.Prompt.Role) || !strings.Contains(string(data), "litellm/fast#profile-it-hilfe") {
		t.Fatal("saved runtime settings absent", string(data), err)
	}
	if len(oc.Evicted) < 2 {
		t.Fatal("stale runtime config retained")
	}
	d = instance.Current()
	if err := st.DeleteProject(ctx, uid1, name); err != nil {
		t.Fatal(err)
	}
	p.Assistant = assistants.Pin{}
	if err := st.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAssistantProject(ctx, uid1, nil, NewProject{Name: name}, def.ID, d.AssistantPin(d.Assistants[0]).Hash); err != ErrExists {
		t.Fatal("unbound project reused", err)
	}
}

func TestAssistantPreparationErrors(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.EnsureUser(context.Background(), identity.Identity{UID: uid1, Username: "alice"}); err != nil {
		t.Fatal(err)
	}
	s := New(config.Config{}, lctest.NewRuntime(), st, slog.New(slog.DiscardHandler))
	for _, response := range []string{"", "not json", `{}`, `{"folder":"help","context":""}`, "failure"} {
		t.Run(response, func(t *testing.T) {
			calls := 0
			s.Complete = func(context.Context, store.User, string, string, int) (string, error) {
				calls++
				if response == "failure" {
					return "", errors.New("model unavailable")
				}
				return response, nil
			}
			if _, _, err := s.PrepareAssistant(context.Background(), uid1, "Support instructions", "question", "per_task"); err == nil || calls != 1 {
				t.Fatal("expected fallback signal from one call", err, calls)
			}
		})
	}
}
