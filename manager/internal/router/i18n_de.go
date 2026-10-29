// SPDX-License-Identifier: MIT

package router

import (
	"cmp"
	"fmt"
	"net/http"

	"golang.org/x/text/language"
)

// de is the German translation of the texts this package shows. The router
// uses German for early errors; runtime branding follows the person's language.
var de = map[string]string{
	"Check results before using them": "Resultate vor der Verwendung prüfen",
	"Internal error":                  "Interner Fehler",
	"Portal not available":            "Portal nicht verfügbar",
	"Please confirm the prototype notice in the portal first: %s": "Bitte zuerst im Portal den Hinweis zum Prototyp bestätigen: %s",
}

// german translates an English source text, then fills in args (if any).
func german(en string, args ...any) string {
	s := translate("de", en)
	if len(args) > 0 {
		return fmt.Sprintf(s, args...)
	}
	return s
}

func translate(lang, en string) string {
	if lang == "en" {
		return en
	}
	return cmp.Or(de[en], en)
}

func (rt *Router) personLanguage(r *http.Request, uid string) string {
	if rt.Lang != nil {
		if l, err := rt.Lang(r.Context(), uid); err == nil && (l == "de" || l == "en") {
			return l
		}
	}
	c, err := r.Cookie("lang")
	if err != nil {
		c, err = r.Cookie("sprache")
	}
	if err == nil && (c.Value == "de" || c.Value == "en") {
		return c.Value
	}
	tags, _, _ := language.ParseAcceptLanguage(r.Header.Get("Accept-Language"))
	for _, tag := range tags {
		base, _ := tag.Base()
		if l := base.String(); l == "de" || l == "en" {
			return l
		}
	}
	return "de"
}
