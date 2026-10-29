// SPDX-License-Identifier: MIT

package llm

import (
	"cmp"
	"fmt"
)

const (
	msgDayCap   = "Daily budget reached, it continues tomorrow."
	msgBusy     = "Too many requests at once, please wait a moment."
	msgInternal = "Internal error."
)

// de is the German translation of the texts this package shows. The gateway
// does not know the person's language, so german() is what people see.
var de = map[string]string{
	msgDayCap:   "Tagesbudget erreicht, morgen geht es weiter.",
	msgBusy:     "Zu viele gleichzeitige Anfragen, bitte kurz warten.",
	msgInternal: "Interner Fehler.",
	"Please confirm the prototype notice in the portal first: %s": "Bitte bestätige zuerst im Portal den Hinweis zum Prototyp: %s",
	"AI service not reachable.":                                   "KI-Dienst nicht erreichbar.",
	"No access to the AI service.":                                "Kein Zugang zum KI-Dienst.",
	"Request too large.":                                          "Anfrage zu groß.",
	"Invalid JSON.":                                               "Ungültiges JSON.",
}

// german translates an English source text, then fills in args (if any).
func german(en string, args ...any) string {
	s := cmp.Or(de[en], en)
	if len(args) > 0 {
		return fmt.Sprintf(s, args...)
	}
	return s
}
