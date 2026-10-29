// SPDX-License-Identifier: MIT

package assistantchat

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/testinstance"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/catalog"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/consent"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

const owner = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const other = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func fixture(t *testing.T) (*Service, *instance.Definition, string, string) {
	t.Helper()
	ctx := context.Background()
	d, err := testinstance.Load("../../../instance/example/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(instance.Select(d))
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, uid := range []string{owner, other} {
		if _, err := st.EnsureUser(ctx, identity.Identity{UID: uid, Username: uid[:8], Issuer: "https://id.example", Subject: uid}); err != nil {
			t.Fatal(err)
		}
		if err := st.AcceptConsent(ctx, uid, consent.Version(), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	s := &Service{Store: st}
	s.Items = func(assistants.Definition) []catalog.Item {
		return []catalog.Item{{ID: "workspace-knowledge", LiteLLM: []string{"workspace_kb_wiki"}, Tools: []string{"search_workspace_wiki"}}}
	}
	s.Tools = func(_ context.Context, _ store.User, auth func() (assistants.Pin, error)) ([]*sdk.Tool, error) {
		_, err := auth()
		return []*sdk.Tool{{Name: "workspace-knowledge_search_workspace_wiki", InputSchema: map[string]any{"type": "object"}}}, err
	}
	s.Call = func(_ context.Context, _ store.User, _, _ string, _ json.RawMessage, auth func() (assistants.Pin, error)) (*sdk.CallToolResult, error) {
		_, err := auth()
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "https://wiki.example/help"}}}, err
	}
	s.Complete = func(context.Context, store.User, string, string, int, any, any) (json.RawMessage, error) {
		return json.RawMessage(`{"role":"assistant","content":"Try the documented step: https://wiki.example/help"}`), nil
	}
	folder, err := s.Create(ctx, owner, nil, "it-hilfe", d.AssistantPin(d.Assistants[0]).Hash, "The printer fails to print", false)
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.Open(ctx, owner, nil, folder)
	if err != nil {
		t.Fatal(err)
	}
	return s, d, folder, session
}

func TestPrivateAssistantConversation(t *testing.T) {
	s, _, folder, id := fixture(t)
	ctx := context.Background()
	if err := s.Send(ctx, owner, nil, id, "What should I try?", 0); err != nil {
		t.Fatal(err)
	}
	p, se, m, err := s.View(ctx, owner, id)
	if err != nil || se.Revision != 1 || len(m) != 2 || p.Folder != folder {
		t.Fatalf("%+v %+v %v %v", p, se, m, err)
	}
	if err := s.Store.SharedRuntimeAccess(ctx, owner); err != nil {
		t.Fatal("isolated project blocked unrelated runtime", err)
	}
	if _, _, _, err := s.View(ctx, other, id); err == nil {
		t.Fatal("cross-owner read")
	}
	if err := s.Send(ctx, other, nil, id, "read their chat", 1); err == nil {
		t.Fatal("cross-owner write")
	}
	if err := s.Send(ctx, owner, nil, id, "duplicate form submission", 0); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	if _, err := s.Store.ChatByID(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("operator can read private chat", err)
	}
	if rows, err := s.Store.ChatsToEvaluate(ctx, consent.Version(), 100); err != nil || len(rows) != 0 {
		t.Fatal("private chat enters evaluation", rows, err)
	}
}

func TestAssistantRejectsEscalation(t *testing.T) {
	for _, tool := range []string{"shell", "edit", "write", "subagent", "task", "build", "library_search", "workspace-knowledge_write"} {
		t.Run(tool, func(t *testing.T) {
			s, _, _, id := fixture(t)
			called := false
			s.Call = func(context.Context, store.User, string, string, json.RawMessage, func() (assistants.Pin, error)) (*sdk.CallToolResult, error) {
				called = true
				return nil, nil
			}
			s.Complete = func(context.Context, store.User, string, string, int, any, any) (json.RawMessage, error) {
				return json.RawMessage(`{"role":"assistant","tool_calls":[{"id":"call","type":"function","function":{"name":"` + tool + `","arguments":"{}"}}]}`), nil
			}
			if err := s.Send(context.Background(), owner, nil, id, "please execute this", 0); err == nil || called {
				t.Fatal("escalated", err, called)
			}
		})
	}
}

func TestAssistantRevocationInFlight(t *testing.T) {
	for _, reason := range []string{"grant", "disabled", "pin", "consent", "generation", "identity", "trash", "deleted", "expired"} {
		t.Run(reason, func(t *testing.T) {
			s, d, folder, id := fixture(t)
			ctx := context.Background()
			calls := 0
			revoke := func() {
				switch reason {
				case "grant":
					s.Items = func(assistants.Definition) []catalog.Item { return nil }
				case "disabled":
					d.Assistants[0].Enabled = false
				case "pin":
					d.Assistants[0].Prompt.Rules = append(d.Assistants[0].Prompt.Rules, "Changed")
				case "consent":
					if err := s.Store.AcceptConsent(ctx, owner, "old", time.Now()); err != nil {
						t.Fatal(err)
					}
				case "generation":
					if _, err := s.Store.NextGeneration(ctx, owner); err != nil {
						t.Fatal(err)
					}
				case "identity":
					if _, err := s.Store.CheckIdentity(ctx, identity.Identity{UID: owner, Username: owner[:8], Issuer: "https://id.example", Subject: "replacement"}); !errors.Is(err, store.ErrIdentityBlocked) {
						t.Fatal(err)
					}
				case "trash":
					p, _ := s.Store.ProjectByFolder(ctx, owner, folder)
					if err := s.Store.MoveToTrash(ctx, p, "trash", time.Now(), time.Now().Add(time.Hour), nil, false, false); err != nil {
						t.Fatal(err)
					}
				case "deleted":
					if _, err := s.Store.DeleteUser(ctx, owner, "deleted-test"); err != nil {
						t.Fatal(err)
					}
				}
			}
			s.Complete = func(context.Context, store.User, string, string, int, any, any) (json.RawMessage, error) {
				calls++
				if calls == 1 {
					return json.RawMessage(`{"role":"assistant","tool_calls":[{"id":"call","type":"function","function":{"name":"workspace-knowledge_search_workspace_wiki","arguments":"{}"}}]}`), nil
				}
				t.Error("model received revoked tool result")
				return nil, nil
			}
			s.Call = func(context.Context, store.User, string, string, json.RawMessage, func() (assistants.Pin, error)) (*sdk.CallToolResult, error) {
				revoke()
				return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "sensitive result"}}}, nil
			}
			if reason == "expired" {
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			}
			if err := s.Send(ctx, owner, nil, id, "read guidance", 0); err == nil {
				t.Fatal("revoked turn accepted")
			}
			if reason != "expired" && calls != 1 {
				t.Fatalf("revocation path not exercised: model calls %d", calls)
			}
			if se, err := s.Store.AssistantSession(context.Background(), owner, id); err == nil && (se.Revision != 0 || strings.Contains(string(se.Messages), "sensitive")) {
				t.Fatal("revoked result stored", se)
			}
		})
	}
}

func TestAssistantConsentAndAudience(t *testing.T) {
	s, d, _, _ := fixture(t)
	d.Assistants = d.Assistants[:1]
	ctx := context.Background()
	hash := d.AssistantPin(d.Assistants[0]).Hash
	for _, args := range []struct {
		id, hash string
		evaluate bool
	}{{"other", hash, false}, {"it-hilfe", "forged", false}, {"it-hilfe", hash, true}} {
		if _, err := s.Create(ctx, owner, nil, args.id, args.hash, "A new support question", args.evaluate); err == nil {
			t.Fatal("invalid choice accepted", args)
		}
	}
	d.Assistants[0].Audience = []string{"supporters"}
	if choices := s.Choices(ctx, owner, nil); len(choices) != 0 {
		t.Fatal("private audience shown")
	}
	if _, err := s.Create(ctx, owner, nil, "it-hilfe", d.AssistantPin(d.Assistants[0]).Hash, "A new support question", false); err == nil {
		t.Fatal("audience bypass")
	}
	s.Items = func(assistants.Definition) []catalog.Item { return nil }
	if choices := s.Choices(ctx, owner, []string{"supporters"}); len(choices) != 1 || choices[0].Missing == "" {
		t.Fatal("missing source invisible", choices)
	}
	if _, err := s.Create(ctx, owner, []string{"supporters"}, "it-hilfe", d.AssistantPin(d.Assistants[0]).Hash, "A new support question", false); err == nil {
		t.Fatal("missing source allowed")
	}
}

func TestEvaluationOptInAndWithdrawal(t *testing.T) {
	s, d, _, _ := fixture(t)
	ctx := context.Background()
	d.Assistants[0].Evaluation = true
	folder, err := s.Create(ctx, owner, nil, "it-hilfe", d.AssistantPin(d.Assistants[0]).Hash, "Optional evaluation support", true)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Open(ctx, owner, nil, folder)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send(ctx, owner, nil, id, "hello support", 0); err != nil {
		t.Fatal(err)
	}
	ch, err := s.Store.ChatByID(ctx, id)
	if err != nil {
		t.Fatal("opted-in snapshot missing", err)
	}
	d.Assistants[0].Evaluation = false
	if _, err := s.Store.ChatByID(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("disabled evaluation still readable", err)
	}
	if ok, err := s.Store.MayEvaluateChat(ctx, owner, id, consent.Version()); err != nil || ok {
		t.Fatal("disabled evaluation still scheduled", ok, err)
	}
	if ok, err := s.Store.PutEval(ctx, owner, id, consent.Version(), store.ChatEval{ChatUpdated: ch.Updated, EvaluatedAt: time.Now(), Source: "test", Result: `{}`}); err != nil || ok {
		t.Fatal("disabled evaluation committed", ok, err)
	}
	d.Assistants[0].Evaluation = true
	p, _ := s.Store.ProjectByFolder(ctx, owner, folder)
	if err := s.Store.WithdrawAssistantEvaluation(ctx, owner, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.ChatByID(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("operator read after withdrawal", err)
	}
	if ok, err := s.Store.PutEval(ctx, owner, id, consent.Version(), store.ChatEval{ChatUpdated: ch.Updated, EvaluatedAt: time.Now(), Source: "test", Result: `{}`}); err != nil || ok {
		t.Fatal("evaluation resurrected", ok, err)
	}
	if err := s.Send(ctx, owner, nil, id, "help after refusing evaluation", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.ChatByID(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("new private turn collected", err)
	}
}

func TestResearchPrerequisites(t *testing.T) {
	s, d, _, _ := fixture(t)
	research, _ := d.Assistant("research")
	s.Items = (&catalog.Service{Dir: "../../../deploy/catalog"}).AssistantItems
	if err := s.prerequisites(research); err != nil {
		t.Fatal(err)
	}
	for _, integration := range []string{"library", "web-research"} {
		d.Integrations[integration] = false
		if err := d.Validate(); err != nil {
			t.Fatal(err)
		}
		if err := s.prerequisites(research); err == nil {
			t.Fatal("missing integration allowed", integration)
		}
		d.Integrations[integration] = true
	}
}

func TestDescriptionByteLimit(t *testing.T) {
	s, d, _, _ := fixture(t)
	for _, description := range []string{strings.Repeat("a", 9), strings.Repeat("a", 10), strings.Repeat("ä", 2000), strings.Repeat("a", 4001)} {
		_, err := s.Create(context.Background(), owner, nil, "it-hilfe", d.AssistantPin(d.Assistants[0]).Hash, description, false)
		if len(description) < 10 || len(description) > 4000 {
			if err == nil || err.Error() != "describe the task in 10 to 4000 bytes" {
				t.Fatalf("%d bytes: %v", len(description), err)
			}
		} else if err != nil {
			t.Fatalf("%d bytes rejected: %v", len(description), err)
		}
	}
}

func TestAssistantSnapshotOwnership(t *testing.T) {
	s, _, folder, id := fixture(t)
	for _, chat := range []store.Chat{
		{UID: other, Project: folder, SessionID: id},
		{UID: owner, Project: "other-project", SessionID: id},
	} {
		if err := s.Store.PutAssistantChat(context.Background(), chat); !errors.Is(err, store.ErrNotFound) {
			t.Fatal("foreign session or project accepted", err)
		}
	}
}
