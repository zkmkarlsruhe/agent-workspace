// SPDX-License-Identifier: MIT

package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/runtime"
)

// TestLive runs the scripts and the reload against a real runtime container:
//
//	OC_CATALOG_IT=<container> OC_CATALOG_IT_URL=http://127.0.0.1:<port> OC_CATALOG_IT_DIR=<catalog dir> \
//	OPENCODE_PASSWORD=... go test -run Live ./internal/catalog/
//
// The container must mount OC_CATALOG_IT_DIR at /opt/workspace/catalog and have a
// project folder /workspace/demo. The catalog needs a valid skill probe-eins.
func TestLive(t *testing.T) {
	name := os.Getenv("OC_CATALOG_IT")
	if name == "" {
		t.Skip("OC_CATALOG_IT not set")
	}
	ctx := context.Background()
	rt, err := runtime.NewDocker()
	if err != nil {
		t.Fatal(err)
	}
	oc := &ocapi.Client{BaseURL: os.Getenv("OC_CATALOG_IT_URL"), Password: os.Getenv("OPENCODE_PASSWORD")}
	s := &Service{Dir: os.Getenv("OC_CATALOG_IT_DIR"), Store: memStore{}, Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
		Client: func(string) (*ocapi.Client, bool) { return oc, true },
		Shell: func(ctx context.Context, _, script string, stdout io.Writer, args ...string) error {
			if stdout == nil {
				stdout = io.Discard
			}
			var stderr bytes.Buffer
			code, err := rt.Exec(ctx, name, append([]string{"sh", "-c", script, "sh"}, args...), nil, stdout, &stderr)
			if err == nil && code != 0 {
				err = fmt.Errorf("exit %d: %s", code, stderr.String())
			}
			return err
		}}
	list := func() []string {
		req := oc.BaseURL + "/api/skill?" + url.Values{"location[directory]": {"/workspace/demo"}}.Encode()
		var r struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := getJSON(req, oc.Password, &r); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, d := range r.Data {
			ids = append(ids, d.ID)
		}
		return ids
	}
	waitFor := func(want bool) time.Duration {
		start := time.Now()
		for time.Since(start) < 10*time.Second {
			if slices.Contains(list(), "probe-eins") == want {
				return time.Since(start)
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("probe-eins present=%v not reached, skills %v", want, list())
		return 0
	}
	start := time.Now()
	eff, err := s.Set(ctx, "u", "probe-eins", true)
	if err != nil || eff != EffectNow {
		t.Fatalf("on: %v %v", eff, err)
	}
	t.Logf("on: Set %v, visible in /api/skill after %v more", time.Since(start), waitFor(true))
	if eff, err = s.Set(ctx, "u", "probe-eins", false); err != nil || eff != EffectNow {
		t.Fatalf("off: %v %v", eff, err)
	}
	t.Logf("off: gone after %v", waitFor(false))

	if err := s.Shell(ctx, "u", `mkdir -p `+SkillsDir+`/my-probe && printf -- '---\nname: my-probe\ndescription: Eigene Probe.\n---\n' > `+SkillsDir+`/my-probe/SKILL.md`, nil); err != nil {
		t.Fatal(err)
	}
	std, own, err := s.Skills(ctx, "u")
	t.Logf("standard %d %+v", len(std), std)
	if err != nil || len(own) != 1 || own[0].ID != "my-probe" || own[0].DescriptionDE != "Eigene Probe." {
		t.Fatalf("own %+v %v", own, err)
	}
	if _, err := s.DeleteOwn(ctx, "u", "my-probe"); err != nil {
		t.Fatal(err)
	}
	if _, own, _ := s.Skills(ctx, "u"); len(own) != 0 {
		t.Fatalf("own after delete %+v", own)
	}
}

func getJSON(u, pw string, out any) error {
	req, _ := http.NewRequest("GET", u, nil)
	req.SetBasicAuth("opencode", pw)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("GET %s: %d", u, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
