// SPDX-License-Identifier: MIT

package portal

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

func TestTeamPortalInvitationPageAndDecisions(t *testing.T) {
	h, st, a, bob, b := knowledgePortal(t)
	ctx := context.Background()
	team, err := st.CreateTeam(ctx, a, "Invited team", "Invitation purpose")
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := st.TeamCandidates(ctx, a, team.ID, "bob")
	if err != nil || len(candidates) != 1 {
		t.Fatal(candidates, err)
	}
	invite := func() int {
		t.Helper()
		current, _, _, err := st.Team(ctx, a, team.ID)
		if err != nil {
			t.Fatal(err)
		}
		form := url.Values{"account": {a.Stamp}, "revision": {strconv.Itoa(current.Revision)}, "member": {b.UID}, "member_stamp": {candidates[0].Stamp}, "role": {"member"}}
		wantStatus(t, do(t, h, req{method: "POST", path: "/teams/" + team.ID + "/members", form: form}), 303)
		return current.Revision + 1
	}
	revision := invite()
	for _, path := range []string{"/teams", "/knowledge"} {
		for _, language := range []string{"en", "de"} {
			w := do(t, h, req{path: path, id: &bob, header: map[string]string{"Accept-Language": language}})
			wantStatus(t, w, 200)
			wantBody(t, w, "Invited team", "Invitation purpose", `name="decision" value="accept"`, `name="decision" value="decline"`, `class="badge badge--waiting"`)
			checkLayoutHTML(t, w.Body.String())
			if strings.Contains(w.Body.String(), `href="/teams/`+team.ID+`"`) {
				t.Fatal("invitation links to team content")
			}
		}
	}
	admin := do(t, h, req{path: "/teams/" + team.ID, header: map[string]string{"Accept-Language": "en"}})
	wantStatus(t, admin, 200)
	wantBody(t, admin, "Pending invitation", "Withdraw invitation")
	checkLayoutHTML(t, admin.Body.String())
	wantStatus(t, do(t, h, req{path: "/teams/" + team.ID, id: &bob}), 404)
	form := url.Values{"account": {b.Stamp}, "revision": {strconv.Itoa(revision)}, "decision": {"accept"}}
	route := "/teams/" + team.ID + "/invitation"
	wantStatus(t, do(t, h, req{method: "POST", path: route, id: &bob, form: form, header: map[string]string{"Origin": "https://foreign.test"}}), 403)
	form.Set("account", a.Stamp)
	wantStatus(t, do(t, h, req{method: "POST", path: route, form: form}), 404)
	form.Set("account", "stale")
	wantStatus(t, do(t, h, req{method: "POST", path: route, id: &bob, form: form}), 404)
	form.Set("account", b.Stamp)
	form.Set("decision", "invalid")
	wantStatus(t, do(t, h, req{method: "POST", path: route, id: &bob, form: form}), 400)
	form.Set("decision", "decline")
	wantStatus(t, do(t, h, req{method: "POST", path: route, id: &bob, form: form}), 303)
	w := do(t, h, req{path: "/teams", id: &bob})
	wantStatus(t, w, 200)
	if strings.Contains(w.Body.String(), "Invited team") {
		t.Fatal("declined invitation remains")
	}
	revision = invite()
	form.Set("decision", "accept")
	// The old form cannot accept a new invitation.
	wantStatus(t, do(t, h, req{method: "POST", path: route, id: &bob, form: form}), 409)
	form.Set("revision", strconv.Itoa(revision))
	wantStatus(t, do(t, h, req{method: "POST", path: route, id: &bob, form: form}), 303)
	wantStatus(t, do(t, h, req{path: "/teams/" + team.ID, id: &bob}), 200)
	current, _, _, err := st.Team(ctx, a, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	leave := url.Values{"account": {b.Stamp}, "revision": {strconv.Itoa(current.Revision)}, "member": {b.UID}, "role": {""}}
	wantStatus(t, do(t, h, req{method: "POST", path: "/teams/" + team.ID + "/members", id: &bob, form: leave}), 303)
	revision = invite()
	withdraw := url.Values{"account": {a.Stamp}, "revision": {strconv.Itoa(revision)}, "member": {b.UID}, "role": {""}}
	wantStatus(t, do(t, h, req{method: "POST", path: "/teams/" + team.ID + "/members", form: withdraw}), 303)
	invitations, err := st.TeamInvitations(ctx, b)
	if err != nil || len(invitations) != 0 {
		t.Fatal(invitations, err)
	}
}

func TestTeamPortalCandidateRateLimit(t *testing.T) {
	h, st, a, _, _ := knowledgePortal(t)
	team, err := st.CreateTeam(context.Background(), a, "Team", "")
	if err != nil {
		t.Fatal(err)
	}
	for range 30 {
		wantStatus(t, do(t, h, req{path: "/teams/" + team.ID + "?q=bob"}), 200)
	}
	for _, language := range []string{"en", "de"} {
		w := do(t, h, req{path: "/teams/" + team.ID + "?q=bob", header: map[string]string{"Accept-Language": language}})
		wantStatus(t, w, 429)
		wantBody(t, w, "30")
	}
}

func TestTeamPortalMembershipLimitError(t *testing.T) {
	h, st, a, _, _ := knowledgePortal(t)
	for range 20 {
		if _, err := st.CreateTeam(context.Background(), a, "Team", ""); err != nil {
			t.Fatal(err)
		}
	}
	w := do(t, h, req{method: "POST", path: "/teams", form: url.Values{"account": {a.Stamp}, "name": {"Another"}}, header: map[string]string{"Accept-Language": "en"}})
	wantStatus(t, w, 429)
	wantBody(t, w, store.ErrTeamMembershipLimit.Error())
}
