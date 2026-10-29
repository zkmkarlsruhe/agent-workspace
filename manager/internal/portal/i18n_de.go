// SPDX-License-Identifier: MIT

package portal

import (
	"fmt"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/settings"
)

// German catalog: English source text -> German. The German texts are the
// former source texts, word for word; du for "you". Terms: helper = Helfer,
// environment = Umgebung, project = Projekt, skill = Fähigkeit, Trash =
// Papierkorb, Usage = Verbrauch; the folders keep their names: "Results
// (results/)", "Inbox (inbox/)". The product name "Agent Workspace" is never
// translated.
var catalogDE, deConflicts = mergeCatalogs(deCore, deProjects, deEnvironment, deSettings, deHelp, deHowto, deFeedback, deIdentity, deTaskAssistants)

// catalogEN is the reverse: German texts that still arrive as data from other
// packages (settings options and errors) are shown in English.
var catalogEN = map[string]string{}

func init() {
	// the length errors of the settings carry the limit already filled in
	for _, n := range []int{settings.MaxField, settings.MaxList, settings.MaxNote, settings.MaxText} {
		catalogDE[fmt.Sprintf("At most %d characters, please.", n)] = fmt.Sprintf(catalogDE["At most %d characters, please."], n)
	}
	for e, d := range catalogDE {
		catalogEN[d] = e
	}
}

// deCore: layout, error pages, consent pages, shared helpers.
var deCore = map[string]string{
	"Connections (Nextcloud)": "Verbindungen (Nextcloud)",
	"Delete account":          "Konto löschen",
	"Skip to content":         "Zum Inhalt",
	"What this means":         "Was das heißt",
	"Signed in as":            "Angemeldet als",
	"Sign out":                "Abmelden",
	"Language":                "Sprache",
	"Main navigation":         "Hauptnavigation",
	"Projects":                "Projekte",
	"Capabilities":            "Fähigkeiten",
	"Connections":             "Verbindungen",
	"About me & how I work":   "Über mich & Arbeitsweise",
	"Usage":                   "Verbrauch",
	"Environment":             "Umgebung",
	"Help":                    "Hilfe",
	"Evaluation":              "Auswertung",
	"Privacy":                 "Datenschutz",
	"Contact [[team]]":        "Kontakt [[team]]",
	"Overview":                "Übersicht",
	"Chats":                   "Chats",
	"For evaluating the prototype only – access is logged": "Nur zur Auswertung des Prototyps – Zugriff wird protokolliert",
	"Please correct":            "Bitte korrigieren",
	"You agreed on %s.":         "Du hast am %s\u00a0Uhr zugestimmt.",
	"To the notice and consent": "Zum Hinweis und zur Zustimmung",
	" at ":                      " um ",
	"today, ":                   "heute, ",
	"yesterday, ":               "gestern, ",
	"Internal error":            "Interner Fehler",

	// connection partials of layout.html
	"Connection expired": "Verbindung abgelaufen",
	"<span class=\"badge badge--done\">connected</span> as <strong>%s</strong> since %s": "<span class=\"badge badge--done\">verbunden</span> als <strong>%s</strong> seit %s",
	"waiting for you to sign in": "wartet auf deine Anmeldung",
	"Error":                      "Fehler",
	"not connected":              "nicht verbunden",
	"Please connect your own Nextcloud account. You were signed in with a different account; that access has already been deleted again.": "Bitte mit deinem eigenen Nextcloud-Konto verbinden. Du warst mit einem anderen Konto angemeldet; dieser Zugang ist schon wieder gelöscht.",
	"The sign-in was not confirmed within 20\u00a0minutes. Please connect again.":                                                         "Die Anmeldung wurde nicht innerhalb von 20\u00a0Minuten bestätigt. Bitte noch einmal verbinden.",
	"The connection could not be established. Please connect again; if the error persists, let [[team]] know.":                            "Die Verbindung kam nicht zustande. Bitte noch einmal verbinden; bleibt der Fehler, sag [[team]] Bescheid.",
	"Reconnect":  "Neu verbinden",
	"Connect":    "Verbinden",
	"Disconnect": "Trennen",

	// status labels (portal.go)
	"helper is working":      "Helfer arbeitet",
	"waiting for your reply": "wartet auf deine Antwort",
	"done":                   "fertig",
	"running":                "läuft",
	"starting":               "startet",
	"does not start":         "startet nicht",
	"asleep":                 "schläft",
	"all places taken":       "alle Plätze belegt",

	// error pages
	"Error ID":       "Fehlerkennung",
	"To my projects": "Zu meinen Projekten",
	"No access":      "Kein Zugang",
	"Your account has not been activated for [[name]].": "Dein Konto ist für [[name]] nicht freigeschaltet.",
	"Access is granted by the [[team]] team.":           "Den Zugang vergibt das Team [[team]].",
	"What you can do": "Was du tun kannst",
	"Write to the [[team]] team in the Element room <a href=\"%s\">[[support_label]]</a> and ask for access.": "Schreib dem Team [[team]] im Element-Raum <a href=\"%s\">[[support_label]]</a> und bitte um Zugang.",
	"If you have already been activated: sign out once and sign in again.":                                    "Wenn du schon freigeschaltet bist: einmal abmelden und neu anmelden.",
	"Page not found":            "Seite nicht gefunden",
	"This page does not exist.": "Diese Seite gibt es nicht.",
	"Request refused":           "Anfrage abgelehnt",
	"The request did not come from this site and was therefore not carried out.": "Die Anfrage kam nicht von dieser Seite und wurde deshalb nicht ausgeführt.",
	"Invalid request":                                "Anfrage ungültig",
	"This language is not available here.":           "Diese Sprache gibt es hier nicht.",
	"Not found":                                      "Nicht gefunden",
	"The project or file does not exist (any more).": "Das Projekt oder die Datei gibt es nicht (mehr).",
	"The helper is working right now":                "Der Helfer arbeitet gerade",
	"This is only possible once the helper has finished. Wait until it is done, or stop the environment under “Environment”.": "Das geht erst, wenn der Helfer fertig ist. Warte, bis er fertig ist, oder halte unter „Umgebung“ die Umgebung an.",
	"Your environment is not running": "Deine Umgebung läuft nicht",
	"Your environment has to be running for this. You can start it under “Environment”; “Continue” or “New task” starts it automatically.": "Dafür muss deine Umgebung laufen. Unter „Umgebung“ kannst du sie starten; mit „Weiterarbeiten“ oder „Neue Aufgabe“ startet sie von selbst.",
	"Storage full": "Speicherplatz voll",
	"Your storage is full. Empty the Trash or delete old projects. Under “Environment” you can see how much is used.": "Dein Speicherplatz ist voll. Leere den Papierkorb oder lösche alte Projekte. Unter „Umgebung“ siehst du, wie viel belegt ist.",
	"Not possible right now": "Gerade nicht möglich",
	"This is not possible on this installation at the moment. Please contact [[team]].": "Das geht auf dieser Installation gerade nicht. Bitte wende dich an [[team]].",
	"All places taken": "Alle Plätze belegt",
	"All places are taken right now, please try again in a few minutes.": "Gerade sind alle Plätze belegt, bitte in ein paar Minuten nochmal.",
	"Already exists": "Gibt es schon",
	"A project with this name already exists. Please choose a different name.": "Ein Projekt mit diesem Namen gibt es schon. Bitte wähle einen anderen Namen.",
	"Something went wrong": "Etwas ist schiefgegangen",
	"The action could not be carried out. Please try again. If the error persists, give [[team]] the error ID.": "Die Aktion konnte nicht ausgeführt werden. Bitte versuche es noch einmal. Wenn der Fehler bleibt, nenne [[team]] die Fehlerkennung.",

	// consent pages
	"Please read first":           "Bitte zuerst lesen",
	"Before you start":            "Bevor du anfängst",
	"I have read this and agree.": "Ich habe das gelesen und bin einverstanden.",
	"Next":                        "Weiter",
	"I do not agree – back":       "Nicht einverstanden – zurück",
	"Please confirm that you have read the notice and agree.": "Bitte bestätige, dass du den Hinweis gelesen hast und einverstanden bist.",
	"Not agreed":        "Nicht einverstanden",
	"You did not agree": "Du hast nicht zugestimmt",
	"Your refusal was not stored. Your existing projects are kept; you can only use [[name]] again once you agree.": "Deine Ablehnung wurde nicht gespeichert. Deine bisherigen Projekte bleiben erhalten; nutzen kannst du [[name]] erst wieder nach deiner Zustimmung.",
	"Nothing was stored.": "Es wurde nichts gespeichert.",
	"You cannot use [[name]] without agreeing. If you change your mind, you can go back to the notice at any time.": "Ohne Zustimmung kannst du [[name]] nicht nutzen. Überlegst du es dir anders, kannst du jederzeit zum Hinweis zurück.",
	"Back to the notice": "Zurück zum Hinweis",
}
