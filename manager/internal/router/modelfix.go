// SPDX-License-Identifier: MIT

package router

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"
)

// OpenCode 2.2.0 defaults config-defined models to time.released = 0 (1970).
// The picker shows only recent family winners unless explicitly toggled. Stamp
// zero dates with the current time; a static config releaseDate would age out.
// Distinct families in /etc/opencode keep all our models visible (lint.py and
// TestBrandContract check them).
// shortcut: remove once upstream treats released=0 as "no date" (models.tsx `visible`).
const modelListPath = "/api/model"

const maxModelListBytes = 8 << 20

func isModelList(r *http.Request) bool {
	return r.Method == http.MethodGet && r.URL.Path == modelListPath
}

func fixModelResponse(resp *http.Response) error {
	if !isModelList(resp.Request) || resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "" {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelListBytes+1))
	resp.Body.Close()
	if err != nil {
		return err
	}
	if len(body) <= maxModelListBytes {
		if fixed, ok := stampReleaseDates(body, time.Now()); ok {
			body = fixed
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return nil
}

// stampReleaseDates accepts both {"data":[...]} and a bare array and leaves
// every other field untouched.
func stampReleaseDates(body []byte, now time.Time) ([]byte, bool) {
	var wrapped map[string]json.RawMessage
	if json.Unmarshal(body, &wrapped) == nil && wrapped["data"] != nil {
		list, ok := stampList(wrapped["data"], now)
		if !ok {
			return nil, false
		}
		wrapped["data"] = list
		out, err := json.Marshal(wrapped)
		return out, err == nil
	}
	return stampList(body, now)
}

func stampList(raw json.RawMessage, now time.Time) ([]byte, bool) {
	var models []map[string]json.RawMessage
	if json.Unmarshal(raw, &models) != nil {
		return nil, false
	}
	changed := false
	for _, m := range models {
		var t map[string]float64
		if json.Unmarshal(m["time"], &t) != nil || t["released"] != 0 {
			continue
		}
		t["released"] = float64(now.UnixMilli())
		b, _ := json.Marshal(t)
		m["time"] = b
		changed = true
	}
	if !changed {
		return nil, false
	}
	out, err := json.Marshal(models)
	return out, err == nil
}
