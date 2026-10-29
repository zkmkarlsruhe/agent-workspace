// SPDX-License-Identifier: MIT

package portal

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"golang.org/x/text/language"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// English is the source language: every text in templates and handlers is
// English and doubles as the key of the German catalog (catalogDE, i18n_de*.go).
const (
	langDE = "de"
	langEN = "en"
)

// langCookie keeps the choice before an account exists (and on every browser).
const langCookie = "lang"

// oldLangCookie is read as a fallback (compatibility, remove after one release).
const oldLangCookie = "sprache"

// Prefs keeps the chosen language per person (users.lang, migration 11);
// nil = cookie and Accept-Language only.
type Prefs interface {
	// Lang returns the stored choice, "" for none or no account yet.
	Lang(ctx context.Context, uid string) (string, error)
	// UserByUID distinguishes an unset language from a missing account on older stores.
	UserByUID(ctx context.Context, uid string) (store.User, error)
	// SetLang stores the choice; without an account it does nothing (the cookie keeps it).
	SetLang(ctx context.Context, uid, lang string) error
}

type langKey struct{}

func langOf(ctx context.Context) string {
	if l, _ := ctx.Value(langKey{}).(string); l == langEN {
		return langEN
	}
	return langDE
}

func lang(r *http.Request) string { return langOf(r.Context()) }

func validLang(l string) bool { return l == langDE || l == langEN }

// withLang resolves the language once per request: stored choice, then the
// cookie, then Accept-Language. A cookie choice made before the account
// existed is copied to the account on the first request that has one.
func (s *Server) withLang(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l := ""
		persist := false
		id, ok := IdentityFrom(r.Context())
		if ok && s.prefs != nil {
			var err error
			l, err = s.prefs.Lang(r.Context(), id.UID)
			if err == nil && l == "" {
				_, err = s.prefs.UserByUID(r.Context(), id.UID)
			}
			persist = err == nil
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				slog.WarnContext(r.Context(), "portal: lang", "err", err)
			}
		}
		if !validLang(l) {
			l = ""
			c, err := r.Cookie(langCookie)
			if err != nil {
				c, err = r.Cookie(oldLangCookie)
			}
			if err == nil && validLang(c.Value) {
				l = c.Value
				if persist {
					if err := s.prefs.SetLang(r.Context(), id.UID, l); err != nil {
						slog.WarnContext(r.Context(), "portal: lang", "err", err)
					}
				}
			}
		}
		if l == "" {
			l = acceptLang(r.Header.Get("Accept-Language"))
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), langKey{}, l)))
	})
}

// acceptLang is the first of de/en in the browser's q order (ties keep the
// header order), German if neither. language.Matcher is not used: it ranks
// an exact "de" above "en-GB" for English even when en-GB has the higher q.
func acceptLang(h string) string {
	tags, _, _ := language.ParseAcceptLanguage(h)
	for _, t := range tags {
		if b, _ := t.Base(); b.String() == langEN {
			return langEN
		} else if b.String() == langDE {
			return langDE
		}
	}
	return langDE
}

// setLang is POST /lang from the switch in the header; it also works
// without consent and without an identity (error pages), then only the cookie.
func (s *Server) setLang(w http.ResponseWriter, r *http.Request) {
	l := r.FormValue("lang")
	if !validLang(l) {
		s.errorPage(w, r, http.StatusBadRequest, "Invalid request", "This language is not available here.")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: langCookie, Value: l, Path: "/", MaxAge: 400 * 24 * 3600,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	if id, ok := IdentityFrom(r.Context()); ok && s.prefs != nil {
		if err := s.prefs.SetLang(r.Context(), id.UID, l); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	http.Redirect(w, r, localPath(r.FormValue("next")), http.StatusSeeOther)
}

// localPath keeps only a path on the portal; browsers drop tabs and newlines
// and read a backslash as a slash, so "/\t/x" would be "//x".
func localPath(v string) string {
	if !strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//") || strings.ContainsFunc(v, func(c rune) bool { return c < 0x20 || c == 0x7f || c == '\\' }) {
		return "/"
	}
	return v
}

// tr translates an English source text; a missing entry stays English (the
// catalog test keeps that from happening). German texts that arrive as data
// from other packages get their English in lang=en.
func translated(l, s string) string {
	if l == langEN {
		return cmp.Or(catalogEN[s], s)
	}
	return cmp.Or(catalogDE[s], s)
}

func tr(l, s string) string { return instance.Text(translated(l, s)) }

// trf translates a format string, then fills it in.
func trf(l, s string, args ...any) string {
	return fmt.Sprintf(instance.Current().Text(translated(l, s), false, true), args...)
}

// th translates a catalog text that carries markup; only the arguments are escaped.
func th(l, s string, args ...any) template.HTML {
	for i, a := range args {
		args[i] = template.HTMLEscapeString(fmt.Sprint(a))
	}
	return template.HTML(fmt.Sprintf(instance.Current().Text(translated(l, s), true, true), args...))
}

// num turns a German number ("1.234,5") into English ("1,234.5").
func num(l, s string) string {
	if l != langEN {
		return s
	}
	return strings.Map(func(c rune) rune {
		switch c {
		case '.':
			return ','
		case ',':
			return '.'
		}
		return c
	}, s)
}

// eurL is "1.234,56 €" in German and "€1,234.56" in English.
func eurL(l string, v float64) string {
	s := eur(v)
	if l != langEN {
		return s
	}
	s = num(l, strings.TrimSuffix(s, " €"))
	if neg, ok := strings.CutPrefix(s, "-"); ok {
		return "-€" + neg
	}
	return "€" + s
}

// dayLayout is the short date: 17.10.2026 / 17 Oct 2026.
func dayLayout(l string) string {
	if l == langEN {
		return "2 Jan 2006"
	}
	return "02.01.2006"
}

// timeAt is "17.10.2026 um 14:03" / "17 Oct 2026 at 14:03".
func timeAt(l string, t time.Time) string {
	return t.Format(dayLayout(l)) + tr(l, " at ") + t.Format("15:04")
}

// mergeCatalogs joins the per-area English catalogs; conflicts lists keys
// that two areas translate differently (the catalog test fails on them).
func mergeCatalogs(parts ...map[string]string) (all map[string]string, conflicts []string) {
	all = map[string]string{}
	for _, p := range parts {
		for k, v := range p {
			if old, ok := all[k]; ok && old != v {
				conflicts = append(conflicts, k)
			}
			all[k] = v
		}
	}
	return all, conflicts
}
