// SPDX-License-Identifier: MIT

// Package identity validates mapped headers set by trusted forward
// auth and derives the per-person host slug (docs/adr/README.md §3.5, §3.6).
package identity

import (
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

type Identity struct {
	UID      string // 64 hex, the per-person key
	Username string
	Email    string
	Name     string
	Groups   []string
	Slug     string // filled by the router after the store lookup
	Issuer   string
	Subject  string
	Blocked  bool
}

var (
	ErrUntrusted = errors.New("identity: request not from trusted proxy")
	ErrInvalid   = errors.New("identity: missing or malformed identity headers")
	ErrNoGroup   = errors.New("identity: required group missing")
)

var uidRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidUID reports whether s has the internal account key format.
func ValidUID(s string) bool { return uidRE.MatchString(s) }

// Headers maps identity fields to trusted proxy response headers; UID "-" always derives the internal UID.
type Headers struct {
	UID, Username, Email, Name, Groups string
}

// WithDefaults preserves the Authentik contract for callers without a mapping.
func (h Headers) WithDefaults() Headers {
	return Headers{
		UID:      cmp.Or(h.UID, "X-Authentik-Uid"),
		Username: cmp.Or(h.Username, "X-Authentik-Username"),
		Email:    cmp.Or(h.Email, "X-Authentik-Email"),
		Name:     cmp.Or(h.Name, "X-Authentik-Name"),
		Groups:   cmp.Or(h.Groups, "X-Authentik-Groups"),
	}
}

type Checker struct {
	Headers         Headers
	GroupsSeparator string // empty: |

	Trusted       []netip.Prefix
	RequiredGroup string // empty: no group check
	Issuer        string // fixed instance namespace; empty: authentik
	SubjectHeader string // empty: X-Authentik-Uid
}

// SourceTrusted reports whether the direct peer is the trusted proxy.
func (c Checker) SourceTrusted(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	a = a.Unmap()
	return slices.ContainsFunc(c.Trusted, func(p netip.Prefix) bool { return p.Contains(a) })
}

// FromRequest returns the identity only if the request came from the trusted
// proxy; headers from anywhere else are never read.
func (c Checker) FromRequest(r *http.Request) (Identity, error) {
	if !c.SourceTrusted(r) {
		return Identity{}, ErrUntrusted
	}
	h := r.Header
	headers := c.Headers.WithDefaults()
	subjectHeader := cmp.Or(c.SubjectHeader, "X-Authentik-Uid")
	if len(h.Values(subjectHeader)) != 1 {
		return Identity{}, ErrInvalid
	}
	id := Identity{
		UID:      h.Get(headers.UID),
		Issuer:   cmp.Or(c.Issuer, "authentik"),
		Subject:  h.Get(subjectHeader),
		Username: strings.TrimSpace(h.Get(headers.Username)),
		Email:    h.Get(headers.Email),
		Name:     h.Get(headers.Name),
	}
	for _, g := range strings.Split(h.Get(headers.Groups), cmp.Or(c.GroupsSeparator, "|")) {
		if g = strings.TrimSpace(g); g != "" {
			id.Groups = append(id.Groups, g)
		}
	}
	if id.Username == "" || !ValidSubject(id.Issuer, id.Subject) {
		return Identity{}, ErrInvalid
	}
	if headers.UID == "-" || len(h.Values(headers.UID)) == 0 {
		hash := sha256.Sum256([]byte(id.Issuer + "\x00" + id.Subject))
		id.UID = fmt.Sprintf("%x", hash[:])
	} else if !ValidUID(id.UID) {
		return Identity{}, ErrInvalid
	}
	if c.RequiredGroup != "" && !slices.Contains(id.Groups, c.RequiredGroup) {
		return id, ErrNoGroup
	}
	return id, nil
}

// ValidSubject rejects missing, oversized and ambiguous header values.
func ValidSubject(issuer, subject string) bool {
	for _, s := range []string{issuer, subject} {
		if s == "" || len(s) > 2048 || strings.TrimSpace(s) != s || strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return false
		}
	}
	return true
}

// SubjectHash identifies a binding in logs without exposing provider ids.
func SubjectHash(issuer, subject string) string {
	h := sha256.Sum256([]byte(issuer + "\x00" + subject))
	return fmt.Sprintf("%x", h[:16])
}

type ctxKey struct{}

func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the identity the router attached to a request.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(Identity)
	return id, ok
}

// MaxSlugLen keeps <slug>--<port> well below the 63-char DNS label limit.
const MaxSlugLen = 32

var reserved = []string{
	"www", "auth", "login", "portal", "api", "admin", "static", "assets", "outpost",
	"files", "mail", "git", "code", "grafana", "prometheus", "metrics", "status", "test",
}

// Reserved reports whether a slug may never be handed out.
func Reserved(s string) bool { return slices.Contains(reserved, s) }

var translit = strings.NewReplacer(
	"ä", "ae", "ö", "oe", "ü", "ue", "ß", "ss",
	"á", "a", "à", "a", "â", "a", "ã", "a", "å", "a",
	"é", "e", "è", "e", "ê", "e", "ë", "e",
	"í", "i", "ì", "i", "î", "i", "ï", "i",
	"ó", "o", "ò", "o", "ô", "o", "õ", "o", "ø", "o",
	"ú", "u", "ù", "u", "û", "u", "ç", "c", "ñ", "n", "ý", "y", "ł", "l", "š", "s", "ž", "z", "č", "c",
)

// Slugify derives the base slug from a username: lowercase [a-z0-9-], no "--",
// starts with a letter, at most MaxSlugLen. Reserved or short results are
// resolved by Candidate.
// shortcut: transliterates common Latin diacritics only, others become "-"; use x/text NFKD if non-Latin names appear.
func Slugify(username string) string {
	s := translit.Replace(strings.ToLower(username))
	var b strings.Builder
	dash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if out != "" && (out[0] < 'a' || out[0] > 'z') {
		out = "u-" + out
	}
	return trim(out, MaxSlugLen)
}

// Candidate returns the n-th slug to try for base (n starts at 1). It returns
// "" when attempt n must be skipped (base reserved or too short).
func Candidate(base string, n int) string {
	if base == "" {
		base = "user"
	}
	if n == 1 {
		if len(base) < 2 || Reserved(base) {
			return ""
		}
		return base
	}
	suf := "-" + strconv.Itoa(n)
	return trim(base, MaxSlugLen-len(suf)) + suf
}

func trim(s string, max int) string {
	if len(s) > max {
		s = s[:max]
	}
	return strings.TrimRight(s, "-")
}
