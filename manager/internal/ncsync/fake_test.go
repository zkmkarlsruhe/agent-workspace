// SPDX-License-Identifier: MIT

package ncsync

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
)

// fakeDAV is a Nextcloud files endpoint for one user "z" with Basic z:pw:
// MKCOL, PUT with If-Match/If-None-Match, GET, PROPFIND depth 0/1, MOVE
// without overwrite, SEARCH by name.
type fakeDAV struct {
	mu    sync.Mutex
	files map[string]*davFile // path below the user root
	dirs  map[string]bool
	n     int
	calls []string
	bad   []string // requests that break D58; setup fails the test on any
	// noETag: PUT answers without an ETag header (Nextcloud may omit it)
	noETag bool
	// raceEdit: content the person saves right after the next PUT
	raceEdit string
}

// violation describes a request the manager must never send (D58): any
// method but PROPFIND, GET, SEARCH, MKCOL, PUT, MOVE; a write outside
// "Agent Workspace/"; a PUT without If-None-Match: * or a concrete If-Match;
// a MOVE that may overwrite.
func violation(r *http.Request) string {
	inside := func(p string) bool {
		p = path.Clean(p)
		return p == davRoot+"/"+Root() || strings.HasPrefix(p, davRoot+"/"+Root()+"/")
	}
	switch r.Method {
	case "PROPFIND", "GET", "SEARCH":
		return ""
	case "MKCOL":
	case "PUT":
		if r.Header.Get("If-None-Match") != "*" && (r.Header.Get("If-Match") == "" || strings.Contains(r.Header.Get("If-Match"), "*")) {
			return "unconditional PUT " + r.URL.Path
		}
	case "MOVE":
		du, err := url.Parse(r.Header.Get("Destination"))
		if err != nil || r.Header.Get("Overwrite") != "F" || !inside(du.Path) {
			return "MOVE may overwrite or leave the root: " + r.Header.Get("Destination")
		}
	default:
		return r.Method + " " + r.URL.Path
	}
	if !inside(r.URL.Path) {
		return r.Method + " outside the root: " + r.URL.Path
	}
	return ""
}

type davFile struct {
	data []byte
	etag string
}

func newFakeDAV() *fakeDAV {
	return &fakeDAV{files: map[string]*davFile{}, dirs: map[string]bool{"": true}}
}

const davRoot = "/remote.php/dav/files/z"

// set writes a file as the person would in Nextcloud Office.
func (f *fakeDAV) set(p, data string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	f.files[p] = &davFile{[]byte(data), fmt.Sprintf(`"e%d"`, f.n)}
}

func (f *fakeDAV) get(p string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	x, ok := f.files[p]
	if !ok {
		return "", false
	}
	return string(x.data), true
}

func (f *fakeDAV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, p, _ := r.BasicAuth(); u != "z" || p != "pw" {
		w.WriteHeader(401)
		return
	}
	f.calls = append(f.calls, r.Method)
	if bad := violation(r); bad != "" {
		f.bad = append(f.bad, bad)
		w.WriteHeader(400)
		return
	}
	if r.Method == "SEARCH" {
		f.search(w, r)
		return
	}
	p, ok := strings.CutPrefix(r.URL.Path, davRoot)
	if !ok {
		w.WriteHeader(404)
		return
	}
	p = strings.Trim(p, "/")
	parent := path.Dir(p)
	if parent == "." {
		parent = ""
	}
	switch r.Method {
	case "MKCOL":
		switch {
		case f.dirs[p] || f.files[p] != nil:
			w.WriteHeader(405)
		case !f.dirs[parent]:
			w.WriteHeader(409)
		default:
			f.dirs[p] = true
			w.WriteHeader(201)
		}
	case "PUT":
		old := f.files[p]
		if !f.dirs[parent] {
			w.WriteHeader(409)
			return
		}
		if (r.Header.Get("If-None-Match") == "*" && old != nil) || (r.Header.Get("If-Match") != "" && (old == nil || old.etag != r.Header.Get("If-Match"))) {
			w.WriteHeader(412)
			return
		}
		b, _ := io.ReadAll(r.Body)
		f.n++
		nf := &davFile{b, fmt.Sprintf(`"e%d"`, f.n)}
		f.files[p] = nf
		if !f.noETag {
			w.Header().Set("ETag", nf.etag)
		}
		if f.raceEdit != "" { // the person saves right after the manager's PUT
			f.n++
			f.files[p] = &davFile{[]byte(f.raceEdit), fmt.Sprintf(`"e%d"`, f.n)}
			f.raceEdit = ""
		}
		if old == nil {
			w.WriteHeader(201)
		} else {
			w.WriteHeader(204)
		}
	case "GET":
		x := f.files[p]
		if x == nil {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("ETag", x.etag)
		w.Write(x.data)
	case "MOVE":
		du, _ := url.Parse(r.Header.Get("Destination"))
		dst := strings.Trim(strings.TrimPrefix(du.Path, davRoot), "/")
		if !f.dirs[p] {
			w.WriteHeader(404)
			return
		}
		if f.dirs[dst] {
			w.WriteHeader(412)
			return
		}
		for k := range f.dirs {
			if k == p || strings.HasPrefix(k, p+"/") {
				delete(f.dirs, k)
				f.dirs[dst+strings.TrimPrefix(k, p)] = true
			}
		}
		for k, v := range f.files {
			if strings.HasPrefix(k, p+"/") {
				delete(f.files, k)
				f.files[dst+strings.TrimPrefix(k, p)] = v
			}
		}
		w.WriteHeader(201)
	case "PROPFIND":
		var names []string
		if f.files[p] != nil || f.dirs[p] {
			names = append(names, p)
		} else {
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("Depth") == "1" && f.dirs[p] {
			for k := range f.files {
				if path.Dir(k) == p || (p == "" && !strings.Contains(k, "/")) {
					names = append(names, k)
				}
			}
			for k := range f.dirs {
				if k != "" && k != p && (path.Dir(k) == p || (p == "" && !strings.Contains(k, "/"))) {
					names = append(names, k)
				}
			}
		}
		f.multistatus(w, names)
	default:
		w.WriteHeader(405)
	}
}

func (f *fakeDAV) search(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	body := string(b)
	lit := body[strings.Index(body, "<d:literal>")+len("<d:literal>") : strings.Index(body, "</d:literal>")]
	term := strings.ToLower(strings.Trim(lit, "%"))
	var names []string
	for k := range f.files {
		if strings.Contains(strings.ToLower(path.Base(k)), term) {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	f.multistatus(w, names)
}

func (f *fakeDAV) multistatus(w http.ResponseWriter, names []string) {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0"?><d:multistatus xmlns:d="DAV:">`)
	for _, n := range names {
		href := davRoot + "/" + escape(n)
		if x := f.files[n]; x != nil {
			fmt.Fprintf(&b, `<d:response><d:href>%s</d:href><d:propstat><d:prop><d:resourcetype/><d:getcontentlength>%d</d:getcontentlength><d:getetag>%s</d:getetag><d:getlastmodified>%s</d:getlastmodified></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`,
				href, len(x.data), x.etag, time.Now().UTC().Format(http.TimeFormat))
		} else {
			fmt.Fprintf(&b, `<d:response><d:href>%s/</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`, href)
		}
	}
	b.WriteString(`</d:multistatus>`)
	w.WriteHeader(207)
	w.Write(b.Bytes())
}

// fakeFiles is a person's projects in memory: project -> rel -> file.
type fakeFiles struct {
	mu      sync.Mutex
	p       map[string]map[string]*memFile
	meta    map[string]string
	now     func() time.Time
	reads   int
	running []string
	client  *ocapi.Client
}

type memFile struct {
	data  string
	mtime time.Time
}

func newFakeFiles(now func() time.Time) *fakeFiles {
	return &fakeFiles{p: map[string]map[string]*memFile{}, meta: map[string]string{}, now: now}
}

func (f *fakeFiles) write(project, rel, data string, mtime time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.p[project] == nil {
		f.p[project] = map[string]*memFile{}
	}
	f.p[project][rel] = &memFile{data, mtime}
}

func (f *fakeFiles) read(project, rel string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	x, ok := f.p[project][rel]
	if !ok {
		return "", false
	}
	return x.data, true
}

func (f *fakeFiles) rename(from, to string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.p[to] = f.p[from]
	delete(f.p, from)
}

func (f *fakeFiles) ScanFiles(_ context.Context, _, project, sub string) ([]lifecycle.File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []lifecycle.File
	for rel, x := range f.p[project] {
		if sub == "" || strings.HasPrefix(rel, sub+"/") {
			out = append(out, lifecycle.File{Name: path.Base(rel), Path: rel, Size: int64(len(x.data)), Modified: x.mtime})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (f *fakeFiles) ReadFile(_ context.Context, _, project, rel string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	x, ok := f.p[project][rel]
	if !ok {
		return nil, lifecycle.ErrNotFound
	}
	return io.NopCloser(strings.NewReader(x.data)), nil
}

func (f *fakeFiles) ReplaceFile(_ context.Context, _, project, rel string, r io.Reader) error {
	b, _ := io.ReadAll(r)
	f.write(project, rel, string(b), f.now())
	return nil
}

func (f *fakeFiles) Upload(_ context.Context, _, project, name string, r io.Reader) (string, error) {
	b, _ := io.ReadAll(r)
	f.write(project, "inbox/"+name, string(b), f.now())
	return name, nil
}

func (f *fakeFiles) WriteMeta(_ context.Context, _, project, name, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.meta[project+"/"+name] = content
	return nil
}

func (f *fakeFiles) RunningUIDs() []string               { return f.running }
func (f *fakeFiles) Client(string) (*ocapi.Client, bool) { return f.client, f.client != nil }
