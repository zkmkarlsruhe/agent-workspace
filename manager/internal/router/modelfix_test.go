// SPDX-License-Identifier: MIT

package router

import (
	"encoding/json"
	"testing"
	"time"
)

func TestStampReleaseDates(t *testing.T) {
	now := time.UnixMilli(1792170000000)
	in := `{"data":[{"id":"standard","time":{"released":0},"name":"Standard"},{"id":"gpt","time":{"released":1700000000000}}]}`
	out, ok := stampReleaseDates([]byte(in), now)
	if !ok {
		t.Fatal("expected a change")
	}
	var got struct {
		Data []struct {
			ID   string
			Name string
			Time struct{ Released float64 }
		}
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Data[0].Time.Released != 1792170000000 || got.Data[0].Name != "Standard" {
		t.Fatalf("config model not stamped or fields lost: %+v", got.Data[0])
	}
	if got.Data[1].Time.Released != 1700000000000 {
		t.Fatalf("catalog model date changed: %+v", got.Data[1])
	}

	bare, ok := stampReleaseDates([]byte(`[{"id":"x","time":{"released":0}}]`), now)
	if !ok || string(bare) != `[{"id":"x","time":{"released":1792170000000}}]` {
		t.Fatalf("bare array: %s %v", bare, ok)
	}
	if _, ok := stampReleaseDates([]byte(`{"data":[{"id":"y","time":{"released":5}}]}`), now); ok {
		t.Fatal("nothing to stamp must report no change")
	}
	if _, ok := stampReleaseDates([]byte(`not json`), now); ok {
		t.Fatal("invalid body must be left alone")
	}
}
