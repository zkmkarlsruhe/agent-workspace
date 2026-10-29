// SPDX-License-Identifier: MIT

package portal

import (
	"net/http"
	"net/url"
	"strings"
)

// legacyRoutes maps the German GET routes to the English ones with a 301.
// Compatibility, remove after one release (docs/RENAME-ENGLISH.md 2b).
var legacyRoutes = map[string]string{
	"/profiles":                          "/assistants",
	"/profile":                           "/settings",
	"/projekte/neu":                      "/projects/new",
	"/projekte/{name}":                   "/projects/{name}",
	"/projekte/{name}/weiter":            "/projects/{name}/resume",
	"/projekte/{name}/dateien/{pfad...}": "/projects/{name}/files/{pfad...}",
	"/projekte/{name}/zip":               "/projects/{name}/zip",
	"/projekte/{name}/pdf":               "/projects/{name}/pdf",
	"/projekte/{name}/export":            "/projects/{name}/export",
	"/projekte/{name}/loeschen":          "/projects/{name}/delete",
	"/papierkorb":                        "/trash",
	"/papierkorb/{id}/loeschen":          "/trash/{id}/delete",
	"/umgebung":                          "/environment",
	"/umgebung/start":                    "/environment/starting",
	"/umgebung/zuruecksetzen":            "/environment/reset",
	"/umgebung/anhalten":                 "/environment/stop",
	"/umgebung/export":                   "/environment/export",
	"/umgebung/loeschen":                 "/environment/delete",
	"/geloescht":                         "/deleted",
	"/verbrauch":                         "/usage",
	"/verbrauch/methode":                 "/usage/method",
	"/werkzeuge":                         "/tools",
	"/werkzeuge/eigene/{id}/loeschen":    "/tools/own/{id}/delete",
	"/verbindungen":                      "/connections",
	"/verbindungen/{id}":                 "/connections/{id}",
	"/profil":                            "/settings",
	"/einrichtung":                       "/onboarding",
	"/zustimmung":                        "/consent",
	"/zustimmung/abgelehnt":              "/consent/declined",
	"/hilfe":                             "/help",
	"/so-gehts":                          "/how-it-works",
	"/datenschutz":                       "/privacy",
	"/admin/bericht":                     "/admin/report",
}

// legacyQuery renames the query keys a bookmark may carry; teil also changes its values.
var legacyQuery = map[string]string{"vorlage": "template", "teil": "part", "datei": "file", "weiter": "next",
	"tag": "day", "von": "from", "bis": "to", "markierung": "flag", "schritt": "step"}

var legacyPart = map[string]string{"results": "results", "alles": "all"}

func legacyRedirect(to string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := to
		for _, k := range []string{"name", "id"} {
			p = strings.Replace(p, "{"+k+"}", url.PathEscape(r.PathValue(k)), 1)
		}
		if strings.Contains(p, "{pfad...}") {
			segs := strings.Split(r.PathValue("pfad"), "/")
			for i := range segs {
				segs[i] = url.PathEscape(segs[i])
			}
			p = strings.Replace(p, "{pfad...}", strings.Join(segs, "/"), 1)
		}
		if q := r.URL.Query(); len(q) > 0 {
			out := url.Values{}
			for k, vs := range q {
				if n, ok := legacyQuery[k]; ok {
					k = n
				}
				for _, v := range vs {
					if k == "part" && legacyPart[v] != "" {
						v = legacyPart[v]
					}
					out.Add(k, v)
				}
			}
			p += "?" + out.Encode()
		}
		http.Redirect(w, r, p, http.StatusMovedPermanently)
	}
}
