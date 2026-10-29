// SPDX-License-Identifier: MIT

package lifecycle

import (
	"archive/zip"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// exportPage is the number of messages per request: one page is decoded at
// a time, so memory stays bounded however long a chat is.
const exportPage = 20

const maxPages = 500

const exportReadme = `[[name]] - export of your data
Created on %s.

Layout
  <project>/files/     all files of the project ([[folders.inbox]], [[folders.results]], [[folders.work]], ...)
  <project>/chats/     every chat as readable text: <date>-<title>.md
%s%s
The chats are Markdown files and can be read with any text editor.
They contain your messages and the helper's answers. Tool calls of
the helper (read web page, search, run command, write file) appear
as one line, sources and links found as a list below the answer.
The helper's reasoning and subtasks it gave to further helpers
are not included; a subtask appears as one line in the chat.

Not included are hidden files and folders (name starts with a dot,
for example .git with the history of the file versions), links
(symlinks), settings and credentials.
`

// Export streams a ZIP with one project (project != "") or with every
// project and trash item: files without dot paths and symlinks (like
// WriteZip "alles"), each chat as Markdown, and README.txt. It starts the
// environment if needed, like the other file operations.
func (s *Service) Export(ctx context.Context, uid, project string, w io.Writer) error {
	if project != "" && !ValidProject(project) {
		return ErrInvalid
	}
	ps, err := s.st.Projects(ctx, uid)
	if err != nil {
		return err
	}
	if project != "" {
		ps = slices.DeleteFunc(ps, func(p store.Project) bool { return p.Folder != project })
		if len(ps) == 0 {
			return ErrNotFound
		}
	}
	needRuntime := slices.ContainsFunc(ps, func(p store.Project) bool { return !p.Isolated })
	if project == "" {
		ts, err := s.st.Trash(ctx, uid)
		if err != nil {
			return err
		}
		needRuntime = needRuntime || slices.ContainsFunc(ts, func(t store.TrashItem) bool { return !t.Project.Isolated })
	}
	var c *ocapi.Client
	if needRuntime {
		if err := s.ready(ctx, uid); err != nil {
			return err
		}
		s.finishPending(ctx, uid)
		var ok bool
		c, ok = s.Client(uid)
		if !ok {
			return ErrNotRunning
		}
	}
	lang, _ := s.st.Lang(ctx, uid)
	en := lang == "en"
	zw := zip.NewWriter(w)
	trashNote, settingsNote, personalSettings := "", "", ""
	if project == "" {
		trashNote = tr(en, "  trash/<project>-<id>/        projects in the trash, same layout\n")
		if s.Settings != nil {
			if personalSettings, err = s.Settings(ctx, uid); err != nil {
				return err
			}
			settingsNote = tr(en, "  profile.md           About me & how I work\n")
		}
	}
	if err := zipText(zw, "README.txt", tr(en, exportReadme, s.now().Format(dateLayout(en)), trashNote, settingsNote), s.now()); err != nil {
		return err
	}
	if personalSettings != "" {
		if err := zipText(zw, "profile.md", personalSettings, s.now()); err != nil {
			return err
		}
	}
	for _, p := range ps {
		if p.Isolated {
			if err := s.exportAssistantChats(ctx, p, zw, p.Folder+"/chats/"); err != nil {
				return err
			}
			continue
		}
		s.Touch(uid) // a long export must not look idle
		dir := s.projectDir(p.Folder)
		if err := s.exportFiles(ctx, uid, dir, zw, p.Folder+"/files/"); err != nil {
			return err
		}
		sess, err := c.SessionListAll(ctx, dir)
		if err != nil {
			return err
		}
		if err := s.exportChats(ctx, c, sess, p.Folder, zw, p.Folder+"/chats/", en); err != nil {
			return err
		}
	}
	if project == "" {
		ts, err := s.st.Trash(ctx, uid)
		if err != nil {
			return err
		}
		for _, t := range ts {
			s.Touch(uid)
			prefix := "trash/" + t.Project.Folder + "-" + t.ID + "/"
			if t.Project.Isolated {
				if err := s.exportAssistantChats(ctx, t.Project, zw, prefix+"chats/"); err != nil {
					return err
				}
				continue
			}
			if err := s.exportFiles(ctx, uid, s.trashDir(t.ID), zw, prefix+"files/"); err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
			stored, err := s.st.TrashSessions(ctx, t.ID)
			if err != nil {
				return err
			}
			var sess []ocapi.Session
			for _, se := range stored {
				got, err := c.SessionGet(ctx, se.ID)
				if ocapi.IsStatus(err, http.StatusNotFound) {
					continue
				}
				if err != nil {
					return err
				}
				sess = append(sess, got)
			}
			if err := s.exportChats(ctx, c, sess, t.Project.Folder, zw, prefix+"chats/", en); err != nil {
				return err
			}
		}
	}
	gapProject := ""
	if project != "" {
		gapProject = ps[0].ID
	}
	gaps, err := s.st.AssistantGaps(ctx, uid, gapProject)
	if err != nil {
		return err
	}
	if len(gaps) > 0 {
		data, err := json.MarshalIndent(gaps, "", "  ")
		if err != nil {
			return err
		}
		if err := zipText(zw, "knowledge-gaps.json", string(data), s.now()); err != nil {
			return err
		}
	}
	if project == "" {
		actor, err := s.st.KnowledgeActor(ctx, uid)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err == nil {
			collections, err := s.st.KnowledgeCollections(ctx, actor)
			if err != nil {
				return err
			}
			for _, collection := range collections {
				if collection.OwnerUID != uid {
					continue
				}
				bundle, err := s.st.ExportKnowledge(ctx, actor, collection.ID)
				if err != nil {
					return err
				}
				id := collection.ID
				data, err := json.MarshalIndent(bundle, "", "  ")
				if err != nil {
					return err
				}
				if err := zipText(zw, "knowledge/"+id+"/manifest.json", string(data), s.now()); err != nil {
					return err
				}
			}
		}
	}
	return zw.Close()
}

func zipText(zw *zip.Writer, name, text string, now time.Time) error {
	f, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: now})
	if err == nil {
		_, err = io.WriteString(f, text)
	}
	return err
}

// exportFiles streams the regular files below dir (no dot paths) into zw.
func (s *Service) exportFiles(ctx context.Context, uid, dir string, zw *zip.Writer, prefix string) error {
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(s.sh(ctx, uid, checkPath+`[ -d "$p" ] || exit 3; cd "$p" && exec tar -cf - --hard-dereference --exclude='*/.*' .`, nil, pw, dir))
	}()
	defer pr.Close()
	return tarInto(pr, zw, prefix)
}

// exportChats writes one Markdown file per root session, oldest first.
// Subagent sessions are not exported; their task call is one line in the parent.
func (s *Service) exportChats(ctx context.Context, c *ocapi.Client, sess []ocapi.Session, project string, zw *zip.Writer, prefix string, en bool) error {
	sess = slices.DeleteFunc(slices.Clone(sess), func(se ocapi.Session) bool { return se.ParentID != "" })
	slices.SortStableFunc(sess, func(a, b ocapi.Session) int { return cmp.Compare(a.Time.Created, b.Time.Created) })
	used := map[string]bool{}
	for _, se := range sess {
		created := sessionTime(se)
		if se.Time.Created > 0 {
			created = time.UnixMilli(int64(se.Time.Created))
		}
		base := created.Local().Format("2006-01-02") + "-" + fileSlug(se.Title)
		name := base
		for n := 2; used[name]; n++ {
			name = fmt.Sprintf("%s-%d", base, n)
		}
		used[name] = true
		f, err := zw.CreateHeader(&zip.FileHeader{Name: prefix + name + ".md", Method: zip.Deflate, Modified: sessionTime(se)})
		if err != nil {
			return err
		}
		if err := writeTranscript(ctx, c, se, project, f, en); err != nil {
			return err
		}
	}
	return nil
}

var umlauts = strings.NewReplacer("ä", "ae", "ö", "oe", "ü", "ue", "ß", "ss")

// fileSlug turns a chat title into a portable file name part.
func fileSlug(title string) string {
	var b strings.Builder
	dash := false
	for _, r := range umlauts.Replace(strings.ToLower(title)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 60 {
			break
		}
	}
	if out := strings.Trim(b.String(), "-"); out != "" {
		return out
	}
	return "chat"
}

func when(ms float64, en bool) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(int64(ms)).Local().Format(dateLayout(en))
}

func dateLayout(en bool) string {
	if en {
		return "2006-01-02 15:04"
	}
	return "02.01.2006, 15:04"
}

// writeTranscript renders one chat page by page.
func writeTranscript(ctx context.Context, c *ocapi.Client, se ocapi.Session, project string, w io.Writer, en bool) error {
	title := se.Title
	if title == "" {
		title = tr(en, "Untitled")
	}
	fmt.Fprintf(w, "# %s\n\n", oneLine(title, 200))
	io.WriteString(w, tr(en, "- Project: %s\n", project))
	if t := when(se.Time.Created, en); t != "" {
		io.WriteString(w, tr(en, "- Started: %s\n", t))
	}
	if t := when(se.Time.Updated, en); t != "" {
		io.WriteString(w, tr(en, "- Last: %s\n", t))
	}
	io.WriteString(w, tr(en, "- Chat ID: %s\n", se.ID))
	cursor := ""
	for page := 0; page < maxPages; page++ {
		msgs, next, err := c.Messages(ctx, se.ID, cursor, exportPage)
		if err != nil {
			return err
		}
		for _, m := range msgs {
			if _, err := io.WriteString(w, renderMessage(m, en)); err != nil {
				return err
			}
		}
		if next == "" {
			return nil
		}
		cursor = next
	}
	_, err := io.WriteString(w, tr(en, "\n[Transcript truncated: page limit reached.]\n"))
	return err
}

// renderMessage is the Markdown of one message in English (en) or German;
// "" for types without content for the person (system, synthetic, switches,
// compaction, idle).
func renderMessage(m ocapi.Message, en bool) string {
	var b strings.Builder
	head := func(who string) {
		b.WriteString("\n## " + tr(en, who))
		if t := when(m.Time.Created, en); t != "" {
			b.WriteString(", " + t)
		}
		b.WriteString("\n\n")
	}
	switch m.Type {
	case "user":
		head("You")
		b.WriteString(strings.TrimSpace(m.Text) + "\n")
		if len(m.Files) > 0 {
			var names []string
			for _, f := range m.Files {
				names = append(names, orElse(f.Name, f.Mime))
			}
			b.WriteString(tr(en, "\nAttached: %s\n", strings.Join(names, ", ")))
		}
	case "shell":
		head("You (command)")
		b.WriteString(tr(en, "- Command: `%s`\n", oneLine(m.Command, 200)))
	case "assistant":
		head("Helper")
		var tools, sources []string
		for _, p := range m.Content {
			switch p.Type {
			case "text":
				if t := strings.TrimSpace(p.Text); t != "" {
					b.WriteString(t + "\n\n")
				}
			case "tool":
				tools = append(tools, toolLine(p, en))
				sources = appendNew(sources, toolSources(p)...)
			}
		}
		if msg := errMessage(m.Error); msg != "" {
			b.WriteString(tr(en, "Error: %s\n\n", oneLine(msg, 300)))
		}
		for _, t := range tools {
			b.WriteString("- " + t + "\n")
		}
		if len(sources) > 0 {
			if len(tools) > 0 {
				b.WriteString("\n")
			}
			b.WriteString(tr(en, "Sources:\n"))
			for _, src := range sources {
				b.WriteString("- " + src + "\n")
			}
		}
	default:
		return ""
	}
	return b.String()
}

func orElse(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// toolLine summarises a tool call in one line: name, main argument, outcome.
func toolLine(p ocapi.Part, en bool) string {
	var in map[string]any
	json.Unmarshal(p.State.Input, &in) // a streaming call has a raw string input: no argument
	arg := ""
	for _, k := range []string{"url", "query", "command", "filePath", "path", "pattern", "description", "datei"} {
		if v, ok := in[k].(string); ok && v != "" {
			arg = oneLine(v, 160)
			break
		}
	}
	line := tr(en, "Tool %s", orElse(p.Name, tr(en, "unknown")))
	if arg != "" {
		line += ": " + arg
	}
	switch p.State.Status {
	case "completed":
		line += tr(en, " (done)")
	case "error":
		if msg := errMessage(p.State.Error); msg != "" {
			line += tr(en, " (error: %s)", oneLine(msg, 160))
		} else {
			line += tr(en, " (error)")
		}
	default:
		line += tr(en, " (aborted)")
	}
	if p.Name == "task" {
		line += tr(en, ", subtask for another helper, its history is not in the export")
	}
	return line
}

var mdLink = regexp.MustCompile(`\[([^\]\n]{1,300})\]\((https?://[^)\s]{1,1000})\)`)

// toolSources returns the links a tool call read or found: the url argument
// (webfetch) and the Markdown links in the output of search tools.
func toolSources(p ocapi.Part) []string {
	var in struct {
		URL string `json:"url"`
	}
	var out []string
	if json.Unmarshal(p.State.Input, &in) == nil && strings.HasPrefix(in.URL, "http") {
		out = append(out, oneLine(in.URL, 2000))
	}
	if n := strings.ToLower(p.Name); strings.Contains(n, "search") || strings.Contains(n, "suche") {
		for _, c := range p.State.Content {
			for _, m := range mdLink.FindAllStringSubmatch(c.Text, 50) {
				out = append(out, "["+oneLine(m[1], 300)+"]("+m[2]+")")
			}
		}
	}
	return out
}

func appendNew(list []string, add ...string) []string {
	for _, a := range add {
		if !slices.Contains(list, a) {
			list = append(list, a)
		}
	}
	return list
}

// errMessage reads {"message": ...} or a plain string.
func errMessage(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Message != "" {
		return e.Message
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return "unbekannt"
}

// oneLine collapses whitespace and cuts to n bytes at a rune boundary.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + " ..."
}

func (s *Service) exportAssistantChats(ctx context.Context, p store.Project, zw *zip.Writer, prefix string) error {
	sessions, err := s.st.AssistantSessions(ctx, p.UID, p.ID)
	if err != nil {
		return err
	}
	for _, se := range sessions {
		var messages []struct{ Role, Content string }
		if err := json.Unmarshal(se.Messages, &messages); err != nil {
			return err
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s, revision %d (%s)\n\n", p.Assistant.ID, p.Assistant.Version, p.Assistant.Hash)
		for _, m := range messages {
			if m.Role == "user" || m.Role == "assistant" {
				fmt.Fprintf(&b, "## %s\n\n%s\n\n", m.Role, m.Content)
			}
		}
		if err := zipText(zw, prefix+se.ID+".md", b.String(), se.Updated); err != nil {
			return err
		}
	}
	return nil
}
