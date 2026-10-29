// SPDX-License-Identifier: MIT

// Package filetools exchanges bytes with the internal, stateless file worker.
package filetools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type Output struct {
	Data      []byte `json:"data"`
	Extension string `json:"extension"`
	MediaType string `json:"media_type"`
	Role      string `json:"role"`
}

type Result struct {
	Status   string         `json:"status"`
	Outputs  []Output       `json:"outputs"`
	Warnings []string       `json:"warnings"`
	Metadata map[string]any `json:"metadata"`
	Error    string         `json:"error,omitempty"`
}

type Process func(context.Context, string, []byte, map[string]any) (Result, error)

func New(base string) Process {
	secret := os.Getenv("OC_FILE_TOOLS_SECRET")
	client := &http.Client{Timeout: 100 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return func(ctx context.Context, operation string, data []byte, options map[string]any) (Result, error) {
		var result Result
		body, err := json.Marshal(map[string]any{"operation": operation, "data": data, "options": options})
		if err != nil {
			return result, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/process", bytes.NewReader(body))
		if err != nil {
			return result, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+secret)
		resp, err := client.Do(req)
		if err != nil {
			return result, fmt.Errorf("file service unavailable: %w", err)
		}
		defer resp.Body.Close()
		const limit = 60 << 20
		raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if err != nil || len(raw) > limit {
			return result, fmt.Errorf("invalid file service response size")
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return result, fmt.Errorf("invalid file service response")
		}
		if resp.StatusCode != http.StatusOK || result.Error != "" {
			return result, fmt.Errorf("file service (%d): %.500s", resp.StatusCode, result.Error)
		}
		if result.Status != "complete" && result.Status != "partial" || len(result.Outputs) == 0 || len(result.Outputs) > 2 {
			return result, fmt.Errorf("invalid file service result")
		}
		return result, nil
	}
}
