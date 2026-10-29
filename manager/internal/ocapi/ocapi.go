// SPDX-License-Identifier: MIT

// Package ocapi is the manager's client for the OpenCode 2.2.0 server API
// (docs/adr/README.md §3.14, corrected session create from docs/adr/README.md §0 K1).
package ocapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

type Client struct {
	BaseURL  string // http://<container-ip>:4096
	Password string
	HTTP     *http.Client
}

// StatusError carries a non-2xx answer.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string { return fmt.Sprintf("opencode: HTTP %d: %s", e.Code, e.Body) }

// IsStatus reports whether err is a StatusError with the given code.
func IsStatus(err error, code int) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == code
}

// loc builds ?location[directory]=<abs>, the only query form OpenCode honours
// for folder-scoped reads.
func loc(dir string) url.Values {
	v := url.Values{}
	if dir != "" {
		v.Set("location[directory]", dir)
	}
	return v
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, body io.Reader, ctype string, out any) error {
	_, err := c.doN(ctx, method, path, q, body, ctype, out, maxBody)
	return err
}

// doN is do with the answer bounded to limit bytes; it returns the bytes read.
func (c *Client) doN(ctx context.Context, method, path string, q url.Values, body io.Reader, ctype string, out any, limit int64) (int64, error) {
	u := strings.TrimRight(c.BaseURL, "/") + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return 0, err
	}
	req.SetBasicAuth("opencode", c.Password)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.client().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, &StatusError{Code: resp.StatusCode, Body: string(b)}
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return 0, nil
	}
	cr := &countReader{r: io.LimitReader(resp.Body, limit)}
	err = json.NewDecoder(cr).Decode(out)
	return cr.n, err
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// capped decodes a JSON array of at most max entries, counting while it
// decodes: an untrusted container cannot make the manager hold more.
type capped[T any] struct {
	v   []T
	max int
}

func (c *capped[T]) UnmarshalJSON(b []byte) error {
	d := json.NewDecoder(bytes.NewReader(b))
	t, err := d.Token()
	if err != nil || t == nil { // null = empty
		return err
	}
	if t != json.Delim('[') {
		return errors.New("opencode: list expected")
	}
	for d.More() {
		if len(c.v) >= c.max {
			return ErrTooManySessions
		}
		var e T
		if err := d.Decode(&e); err != nil {
			return err
		}
		c.v = append(c.v, e)
	}
	_, err = d.Token()
	return err
}

// maxBody bounds one JSON answer; the container is not trusted, and a page of
// messages with tool output stays far below it.
const maxBody = 32 << 20

// client never follows redirects: the container is untrusted and a 3xx must
// not steer manager requests to any host but the assigned container.
func (c *Client) client() *http.Client {
	hc := http.Client{}
	if c.HTTP != nil {
		hc = *c.HTTP
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &hc
}

func (c *Client) json(ctx context.Context, method, path string, q url.Values, in, out any) error {
	var body io.Reader
	ctype := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ctype = bytes.NewReader(b), "application/json"
	}
	return c.do(ctx, method, path, q, body, ctype, out)
}

type Info struct {
	Version string `json:"version"`
	PID     int    `json:"pid"`
}

// Info is the readiness probe: 200 ready, 503 booting, 401 wrong password.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var i Info
	return i, c.json(ctx, "GET", "/api/info", nil, nil, &i)
}

// Active returns the ids of sessions with a running execution; empty = idle.
func (c *Client) Active(ctx context.Context) ([]string, error) {
	var r struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := c.json(ctx, "GET", "/api/session/active", nil, nil, &r); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(r.Data))
	for id := range r.Data {
		ids = append(ids, id)
	}
	return ids, nil
}

type Location struct {
	Directory string `json:"directory"`
}

type Session struct {
	ID        string   `json:"id"`
	ProjectID string   `json:"projectID"`
	Title     string   `json:"title"`
	Agent     string   `json:"agent"`
	ParentID  string   `json:"parentID,omitempty"`
	Location  Location `json:"location"`
	Outcome   string   `json:"outcome"` // succeeded | failed | interrupted | ""
	CostUSD   float64  `json:"cost"`
	Time      struct {
		Created  float64  `json:"created"` // unix ms
		Updated  float64  `json:"updated"`
		Archived *float64 `json:"archived,omitempty"`
	} `json:"time"`
}

// SessionCreate creates a session in dir. The directory must go in the JSON
// body; the query form is silently ignored by OpenCode.
func (c *Client) SessionCreate(ctx context.Context, dir, title string, agent ...string) (Session, error) {
	in := map[string]any{"location": Location{Directory: dir}}
	if len(agent) > 0 && agent[0] != "" {
		in["agent"] = agent[0]
	}
	if title != "" {
		in["title"] = title
	}
	var r struct {
		Data Session `json:"data"`
	}
	if err := c.json(ctx, "POST", "/api/session", nil, in, &r); err != nil {
		return r.Data, err
	}
	if r.Data.Location.Directory != dir {
		return r.Data, fmt.Errorf("opencode: session %s created in %q, want %q", r.Data.ID, r.Data.Location.Directory, dir)
	}
	return r.Data, nil
}

// SessionGet reads one session (GET /api/session/{id}, docs/adr/README.md §3.14).
func (c *Client) SessionGet(ctx context.Context, id string) (Session, error) {
	var r struct {
		Data Session `json:"data"`
	}
	return r.Data, c.json(ctx, "GET", "/api/session/"+url.PathEscape(id), nil, nil, &r)
}

// SessionPrompt queues text as the next user message; the agent runs without a browser.
func (c *Client) SessionPrompt(ctx context.Context, id, text string) error {
	return c.json(ctx, "POST", "/api/session/"+url.PathEscape(id)+"/prompt", nil, map[string]string{"text": text}, nil)
}

// LoadLocation waits for plugin activation before the first prompt or redirect.
// /api/model waits up to 5 s, then returns 503 (packages/server/src/location.ts).
func (c *Client) LoadLocation(ctx context.Context, dir string) error {
	return c.json(ctx, "GET", "/api/model", loc(dir), nil, nil)
}

// MCPConnected reports whether the named MCP server is connected for dir.
// OpenCode connects MCP servers per location after it loaded; a prompt that
// runs before that sees no MCP tools ("No tool named workspace_web_search").
func (c *Client) MCPConnected(ctx context.Context, dir, name string) (bool, error) {
	var r struct {
		Data []struct {
			Name   string `json:"name"`
			Status struct {
				Status string `json:"status"`
			} `json:"status"`
		} `json:"data"`
	}
	if err := c.json(ctx, "GET", "/api/mcp", loc(dir), nil, &r); err != nil {
		return false, err
	}
	for _, m := range r.Data {
		if m.Name == name {
			return m.Status.Status == "connected", nil
		}
	}
	return false, nil
}

// MessageTypes returns the type of each stored message of a session, newest
// first. A prompt counts as "user" only once the runner took it from the inbox.
func (c *Client) MessageTypes(ctx context.Context, id string) ([]string, error) {
	var r struct {
		Data []struct {
			Type string `json:"type"`
		} `json:"data"`
	}
	if err := c.json(ctx, "GET", "/api/session/"+url.PathEscape(id)+"/message", nil, nil, &r); err != nil {
		return nil, err
	}
	types := make([]string, len(r.Data))
	for i, m := range r.Data {
		types[i] = m.Type
	}
	return types, nil
}

// sessionPage is the page size the manager asks for and the most entries a
// page may carry.
const sessionPage = 200

// SessionList lists sessions of dir (first page; limit 0 = sessionPage, at most sessionPage).
func (c *Client) SessionList(ctx context.Context, dir string, limit int) ([]Session, error) {
	if limit <= 0 || limit > sessionPage {
		limit = sessionPage
	}
	q := url.Values{"directory": {dir}, "limit": {fmt.Sprint(limit)}}
	r := struct {
		Data capped[Session] `json:"data"`
	}{Data: capped[Session]{max: limit}}
	_, err := c.doN(ctx, "GET", "/api/session", q, nil, "", &r, maxSessionBytes)
	return r.Data.v, err
}

// maxPages bounds SessionListAll (maxPages × sessionPage sessions): the
// container is untrusted and could answer with full pages and a cursor forever.
const maxPages = 50

// maxSessionBytes bounds the answers of one session listing together.
const maxSessionBytes = 16 << 20

var ErrTooManySessions = errors.New("opencode: session list does not end")

// SessionListAll lists every session of dir, roots and children, following
// cursor.next (the cursor carries the directory filter). A repeated cursor,
// a page with more than sessionPage entries, more than maxPages pages or
// more than maxSessionBytes in all is an error.
func (c *Client) SessionListAll(ctx context.Context, dir string) ([]Session, error) {
	q := url.Values{"directory": {dir}, "limit": {fmt.Sprint(sessionPage)}}
	var all []Session
	seen := map[string]bool{}
	budget := int64(maxSessionBytes)
	for range maxPages {
		r := struct {
			Data   capped[Session] `json:"data"`
			Cursor struct {
				Next string `json:"next"`
			} `json:"cursor"`
		}{Data: capped[Session]{max: sessionPage}}
		n, err := c.doN(ctx, "GET", "/api/session", q, nil, "", &r, budget)
		if err != nil {
			return nil, err
		}
		if budget -= n; budget <= 0 {
			return nil, ErrTooManySessions
		}
		all = append(all, r.Data.v...)
		if len(r.Data.v) < sessionPage || r.Cursor.Next == "" {
			return all, nil
		}
		if seen[r.Cursor.Next] {
			return nil, ErrTooManySessions
		}
		seen[r.Cursor.Next] = true
		q = url.Values{"cursor": {r.Cursor.Next}, "limit": {fmt.Sprint(sessionPage)}}
	}
	return nil, ErrTooManySessions
}

// SessionMove points a session at another directory, which must exist
// (session.move, packages/protocol/src/groups/session.ts:323).
func (c *Client) SessionMove(ctx context.Context, id, dir string) error {
	return c.json(ctx, "POST", "/api/session/"+url.PathEscape(id)+"/move", nil, map[string]string{"directory": dir}, nil)
}

// SessionArchive sets the archive state without deleting the chat.
func (c *Client) SessionArchive(ctx context.Context, id string, archived bool) error {
	return c.json(ctx, "PATCH", "/api/session/"+url.PathEscape(id), nil, map[string]bool{"archived": archived}, nil)
}

// SessionDelete removes a session and its children; 404 if already gone.
func (c *Client) SessionDelete(ctx context.Context, id string) error {
	return c.json(ctx, "DELETE", "/api/session/"+url.PathEscape(id), nil, nil, nil)
}

func (c *Client) SessionInterrupt(ctx context.Context, id string) error {
	return c.json(ctx, "POST", "/api/session/"+url.PathEscape(id)+"/interrupt", nil, nil, nil)
}

// VCSInit runs git init in dir (D11 undo).
func (c *Client) VCSInit(ctx context.Context, dir string) error {
	return c.json(ctx, "POST", "/api/vcs/init", loc(dir), nil, nil)
}

type Project struct {
	ID        string `json:"id"`
	Canonical string `json:"canonical"` // absolute project root
	VCS       string `json:"vcs"`
}

// Projects is a bare JSON array, unlike most other endpoints.
func (c *Client) Projects(ctx context.Context) ([]Project, error) {
	var r []Project
	return r, c.json(ctx, "GET", "/api/project", nil, nil, &r)
}

// PermissionRequests and Forms are per location; the manager asks per project folder.
func (c *Client) PermissionRequests(ctx context.Context, dir string) ([]json.RawMessage, error) {
	return c.list(ctx, "/api/permission/request", dir)
}

func (c *Client) Forms(ctx context.Context, dir string) ([]json.RawMessage, error) {
	return c.list(ctx, "/api/form", dir)
}

func (c *Client) list(ctx context.Context, path, dir string) ([]json.RawMessage, error) {
	var r struct {
		Data []json.RawMessage `json:"data"`
	}
	return r.Data, c.json(ctx, "GET", path, loc(dir), nil, &r)
}

// SessionStats is the optional metering cross-check (raw, fields not yet used).
func (c *Client) SessionStats(ctx context.Context) (json.RawMessage, error) {
	var r json.RawMessage
	return r, c.json(ctx, "GET", "/api/experimental/session/stats", nil, nil, &r)
}

// FSWrite writes data to the absolute path p inside dir; missing parents are created.
func (c *Client) FSWrite(ctx context.Context, dir, p string, data []byte) error {
	q := loc(dir)
	q.Set("path", p)
	return c.do(ctx, "POST", "/api/experimental/fs/write", q, bytes.NewReader(data), "application/octet-stream", nil)
}

type Entry struct {
	Path string `json:"path"` // relative to the listed directory
	Type string `json:"type"` // file | directory
}

// FSList lists the entries of sub (relative) inside dir, one level.
func (c *Client) FSList(ctx context.Context, dir, sub string) ([]Entry, error) {
	q := loc(dir)
	if sub != "" {
		q.Set("path", sub)
	}
	var r struct {
		Data []Entry `json:"data"`
	}
	return r.Data, c.json(ctx, "GET", "/api/fs/list", q, nil, &r)
}

// Message is one entry of GET /api/session/{id}/message (packages/schema/src/
// session-message.ts:304): Type is the discriminator (user, assistant, shell,
// synthetic, system, compaction, idle, ...). Attachment bytes are not decoded.
type Message struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Time struct {
		Created float64 `json:"created"` // unix ms
	} `json:"time"`
	Text  string `json:"text"` // user, synthetic, system
	Files []struct {
		Name string `json:"name"`
		Mime string `json:"mime"`
	} `json:"files"` // user attachments
	Content []Part          `json:"content"` // assistant
	Command string          `json:"command"` // shell
	Error   json.RawMessage `json:"error"`   // assistant: {type, message}
}

// Part is an assistant content part: text, reasoning or tool.
// Tool execution stays in state; provider-owned native data is not exported.
type Part struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Name  string `json:"name"` // tool name
	State struct {
		Status  string          `json:"status"` // streaming | running | completed | error
		Input   json.RawMessage `json:"input"`  // object; a raw string while streaming
		Content []struct {
			Type string `json:"type"` // text | file
			Text string `json:"text"`
			URI  string `json:"uri"`
		} `json:"content"`
		Error json.RawMessage `json:"error"`
	} `json:"state"`
}

// Messages returns one page of a session's messages, oldest first, and the
// cursor of the next page ("" at the end). The server sets cursor.next on
// every non-empty page, so a short page ends the list.
func (c *Client) Messages(ctx context.Context, id, cursor string, limit int) ([]Message, string, error) {
	q := url.Values{"limit": {fmt.Sprint(limit)}}
	if cursor == "" {
		q.Set("order", "asc") // the cursor carries its own order; both together are refused
	} else {
		q.Set("cursor", cursor)
	}
	var r struct {
		Data   []Message `json:"data"`
		Cursor struct {
			Next string `json:"next"`
		} `json:"cursor"`
	}
	if err := c.json(ctx, "GET", "/api/session/"+url.PathEscape(id)+"/message", q, nil, &r); err != nil {
		return nil, "", err
	}
	if len(r.Data) < limit {
		r.Cursor.Next = ""
	}
	return r.Data, r.Cursor.Next, nil
}

// LastMessages returns the newest n messages of a session, oldest first.
func (c *Client) LastMessages(ctx context.Context, id string, n int) ([]Message, error) {
	q := url.Values{"limit": {fmt.Sprint(n)}, "order": {"desc"}}
	var r struct {
		Data []Message `json:"data"`
	}
	if err := c.json(ctx, "GET", "/api/session/"+url.PathEscape(id)+"/message", q, nil, &r); err != nil {
		return nil, err
	}
	slices.Reverse(r.Data)
	return r.Data, nil
}
