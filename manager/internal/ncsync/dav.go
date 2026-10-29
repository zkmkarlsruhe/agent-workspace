// SPDX-License-Identifier: MIT

package ncsync

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/logx"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ncguard"
)

// Creds is a person's Nextcloud login from internal/house: User is the
// WebDAV user id, Login and Password go into the Basic header.
type Creds struct{ User, Login, Password string }

// DAV is the small WebDAV client of the sync and the write tools. It never
// follows a redirect, so the Basic header only reaches the configured host.
type DAV struct {
	Base *url.URL // https://cloud.example.org, without /remote.php
	HTTP *http.Client
}

// StatusError is a Nextcloud answer the caller did not expect.
type StatusError struct{ Code int }

func (e *StatusError) Error() string { return fmt.Sprintf("nextcloud: HTTP %d", e.Code) }

// HTTPStatus returns the HTTP status inside err, 0 for transport errors.
func HTTPStatus(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code
	}
	return 0
}

func NewDAV(base *url.URL) *DAV {
	return &DAV{Base: base, HTTP: &http.Client{Transport: ncguard.Transport{Base: base,
		Next: logx.Transport{Upstream: "nextcloud", Propagate: true, Body: true}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// CleanPath checks a path inside the person's Nextcloud: "" and "/" are the
// root, ".." and NUL are refused. Returns it without leading slash.
func CleanPath(p string) (string, bool) {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" {
		return "", true
	}
	if len(p) > 1024 || strings.ContainsRune(p, 0) || strings.Contains(p, "\\") {
		return "", false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
	}
	return p, true
}

// ErrOutside is a write the client refuses: outside Root, not normalised or
// destructive (D58).
type outsideError struct{}

func (outsideError) Error() string { return "nextcloud: write outside " + Root() + " refused" }

var ErrOutside error = outsideError{}

// Writable cleans p and reports whether a write may touch it (D58): Root or
// below, no "..", "." or empty segments, no NUL or backslash, no segment that
// starts with a dot, also not percent-encoded (%2e%2e).
func Writable(p string) (string, bool) {
	c, ok := CleanPath(p)
	if !ok || (c != Root() && !strings.HasPrefix(c, Root()+"/")) {
		return "", false
	}
	for _, seg := range strings.Split(c, "/") {
		u, err := url.PathUnescape(seg)
		if strings.HasPrefix(seg, ".") || err == nil && (strings.HasPrefix(u, ".") || strings.ContainsAny(u, "/\\\x00")) {
			return "", false
		}
	}
	return c, true
}

// writeURL is filesURL for a write; anything Writable refuses is ErrOutside.
func (d *DAV) writeURL(user, p string) (string, error) {
	c, ok := Writable(p)
	if !ok {
		return "", ErrOutside
	}
	return d.filesURL(user, c), nil
}

func escape(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// filesURL is <base>/remote.php/dav/files/<user>/<p>.
func (d *DAV) filesURL(user, p string) string {
	u := strings.TrimRight(d.Base.String(), "/") + "/remote.php/dav/files/" + url.PathEscape(user)
	if p != "" {
		u += "/" + escape(p)
	}
	return u
}

// WebURL opens a folder in the Files app.
func (d *DAV) WebURL(dir string) string {
	return strings.TrimRight(d.Base.String(), "/") + "/apps/files/?dir=" + url.QueryEscape("/"+dir)
}

func (d *DAV) do(ctx context.Context, c Creds, method, u string, body io.Reader, hdr map[string]string) (*http.Response, error) {
	switch method {
	case "PROPFIND", "SEARCH", http.MethodGet, "MKCOL", http.MethodPut, "MOVE":
	default:
		return nil, ErrOutside // no DELETE, COPY, PROPPATCH (D58)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.Login, c.Password)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return d.HTTP.Do(req)
}

func drain(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
}

// The client reads (PROPFIND, GET, SEARCH) anywhere in the person's
// Nextcloud and writes (MKCOL, PUT, MOVE) only below Root, never
// destructively: there is no DELETE, a PUT either creates (If-None-Match: *)
// or replaces the helper's own unchanged version (If-Match), a MOVE never
// overwrites (D58).

// Mkdirs creates dir and its parents below Root (MKCOL; 405 = exists).
func (d *DAV) Mkdirs(ctx context.Context, c Creds, dir string) error {
	dir, ok := Writable(dir)
	if !ok {
		return ErrOutside
	}
	cur := ""
	for _, seg := range strings.Split(dir, "/") {
		cur = path.Join(cur, seg)
		resp, err := d.do(ctx, c, "MKCOL", d.filesURL(c.User, cur), nil, nil)
		if err != nil {
			return err
		}
		drain(resp)
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusMethodNotAllowed {
			return &StatusError{resp.StatusCode}
		}
	}
	return nil
}

// Put uploads r to p below Root. ifMatch "" = only if p does not exist yet
// (If-None-Match: *), else only if p still has that ETag (the helper's last
// write); there is no unconditional mode. A refused precondition is
// StatusError 412. Returns the new ETag ("" if Nextcloud sent none). mtime,
// if set, becomes the modification time shown in Nextcloud.
func (d *DAV) Put(ctx context.Context, c Creds, p string, r io.Reader, ifMatch string, mtime time.Time) (string, error) {
	u, err := d.writeURL(c.User, p)
	if err != nil {
		return "", err
	}
	hdr := map[string]string{"Content-Type": "application/octet-stream"}
	switch {
	case ifMatch == "":
		hdr["If-None-Match"] = "*"
	case strings.Contains(ifMatch, "*") || strings.Trim(ifMatch, `W/"`) == "":
		return "", ErrOutside // would replace whatever is there
	default:
		hdr["If-Match"] = ifMatch
	}
	if !mtime.IsZero() {
		hdr["X-OC-Mtime"] = strconv.FormatInt(mtime.Unix(), 10)
	}
	resp, err := d.do(ctx, c, http.MethodPut, u, r, hdr)
	if err != nil {
		return "", err
	}
	drain(resp)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return "", &StatusError{resp.StatusCode}
	}
	return resp.Header.Get("ETag"), nil
}

// Move renames from to to without overwriting (412 if to exists); both below Root.
func (d *DAV) Move(ctx context.Context, c Creds, from, to string) error {
	src, err := d.writeURL(c.User, from)
	if err != nil {
		return err
	}
	dst, err := d.writeURL(c.User, to)
	if err != nil {
		return err
	}
	resp, err := d.do(ctx, c, "MOVE", src, nil, map[string]string{"Destination": dst, "Overwrite": "F"})
	if err != nil {
		return err
	}
	drain(resp)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return &StatusError{resp.StatusCode}
	}
	return nil
}

// Get opens p for reading; the caller closes the body.
func (d *DAV) Get(ctx context.Context, c Creds, p string) (io.ReadCloser, string, error) {
	resp, err := d.do(ctx, c, http.MethodGet, d.filesURL(c.User, p), nil, nil)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusOK {
		drain(resp)
		return nil, "", &StatusError{resp.StatusCode}
	}
	return resp.Body, resp.Header.Get("ETag"), nil
}

// Entry is one file or folder of a listing; Path is relative to the user root.
type Entry struct {
	Path     string
	Dir      bool
	Size     int64
	ETag     string
	Modified time.Time
}

const propfindBody = `<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:resourcetype/><d:getcontentlength/><d:getlastmodified/><d:getetag/></d:prop></d:propfind>`

type multistatus struct {
	Responses []struct {
		Href  string `xml:"href"`
		Props []struct {
			Status string `xml:"status"`
			Prop   struct {
				Collection *struct{} `xml:"resourcetype>collection"`
				Length     string    `xml:"getcontentlength"`
				Modified   string    `xml:"getlastmodified"`
				ETag       string    `xml:"getetag"`
			} `xml:"prop"`
		} `xml:"propstat"`
	} `xml:"response"`
}

// parse reads a multistatus answer. Paths come from the href relative to the
// user root; an href outside that root is dropped.
func (d *DAV) parse(r io.Reader, user string) ([]Entry, error) {
	var ms multistatus
	if err := xml.NewDecoder(io.LimitReader(r, 16<<20)).Decode(&ms); err != nil {
		return nil, err
	}
	root, _ := url.Parse(d.filesURL(user, ""))
	var out []Entry
	for _, resp := range ms.Responses {
		u, err := url.Parse(resp.Href)
		if err != nil {
			continue
		}
		// cleaned first: a decoded %2e%2e must not climb out of the user root
		rel, ok := strings.CutPrefix(path.Clean(u.Path), root.Path)
		if !ok || rel != "" && rel[0] != '/' {
			continue
		}
		e := Entry{Path: strings.Trim(rel, "/")}
		for _, ps := range resp.Props {
			if !strings.Contains(ps.Status, " 200 ") {
				continue
			}
			e.Dir = ps.Prop.Collection != nil
			e.Size, _ = strconv.ParseInt(ps.Prop.Length, 10, 64)
			e.ETag = ps.Prop.ETag
			e.Modified, _ = http.ParseTime(ps.Prop.Modified)
		}
		out = append(out, e)
	}
	return out, nil
}

// List is PROPFIND with depth "0" (p itself) or "1" (p and its children).
func (d *DAV) List(ctx context.Context, c Creds, p, depth string) ([]Entry, error) {
	resp, err := d.do(ctx, c, "PROPFIND", d.filesURL(c.User, p), strings.NewReader(propfindBody),
		map[string]string{"Depth": depth, "Content-Type": "application/xml"})
	if err != nil {
		return nil, err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusMultiStatus {
		return nil, &StatusError{resp.StatusCode}
	}
	return d.parse(resp.Body, c.User)
}

// Search finds files and folders below dir whose name contains term (WebDAV
// SEARCH, DASL basicsearch on the files scope), at most limit hits.
func (d *DAV) Search(ctx context.Context, c Creds, dir, term string, limit int) ([]Entry, error) {
	dir, ok := CleanPath(dir)
	if !ok || strings.ContainsAny(c.User, "/\\") {
		return nil, ErrOutside // the scope must stay below /files/<user>/
	}
	var esc strings.Builder
	xml.EscapeText(&esc, []byte("%"+strings.NewReplacer("%", "", "_", "").Replace(term)+"%"))
	scope := "/files/" + c.User
	if dir != "" {
		scope += "/" + dir
	}
	var sc strings.Builder
	xml.EscapeText(&sc, []byte(scope))
	body := `<?xml version="1.0" encoding="UTF-8"?>
<d:searchrequest xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:basicsearch>
<d:select><d:prop><d:resourcetype/><d:getcontentlength/><d:getlastmodified/><d:getetag/></d:prop></d:select>
<d:from><d:scope><d:href>` + sc.String() + `</d:href><d:depth>infinity</d:depth></d:scope></d:from>
<d:where><d:like><d:prop><d:displayname/></d:prop><d:literal>` + esc.String() + `</d:literal></d:like></d:where>
<d:orderby><d:order><d:prop><d:getlastmodified/></d:prop><d:descending/></d:order></d:orderby>
<d:limit><d:nresults>` + strconv.Itoa(limit) + `</d:nresults></d:limit>
</d:basicsearch></d:searchrequest>`
	resp, err := d.do(ctx, c, "SEARCH", strings.TrimRight(d.Base.String(), "/")+"/remote.php/dav/", strings.NewReader(body),
		map[string]string{"Content-Type": "text/xml"})
	if err != nil {
		return nil, err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusMultiStatus {
		return nil, &StatusError{resp.StatusCode}
	}
	return d.parse(resp.Body, c.User)
}
