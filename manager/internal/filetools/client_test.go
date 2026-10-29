// SPDX-License-Identifier: MIT

package filetools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTransportBytesAndRefusals(t *testing.T) {
	t.Setenv("OC_FILE_TOOLS_SECRET", "private-test-secret")
	for _, status := range []int{200, 302, 422, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("Authorization") != "Bearer private-test-secret" {
					t.Error("missing worker authentication")
				}
				var request struct {
					Operation string
					Data      []byte
					Options   map[string]any
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil || string(request.Data) != "binary\x00data" || request.Operation != "pdf_to_text" {
					t.Errorf("request: %+v %v", request, err)
				}
				if status == 302 {
					w.Header().Set("Location", "/other")
				}
				w.WriteHeader(status)
				json.NewEncoder(w).Encode(Result{Status: "complete", Outputs: []Output{{Data: []byte("text"), Extension: ".txt", MediaType: "text/plain; charset=utf-8", Role: "result"}}})
			}))
			defer server.Close()
			result, err := New(server.URL)(context.Background(), "pdf_to_text", []byte("binary\x00data"), map[string]any{})
			if status == 200 {
				if err != nil || string(result.Outputs[0].Data) != "text" {
					t.Fatalf("result: %+v %v", result, err)
				}
			} else if err == nil {
				t.Fatal("accepted failed response")
			}
			if calls != 1 {
				t.Fatalf("retried/redirected: %d calls", calls)
			}
		})
	}
}

type secretTransport struct{ t *testing.T }

func (s secretTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Header.Get("Authorization") != "Bearer private-test-secret" {
		s.t.Error("missing worker secret")
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"status":"complete","outputs":[{"data":"dGV4dA=="}]}`)), Header: make(http.Header)}, nil
}
func TestWorkerSecretHeader(t *testing.T) {
	t.Setenv("OC_FILE_TOOLS_SECRET", "private-test-secret")
	previous := http.DefaultTransport
	http.DefaultTransport = secretTransport{t}
	defer func() { http.DefaultTransport = previous }()
	if _, err := New("http://worker")(context.Background(), "pdf_to_text", []byte("input"), nil); err != nil {
		t.Fatal(err)
	}
}
