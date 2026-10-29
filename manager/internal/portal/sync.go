// SPDX-License-Identifier: MIT

package portal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Sync is the core side of "In Nextcloud ablegen" on the project page
// (internal/ncsync): link the project to a Nextcloud folder, fetch the
// person's versions back, record collections (D47). nil = no section.
type Sync interface {
	// Connected reports whether the person has a working Nextcloud login.
	Connected(ctx context.Context, uid string) bool
	Status(ctx context.Context, uid, project string) (SyncStatus, error)
	// Link creates the folder and links the project; ErrNotConnected without login.
	Link(ctx context.Context, uid, project string, all bool) error
	Unlink(ctx context.Context, uid, project string) error
	Pull(ctx context.Context, uid, project string) (PullResult, error)
	// AddCollection returns ErrNotFound unless pfad is a folder in the person's Nextcloud.
	AddCollection(ctx context.Context, uid, project, path string) error
	RemoveCollection(ctx context.Context, uid, project, path string) error
}

var ErrNotConnected = errors.New("nextcloud not connected")

type SyncStatus struct {
	Connected, Linked, All bool
	Folder, URL            string
	Last                   time.Time
	Notice                 string // last conflict, e.g. a "(Helfer)" copy
	Collections            []Collection
}

type Collection struct{ Path, URL string }

// PullResult lists project paths: updated in place, new, and the person's
// versions that went to inbox/ because both sides changed.
type PullResult struct{ Updated, New, Conflicts []string }

func (s *Server) syncRoutes(page func(string, func(http.ResponseWriter, *http.Request, Identity))) {
	if s.sync == nil {
		return
	}
	page("POST /projects/{name}/nextcloud/link", s.ncLink)
	page("POST /projects/{name}/nextcloud/unlink", s.ncUnlink)
	page("POST /projects/{name}/nextcloud/pull", s.ncPull)
	page("POST /projects/{name}/collections", s.collectionAdd)
	page("POST /projects/{name}/collections/remove", s.collectionRemove)
}

// syncText turns ?nc= of the project page into the notice.
func syncText(l string, q url.Values) string {
	switch q.Get("nc") {
	case "linked":
		return "The project is linked to Nextcloud. New and changed results appear there after a few seconds."
	case "unlinked":
		return "The project is unlinked. The files in Nextcloud stay there."
	case "not-connected":
		return "Nextcloud is not connected. Connect it first under Connections."
	case "error":
		return "Nextcloud did not respond. Nothing has changed in your files. Try again in a few minutes."
	case "collection-added":
		return "The collection is linked. The helper can search and read it from its next step in the chat. It cannot write there."
	case "collection-missing":
		return "This folder does not exist in your Nextcloud. Enter the path as shown in Nextcloud under Files, for example Exhibition 2027/Loan agreements."
	case "collection-removed":
		return "The collection is removed."
	case "pulled":
		a, _ := strconv.Atoi(q.Get("u")) // updated
		n, _ := strconv.Atoi(q.Get("n"))
		k, _ := strconv.Atoi(q.Get("c")) // conflicts
		if a+n+k == 0 {
			return "There is nothing newer in Nextcloud. The project is up to date."
		}
		var parts []string
		if a > 0 {
			parts = append(parts, trf(l, plural(a, "%d file updated", "%d files updated"), a))
		}
		if n > 0 {
			parts = append(parts, trf(l, plural(n, "%d new file", "%d new files"), n))
		}
		t := trf(l, "Fetched from Nextcloud: %s.", strings.Join(parts, ", "))
		if len(parts) == 0 {
			t = ""
		}
		if k > 0 {
			t += " " + trf(l, plural(k, "%d file was also changed by the helper; your version is under “My files” with “(Nextcloud)” in its name.",
				"%d files were also changed by the helper; your version is under “My files” with “(Nextcloud)” in their names."), k)
		}
		return strings.TrimSpace(t)
	}
	return ""
}

// plural picks the singular or plural format string.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func (s *Server) ncBack(w http.ResponseWriter, r *http.Request, name, query string) {
	http.Redirect(w, r, "/projects/"+name+"?"+query+"#nextcloud", http.StatusSeeOther)
}

// ncErr answers a failed Nextcloud action on the project page.
func (s *Server) ncErr(w http.ResponseWriter, r *http.Request, name string, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		s.errorPage(w, r, http.StatusNotFound, "Project not found", "This project does not exist.")
	case errors.Is(err, ErrNotConnected):
		s.ncBack(w, r, name, "nc=not-connected")
	default:
		slog.WarnContext(r.Context(), "portal: nextcloud", "err", err)
		s.ncBack(w, r, name, "nc=error")
	}
}

func (s *Server) ncLink(w http.ResponseWriter, r *http.Request, id Identity) {
	name, ok := s.projectName(w, r)
	if !ok {
		return
	}
	if err := s.sync.Link(r.Context(), id.UID, name, r.FormValue("all") == "1"); err != nil {
		s.ncErr(w, r, name, err)
		return
	}
	s.ncBack(w, r, name, "nc=linked")
}

func (s *Server) ncUnlink(w http.ResponseWriter, r *http.Request, id Identity) {
	name, ok := s.projectName(w, r)
	if !ok {
		return
	}
	if err := s.sync.Unlink(r.Context(), id.UID, name); err != nil {
		s.ncErr(w, r, name, err)
		return
	}
	s.ncBack(w, r, name, "nc=unlinked")
}

func (s *Server) ncPull(w http.ResponseWriter, r *http.Request, id Identity) {
	name, ok := s.projectName(w, r)
	if !ok {
		return
	}
	g, err := s.sync.Pull(r.Context(), id.UID, name)
	if err != nil {
		s.ncErr(w, r, name, err)
		return
	}
	s.ncBack(w, r, name, fmt.Sprintf("nc=pulled&u=%d&n=%d&c=%d", len(g.Updated), len(g.New), len(g.Conflicts)))
}

func (s *Server) collectionAdd(w http.ResponseWriter, r *http.Request, id Identity) {
	name, ok := s.projectName(w, r)
	if !ok {
		return
	}
	err := s.sync.AddCollection(r.Context(), id.UID, name, strings.TrimSpace(r.FormValue("path")))
	if errors.Is(err, ErrNotFound) {
		s.ncBack(w, r, name, "nc=collection-missing")
		return
	}
	if err != nil {
		s.ncErr(w, r, name, err)
		return
	}
	s.ncBack(w, r, name, "nc=collection-added")
}

func (s *Server) collectionRemove(w http.ResponseWriter, r *http.Request, id Identity) {
	name, ok := s.projectName(w, r)
	if !ok {
		return
	}
	if err := s.sync.RemoveCollection(r.Context(), id.UID, name, r.FormValue("path")); err != nil {
		s.ncErr(w, r, name, err)
		return
	}
	s.ncBack(w, r, name, "nc=collection-removed")
}
