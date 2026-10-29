// SPDX-License-Identifier: MIT

package main

import (
	"net/url"
	"slices"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
)

func TestFetchDenyHosts(t *testing.T) {
	cfg := config.Config{RoomsURL: &url.URL{Scheme: "https", Host: "rooms.test:443"},
		BikesURL: &url.URL{Host: "bikes.test"}, LiteLLMBaseURL: "https://llm.test/v1",
		SearXNGURL: "https://search.test:8080/search", PDFURL: "http://gotenberg.test:3000",
		House: []config.HouseService{{URL: &url.URL{Host: "cloud.test"}}}, FetchDenyHosts: []string{"extra.test"}}
	want := []string{"extra.test", "rooms.test", "bikes.test", "llm.test", "search.test", "gotenberg.test", "cloud.test"}
	if got := fetchDenyHosts(cfg); !slices.Equal(got, want) {
		t.Fatalf("hosts=%v, want %v", got, want)
	}
}

func TestFetchDenyHostsSplitUpstreams(t *testing.T) {
	for _, legacy := range []string{"", "https://legacy.test/v1"} {
		cfg := config.Config{LiteLLMBaseURL: legacy, LLMUpstreamURL: "https://models.test:443", MCPUpstreamURL: "https://broker.test:8443"}
		got := fetchDenyHosts(cfg)
		want := []string{"models.test", "broker.test"}
		if legacy != "" {
			want = append([]string{"legacy.test"}, want...)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("hosts=%v want %v", got, want)
		}
	}
}
