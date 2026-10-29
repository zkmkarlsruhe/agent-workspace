// SPDX-License-Identifier: MIT

package lifecycle

import (
	"bytes"
	"context"
	"io"
	"path"
	"strings"
)

// File operations for the Nextcloud sync (internal/ncsync). Unlike Tree and
// OpenFile they neither start the environment nor count as project activity:
// the sync only runs while the container runs, and a transfer is not a sign
// that the person works on the project.

// ScanFiles lists the regular files below <project>/<sub> ("" = whole
// project) like Tree does: no symlinks, nothing under a dot name. Path is
// relative to the project. ErrNotFound if sub does not exist.
func (s *Service) ScanFiles(ctx context.Context, uid, project, sub string) ([]File, error) {
	if !ValidProject(project) || (sub != "" && (!validRel(sub) || hidden(sub))) {
		return nil, ErrInvalid
	}
	dir := path.Join(s.projectDir(project), sub)
	var out bytes.Buffer
	err := s.sh(ctx, uid, checkPath+`[ -d "$p" ] || exit 3; cd "$p" && find . -mindepth 1 -maxdepth 12 -name '.*' -prune -o -type f -printf 'f\t%s\t%T@\t%P\0' | head -z -n 5000`,
		nil, &out, dir)
	if err != nil {
		return nil, err
	}
	files, _ := parseTree(out.String())
	for i := range files {
		files[i].Path = path.Join(sub, files[i].Path)
	}
	return files, nil
}

// ReadFile streams a regular file of a running environment; dot paths are refused.
func (s *Service) ReadFile(ctx context.Context, uid, project, rel string) (io.ReadCloser, error) {
	if !ValidProject(project) || !validRel(rel) || hidden(rel) {
		return nil, ErrInvalid
	}
	p := path.Join(s.projectDir(project), rel)
	if err := s.sh(ctx, uid, checkPath+`[ -f "$p" ] || exit 3`, nil, nil, p); err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(s.sh(ctx, uid, checkPath+`[ -f "$p" ] || exit 3; exec cat -- "$p"`, nil, pw, p))
	}()
	return pr, nil
}

// ReplaceFile writes r to rel, replacing an existing file in one rename
// (missing folders are created). Used when the person's newer version comes
// back from Nextcloud; the temporary file has a dot name, so a scan never
// sees it half written.
func (s *Service) ReplaceFile(ctx context.Context, uid, project, rel string, r io.Reader) error {
	if !ValidProject(project) || !validRel(rel) || hidden(rel) {
		return ErrInvalid
	}
	dir, base := path.Split(rel)
	// every component below the project must be a real directory, the target no link
	return s.sh(ctx, uid, checkPath+`set -f; d=$p; IFS=/; for c in $2; do d=$d/$c; [ -L "$d" ] && exit 3; [ -d "$d" ] || mkdir -- "$d" || exit 1; done; unset IFS
t=$d/$3; tmp="$d/.$3.nc-$$"
{ [ -L "$t" ] || [ -d "$t" ]; } && exit 3
cat > "$tmp" || { rm -f -- "$tmp"; exit 1; }
mv -f -- "$tmp" "$t"`, r, nil, s.projectDir(project), strings.TrimSuffix(dir, "/"), base)
}

// WriteMeta replaces <project>/<Layout.ProjectMeta>/<name> with content ("" removes it).
// Starts the environment if needed: the portal toggles call it.
func (s *Service) WriteMeta(ctx context.Context, uid, project, name, content string) error {
	if !ValidProject(project) || name == "" || path.Base(name) != name || strings.HasPrefix(name, ".") {
		return ErrInvalid
	}
	if err := s.ready(ctx, uid); err != nil {
		return err
	}
	dir := path.Join(s.projectDir(project), Layout().ProjectMeta)
	if content == "" {
		return s.sh(ctx, uid, checkPath+`rm -f -- "$p/$2"`, nil, nil, dir, name)
	}
	return s.sh(ctx, uid, checkPath+`[ -d "$p" ] || exit 3; [ -L "$p/$2" ] && exit 3; t="$p/.$2.tmp"; cat > "$t" && mv -f -- "$t" "$p/$2"`,
		strings.NewReader(content), nil, dir, name)
}
