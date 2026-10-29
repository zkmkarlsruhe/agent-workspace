// SPDX-License-Identifier: MIT

package catalog

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
)

func TestReloadEvictsMissingLocations(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	healthy := t.TempDir()
	missing := filepath.Join(t.TempDir(), "gone ' $(false)")
	f.oc.Locations = []ocapi.Location{{Directory: missing}, {Directory: healthy}, {Directory: "/"}}
	f.oc.MissingDirs = map[string]bool{missing: true}
	f.oc.Sessions = []ocapi.Session{{ID: "ses_restore", Location: ocapi.Location{Directory: missing}}}
	if effect, err := f.svc.Set(ctx, "u", "minutes", true); err != nil || effect != EffectNow {
		t.Fatalf("toggle: %v, %v", effect, err)
	}
	if !slices.Equal(f.oc.Evicted, []string{missing}) || !slices.Equal(f.oc.Reloaded, []string{healthy, "/"}) {
		t.Fatalf("evicted %v, reloaded %v", f.oc.Evicted, f.oc.Reloaded)
	}
	if len(f.oc.Sessions) != 1 || len(f.oc.Deleted) != 0 || len(f.svc.pending) != 0 {
		t.Fatal("cleanup must preserve chats and complete the apply")
	}
	// A restored folder is loaded again on use and participates in later reloads.
	if err := os.Mkdir(missing, 0755); err != nil {
		t.Fatal(err)
	}
	delete(f.oc.MissingDirs, missing)
	f.oc.Locations = append(f.oc.Locations, ocapi.Location{Directory: missing})
	if _, err := f.svc.Apply(ctx, "u"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.oc.Reloaded, missing) {
		t.Fatal("restored location not reloaded")
	}
}

func TestReloadMissingDirectoryRace(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	gone, healthy := t.TempDir(), t.TempDir()
	f.oc.Locations = []ocapi.Location{{Directory: gone}, {Directory: healthy}}
	// The directory passes the shell probe but disappears before OpenCode boots it.
	f.oc.MissingDirs = map[string]bool{gone: true}
	_, err := f.svc.Apply(ctx, "u")
	if !ocapi.IsStatus(err, 503) || !strings.Contains(err.Error(), "FileSystem.DirectoryNotFoundError") {
		t.Fatalf("want production 503, got %v", err)
	}
	if !slices.Equal(f.oc.Reloaded, []string{healthy}) {
		t.Fatalf("healthy skipped: %v", f.oc.Reloaded)
	}
	// Repeat through the portal entry point: it must defer, with the opt-in saved.
	f.oc.Locations = append(f.oc.Locations, ocapi.Location{Directory: gone})
	if effect, err := f.svc.Set(ctx, "u", "minutes", true); err != nil || effect != EffectBusy {
		t.Fatalf("toggle: %v, %v", effect, err)
	}
	on, err := f.svc.On(ctx, "u")
	if err != nil || !on["minutes"] || !f.svc.pending["u"] {
		t.Fatal("switch not saved/queued", err)
	}
	f.svc.RetryPending(ctx)
	if len(f.svc.pending) != 0 || f.oc.Reloads != 3 {
		t.Fatalf("retry: pending %v, reloads %d", f.svc.pending, f.oc.Reloads)
	}
}

func TestReloadCallersDeferAndRetry(t *testing.T) {
	for _, caller := range []string{"toggle", "admin", "start", "delete skill"} {
		t.Run(caller, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			f.oc.FailReload = true
			switch caller {
			case "toggle":
				if e, err := f.svc.Set(ctx, "u", "minutes", true); err != nil || e != EffectBusy {
					t.Fatalf("%v %v", e, err)
				}
			case "admin":
				f.svc.Running = func() []string { return []string{"u"} }
				if now, later := f.svc.ReloadAll(ctx); now != 0 || later != 1 {
					t.Fatalf("now %d later %d", now, later)
				}
			case "start":
				f.svc.AfterStart(ctx, "u")
			case "delete skill":
				if e, err := f.svc.DeleteOwn(ctx, "u", "my-visitors"); err != nil || e != EffectBusy {
					t.Fatalf("%v %v", e, err)
				}
			}
			if !f.svc.pending["u"] {
				t.Fatal("failed apply not pending")
			}
			f.svc.RetryPending(ctx)
			if !f.svc.pending["u"] {
				t.Fatal("persistent failure lost retry")
			}
			f.oc.FailReload = false
			f.svc.RetryPending(ctx)
			if len(f.svc.pending) != 0 || f.oc.Reloads != 3 {
				t.Fatalf("pending %v reloads %d", f.svc.pending, f.oc.Reloads)
			}
		})
	}
}

func TestReloadBusyDoesNotEvict(t *testing.T) {
	f := newFixture(t)
	f.active.Store(true)
	f.oc.Locations = []ocapi.Location{{Directory: filepath.Join(t.TempDir(), "gone")}}
	f.svc.AfterStart(context.Background(), "u")
	if !f.svc.pending["u"] || len(f.oc.Evicted) != 0 || f.oc.Reloads != 0 {
		t.Fatal("busy location touched or retry lost")
	}
}

type failedOptInStore struct{ memStore }

func (failedOptInStore) SetOptIn(context.Context, string, string, bool) error {
	return errors.New("store down")
}

func TestToggleStoreFailureIsNotDeferred(t *testing.T) {
	f := newFixture(t)
	f.svc.Store = failedOptInStore{}
	if _, err := f.svc.Set(context.Background(), "u", "minutes", true); err == nil {
		t.Fatal("unsaved switch accepted")
	}
	if len(f.svc.pending) != 0 || f.oc.Reloads != 0 {
		t.Fatal("unsaved switch queued")
	}
}
