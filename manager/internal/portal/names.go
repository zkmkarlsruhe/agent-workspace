// SPDX-License-Identifier: MIT

package portal

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
)

const maxNameLen = 50

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,49}$`)

// reserved names collide with manager folders inside the workspace, or with
// the portal route /projects/new.
var reserved = map[string]bool{"papierkorb": true, "trash": true, "inbox": true, "results": true, "work": true, "neu": true, "new": true}

const takenMsg = "This name already exists (the Trash counts too). Please choose another one."

// validName reports the German error text for an invalid folder name, or "".
func validName(n string) string {
	switch {
	case n == "":
		return "Please enter a name."
	case len(n) > maxNameLen:
		return "The name may be at most 50 characters long."
	case !nameRE.MatchString(n):
		return "Only lower-case letters, digits and hyphens, not starting with a hyphen."
	case reserved[n] || n == instance.Current().Folders.Inbox || n == instance.Current().Folders.Results || n == instance.Current().Folders.Work:
		return "This name is reserved."
	}
	return ""
}

var translit = strings.NewReplacer("ä", "ae", "ö", "oe", "ü", "ue", "ß", "ss", "Ä", "ae", "Ö", "oe", "Ü", "ue")

// normalizeName turns free text (model answer or description) into a folder
// name per docs/adr/README.md §3.6 step 3. May return "".
func normalizeName(s string) string {
	s = strings.ToLower(translit.Replace(strings.TrimSpace(s)))
	var b strings.Builder
	dash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	n := strings.Trim(b.String(), "-")
	if len(n) > maxNameLen {
		n = n[:maxNameLen]
		if i := strings.LastIndexByte(n, '-'); i > 0 {
			n = n[:i] // cut at a word boundary
		}
		n = strings.Trim(n, "-")
	}
	if reserved[n] {
		return ""
	}
	return n
}

// fallbackName is used when the model fails: JJJJ-MM-TT-<first three words>.
func fallbackName(desc string, now time.Time) string {
	words := strings.FieldsFunc(desc, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	if len(words) > 3 {
		words = words[:3]
	}
	n := normalizeName(now.Format("2006-01-02") + " " + strings.Join(words, " "))
	if n == "" {
		n = now.Format("2006-01-02")
	}
	return n
}

// proposeName returns a unique, valid name and whether the model was used.
func (s *Server) proposeName(ctx context.Context, uid, desc string) (string, bool, error) {
	n, err := s.be.SuggestName(ctx, uid, desc)
	// The model sometimes answers with a sentence; keep only the first line.
	n = normalizeName(strings.SplitN(n, "\n", 2)[0])
	ai := err == nil && n != ""
	if !ai {
		n = fallbackName(desc, s.now())
	}
	return s.uniqueName(ctx, uid, n, ai)
}

func (s *Server) uniqueName(ctx context.Context, uid, base string, ai bool) (string, bool, error) {
	for i := 1; i < 100; i++ {
		n := base
		if i > 1 {
			suf := fmt.Sprintf("-%d", i)
			n = strings.TrimRight(base[:min(len(base), maxNameLen-len(suf))], "-") + suf
		}
		taken, err := s.be.ProjectExists(ctx, uid, n)
		if err != nil {
			return "", false, err
		}
		if !taken {
			return n, ai, nil
		}
	}
	return "", false, fmt.Errorf("no free name for %q", base)
}

// cleanRelPath validates a download path: relative, no "..", no hidden segments.
func cleanRelPath(p string) (string, bool) {
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") {
		return "", false
	}
	c := path.Clean(p)
	for _, seg := range strings.Split(c, "/") {
		if seg == "" || seg == ".." || strings.HasPrefix(seg, ".") {
			return "", false
		}
	}
	return c, true
}

// cleanFilename reduces an uploaded file name to a safe base name, or "".
func cleanFilename(n string) string {
	n = strings.ReplaceAll(n, "\\", "/")
	n = path.Base(n)
	n = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '/' {
			return -1
		}
		return r
	}, n)
	n = strings.TrimLeft(strings.TrimSpace(n), ".")
	if r := []rune(n); len(r) > 200 {
		n = strings.TrimLeft(string(r[len(r)-200:]), ".") // keep the extension
	}
	return n
}
