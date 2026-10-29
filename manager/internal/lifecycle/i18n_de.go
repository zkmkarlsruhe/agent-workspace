// SPDX-License-Identifier: MIT

package lifecycle

import (
	"cmp"
	"fmt"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
)

// de is the German translation of the texts this package writes for people:
// the export (README.txt, chat transcripts) in the person's language
// (users.lang, German unless "en"), the feedback transcript always German.
var de = map[string]string{
	"\n[Transcript truncated: page limit reached.]\n": "\n[Chatverlauf gekürzt: Seitenlimit erreicht.]\n",
	exportReadme: `[[name]] - Export deiner Daten
Erstellt am %s.

Aufbau
  <projekt>/files/     alle Dateien des Projekts ([[folders.inbox]], [[folders.results]], [[folders.work]], ...)
  <projekt>/chats/     jeder Chat als lesbarer Text: <datum>-<titel>.md
%s%s
Die Chats sind Markdown-Dateien und lassen sich mit jedem Texteditor lesen.
Sie enthalten deine Nachrichten und die Antworten des Helfers. Werkzeugaufrufe
des Helfers (Webseite lesen, Suche, Befehl ausführen, Datei schreiben) stehen
als eine Zeile, gefundene Quellen und Links als Liste unter der Antwort.
Gedankengänge des Helfers und Teilaufgaben, die er an weitere Helfer gegeben
hat, sind nicht enthalten; eine Teilaufgabe steht als eine Zeile im Chat.

Nicht enthalten sind versteckte Dateien und Ordner (Name beginnt mit einem
Punkt, zum Beispiel .git mit dem Verlauf der Dateiversionen), Verknüpfungen
(Symlinks), Einstellungen und Zugangsdaten.
`,
	"  trash/<project>-<id>/        projects in the trash, same layout\n": "  trash/<projekt>-<kennung>/        Projekte im Papierkorb, gleicher Aufbau\n",
	"  profile.md           About me & how I work\n":                      "  profile.md           Über mich & Arbeitsweise\n",
	"Untitled":          "Ohne Titel",
	"- Project: %s\n":   "- Projekt: %s\n",
	"- Started: %s\n":   "- Begonnen: %s\n",
	"- Last: %s\n":      "- Zuletzt: %s\n",
	"- Chat ID: %s\n":   "- Chat-Kennung: %s\n",
	"You":               "Du",
	"You (command)":     "Du (Befehl)",
	"Helper":            "Helfer",
	"\nAttached: %s\n":  "\nAngehängt: %s\n",
	"- Command: `%s`\n": "- Befehl: `%s`\n",
	"Error: %s\n\n":     "Fehler: %s\n\n",
	"Sources:\n":        "Quellen:\n",
	"Tool %s":           "Werkzeug %s",
	"unknown":           "unbekannt",
	" (done)":           " (fertig)",
	" (error: %s)":      " (Fehler: %s)",
	" (error)":          " (Fehler)",
	" (aborted)":        " (abgebrochen)",
	", subtask for another helper, its history is not in the export": ", Teilaufgabe an einen weiteren Helfer, ihr Verlauf ist nicht im Export",
	"[... older messages cut ...]\n":                                 "[... ältere Nachrichten gekürzt ...]\n",
	"\n[... message cut ...]\n":                                      "\n[... Nachricht gekürzt ...]\n",
}

// tr is s in English when en, else in German, with args filled in (if any).
func tr(en bool, s string, args ...any) string {
	if !en {
		s = cmp.Or(de[s], s)
	}
	if len(args) > 0 {
		return fmt.Sprintf(instance.Current().Text(s, false, true), args...)
	}
	return instance.Text(s)
}
