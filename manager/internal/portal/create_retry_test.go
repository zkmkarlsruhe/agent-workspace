// SPDX-License-Identifier: MIT

package portal

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

type racingCreate struct {
	*fake
	attempts int
	conflict error
}

func (f *racingCreate) CreateProject(ctx context.Context, uid string, p NewProject) error {
	f.attempts++
	if f.attempts == 1 {
		if err := f.fake.CreateProject(ctx, uid, p); err != nil {
			return err
		}
		return f.conflict
	}
	return f.fake.CreateProject(ctx, uid, p)
}

func TestCreateRetriesSuggestedNameConflict(t *testing.T) {
	for _, conflict := range []error{ErrExists, store.ErrExists} {
		t.Run(conflict.Error(), func(t *testing.T) {
			f := &racingCreate{fake: newFake(), conflict: conflict}
			f.suggest = "report"
			h, err := New(Config{Backend: f, Assets: os.DirFS("../../web"), Origin: "https://apps.example.org", Group: UserGroup})
			if err != nil {
				t.Fatal(err)
			}
			body, ct := taskBody(t, "Write a report about visitor numbers.", "")
			w := do(t, h, req{method: "POST", path: "/projects/start", body: body, ctype: ct})
			wantStatus(t, w, 303)
			if f.attempts != 2 || f.projects["report-2"] == nil || len(f.sessions) != 1 || !strings.HasPrefix(f.sessions[0], "report-2|") {
				t.Fatal(f.attempts, f.projects, f.sessions)
			}
		})
	}
}
