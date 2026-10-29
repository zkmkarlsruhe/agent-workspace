// SPDX-License-Identifier: MIT

package evaluation

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
)

// Report renders the Markdown report of one Berlin day from the current
// data: usage, top topics, what failed, missing skills and tools, flagged
// chats. It names no person; flagged chats link to /admin/chats/<id>. It is
// rendered on request, so a deleted account leaves only its anonymous counts.
func (s *Service) Report(ctx context.Context, day time.Time) (string, error) {
	day = s.DayStart(day)
	next := s.DayStart(day.Add(36 * time.Hour))
	st, err := s.Store.DayStat(ctx, day, next)
	if err != nil {
		return "", err
	}
	chats, err := s.Chats(ctx, Filter{From: day, To: next})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, german("# Evaluation %s, %s\n\nOnly for evaluating the prototype. Created on %s.\n\n"),
		instance.Value("name"), day.Format("02.01.2006"), s.Now().In(day.Location()).Format("02.01.2006, 15:04"))

	if chats.Truncated {
		b.WriteString(german("Chat limit reached; this report includes only the newest 500 chats.\n\n"))
	}
	var llm, heur, open, errChats int
	success := map[string]int{}
	topics := map[string]int{}
	topicName := map[string]string{}
	problems := map[string]int{}
	ideas := map[string]int{}
	var flagged []Item
	for _, it := range chats.Items {
		if it.ToolErrors > 0 {
			errChats++
		}
		if it.E == nil || it.Stale {
			open++
		}
		if it.E == nil {
			continue
		}
		if it.Eval.Source == "llm" {
			llm++
		} else {
			heur++
		}
		success[it.E.Success]++
		k := strings.ToLower(it.E.Topic)
		topics[k]++
		topicName[k] = it.E.Topic
		for _, p := range it.E.Problems {
			problems[p]++
		}
		for _, i := range it.E.Improvements {
			ideas[i.Kind+": "+i.Text]++
		}
		if it.E.PrivateData || it.E.Abuse || it.E.Success == "no" {
			flagged = append(flagged, it)
		}
	}

	b.WriteString(german("## Usage\n\n"))
	fmt.Fprintf(&b, german("- Active people: %d\n- Chats with usage: %d\n- Cost: %s (evaluation itself: %s)\n- CO2: %s to %s g\n"),
		st.People, st.Chats, euro(st.CostEUR), euro(st.EvalCostEUR), num(st.CO2LoG), num(st.CO2HiG))
	fmt.Fprintf(&b, german("- Tool errors: %d in %d chats\n- Chats with activity: %d, evaluated by AI: %d, heuristics only: %d, open: %d\n"),
		st.ToolErrors, errChats, len(chats.Items), llm, heur, open)
	if llm > 0 {
		fmt.Fprintf(&b, german("- Successful (AI verdict): yes %d, partly %d, no %d\n"), success["yes"], success["partly"], success["no"])
	}

	b.WriteString(german("\n## Most frequent topics\n\n"))
	list(&b, topics, 10, func(k string, n int) string { return fmt.Sprintf("%s (%d)", topicName[k], n) })
	b.WriteString(german("\n## What failed most often\n\n"))
	list(&b, problems, 10, func(k string, n int) string { return fmt.Sprintf(german("%s: %d chats"), k, n) })
	b.WriteString(german("\n## Missing skills, tools and guides\n\n"))
	list(&b, ideas, 15, func(k string, n int) string { return fmt.Sprintf("%s (%d)", k, n) })

	b.WriteString(german("\n## Flagged chats\n\n"))
	if len(flagged) == 0 {
		b.WriteString(german("None.\n"))
	}
	for _, it := range flagged {
		var why []string
		if it.E.PrivateData {
			why = append(why, german("private data: ")+orDash(it.E.PrivateDataReason))
		}
		if it.E.Abuse {
			why = append(why, german("misuse: ")+orDash(it.E.AbuseReason))
		}
		if it.E.Success == "no" {
			why = append(why, german("not successful: ")+orDash(it.E.Outcome))
		}
		fmt.Fprintf(&b, "- [%s](/admin/chats/%s): %s\n", mdText(it.E.Topic), mdText(it.SessionID), mdText(strings.Join(why, "; ")))
	}
	return b.String(), nil
}

// list writes the n most frequent keys, ties alphabetically; "Keine." if empty.
func list(b *strings.Builder, m map[string]int, n int, line func(k string, n int) string) {
	keys := slices.SortedFunc(maps.Keys(m), func(a, c string) int { return cmp.Or(cmp.Compare(m[c], m[a]), cmp.Compare(a, c)) })
	if len(keys) == 0 {
		b.WriteString(german("None.\n"))
	}
	for _, k := range keys[:min(n, len(keys))] {
		b.WriteString("- " + mdText(line(k, m[k])) + "\n")
	}
}

// mdText keeps model text from opening links or headings in the report.
func mdText(s string) string {
	return strings.NewReplacer("[", "(", "]", ")", "\n", " ", "#", "").Replace(s)
}

func orDash(s string) string {
	if s == "" {
		return german("no reason given")
	}
	return s
}

func euro(v float64) string { return strings.Replace(fmt.Sprintf("%.2f €", v), ".", ",", 1) }
func num(v float64) string  { return strings.Replace(fmt.Sprintf("%.0f", v), ".", ",", 1) }
