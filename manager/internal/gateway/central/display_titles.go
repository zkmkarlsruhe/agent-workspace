// SPDX-License-Identifier: MIT

package central

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/logx"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// DisplayTitles discovers labels using the same user, project grants and source
// allowlists as tools/list. It never calls a tool or shares metadata across users.
func (s *Server) DisplayTitles(ctx context.Context, u store.User, session string) (map[string]string, error) {
	if !sessionRE.MatchString(session) || strings.HasPrefix(session, "ses_profile_") {
		return nil, errors.New("invalid chat session")
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	ctx = logx.With(ctx, logx.Fields{SessionID: session})
	titles := map[string]string{}
	if s.Items == nil || s.Upstream == nil {
		return titles, nil
	}
	items := s.Items(ctx, u.UID)
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, 4)
	for _, item := range items {
		if !instance.Current().RemoteEnabled(item.ID) {
			continue
		}
		wg.Go(func() {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-slots }()
			scope, err := s.sourceScope(ctx, u.UID, &item)
			if err != nil {
				return
			}
			result, err := s.listTools(ctx, u, item, scope)
			if err != nil {
				return
			} // A failed source must not hide labels from healthy sources.
			mu.Lock()
			defer mu.Unlock()
			for _, tool := range result.Tools {
				titles[item.ID+"_"+tool.Name] = displayTitle(tool)
			}
		})
	}
	wg.Wait()
	return titles, nil
}

func displayTitle(tool *sdk.Tool) string {
	annotation := ""
	if tool.Annotations != nil {
		annotation = tool.Annotations.Title
	}
	for _, candidate := range []string{tool.Title, annotation, tool.Name} {
		// Labels are untrusted plain text. Bound layout size and remove direction/control characters.
		title := strings.Join(strings.Fields(strings.Map(func(r rune) rune {
			if unicode.Is(unicode.Cf, r) {
				return -1
			}
			if unicode.IsControl(r) {
				return ' '
			}
			return r
		}, candidate)), " ")
		if title == "" {
			continue
		}
		runes := []rune(title)
		if len(runes) > 160 {
			title = string(runes[:160])
		}
		return title
	}
	return tool.Name
}
