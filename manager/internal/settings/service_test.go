// SPDX-License-Identifier: MIT

package settings_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/settings"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// fakeContainer stands in for lifecycle.Service.Shell: it runs the two
// AGENTS.md scripts against an in-memory file.
type fakeContainer struct {
	running bool
	file    string
	writes  int
}

func (c *fakeContainer) shell(_ context.Context, _, script string, stdout io.Writer, args ...string) error {
	if !c.running {
		return settings.ErrNotRunning
	}
	if args[0] != settings.AgentsPath {
		return errors.New("wrong path " + args[0])
	}
	if strings.Contains(script, "head -c") {
		io.WriteString(stdout, c.file)
		return nil
	}
	c.file, c.writes = args[1], c.writes+1
	return nil
}

type fakeDir struct {
	e     settings.Entry
	err   error
	calls int
}

func (d *fakeDir) Lookup(context.Context, string, string) (settings.Entry, error) {
	d.calls++
	return d.e, d.err
}

func setup(t *testing.T) (*settings.Service, *store.Store, *fakeContainer, *fakeDir, settings.Person) {
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	p := settings.Person{UID: strings.Repeat("b", 64), Username: "anna", Email: "anna@example.org", Name: "Anna Beispiel",
		Groups: []string{"opencode-users", "operations"}}
	if _, err := st.EnsureUser(context.Background(), identity.Identity{UID: p.UID, Username: p.Username, Email: p.Email, Name: p.Name}); err != nil {
		t.Fatal(err)
	}
	c := &fakeContainer{running: true, file: "## Gemerkt\n- Kaffee schwarz.\n"}
	d := &fakeDir{e: settings.Entry{Department: "Technik", Phone: "1234"}}
	s := &settings.Service{Store: st, Dir: d, Shell: c.shell, Log: slog.New(slog.DiscardHandler), Hidden: []string{"opencode-users"}}
	return s, st, c, d, p
}

func TestRefreshAndApply(t *testing.T) {
	s, st, c, d, p := setup(t)
	ctx := context.Background()
	got, err := s.Refresh(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Facts.Department != "Technik" || got.Facts.Name != "Anna Beispiel" || strings.Join(got.Facts.Groups, ",") != "operations" {
		t.Fatalf("facts %+v", got.Facts)
	}
	if c.writes != 1 || !strings.Contains(c.file, "- Abteilung: Technik\n") || !strings.HasSuffix(c.file, "## Gemerkt\n- Kaffee schwarz.\n") ||
		!strings.Contains(c.file, "„du“") {
		t.Fatalf("file after refresh (%d writes):\n%s", c.writes, c.file)
	}
	// fresh facts: no second directory call, no write
	if _, err := s.Refresh(ctx, p); err != nil || d.calls != 1 || c.writes != 1 {
		t.Errorf("second refresh: %v, %d lookups, %d writes", err, d.calls, c.writes)
	}
	// a changed Authentik name refreshes at once
	p.Name = "Anna Muster"
	if _, err := s.Refresh(ctx, p); err != nil || d.calls != 2 || !strings.Contains(c.file, "Anna Muster") {
		t.Errorf("rename: %v, %d lookups", err, d.calls)
	}
	// directory down: last values stay
	d.err = errors.New("timeout")
	p.Name = "Anna B."
	if got, _ := s.Refresh(ctx, p); got.Facts.Department != "Technik" {
		t.Errorf("directory error dropped facts: %+v", got.Facts)
	}

	// save while stopped: stored, written on the next start
	c.running = false
	a := settings.Default(got.Facts)
	a.AddressForm = settings.AddressFormal
	if err := s.Save(ctx, p.UID, a, ""); err != nil {
		t.Fatal(err)
	}
	c.running = true
	s.AfterStart(ctx, p.UID)
	if !strings.Contains(c.file, "„Sie“") || strings.Count(c.file, "## So arbeite ich") != 1 || !strings.HasSuffix(c.file, "- Kaffee schwarz.\n") {
		t.Errorf("after start:\n%s", c.file)
	}
	n := c.writes
	s.AfterStart(ctx, p.UID)
	if c.writes != n {
		t.Error("unchanged block rewritten")
	}

	// edited text wins over the template
	if err := s.Save(ctx, p.UID, a, "- Nur Stichpunkte.\n"); err != nil || !strings.Contains(c.file, "## So arbeite ich\n- Nur Stichpunkte.\n") {
		t.Errorf("edited text: %v\n%s", err, c.file)
	}
	// a full "Gemerkt" refuses the block instead of cutting the file
	c.file += strings.Repeat("x", settings.MaxAgents)
	if err := s.Save(ctx, p.UID, a, "- Lang.\n"); !errors.Is(err, settings.ErrTooLarge) {
		t.Errorf("full file: %v", err)
	}

	// account deletion drops the settings (FK cascade)
	if _, err := st.DeleteUser(ctx, p.UID, "geloescht-x"); err != nil {
		t.Fatal(err)
	}
	if d, err := st.LoadSettings(ctx, p.UID); err != nil || d.Answers != nil || d.Facts.Name != "" {
		t.Errorf("after delete: %+v %v", d, err)
	}
}

func TestSaveTooLargeBlock(t *testing.T) {
	s, st, _, _, p := setup(t)
	a := settings.Default(settings.Facts{})
	if err := s.Save(context.Background(), p.UID, a, strings.Repeat("ü", settings.MaxText)); !errors.Is(err, settings.ErrTooLarge) {
		t.Fatalf("got %v", err)
	}
	if d, _ := st.LoadSettings(context.Background(), p.UID); d.Answers != nil {
		t.Error("stored although too large")
	}
}
