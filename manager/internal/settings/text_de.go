// SPDX-License-Identifier: MIT

package settings

// LangEN is the portal language (users.lang) that writes the block in English;
// anything else is German.
const LangEN = "en"

// de is the German text of the managed block, keyed by the English source.
var de = map[string]string{
	"## About me":   "## Über mich",
	"## How I work": "## So arbeite ich",
	"Name":          "Name",
	"Email":         "E-Mail",
	"Department":    "Abteilung",
	"Role":          "Funktion",
	"Phone":         "Telefon",
	"Room":          "Raum",
	"Groups":        "Gruppen",
	"\n(As of %s, from the login and the %s directory.)\n": "\n(Stand %s, aus der Anmeldung und dem %s-Verzeichnis.)\n",

	"Answer in English.":                                                    "Antworte auf Englisch.",
	"Answer in German.":                                                     "Antworte auf Deutsch.",
	"In German, address me as “Sie”.":                                       "Sprich mich mit „Sie“ an.",
	"In German, address me as “du”.":                                        "Sprich mich mit „du“ an.",
	"Write factually and concisely, without filler phrases.":                "Schreib sachlich und knapp, ohne Floskeln.",
	"Write in a friendly, approachable way.":                                "Schreib freundlich und zugewandt.",
	"Write casually and directly.":                                          "Schreib locker und direkt.",
	"Keep answers short: the result first, explanations only when I ask.":   "Halte Antworten kurz: zuerst das Resultat, Erklärungen nur auf Nachfrage.",
	"Answer at medium length: the result and a brief rationale.":            "Antworte mittellang: das Resultat und eine kurze Begründung.",
	"Explain in detail, with intermediate steps and background.":            "Erkläre ausführlich, mit Zwischenschritten und Hintergrund.",
	"Results preferably right in the chat; a file only when I ask for one.": "Ergebnisse am liebsten direkt im Chat; eine Datei nur, wenn ich sie will.",
	"If a file is produced, preferably as %s.":                              "Wenn eine Datei entsteht, am liebsten als %s.",
	" or ":                   " oder ",
	"My field: %s.":          "Mein Fachgebiet: %s.",
	"Typical tasks: %s.":     "Typische Aufgaben: %s.",
	"Always: %s":             "Immer: %s",
	"Never: %s":              "Nie: %s",
	"Research":               "Recherche",
	"Analysing spreadsheets": "Tabellen auswerten",
	"Writing texts":          "Texte schreiben",
	"Presentations":          "Präsentationen",
	"Proofreading":           "Korrekturlesen",
	"Translating":            "Übersetzen",
	llmSystem:                `Du formulierst Arbeitsanweisungen für einen KI-Helfer. Eine Person hat beschrieben, wie sie arbeitet. Mach daraus kurze, klare Regeln an den Helfer, in der Ich-Form der Person (z. B. "Tabellen bitte immer mit Summenzeile."). Nichts erfinden, nichts weglassen, keine Wiederholungen, keine Anrede, keine Überschriften, keine Emojis. Antworte nur mit JSON in genau dieser Form: {"rules": ["...", "..."]}. Höchstens 12 Regeln, jede höchstens 200 Zeichen, eine Zeile.`,
}

// tx translates an English block text for lang; a missing entry stays English
// (TestBlockGerman keeps that from happening).
func tx(lang, en string) string {
	if lang != LangEN {
		if d := de[en]; d != "" {
			return d
		}
	}
	return en
}
