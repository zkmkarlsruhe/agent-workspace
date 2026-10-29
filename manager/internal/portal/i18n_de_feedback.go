// SPDX-License-Identifier: MIT

package portal

// deFeedback: feedback form and /admin/feedback (see i18n_de.go).
var deFeedback = map[string]string{
	"List limit reached; only the newest 200 feedback entries are shown.":     "Listenlimit erreicht; angezeigt werden nur die neuesten 200 Rückmeldungen.",
	"Chat limit reached; filters and totals cover only the newest 500 chats.": "Chat-Limit erreicht; Filter und Summen berücksichtigen nur die neuesten 500 Chats.",
	"Problem":  "Problem",
	"Idea":     "Idee",
	"Praise":   "Lob",
	"Question": "Frage",
	"Thank you. Your feedback has reached [[recipient]]; only he reads it.":            "Danke. Deine Rückmeldung ist bei [[recipient]] angekommen; nur er liest sie.",
	"You have already sent 10 messages in the last 24 hours. More are possible later.": "In den letzten 24 Stunden sind schon 10 Rückmeldungen von dir angekommen. Weitere gehen später wieder.",
	"Feedback not sent": "Rückmeldung nicht gesendet",
	"Please write what you want to tell [[recipient]] in the field, at most 4000 characters.": "Bitte schreib in das Feld, was du [[recipient]] sagen möchtest, höchstens 4000 Zeichen.",
	"This feedback does not exist (any more).":                                                "Diese Rückmeldung gibt es nicht (mehr).",
	// feedback-form.html
	"Feedback to [[recipient]]": "Feedback an [[recipient]]",
	"Type":                      "Art",
	"What do you want to tell [[recipient]]?": "Was möchtest du [[recipient]] sagen?",
	"A problem, an idea, praise or a question. Only [[recipient]] reads it. You can also do this in the chat: tell the helper “Report this to [[recipient]]” and it attaches the last messages.": "Ein Problem, eine Idee, Lob oder eine Frage. Nur [[recipient]] liest es. Im Chat geht es auch: Sag dem Helfer „Melde das an [[recipient]]“, dann hängt er die letzten Nachrichten an.",
	"Send to [[recipient]]": "An [[recipient]] senden",
	// admin-feedback.html, admin-feedback-detail.html
	"Feedback – Evaluation": "Feedback – Auswertung",
	"Feedback":              "Feedback",
	"Feedback from the chat (tool feedback_senden) and from the portal, newest first. The attached chat messages are in the view of each item; every opening is logged.": "Rückmeldungen aus dem Chat (Werkzeug feedback_senden) und aus dem Portal, die neuesten zuerst. Die angehängten Chat-Nachrichten stehen in der Ansicht einer Rückmeldung; jedes Öffnen wird protokolliert.",
	"Time":             "Zeit",
	"Text":             "Text",
	"Progress":         "Stand",
	"resolved":         "erledigt",
	"open":             "offen",
	"No feedback yet.": "Noch keine Rückmeldungen.",
	"Source":           "Quelle",
	"Portal":           "Portal",
	"OpenCode version": "OpenCode-Version",
	"Reopen":           "Wieder öffnen",
	"Mark as done":     "Als erledigt markieren",
	"Last messages":    "Letzte Nachrichten",
	"Read from the person's environment when sent: one line per tool call, without reasoning and tool output.": "Beim Senden aus der Umgebung der Person gelesen: Werkzeugaufrufe eine Zeile, ohne Gedankengänge und Werkzeugausgaben.",
}
