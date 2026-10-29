// SPDX-License-Identifier: MIT

package portal

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/co2"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/convert"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// --- Start page: task and project list ---

type startData struct {
	Capabilities         []Tool
	SelectedCapabilities []string
	Projects             []Project
	Trash                int
	Templates            []taskTemplate
	Env                  EnvState // asked after ListProjects, which may wait for a start
	Description          string
	Template             taskTemplate // template that prefilled Description (zero if none)
	FilesLost            bool         // a refused submit carried files: they must be picked again
	MaxUpload            int64
	Assistants           []assistantCard
	CapabilitySummary    string
	Nextcloud            bool   // offer "In Nextcloud ablegen" (connected)
	Deleted              string // project just moved to the trash (?deleted=)
	UndoID               string // its trash id for "Rückgängig"
}

// taskTemplate is a task template (D38). Hardcoded for the demo; the admin-maintained
// template folder of docs/adr/README.md §3.4 comes after the colleagues' feedback (D37).
// Answers go to the chat first; only Tabelle asks for files in results (alice, 2026-10-16).
type taskTemplate struct{ ID, Title, Teaser, Description string }

var taskTemplates = []taskTemplate{
	{"research", "Research with sources", "The helper searches the web, answers in the chat and names the source for every statement.",
		"Research [topic] on the web and give the source with a link for every statement. Summarise the result briefly."},
	{"table", "Analyse a spreadsheet", "Add an Excel or CSV file, ask a question, get the result as a table or chart.",
		"Analyse the spreadsheet in the inbox ([[folders.inbox]]/): [your question, for example visitor numbers by month]. Save the result as a table and as a chart in [[folders.results]]."},
	{"text", "Write a text or document", "Press release, letter or report from notes and documents, first as a draft in the chat.",
		"Write a [press release, letter or report] on [topic]. Use my notes: [notes] and the documents in the inbox ([[folders.inbox]]/), if any. Show me the draft here in the chat first."},
}

// in translates the template's texts.
func (v taskTemplate) in(l string) taskTemplate {
	return taskTemplate{v.ID, tr(l, v.Title), tr(l, v.Teaser), tr(l, v.Description)}
}

func findTemplate(id string) taskTemplate {
	for _, v := range taskTemplates {
		if v.ID == id {
			return v
		}
	}
	return taskTemplate{}
}

func (s *Server) home(w http.ResponseWriter, r *http.Request, id Identity) {
	if s.firstVisit(w, r, id) {
		return
	}
	d := startData{Template: findTemplate(r.URL.Query().Get("template"))}
	d.Description = tr(lang(r), d.Template.Description)
	if n := r.URL.Query().Get("deleted"); n != "" && validName(n) == "" {
		d.Deleted = n
	}
	s.renderStart(w, r, id, http.StatusOK, d, nil)
}

func (s *Server) renderStart(w http.ResponseWriter, r *http.Request, id Identity, status int, d startData, errs []fieldError) {
	ctx := r.Context()
	ps, err := s.be.ListProjects(ctx, id.UID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	trashItems, err := s.be.ListTrash(ctx, id.UID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// The core's ListProjects waits for the environment (EnsureRunning), so the
	// status asked afterwards is what the cards really show.
	if st, err := s.be.EnvStatus(ctx, id.UID); err == nil {
		d.Env = st.State
	} else {
		slog.WarnContext(r.Context(), "portal: env status", "err", err)
	}
	var newest time.Time
	for _, it := range trashItems {
		if d.Deleted != "" && it.Name == d.Deleted && (d.UndoID == "" || it.DeletedAt.After(newest)) {
			d.UndoID, newest = it.ID, it.DeletedAt
		}
	}
	d.Projects, d.Trash, d.MaxUpload = ps, len(trashItems), s.maxUp
	slices.SortStableFunc(d.Projects, func(a, b Project) int { return b.LastActivity.Compare(a.LastActivity) })
	if r.URL.Path == "/projects" {
		s.render(w, r, status, "projects.html", view{User: id, Nav: "projects", D: d})
		return
	}
	if len(d.Projects) > 5 {
		d.Projects = d.Projects[:5]
	}
	d.Assistants = s.assistantCards(r, id, "", true)
	l := lang(r)
	for _, v := range taskTemplates {
		d.Templates = append(d.Templates, v.in(l))
	}
	d.Template = d.Template.in(l)
	if c := s.projectCapabilities(); c != nil {
		choices, err := c.ProjectCapabilities(ctx, id.UID, "")
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		d.Capabilities = localize(lang(r), choices)
		if d.SelectedCapabilities != nil {
			for i := range d.Capabilities {
				d.Capabilities[i].On = slices.Contains(d.SelectedCapabilities, d.Capabilities[i].ID)
			}
		}
	}
	var selected []string
	for _, capability := range d.Capabilities {
		if capability.On {
			selected = append(selected, capability.Title)
		}
	}
	d.CapabilitySummary = strings.Join(selected, ", ")
	if d.CapabilitySummary == "" {
		d.CapabilitySummary = tr(l, "No optional capabilities")
	}
	d.Nextcloud = s.sync != nil && s.sync.Connected(ctx, id.UID)
	s.render(w, r, status, "start.html", view{User: id, Nav: "projects", D: d, Errors: errs})
}

// newForm keeps old links (/projects/new?template=) working: the form is on the start page.
func (s *Server) newForm(w http.ResponseWriter, r *http.Request, id Identity) {
	if assistant := cmp.Or(r.URL.Query().Get("assistant"), r.URL.Query().Get("profile")); assistant != "" {
		http.Redirect(w, r, "/new?assistant="+url.QueryEscape(assistant), http.StatusSeeOther)
		return
	}
	target := "/"
	if v := findTemplate(r.URL.Query().Get("template")); v.ID != "" {
		target = "/?template=" + v.ID
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// cleanDescription strips control characters except text whitespace and checks length.
func cleanDescription(d string) (string, string) {
	d = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, d)
	switch {
	case utf8.RuneCountInString(strings.TrimSpace(d)) < 10:
		return d, "Please describe the task in at least 10 characters."
	case utf8.RuneCountInString(d) > 1000:
		return d, "The description may be at most 1000 characters long."
	}
	return d, ""
}

// placeholderRE finds an unfilled template gap like [Thema]; app.js checks the same.
var placeholderRE = regexp.MustCompile(`\[[^\]\n]*\]`)

const (
	placeholderMsg = "First replace the parts in square brackets, for example [topic], with your details."
	tableMsg       = "For this template the helper needs a spreadsheet. Add the Excel or CSV file with “Add files”."
	incompleteMsg  = "The upload did not arrive completely. Please try again."
)

// checkTask validates the start form before anything is created; it returns
// the cleaned text and, on error, the field and message.
func checkTask(desc, templateID string, files bool) (string, string, string) {
	desc, msg := cleanDescription(desc)
	switch {
	case msg != "":
		return desc, "description", msg
	case placeholderRE.MatchString(desc):
		return desc, "description", placeholderMsg
	case templateID == "table" && !files:
		return desc, "files", tableMsg
	}
	return desc, "", ""
}

// loslegen is the one-click start: validate the text, name and create the
// project, stream the files into inbox/ and open the first chat. The form
// sends "description" and "template" before the file parts, so nothing is
// created for a refused text.
func (s *Server) start(w http.ResponseWriter, r *http.Request, id Identity) {
	ctx := r.Context()
	var d startData
	var name string // set once the project exists
	var files []string
	var linkNC bool // "In Nextcloud ablegen" ticked; linked once the project exists
	tooBig := trf(lang(r), "The files are too large. At most %s per upload.", num(lang(r), gb(s.maxUp)))
	fail := func(status int, field, msg string) {
		s.renderStart(w, r, id, status, d, []fieldError{{field, msg}})
	}
	// partErr answers a broken or oversized request body.
	partErr := func(err error) {
		status, msg := http.StatusBadRequest, incompleteMsg
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			status, msg = http.StatusRequestEntityTooLarge, tooBig
		}
		if name != "" {
			s.uploadError(w, r, id, name, status, msg)
			return
		}
		fail(status, "files", msg)
	}
	create := func(withFiles bool) bool {
		desc, field, msg := checkTask(d.Description, d.Template.ID, withFiles)
		d.Description = desc
		if msg != "" {
			d.FilesLost = withFiles
			fail(http.StatusUnprocessableEntity, field, msg)
			return false
		}
		selected := d.SelectedCapabilities
		if c := s.projectCapabilities(); c != nil {
			var err error
			selected, err = c.SelectCapabilities(ctx, id.UID, selected)
			if err != nil {
				fail(http.StatusUnprocessableEntity, "capability", "Choose only capabilities available to you.")
				return false
			}
		}
		n, _, err := s.proposeName(ctx, id.UID, desc)
		if err != nil {
			s.serverError(w, r, err)
			return false
		}
		err = s.be.CreateProject(ctx, id.UID, NewProject{Name: n, Description: desc, Capabilities: selected})
		if errors.Is(err, ErrExists) || errors.Is(err, store.ErrExists) {
			n, _, err = s.uniqueName(ctx, id.UID, n, false)
			if err == nil {
				err = s.be.CreateProject(ctx, id.UID, NewProject{Name: n, Description: desc, Capabilities: selected})
			}
		}
		if errors.Is(err, ErrQuota) {
			d.FilesLost = withFiles
			fail(http.StatusInsufficientStorage, "description", "The project could not be created: storage is full. Empty the Trash or delete old projects.")
			return false
		}
		if err != nil {
			s.serverError(w, r, err)
			return false
		}
		name = n
		return true
	}

	r.Body = http.MaxBytesReader(w, r.Body, s.maxUp)
	mr, err := r.MultipartReader()
	if err != nil {
		fail(http.StatusBadRequest, "description", incompleteMsg)
		return
	}
	for parts := 0; ; parts++ {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			partErr(err)
			return
		}
		if parts >= 64 {
			partErr(errors.New("too many multipart parts"))
			return
		}
		switch part.FormName() {
		case "nextcloud":
			linkNC = true
			part.Close()
			continue
		case "capabilities_present", "capability":
			if name != "" {
				part.Close()
				partErr(errors.New("capabilities must precede files"))
				return
			}
			b, err := io.ReadAll(io.LimitReader(part, 16<<10))
			part.Close()
			if err != nil {
				partErr(err)
				return
			}
			if d.SelectedCapabilities == nil {
				d.SelectedCapabilities = []string{}
			}
			if part.FormName() == "capability" {
				d.SelectedCapabilities = append(d.SelectedCapabilities, string(b))
			}
			continue
		case "description", "template":
			b, err := io.ReadAll(io.LimitReader(part, 16<<10))
			part.Close()
			if err != nil {
				partErr(err)
				return
			}
			if part.FormName() == "template" {
				d.Template = findTemplate(string(b))
			} else {
				d.Description = string(b)
			}
			continue
		case "files":
		default:
			part.Close()
			continue
		}
		fn := cleanFilename(part.FileName())
		if fn == "" { // an empty file input still sends a part, without a name
			part.Close()
			continue
		}
		if name == "" && !create(true) {
			part.Close()
			return
		}
		used, err := s.be.Upload(ctx, id.UID, name, fn, part)
		part.Close()
		var mbe *http.MaxBytesError
		switch {
		case err == nil:
			files = append(files, used)
		case errors.As(err, &mbe):
			partErr(err)
			return
		case errors.Is(err, ErrQuota):
			s.uploadError(w, r, id, name, http.StatusRequestEntityTooLarge, "Storage full. Empty the Trash or delete old projects.")
			return
		default:
			s.serverError(w, r, err)
			return
		}
	}
	if name == "" && !create(false) {
		return
	}
	if linkNC && s.sync != nil {
		// the project exists either way; a failed link is shown on its page later, not here
		if err := s.sync.Link(ctx, id.UID, name, false); err != nil {
			slog.WarnContext(r.Context(), "portal: nextcloud link at start", "err", err)
		}
	}
	prompt := d.Description
	if len(files) > 0 {
		prompt += "\n\n" + trf(lang(r), "Files in the inbox ([[folders.inbox]]/): %s", strings.Join(files, ", "))
	}
	s.openChat(w, r, id, name, prompt, "")
}

// openChat makes sure the environment runs, creates a chat and sends the
// browser to its deep link. If the environment is not ready it parks the chat
// under a one-time token and shows the "Umgebung startet" page, which posts the
// token back here once it runs (pending.go).
func (s *Server) openChat(w http.ResponseWriter, r *http.Request, id Identity, project, prompt, token string) {
	ctx := r.Context()
	if s.taskAssistants != nil {
		p, err := s.be.Project(ctx, id.UID, project)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if p.Assistant.ID != "" && !p.Isolated {
			if _, err := s.taskAssistants.Authorize(ctx, id.UID, id.Groups, p.Assistant.ID, p.Assistant.Hash); err != nil {
				s.assistantError(w, r, id, err)
				return
			}
		}
		if p.Isolated {
			session, err := s.taskAssistants.Open(ctx, id.UID, id.Groups, project)
			if err != nil {
				s.assistantError(w, r, id, err)
				return
			}
			navigate(w, r, "/support/"+session)
			return
		}
	}
	var pc *pendingChat
	if token != "" {
		if pc = s.pending.get(token, id.UID, project, s.now()); pc == nil {
			// Expired or foreign token: never start a run; show the project instead.
			navigate(w, r, "/projects/"+project)
			return
		}
		pc.mu.Lock()
		defer pc.mu.Unlock()
		if pc.link != "" {
			navigate(w, r, pc.link)
			return
		}
		prompt = pc.prompt
	}
	wait := func() {
		if pc == nil {
			token = s.pending.add(id.UID, project, prompt, s.now())
		}
		navigate(w, r, startURL(project, token))
	}
	st, err := s.be.EnvStatus(ctx, id.UID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if st.State != EnvRunning {
		if st.State != EnvStarting {
			if err := s.be.EnvStart(ctx, id.UID); err != nil {
				s.serverError(w, r, err)
				return
			}
		}
		wait()
		return
	}
	link, err := s.be.CreateSession(ctx, id.UID, project, prompt)
	if errors.Is(err, ErrNotRunning) {
		wait()
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "portal: create session", "project", project, "err", err)
		s.render(w, r, http.StatusBadGateway, "error.html", view{User: id, D: errorData{
			Title: "The chat could not be started",
			Text:  "The project is created, but the chat could not be opened. Open the project and try again with “New task”.",
		}})
		return
	}
	if pc != nil {
		pc.link = link
	}
	navigate(w, r, link)
}

// --- Project-Detail ---

type detailData struct {
	FixedName    bool
	Capabilities []Tool
	ProjectDetail
	Results   []*treeNode // regular files below results/, Name = path below results/
	Inbox     []*treeNode // the same for inbox/
	Tree      []*treeNode // everything else (Zwischenstände)
	Truncated bool
	HasFiles  bool
	Uploaded  string // message after an upload, translated
	NoneSent  bool   // the upload carried no file
	MaxUpload int64
	Env       EnvState    // "" if unknown; chats are only current while it runs
	Latest    *Chat       // newest chat with a deep link, nil without
	Last      time.Time   // last activity, creation time if none yet
	NewName   string      // rename field after an error
	Sync      *SyncStatus // nil without Nextcloud or when it could not be asked
}

type treeNode struct {
	File
	Href     string // download URL, files only
	PDFHref  string // on-the-fly PDF of a .md file; "" without OC_PDF_URL
	Children []*treeNode
}

func fileNode(project string, f File, pdf bool) *treeNode {
	n := &treeNode{File: f}
	if f.Dir {
		return n
	}
	segs := strings.Split(f.Path, "/")
	for i := range segs {
		segs[i] = url.PathEscape(segs[i])
	}
	n.Href = "/projects/" + project + "/files/" + strings.Join(segs, "/")
	if pdf && strings.EqualFold(path.Ext(f.Path), ".md") {
		n.PDFHref = "/projects/" + project + "/pdf?" + url.Values{"file": {f.Path}}.Encode()
	}
	return n
}

func sortNodes(ns []*treeNode) {
	slices.SortFunc(ns, func(a, b *treeNode) int {
		if a.Dir != b.Dir {
			if a.Dir {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
}

// buildTree nests the flat backend list (parents first) and sorts every
// level folders first, then by lowercase name.
func buildTree(project string, files []File, pdf bool) (nodes []*treeNode, hasFiles bool) {
	root := &treeNode{}
	dirs := map[string]*treeNode{".": root}
	for _, f := range files {
		parent := dirs[path.Dir(f.Path)]
		if parent == nil { // parent cut off by the backend cap
			continue
		}
		n := fileNode(project, f, pdf)
		if f.Dir {
			dirs[f.Path] = n
		} else {
			hasFiles = true
		}
		parent.Children = append(parent.Children, n)
	}
	for _, d := range dirs {
		sortNodes(d.Children)
	}
	return root.Children, hasFiles
}

// splitFiles puts the regular files below results/ and inbox/ into flat
// lists (path below the folder as name) and leaves the rest for buildTree.
func splitFiles(project string, files []File, pdf bool) (results, inbox []*treeNode, rest []File) {
	for _, f := range files {
		top, sub, nested := strings.Cut(f.Path, "/")
		if top != instance.Current().Folders.Results && top != instance.Current().Folders.Inbox {
			rest = append(rest, f)
			continue
		}
		if f.Dir || !nested {
			continue
		}
		n := fileNode(project, f, pdf)
		n.Name = sub
		if top == instance.Current().Folders.Results {
			results = append(results, n)
		} else {
			inbox = append(inbox, n)
		}
	}
	sortNodes(results)
	sortNodes(inbox)
	return results, inbox, rest
}

// latestChat is the most recently updated chat with a deep link.
func latestChat(cs []Chat) *Chat {
	var last *Chat
	for i, c := range cs {
		if c.Link != "" && (last == nil || c.Updated.After(last.Updated)) {
			last = &cs[i]
		}
	}
	return last
}

// projectName reads and validates {name}; on failure it answers 404.
func (s *Server) projectName(w http.ResponseWriter, r *http.Request) (string, bool) {
	n := r.PathValue("name")
	if validName(n) != "" {
		s.errorPage(w, r, http.StatusNotFound, "Project not found", "This project does not exist.")
		return "", false
	}
	return n, true
}

func (s *Server) loadDetail(r *http.Request, uid, name string) (detailData, error) {
	ctx := r.Context()
	p, err := s.be.Project(ctx, uid, name)
	if err != nil {
		return detailData{}, err
	}
	slices.SortStableFunc(p.ChatList, func(a, b Chat) int { return b.Updated.Compare(a.Updated) }) // newest first
	d := detailData{ProjectDetail: p, MaxUpload: s.maxUp, Latest: latestChat(p.ChatList), Last: p.LastActivity}
	if def, ok := instance.Current().Assistant(p.Assistant.ID); ok {
		d.FixedName = !p.Isolated && def.ProjectMode == "shared"
	}
	if d.Last.IsZero() {
		d.Last = p.Created
	}
	if p.Assistant.ID == "" {
		if c := s.projectCapabilities(); c != nil {
			choices, err := c.ProjectCapabilities(r.Context(), uid, name)
			if err != nil {
				return d, err
			}
			d.Capabilities = localize(lang(r), choices)
		}
	}
	if p.Isolated {
		d.Env = EnvRunning
		return d, nil
	}
	files, truncated, err := s.be.Tree(ctx, uid, name)
	var rest []File
	d.Results, d.Inbox, rest = splitFiles(name, files, s.pdf != nil)
	d.Tree, d.HasFiles = buildTree(name, rest, s.pdf != nil)
	d.HasFiles = d.HasFiles || len(d.Results) > 0 || len(d.Inbox) > 0
	d.Truncated = truncated
	if err != nil {
		return d, err
	}
	// Size of the person's files only: du in the core also counts .git and the folder skeleton.
	d.SizeBytes = 0
	for _, f := range files {
		if !f.Dir {
			d.SizeBytes += f.Size
		}
	}
	if st, err := s.be.EnvStatus(ctx, uid); err == nil {
		d.Env = st.State
	} else {
		slog.WarnContext(r.Context(), "portal: env status", "err", err)
	}
	if s.sync != nil {
		if st, err := s.sync.Status(ctx, uid, name); err == nil {
			d.Sync = &st
		} else {
			slog.WarnContext(r.Context(), "portal: nextcloud status", "err", err)
		}
	}
	return d, nil
}

func (s *Server) detail(w http.ResponseWriter, r *http.Request, id Identity) {
	name, ok := s.projectName(w, r)
	if !ok {
		return
	}
	d, err := s.loadDetail(r, id.UID, name)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	switch n := r.URL.Query().Get("uploaded"); n {
	case "":
	case "0":
		d.Uploaded, d.NoneSent = tr(lang(r), "No file selected."), true
	case "1":
		d.Uploaded = tr(lang(r), "1 file uploaded.")
	default:
		if strings.Trim(n, "0123456789") == "" {
			d.Uploaded = trf(lang(r), "%s files uploaded.", n)
		}
	}
	v := view{User: id, Nav: "projects", D: d}
	switch {
	case r.URL.Query().Has("effect"):
		v.Notice = effectText(r.URL.Query().Get("effect"))
		if r.URL.Query().Get("effect") == "now" {
			v.Notice = "This applies to new and running chats in this project."
		}
	case r.URL.Query().Get("renamed") == "1":
		v.Notice = trf(lang(r), "The name is now “%s”.", name)
	case r.URL.Query().Get("restored") == "1":
		v.Notice = trf(lang(r), "The project “%s” is restored.", name)
	case r.URL.Query().Has("feedback"):
		v.Notice = feedbackNotice(r.URL.Query().Get("feedback"))
	default:
		v.Notice = syncText(lang(r), r.URL.Query())
	}
	s.render(w, r, http.StatusOK, "project.html", v)
}

// rename changes the project name (folder); the chats move along in the core.
func (s *Server) rename(w http.ResponseWriter, r *http.Request, id Identity) {
	name, ok := s.projectName(w, r)
	if !ok {
		return
	}
	p, err := s.be.Project(r.Context(), id.UID, name)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if def, ok := instance.Current().Assistant(p.Assistant.ID); ok && !p.Isolated && def.ProjectMode == "shared" {
		s.errorPage(w, r, http.StatusConflict, "Change name", "Shared assistant project names are fixed.")
		return
	}
	newName := strings.TrimSpace(r.FormValue("new_name"))
	if newName == name {
		http.Redirect(w, r, "/projects/"+name, http.StatusSeeOther)
		return
	}
	status, msg := http.StatusUnprocessableEntity, validName(newName)
	if msg == "" {
		taken, err := s.be.ProjectExists(r.Context(), id.UID, newName)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if taken {
			status, msg = http.StatusConflict, takenMsg
		}
	}
	if msg == "" {
		err := s.be.RenameProject(r.Context(), id.UID, name, newName)
		switch {
		case err == nil:
			http.Redirect(w, r, "/projects/"+newName+"?renamed=1", http.StatusSeeOther)
			return
		case errors.Is(err, ErrExists):
			status, msg = http.StatusConflict, takenMsg
		case errors.Is(err, ErrBusy):
			status, msg = http.StatusConflict, "The helper is working in this project right now. You can change the name once it has finished."
		default:
			s.serverError(w, r, err)
			return
		}
	}
	d, err := s.loadDetail(r, id.UID, name)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d.NewName = newName
	s.render(w, r, status, "project.html", view{User: id, Nav: "projects", D: d, Errors: []fieldError{{"new_name", msg}}})
}

func (s *Server) open(w http.ResponseWriter, r *http.Request, id Identity) {
	name, ok := s.projectName(w, r)
	if !ok {
		return
	}
	p, err := s.be.Project(r.Context(), id.UID, name)
	if err != nil {
		s.serverError(w, r, err) // never start a chat (or the environment) for a missing project
		return
	}
	tok := r.FormValue("t")
	if tok != "" && !tokenRE.MatchString(tok) {
		tok = "0" // never matches: treated like an expired token
	}
	prompt := ""
	// "Loslegen" after "Anlegen und Dateien hochladen": the description becomes the
	// first task, taken from the store (never from the form) and only for a project
	// without chats, so a double click cannot start a second paid run.
	if tok == "" && r.FormValue("brief") == "1" && p.Chats == 0 {
		prompt = p.Description
	}
	s.openChat(w, r, id, name, prompt, tok)
}

// resume continues the most recently updated chat of a project instead of
// starting a new one; without a chat it shows the project page.
func (s *Server) resume(w http.ResponseWriter, r *http.Request, id Identity) {
	name, ok := s.projectName(w, r)
	if !ok {
		return
	}
	p, err := s.be.Project(r.Context(), id.UID, name)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	last := latestChat(p.ChatList)
	if last == nil {
		http.Redirect(w, r, "/projects/"+name, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, last.Link, http.StatusSeeOther)
}

// --- Files ---

func attachment(w http.ResponseWriter, filename, ctype string) {
	h := w.Header()
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	h.Set("Content-Type", ctype)
	// Agent-written HTML/SVG must never render on apps.example.org: always attachment.
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
}

func (s *Server) download(w http.ResponseWriter, r *http.Request, id Identity) {
	name, ok := s.projectName(w, r)
	if !ok {
		return
	}
	p, ok := cleanRelPath(r.PathValue("path"))
	if !ok {
		s.errorPage(w, r, http.StatusNotFound, "File not found", "This file does not exist.")
		return
	}
	rc, f, err := s.be.OpenFile(r.Context(), id.UID, name, p)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	defer rc.Close()
	attachment(w, path.Base(p), mime.TypeByExtension(path.Ext(p)))
	w.Header().Set("Content-Length", fmt.Sprint(f.Size))
	if _, err := io.Copy(w, rc); err != nil {
		slog.WarnContext(r.Context(), "portal: download aborted", "project", name, "err", err)
	}
}

// pdfDownload converts a .md file of the project on the fly; same path rules as download.
func (s *Server) pdfDownload(w http.ResponseWriter, r *http.Request, id Identity) {
	name, ok := s.projectName(w, r)
	if !ok {
		return
	}
	p, ok := cleanRelPath(r.URL.Query().Get("file"))
	if !ok || s.pdf == nil || !strings.EqualFold(path.Ext(p), ".md") {
		s.errorPage(w, r, http.StatusNotFound, "File not found", "This file does not exist.")
		return
	}
	rc, _, err := s.be.OpenFile(r.Context(), id.UID, name, p)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	src, err := io.ReadAll(io.LimitReader(rc, maxPDFSource+1))
	rc.Close()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if len(src) > maxPDFSource {
		s.errorPage(w, r, http.StatusRequestEntityTooLarge, "File too large", "This file is too large to convert to PDF (at most 4 MiB).")
		return
	}
	pdf, err := s.pdf(convert.WithLang(r.Context(), lang(r)), src, path.Base(p))
	if err != nil {
		slog.WarnContext(r.Context(), "portal: pdf", "project", name, "err", err)
		s.errorPage(w, r, http.StatusBadGateway, "PDF not possible", "Converting to PDF is not possible right now. You can still download the file itself.")
		return
	}
	attachment(w, strings.TrimSuffix(path.Base(p), path.Ext(p))+".pdf", "application/pdf")
	w.Header().Set("Content-Length", fmt.Sprint(len(pdf)))
	w.Write(pdf)
}

// maxPDFSource matches convert.MaxMarkdown.
const maxPDFSource = 4 << 20

func (s *Server) zip(w http.ResponseWriter, r *http.Request, id Identity) {
	name, ok := s.projectName(w, r)
	if !ok {
		return
	}
	part, label := "results", "results"
	if r.URL.Query().Get("part") == "all" {
		part, label = "all", "alles"
	}
	if _, err := s.be.Project(r.Context(), id.UID, name); err != nil {
		s.serverError(w, r, err)
		return
	}
	attachment(w, fmt.Sprintf("%s-%s-%s.zip", name, label, s.now().Format("2006-01-02")), "application/zip")
	// Headers are out once the stream starts; a late error only truncates the ZIP.
	if err := s.be.WriteZip(r.Context(), id.UID, name, part, w); err != nil {
		slog.ErrorContext(r.Context(), "portal: zip", "project", name, "err", err)
	}
}

// lazyAttachment sets the download headers with the first archive bytes, so
// an error before that (start failed, unknown project) still gets a page.
type lazyAttachment struct {
	w        http.ResponseWriter
	filename string
	started  bool
}

func (l *lazyAttachment) Write(p []byte) (int, error) {
	if !l.started {
		l.started = true
		attachment(l.w, l.filename, "application/zip")
	}
	return l.w.Write(p)
}

// export serves "Meine Daten exportieren" (/environment/export) and "Projekt
// exportieren" (/projects/{name}/export).
func (s *Server) export(w http.ResponseWriter, r *http.Request, id Identity) {
	name, filename := "", instance.Value("export_prefix")+s.now().Format("2006-01-02")+".zip"
	if r.PathValue("name") != "" {
		var ok bool
		if name, ok = s.projectName(w, r); !ok {
			return
		}
		filename = fmt.Sprintf("%s-export-%s.zip", name, s.now().Format("2006-01-02"))
	}
	lw := &lazyAttachment{w: w, filename: filename}
	err := s.be.Export(r.Context(), id.UID, name, lw)
	switch {
	case err != nil && !lw.started:
		s.serverError(w, r, err)
	case err != nil: // headers are out: the ZIP is cut short
		slog.ErrorContext(r.Context(), "portal: export", "err", err)
	}
}

// deleteWord is what the person types to confirm the account deletion, in
// the page language; the German word counts in every language.
const deleteWord = "DELETE"

func (s *Server) deleteConfirm(w http.ResponseWriter, r *http.Request, id Identity) {
	s.render(w, r, http.StatusOK, "environment-delete.html", view{User: id, Nav: "environment"})
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request, id Identity) {
	if got := strings.ToUpper(strings.TrimSpace(r.FormValue("confirmation"))); got != tr(lang(r), deleteWord) && got != tr(langDE, deleteWord) {
		s.render(w, r, http.StatusUnprocessableEntity, "environment-delete.html", view{User: id, Nav: "environment",
			Errors: []fieldError{{"confirmation", "Please type the word DELETE to confirm."}}})
		return
	}
	slug, err := s.be.DeleteAccount(r.Context(), id.UID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.pending.forget(id.UID)
	// The OpenCode web UI keeps tabs and recent chats in the browser storage of
	// <slug>.<domain>; /__workspace/clear there sends Clear-Site-Data and comes back to /deleted.
	if host, ok := strings.CutPrefix(s.origin, "https://"); ok && slug != "" {
		http.Redirect(w, r, "https://"+slug+"."+host+instance.BrandPrefix()+"clear", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/deleted", http.StatusSeeOther)
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request, id Identity) {
	name, ok := s.projectName(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.maxUp)
	mr, err := r.MultipartReader()
	if err != nil {
		s.uploadError(w, r, id, name, http.StatusBadRequest, "The upload did not arrive completely. Please try again.")
		return
	}
	n := 0
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			s.uploadError(w, r, id, name, http.StatusRequestEntityTooLarge, trf(lang(r), "The files are too large. At most %s per upload.", num(lang(r), gb(s.maxUp))))
			return
		}
		if err != nil {
			s.uploadError(w, r, id, name, http.StatusBadRequest, "The upload did not arrive completely. Please try again.")
			return
		}
		fn := cleanFilename(part.FileName())
		if part.FormName() != "files" || fn == "" {
			part.Close()
			continue
		}
		_, err = s.be.Upload(r.Context(), id.UID, name, fn, part)
		part.Close()
		switch {
		case err == nil:
			n++
		case errors.As(err, &mbe):
			s.uploadError(w, r, id, name, http.StatusRequestEntityTooLarge, trf(lang(r), "The files are too large. At most %s per upload.", num(lang(r), gb(s.maxUp))))
			return
		case errors.Is(err, ErrQuota):
			s.uploadError(w, r, id, name, http.StatusRequestEntityTooLarge, "Storage full. Empty the Trash or delete old projects.")
			return
		case errors.Is(err, ErrNotFound):
			s.serverError(w, r, err)
			return
		default:
			s.serverError(w, r, err)
			return
		}
	}
	http.Redirect(w, r, fmt.Sprintf("/projects/%s?uploaded=%d#inbox", name, n), http.StatusSeeOther)
}

// uploadError re-renders the detail page with the message at the upload field,
// so the htmx upload (hx-select="#inbox") and the plain form both show it.
func (s *Server) uploadError(w http.ResponseWriter, r *http.Request, id Identity, name string, status int, msg string) {
	d, err := s.loadDetail(r, id.UID, name)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, status, "project.html", view{User: id, Nav: "projects", D: d, Errors: []fieldError{{"files", msg}}})
}

// --- Deletion and trash ---

func (s *Server) trashConfirm(w http.ResponseWriter, r *http.Request, id Identity) {
	name, ok := s.projectName(w, r)
	if !ok {
		return
	}
	p, err := s.be.Project(r.Context(), id.UID, name)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "project-delete.html", view{User: id, Nav: "projects", D: p})
}

func (s *Server) trash(w http.ResponseWriter, r *http.Request, id Identity) {
	name, ok := s.projectName(w, r)
	if !ok {
		return
	}
	err := s.be.MoveToTrash(r.Context(), id.UID, name)
	if errors.Is(err, ErrBusy) {
		p, perr := s.be.Project(r.Context(), id.UID, name)
		if perr != nil {
			s.serverError(w, r, perr)
			return
		}
		s.render(w, r, http.StatusConflict, "project-delete.html", view{User: id, Nav: "projects", D: p,
			Errors: []fieldError{{"delete", "The helper is working in this project right now. Wait until it has finished, or stop the environment."}}})
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/?deleted="+name, http.StatusSeeOther)
}

var trashIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,120}$`)

type trashData struct {
	Items    []TrashItem
	Conflict string // id whose restore needs a new name
	NewName  string
}

func (s *Server) trashItem(w http.ResponseWriter, r *http.Request, uid string) (TrashItem, []TrashItem, bool) {
	tid := r.PathValue("id")
	items, err := s.be.ListTrash(r.Context(), uid)
	if err != nil {
		s.serverError(w, r, err)
		return TrashItem{}, nil, false
	}
	if trashIDRE.MatchString(tid) {
		for _, it := range items {
			if it.ID == tid {
				return it, items, true
			}
		}
	}
	s.errorPage(w, r, http.StatusNotFound, "Not in the Trash", "This item is not (or no longer) in the Trash.")
	return TrashItem{}, nil, false
}

func (s *Server) trashList(w http.ResponseWriter, r *http.Request, id Identity) {
	items, err := s.be.ListTrash(r.Context(), id.UID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "trash.html", view{User: id, Nav: "projects", D: trashData{Items: items}})
}

func (s *Server) restore(w http.ResponseWriter, r *http.Request, id Identity) {
	it, items, ok := s.trashItem(w, r, id.UID)
	if !ok {
		return
	}
	newName := strings.TrimSpace(r.FormValue("new_name"))
	target := it.Name
	var msg string
	if newName != "" {
		target = newName
		msg = validName(newName)
	}
	if msg == "" {
		err := s.be.Restore(r.Context(), id.UID, it.ID, newName)
		switch {
		case errors.Is(err, ErrExists):
			msg = trf(lang(r), "A project “%s” already exists. Please choose a new name.", target)
		case err != nil:
			s.serverError(w, r, err)
			return
		default:
			http.Redirect(w, r, "/projects/"+target+"?restored=1", http.StatusSeeOther)
			return
		}
	}
	s.render(w, r, http.StatusConflict, "trash.html", view{User: id, Nav: "projects",
		D: trashData{Items: items, Conflict: it.ID, NewName: newName}, Errors: []fieldError{{"new_name", msg}}})
}

func (s *Server) purgeConfirm(w http.ResponseWriter, r *http.Request, id Identity) {
	it, _, ok := s.trashItem(w, r, id.UID)
	if !ok {
		return
	}
	s.render(w, r, http.StatusOK, "trash-delete.html", view{User: id, Nav: "projects", D: it})
}

func (s *Server) purge(w http.ResponseWriter, r *http.Request, id Identity) {
	it, _, ok := s.trashItem(w, r, id.UID)
	if !ok {
		return
	}
	if r.FormValue("confirmed") != "yes" {
		s.render(w, r, http.StatusUnprocessableEntity, "trash-delete.html", view{User: id, Nav: "projects", D: it,
			Errors: []fieldError{{"confirmed", "Please confirm that the project should be deleted permanently."}}})
		return
	}
	if err := s.be.Purge(r.Context(), id.UID, it.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/trash", http.StatusSeeOther)
}

// --- Umgebung ---

type envData struct {
	EnvStatus
	Project string // waiting page: project to open once running
	Token   string // one-time token of the pending chat
}

func (s *Server) env(w http.ResponseWriter, r *http.Request, id Identity) {
	st, err := s.be.EnvStatus(r.Context(), id.UID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	v := view{User: id, Nav: "environment", D: envData{EnvStatus: st}}
	if r.URL.Query().Get("ok") == "stopped" {
		v.Notice = "The environment is stopped. It starts automatically the next time you open it."
		if r.URL.Query().Get("aborted") == "1" {
			v.Notice = "The environment is stopped. Running tasks were cancelled."
		}
	}
	s.render(w, r, http.StatusOK, "environment.html", v)
}

// envStopConfirm asks before a stop aborts running tasks; the POST without
// confirmed=yes shows the same page while tasks run.
func (s *Server) envStopConfirm(w http.ResponseWriter, r *http.Request, id Identity) {
	st, err := s.be.EnvStatus(r.Context(), id.UID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "environment-stop.html", view{User: id, Nav: "environment", D: envData{EnvStatus: st}})
}

func (s *Server) envStarting(w http.ResponseWriter, r *http.Request, id Identity) {
	d := envData{Project: r.URL.Query().Get("project"), Token: r.URL.Query().Get("t")}
	if validName(d.Project) != "" || !tokenRE.MatchString(d.Token) {
		d.Project, d.Token = "", ""
	}
	st, err := s.be.EnvStatus(r.Context(), id.UID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d.EnvStatus = st
	status := http.StatusOK
	if st.State == EnvError || st.State == EnvFull {
		if r.Header.Get("HX-Request") == "true" {
			// The poller only swaps #startbox; reload to show the full error page.
			w.Header().Set("HX-Refresh", "true")
			w.WriteHeader(http.StatusOK)
			return
		}
		status = http.StatusServiceUnavailable
	}
	s.render(w, r, status, "environment-starting.html", view{User: id, Nav: "environment", D: d})
}

func (s *Server) envResetConfirm(w http.ResponseWriter, r *http.Request, id Identity) {
	st, err := s.be.EnvStatus(r.Context(), id.UID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "environment-reset.html", view{User: id, Nav: "environment", D: envData{EnvStatus: st}})
}

func (s *Server) envAction(w http.ResponseWriter, r *http.Request, id Identity) {
	ctx := r.Context()
	p, tok := r.FormValue("project"), r.FormValue("t")
	if validName(p) != "" || !tokenRE.MatchString(tok) {
		p, tok = "", ""
	}
	wait := startURL(p, tok)
	var err error
	switch r.PathValue("action") {
	case "start":
		err = s.be.EnvStart(ctx, id.UID)
	case "stop":
		st, err := s.be.EnvStatus(ctx, id.UID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if len(st.ActiveRuns) > 0 && r.FormValue("confirmed") != "yes" {
			s.render(w, r, http.StatusUnprocessableEntity, "environment-stop.html", view{User: id, Nav: "environment", D: envData{EnvStatus: st}})
			return
		}
		if err := s.be.EnvStop(ctx, id.UID); err != nil {
			s.serverError(w, r, err)
			return
		}
		wait = "/environment?ok=stopped"
		if len(st.ActiveRuns) > 0 {
			wait += "&aborted=1"
		}
	case "restart":
		err = s.be.EnvRestart(ctx, id.UID)
	case "reset":
		if r.FormValue("understood") != "yes" {
			st, err := s.be.EnvStatus(ctx, id.UID)
			if err != nil {
				s.serverError(w, r, err)
				return
			}
			s.render(w, r, http.StatusUnprocessableEntity, "environment-reset.html", view{User: id, Nav: "environment", D: envData{EnvStatus: st},
				Errors: []fieldError{{"understood", "Please tick the box to confirm that you understand what a reset loses."}}})
			return
		}
		err = s.be.EnvReset(ctx, id.UID)
	default:
		s.errorPage(w, r, http.StatusNotFound, "Page not found", "This action does not exist.")
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, wait, http.StatusSeeOther)
}

// --- Usage ---

type usageData struct {
	Rows      []UsageRow
	Total     UsageRow
	Since     time.Time
	NoProject bool    // a row "Ohne Projekt" exists and needs its explanation
	Cap       float64 // daily budget, 0 = none
	Today     float64 // spend since the start of the budget day, as the budget gate counts it
	Before    float64 // part of Today spent by a deleted account of the same uid
}

func (s *Server) usage(w http.ResponseWriter, r *http.Request, id Identity) {
	since := s.now().AddDate(0, 0, -30)
	rows, err := s.be.Usage(r.Context(), id.UID, since)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := usageData{Rows: rows, Since: since}
	for _, u := range rows {
		d.Total.CostEUR += u.CostEUR
		d.Total.CO2MinG += u.CO2MinG
		d.Total.CO2MaxG += u.CO2MaxG
		d.Total.Chats += u.Chats
		d.NoProject = d.NoProject || u.Project == ""
	}
	if s.dayCap > 0 && s.dayFrom != nil {
		today, err := s.be.Usage(r.Context(), id.UID, s.dayFrom(s.now()))
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		d.Cap = s.dayCap
		for _, u := range today {
			d.Today += u.CostEUR
		}
		if s.spent != nil {
			// the gate's own sum: includes what a deleted account of this uid spent today
			all, err := s.spent(r.Context(), id.UID, s.dayFrom(s.now()))
			if err != nil {
				s.serverError(w, r, err)
				return
			}
			if all-d.Today >= 0.005 {
				d.Before = all - d.Today
			}
			d.Today = all
		}
	}
	s.render(w, r, http.StatusOK, "usage.html", view{User: id, Nav: "usage", D: d})
}

type methodData struct {
	Version     string
	PUE, GridDE float64
	CarGPerKm   float64
	Models      []co2.Model
}

func (s *Server) usageMethod(w http.ResponseWriter, r *http.Request, id Identity) {
	d := methodData{Version: co2.Version, PUE: instance.Current().FootprintAssumptions.PUE, GridDE: instance.Current().FootprintAssumptions.GridGPerKWh, CarGPerKm: carGPerKm}
	for _, m := range co2.Models() {
		d.Models = append(d.Models, m)
	}
	slices.SortFunc(d.Models, func(a, b co2.Model) int { return strings.Compare(a.Alias, b.Alias) })
	s.render(w, r, http.StatusOK, "usage-method.html", view{User: id, Nav: "usage", D: d})
}
