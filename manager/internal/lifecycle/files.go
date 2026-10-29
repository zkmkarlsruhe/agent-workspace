// SPDX-License-Identifier: MIT

package lifecycle

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/runtime"
)

// File operations run as uid 1000 inside the person's own container via
// docker exec (D33): the manager never mounts user data. Every script
// re-resolves its path with realpath and refuses it if any component is a
// symlink, so a link planted by the agent cannot redirect a read or write.

var (
	ErrNotFound   = errors.New("not found")
	ErrExists     = errors.New("already exists")
	ErrQuota      = errors.New("storage quota exceeded")
	ErrBusy       = errors.New("agent is working in this project")
	ErrNotRunning = errors.New("environment not running")
	ErrInvalid    = errors.New("invalid name or path")
	// ErrFull: OC_MAX_RUNNING containers run already; nothing was started.
	ErrFull = errors.New("all environment places taken")
)

var projectRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ValidProject checks a project folder name (docs/adr/README.md: <= 50, a-z0-9-).
func ValidProject(name string) bool { return len(name) <= 50 && projectRE.MatchString(name) }

// validRel accepts a cleaned relative path without "..".
func validRel(p string) bool {
	return p != "" && p != "." && !strings.HasPrefix(p, "/") && path.Clean(p) == p &&
		p != ".." && !strings.HasPrefix(p, "../") && !strings.ContainsRune(p, 0)
}

// checkPath resolves $1 and exits 3 unless it exists and contains no symlink.
const checkPath = `p=$(realpath -e -- "$1" 2>/dev/null) && [ "$p" = "$1" ] || exit 3; `

func (s *Service) projectDir(name string) string { return path.Join(s.ws, name) }
func (s *Service) trashDir(id string) string     { return path.Join(s.ws, Layout().Trash, id) }

// sh runs script with args in the person's running container and maps the
// exit codes 3/4 to ErrNotFound/ErrExists. stderr is only scanned for a full
// disk and never returned, because it may contain file names.
//
// The slot name is reused after an account deletion, so sh holds the
// person's operation lease (DeleteAccount drains it before the slot is freed)
// and checks that the container still carries this person and generation.
func (s *Service) sh(ctx context.Context, uid, script string, stdin io.Reader, stdout io.Writer, args ...string) error {
	op := s.opLock(uid)
	op.RLock()
	defer op.RUnlock()
	r := s.get(uid)
	if r == nil {
		return ErrNotRunning
	}
	name := config.SlotContainer(r.slot)
	c, err := s.rt.Inspect(ctx, name)
	if err == nil && (c.Labels["oc.uid"] != uid || c.Labels["oc.generation"] != strconv.FormatInt(r.gen, 10)) {
		s.workers.Go(func() { s.Suspect(uid) })
		return ErrNotRunning
	}
	var stderr bytes.Buffer
	var code int
	if err == nil {
		cmd := append([]string{"sh", "-c", script, "sh"}, args...)
		code, err = s.rt.Exec(ctx, name, cmd, stdin, stdout, &limitWriter{&stderr, 4096})
	}
	switch {
	case err != nil && (errors.Is(err, runtime.ErrNotFound) || strings.Contains(err.Error(), "No such container")):
		// removed from outside: forget it now, the next EnsureRunning starts a fresh one
		s.workers.Go(func() { s.Suspect(uid) }) // may be called with the person's lock held
		return ErrNotRunning
	case err != nil:
		return err
	case code == 0:
		return nil
	case code == 3:
		return ErrNotFound
	case code == 4:
		return ErrExists
	case strings.Contains(stderr.String(), "No space left") || strings.Contains(stderr.String(), "quota"):
		return ErrQuota
	}
	return fmt.Errorf("exec exit %d", code)
}

type limitWriter struct {
	w *bytes.Buffer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if room := l.n - l.w.Len(); room > 0 {
		l.w.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// ready starts the environment if needed; file operations need the container.
func (s *Service) ready(ctx context.Context, uid string) error {
	_, err := s.EnsureRunning(ctx, uid)
	return err
}

// shortcut: a project beyond these caps shows a "gekürzt" tree and needs the
// ZIP; page the tree per folder if people hit them.
const treeMaxEntries, treeMaxDepth = 5000, 12

// Tree lists a project recursively: dirs and regular files, no symlinks and
// nothing whose name starts with "." (dot dirs are not entered). Paths are
// relative to the project, parents come before their children. truncated
// reports that the entry or depth cap cut the list.
func (s *Service) Tree(ctx context.Context, uid, project string) (files []File, truncated bool, err error) {
	if !ValidProject(project) {
		return nil, false, ErrInvalid
	}
	if err := s.ready(ctx, uid); err != nil {
		return nil, false, err
	}
	var out bytes.Buffer
	// one level and one entry beyond the caps, so parseTree can tell that something was cut
	err = s.sh(ctx, uid, checkPath+`[ -d "$p" ] || exit 3; find "$p" -mindepth 1 -maxdepth "$2" -name '.*' -prune -o \( -type d -o -type f \) -printf '%y\t%s\t%T@\t%P\0' | head -z -n "$3"`,
		nil, &out, s.projectDir(project), strconv.Itoa(treeMaxDepth+1), strconv.Itoa(treeMaxEntries+1))
	if err != nil {
		return nil, false, err
	}
	files, truncated = parseTree(out.String())
	var newest time.Time
	for _, f := range files {
		if f.Modified.After(newest) {
			newest = f.Modified
		}
	}
	s.touchProject(ctx, uid, project, newest) // a changed file is activity
	return files, truncated, nil
}

// parseTree reads NUL-separated "type\tsize\tmtime\tpath" records; the path
// is the last field, so tabs and newlines in names survive.
func parseTree(out string) (files []File, truncated bool) {
	for _, rec := range strings.Split(out, "\x00") {
		f := strings.SplitN(rec, "\t", 4)
		if len(f) != 4 || f[3] == "" || hidden(f[3]) {
			continue
		}
		if strings.Count(f[3], "/") >= treeMaxDepth || len(files) == treeMaxEntries {
			truncated = true
			continue
		}
		fi := File{Name: path.Base(f[3]), Path: f[3], Dir: f[0] == "d", Modified: unixFloat(f[2])}
		if !fi.Dir {
			fi.Size, _ = strconv.ParseInt(f[1], 10, 64)
		}
		files = append(files, fi)
	}
	return files, truncated
}

// hidden reports a path with a segment starting with "." (.git, .opencode, .project).
func hidden(rel string) bool { return strings.HasPrefix(rel, ".") || strings.Contains(rel, "/.") }

func unixFloat(s string) time.Time {
	v, _ := strconv.ParseFloat(s, 64)
	return time.UnixMilli(int64(v * 1000))
}

// OpenFile streams a regular file of a project; dot paths are refused.
func (s *Service) OpenFile(ctx context.Context, uid, project, rel string) (io.ReadCloser, File, error) {
	if !ValidProject(project) || !validRel(rel) || hidden(rel) {
		return nil, File{}, ErrInvalid
	}
	if err := s.ready(ctx, uid); err != nil {
		return nil, File{}, err
	}
	p := path.Join(s.projectDir(project), rel)
	var st bytes.Buffer
	if err := s.sh(ctx, uid, checkPath+`[ -f "$p" ] || exit 3; stat -c '%s %Y' -- "$p"`, nil, &st, p); err != nil {
		return nil, File{}, err
	}
	var fi File
	fi.Name = path.Base(rel)
	var mtime int64
	fmt.Sscanf(st.String(), "%d %d", &fi.Size, &mtime)
	fi.Modified = time.Unix(mtime, 0)
	s.touchProject(ctx, uid, project, s.now())
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(s.sh(ctx, uid, checkPath+`[ -f "$p" ] || exit 3; exec cat -- "$p"`, nil, pw, p))
	}()
	return pr, fi, nil
}

// Zip parts of WriteZip: the results folder or the whole project.
const (
	ZipResults = "results"
	ZipAll     = "all"
)

// WriteZip streams part ZipResults or ZipAll (without dot files/dirs and symlinks) as ZIP.
func (s *Service) WriteZip(ctx context.Context, uid, project, part string, w io.Writer) error {
	if !ValidProject(project) || (part != ZipResults && part != ZipAll) {
		return ErrInvalid
	}
	if err := s.ready(ctx, uid); err != nil {
		return err
	}
	dir := s.projectDir(project)
	if part == ZipResults {
		dir = path.Join(dir, Layout().Results)
	}
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(s.sh(ctx, uid, checkPath+`[ -d "$p" ] || exit 3; cd "$p" && exec tar -cf - --hard-dereference --exclude='*/.*' .`, nil, pw, dir))
	}()
	defer pr.Close()
	s.touchProject(ctx, uid, project, s.now())
	zw := zip.NewWriter(w)
	if err := tarInto(pr, zw, project+"/"); err != nil {
		return err
	}
	return zw.Close()
}

// tarInto copies the regular files of a tar stream into zw; links and
// devices never reach the archive.
func tarInto(r io.Reader, zw *zip.Writer, prefix string) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: prefix + strings.TrimPrefix(h.Name, "./"), Method: zip.Deflate, Modified: h.ModTime})
		if err != nil {
			return err
		}
		if _, err := io.Copy(fw, tr); err != nil {
			return err
		}
	}
	return nil
}

// Upload writes r to <project>/<Layout.Inbox>/<free name> and returns the name used.
func (s *Service) Upload(ctx context.Context, uid, project, filename string, r io.Reader) (string, error) {
	if !ValidProject(project) || filename == "" || path.Base(filename) != filename || strings.HasPrefix(filename, ".") {
		return "", ErrInvalid
	}
	if err := s.ready(ctx, uid); err != nil {
		return "", err
	}
	s.touchProject(ctx, uid, project, s.now())
	dir := path.Join(s.projectDir(project), Layout().Inbox)
	ext := path.Ext(filename)
	stem := strings.TrimSuffix(filename, ext)
	names := []string{filename}
	for n := 2; n <= 50; n++ {
		names = append(names, fmt.Sprintf("%s (%d)%s", stem, n, ext))
	}
	return s.writeFree(ctx, uid, dir, names, r)
}

// writeFree writes r to the first of names that does not exist in dir and
// returns that name. dir must exist and contain no symlink.
func (s *Service) writeFree(ctx context.Context, uid, dir string, names []string, r io.Reader) (string, error) {
	var name bytes.Buffer
	err := s.sh(ctx, uid, checkPath+`[ -d "$p" ] || exit 3; shift; for n; do [ -e "$p/$n" ] || [ -L "$p/$n" ] || { printf %s "$n"; exit 0; }; done; exit 4`,
		nil, &name, append([]string{dir}, names...)...)
	if err != nil {
		return "", err
	}
	target := path.Join(dir, name.String())
	// set -C: never overwrite a file that appeared in between (exit 4, nothing to clean up)
	err = s.sh(ctx, uid, `set -C; exec 3>"$1" || exit 4; cat >&3 || { rm -f -- "$1"; exit 1; }`, r, nil, target)
	if err != nil {
		if !errors.Is(err, ErrExists) { // a failing reader leaves a partial file behind
			s.sh(context.WithoutCancel(ctx), uid, `rm -f -- "$1"`, nil, nil, target)
		}
		return "", err
	}
	return name.String(), nil
}

// WriteNew writes r next to rel without overwriting: rel itself, then
// <stem>_v2<ext> up to _v50 (AGENTS.md rule). Returns the path relative to
// the project.
func (s *Service) WriteNew(ctx context.Context, uid, project, rel string, r io.Reader) (string, error) {
	if !ValidProject(project) || !validRel(rel) || hidden(rel) {
		return "", ErrInvalid
	}
	if err := s.ready(ctx, uid); err != nil {
		return "", err
	}
	dir, base := path.Split(rel)
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	names := []string{base}
	for n := 2; n <= 50; n++ {
		names = append(names, fmt.Sprintf("%s_v%d%s", stem, n, ext))
	}
	name, err := s.writeFree(ctx, uid, path.Join(s.projectDir(project), dir), names, r)
	if err != nil {
		return "", err
	}
	return path.Join(dir, name), nil
}

// SessionDir returns the folder of an OpenCode session of the person.
func (s *Service) SessionDir(ctx context.Context, uid, sessionID string) (string, error) {
	c, ok := s.Client(uid)
	if !ok {
		return "", ErrNotRunning
	}
	se, err := c.SessionGet(ctx, sessionID)
	return se.Location.Directory, err
}
