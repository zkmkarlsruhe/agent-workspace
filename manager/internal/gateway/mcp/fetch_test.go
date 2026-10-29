// SPDX-License-Identifier: MIT

package mcp

import (
	"net/http"
	"net/url"
	"testing"
)

func TestCacheable(t *testing.T) {
	for _, tt := range []struct {
		name, header, value, query string
		want                       bool
	}{
		{name: "plain", want: true},
		{name: "allowed query", query: "page=2&id=3&lang=de", want: true},
		{name: "cookie", header: "Set-Cookie", value: "session=secret"},
		{name: "private", header: "Cache-Control", value: "max-age=60, Private"},
		{name: "no store", header: "Cache-Control", value: "public, no-store"},
		{name: "reserved", header: "TDM-Reservation", value: "1"},
		{name: "private query", query: "token=secret"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			if tt.header != "" {
				h.Set(tt.header, tt.value)
			}
			plain, _ := url.Parse("https://example.org/page")
			queried := *plain
			queried.RawQuery = tt.query
			for _, urls := range [][]*url.URL{{plain, &queried}, {&queried, plain}} {
				if got := cacheable(&http.Response{Header: h}, urls...); got != tt.want {
					t.Fatalf("cacheable=%v, want %v", got, tt.want)
				}
			}
		})
	}
}
