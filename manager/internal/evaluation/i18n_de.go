// SPDX-License-Identifier: MIT

package evaluation

import "cmp"

// de is the German translation of the report texts. The report is for the
// operator and has no language setting, so german() is what it shows.
var de = map[string]string{
	"Chat limit reached; this report includes only the newest 500 chats.\n\n":      "Chat-Limit erreicht; dieser Bericht berücksichtigt nur die neuesten 500 Chats.\n\n",
	"# Evaluation %s, %s\n\nOnly for evaluating the prototype. Created on %s.\n\n": "# Auswertung %s, %s\n\nNur zur Auswertung des Prototyps. Erstellt am %s.\n\n",
	"## Usage\n\n": "## Nutzung\n\n",
	"- Active people: %d\n- Chats with usage: %d\n- Cost: %s (evaluation itself: %s)\n- CO2: %s to %s g\n":           "- Aktive Personen: %d\n- Chats mit Verbrauch: %d\n- Kosten: %s (Auswertung selbst: %s)\n- CO2: %s bis %s g\n",
	"- Tool errors: %d in %d chats\n- Chats with activity: %d, evaluated by AI: %d, heuristics only: %d, open: %d\n": "- Werkzeugfehler: %d in %d Chats\n- Chats mit Aktivität: %d, ausgewertet mit KI: %d, nur Heuristik: %d, offen: %d\n",
	"- Successful (AI verdict): yes %d, partly %d, no %d\n":                                                          "- Erfolgreich (KI-Urteil): ja %d, teilweise %d, nein %d\n",
	"\n## Most frequent topics\n\n":             "\n## Häufigste Themen\n\n",
	"\n## What failed most often\n\n":           "\n## Was am häufigsten scheiterte\n\n",
	"%s: %d chats":                              "%s: %d Chats",
	"\n## Missing skills, tools and guides\n\n": "\n## Fehlende Skills, Werkzeuge und Anleitungen\n\n",
	"\n## Flagged chats\n\n":                    "\n## Markierte Chats\n\n",
	"None.\n":                                   "Keine.\n",
	"private data: ":                            "private Daten: ",
	"misuse: ":                                  "Missbrauch: ",
	"not successful: ":                          "nicht erfolgreich: ",
	"no reason given":                           "ohne Begründung",
}

func german(en string) string { return cmp.Or(de[en], en) }
