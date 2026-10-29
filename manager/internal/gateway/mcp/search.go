// SPDX-License-Identifier: MIT

package mcp

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/gateway/llm"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

type hit struct {
	Rank    int      `json:"rank"`
	Title   string   `json:"title"`
	URL     string   `json:"url"`
	Snippet string   `json:"snippet"`
	Date    string   `json:"date,omitempty"`
	Engines []string `json:"engines"`
}

var (
	categories = map[string]string{"general": "general", "news": "news", "science": "science"}
	languages  = map[string]string{"de": "de", "en": "en", "auto": "auto"}
	timeRanges = map[string]string{"day": "day", "week": "week", "month": "month", "year": "year"}
	domainRE   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
)

const searchDown = "Web search is not reachable right now. Continue without web search or try again later."

func (s *Server) webSearch(ctx context.Context, u store.User, raw json.RawMessage) (toolResult, callMeta) {
	m := callMeta{capability: "search", target: "searxng", status: http.StatusBadRequest}
	var a struct {
		Query     string   `json:"query"`
		Count     int      `json:"count"`
		Category  string   `json:"category"`
		Language  string   `json:"language"`
		TimeRange string   `json:"time_range"`
		Domains   []string `json:"domains"`
		Page      int      `json:"page"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return errorResult(ctx, "Invalid parameters for web_search."), m
	}
	a.Query = strings.TrimSpace(a.Query)
	if a.Query == "" || utf8.RuneCountInString(a.Query) > 400 {
		return errorResult(ctx, "The query must be 1 to 400 characters long."), m
	}
	a.Count = min(max(a.Count, 0), 10)
	if a.Count == 0 {
		a.Count = 5
	}
	a.Page = min(max(a.Page, 1), 3)
	a.Category, a.Language = cmp.Or(a.Category, "general"), cmp.Or(a.Language, "de")
	cat, ok1 := categories[a.Category]
	lang, ok2 := languages[a.Language]
	tr, ok3 := timeRanges[a.TimeRange]
	if !ok1 || !ok2 || (!ok3 && a.TimeRange != "") || len(a.Domains) > 5 {
		return errorResult(ctx, "Invalid parameters for web_search: category, language, time_range or more than 5 domains."), m
	}
	q := a.Query
	var sites []string
	for _, d := range a.Domains {
		d = strings.ToLower(strings.TrimSpace(d))
		if !domainRE.MatchString(d) {
			return errorResult(ctx, "Invalid domain in domains: host names only, like bmbf.de."), m
		}
		sites = append(sites, "site:"+d)
	}
	if len(sites) > 0 {
		q += " " + strings.Join(sites, " OR ")
	}
	v := url.Values{"q": {q}, "format": {"json"}, "categories": {cat}, "language": {lang},
		"safesearch": {"1"}, "pageno": {strconv.Itoa(a.Page)}}
	if tr != "" {
		v.Set("time_range", tr)
	}
	sum := sha256.Sum256([]byte(v.Encode()))
	key := hex.EncodeToString(sum[:])

	hits, cached := s.results.get(key)
	m.fromCache = cached
	if !cached {
		charge := instance.Current().Accounting.SearchEUR
		if s.SearchChargeEUR != nil {
			charge = *s.SearchChargeEUR
		}
		if s.Reserve != nil {
			release, err := s.Reserve(ctx, u.UID, charge)
			switch {
			case errors.Is(err, llm.ErrDayCap):
				m.status = http.StatusTooManyRequests
				return errorResult(ctx, "Daily budget reached, it continues tomorrow."), m
			case errors.Is(err, llm.ErrBusy):
				m.status = http.StatusTooManyRequests
				return errorResult(ctx, "Too many requests at once, please wait a moment."), m
			case err != nil:
				m.status = http.StatusInternalServerError
				return errorResult(ctx, "Internal error."), m
			}
			m.release = release // meter records the charge before releasing the hold
		}
		m.costEUR = charge
		var err error
		if hits, m.bytesIn, err = s.querySearXNG(ctx, v); err != nil {
			s.Log.WarnContext(ctx, "searxng error", "err", err)
			m.status = http.StatusBadGateway
			return errorResult(ctx, searchDown), m
		}
		s.results.put(key, hits, 15*time.Minute)
		if cat == "general" {
			m.brave = 1 // approximation, docs/adr/README.md §3.7: Brave bills every general query
		}
	}
	m.status = http.StatusOK
	if len(hits) > a.Count {
		hits = hits[:a.Count]
	}
	m.hits = len(hits)
	keys := make([]string, 0, len(hits))
	for _, h := range hits {
		if pu, err := url.Parse(h.URL); err == nil {
			keys = append(keys, normURL(pu))
		}
	}
	s.seen.add(u.UID, keys...)
	structured := map[string]any{"results": hits, "query": a.Query, "total": len(hits), "cached": cached}
	if len(hits) == 0 {
		return toolResult{Content: textResult(ctx, "No hits. Try other search terms."), Structured: structured}, m
	}
	var b strings.Builder
	for _, h := range hits {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", h.Rank, h.Title, h.URL)
		if h.Snippet != "" {
			fmt.Fprintf(&b, "   %s\n", h.Snippet)
		}
		if h.Date != "" {
			b.WriteString(msg(ctx, "   Date: %s\n", h.Date))
		}
	}
	text := frame(ctx, "web_search", []string{msg(ctx, "Search hits for: %s", a.Query)}, b.String())
	return toolResult{Content: []content{{"text", text}}, Structured: structured}, m
}

func (s *Server) querySearXNG(ctx context.Context, v url.Values) ([]hit, int, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", s.SearXNG.JoinPath("/search").String()+"?"+v.Encode(), nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.search.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, len(body), err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, len(body), fmt.Errorf("searxng: HTTP %d", resp.StatusCode)
	}
	var r struct {
		Results []struct {
			URL           string   `json:"url"`
			Title         string   `json:"title"`
			Content       string   `json:"content"`
			PublishedDate *string  `json:"publishedDate"`
			Engines       []string `json:"engines"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, len(body), err
	}
	var hits []hit
	seen := map[string]bool{}
	for _, x := range r.Results {
		pu, err := url.Parse(x.URL)
		if err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" {
			continue
		}
		k := normURL(pu)
		if seen[k] {
			continue
		}
		seen[k] = true
		h := hit{Rank: len(hits) + 1, Title: clip(cleanText(x.Title), 200), URL: x.URL,
			Snippet: clip(cleanText(x.Content), 300), Engines: x.Engines}
		if x.PublishedDate != nil && len(*x.PublishedDate) >= 10 {
			h.Date = (*x.PublishedDate)[:10]
		}
		slices.Sort(h.Engines)
		hits = append(hits, h)
		if len(hits) == 10 {
			break
		}
	}
	return hits, len(body), nil
}

// clip cuts to n runes on a word boundary where possible.
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	c := string(r[:n])
	if i := strings.LastIndexByte(c, ' '); i > n/2 {
		c = c[:i]
	}
	return c + " …"
}
