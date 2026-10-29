// SPDX-License-Identifier: MIT

package mcp

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/catalog"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/gateway/llm"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// Toolboxes backs toolbox_run (D60, Demo/Test): commands in the
// person's tool containers, only for switched-on catalog toolboxes.
type Toolboxes struct {
	ChargeEUR *float64 // nil uses the selected instance; 0 disables the estimate
	// On returns the person's switched-on toolboxes (catalog.Service.OnItems).
	On func(ctx context.Context, uid string) []catalog.Item
	// Run is lifecycle.Service.RunToolbox.
	Run        func(ctx context.Context, uid, toolbox, image, dir, command string, out io.Writer) (int, bool, error)
	SessionDir func(ctx context.Context, uid, sessionID string) (string, error)
	// Reserve admits one run against the person's daily cap and concurrency
	// (llm.Budget.ReserveEUR); nil = no gate.
	Reserve func(ctx context.Context, uid string, eur float64) (release func(), err error)
}

const maxCommand = 8000

const toolboxDesc = "Runs a shell command (sh -c) in its own tool container that sees only the person's /workspace folder and has no network. " +
	"The working folder is /workspace/<project of this session>/<workdir>. At most %d seconds, output at most 64 KiB (stdout and stderr together). " +
	"Save results as a file in the project.\nAvailable tools:\n%s"

func itemTitle(ctx context.Context, it catalog.Item) string {
	if english(ctx) {
		return cmp.Or(it.Title, it.TitleDE)
	}
	return it.TitleDE
}

// toolboxTool lists the person's toolboxes and their programs, so the model
// knows what each container has. The description is already in the person's
// language; localize leaves a text it has no entry for unchanged.
func toolboxTool(ctx context.Context, on []catalog.Item) map[string]any {
	var ids, lines []string
	for _, it := range on {
		ids = append(ids, it.ID)
		lines = append(lines, fmt.Sprintf("- %s (%s): %s", it.ID, itemTitle(ctx, it), strings.Join(it.Programs, ", ")))
	}
	return map[string]any{
		"name":        "toolbox_run",
		"title":       "Run tool",
		"description": msg(ctx, toolboxDesc, int(lifecycle.ToolboxTimeout.Seconds()), strings.Join(lines, "\n")),
		"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": true, "openWorldHint": false},
		"inputSchema": map[string]any{
			"type":     "object",
			"required": []string{"toolbox", "command"},
			"properties": map[string]any{
				"toolbox": map[string]any{"type": "string", "enum": ids},
				"command": map[string]any{"type": "string", "minLength": 1, "maxLength": maxCommand, "description": "Shell command, e.g. ffmpeg -i in.mov -vf scale=1280:-2 out.mp4"},
				"workdir": map[string]any{"type": "string", "maxLength": 512, "description": "relative to the project folder, empty = project folder", "default": ""},
			},
		},
	}
}

var errWorkdir = errors.New("workdir")

// workDir is /workspace/<project>/<rel>: rel relative, clean, no dot segments.
func workDir(project, rel string) (string, error) {
	rel = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(rel), "./"), "/")
	if !lifecycle.ValidProject(project) {
		return "", errWorkdir
	}
	if rel == "" || rel == "." {
		return workspace + project, nil
	}
	if path.IsAbs(rel) || path.Clean(rel) != rel || strings.ContainsRune(rel, 0) {
		return "", errWorkdir
	}
	for _, seg := range strings.Split(rel, "/") {
		if strings.HasPrefix(seg, ".") {
			return "", errWorkdir
		}
	}
	return workspace + project + "/" + rel, nil
}

func (s *Server) toolboxRun(ctx context.Context, u store.User, session string, raw json.RawMessage) (toolResult, callMeta) {
	m := callMeta{capability: "toolbox", status: http.StatusBadRequest}
	var a struct {
		Toolbox string `json:"toolbox"`
		Command string `json:"command"`
		Workdir string `json:"workdir"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || strings.TrimSpace(a.Command) == "" || len(a.Command) > maxCommand {
		return errorResult(ctx, "Invalid parameters for toolbox_run: give toolbox and command (command at most 8000 characters)."), m
	}
	m.target = a.Toolbox
	var it catalog.Item
	var ids []string
	for _, w := range s.Toolboxes.On(ctx, u.UID) {
		ids = append(ids, w.ID)
		if w.ID == a.Toolbox {
			it = w
		}
	}
	if it.ID == "" {
		// enforced here: the person's container config does not decide which toolboxes run
		return errorResult(ctx, "The tool \"%s\" is not switched on. Switched on: %s. Switch on more in the portal under Capabilities.",
			a.Toolbox, strings.Join(ids, ", ")), m
	}
	if session == "" {
		return errorResult(ctx, "toolbox_run needs a session in a project."), m
	}
	dir, err := s.Toolboxes.SessionDir(ctx, u.UID, session)
	project, ok := strings.CutPrefix(dir, workspace)
	if err != nil || !ok {
		s.Log.WarnContext(ctx, "toolbox: session dir", "uid", u.UID, "err", err)
		return errorResult(ctx, "The project folder of this session is unknown."), m
	}
	wd, err := workDir(project, a.Workdir)
	if err != nil {
		return errorResult(ctx, "workdir must be a folder in the project, given relative, without \"..\" and without dot folders."), m
	}
	if s.Toolboxes.Reserve != nil {
		release, err := s.Toolboxes.Reserve(ctx, u.UID, s.Toolboxes.chargeEUR())
		switch {
		case errors.Is(err, llm.ErrDayCap):
			m.status = http.StatusTooManyRequests
			return errorResult(ctx, "Daily budget reached, it continues tomorrow."), m
		case errors.Is(err, llm.ErrBusy):
			m.status = http.StatusTooManyRequests
			return errorResult(ctx, "Too many requests at once, please wait a moment."), m
		case err != nil:
			m.status = http.StatusInternalServerError
			return errorResult(ctx, "Internal error."), m
		}
		m.release = release // meter releases after the charge is recorded
	}
	m.costEUR = s.Toolboxes.chargeEUR()
	var out bytes.Buffer
	code, cut, err := s.Toolboxes.Run(ctx, u.UID, it.ID, it.Image, wd, a.Command, &out)
	if err != nil {
		m.status = http.StatusBadGateway
		s.Log.WarnContext(ctx, "toolbox: run", "uid", u.UID, "toolbox", it.ID, "err", err)
		return errorResult(ctx, "The tool container \"%s\" could not be started or does not answer.", itemTitle(ctx, it)), m
	}
	m.status, m.bytesIn = http.StatusOK, out.Len()
	text := strings.ToValidUTF8(out.String(), "�")
	if text == "" {
		text = msg(ctx, "(no output)")
	}
	head := fmt.Sprintf("Exit-Code %d", code)
	switch {
	case code == 124 || code == 137:
		head += msg(ctx, " (stopped after %d seconds)", int(lifecycle.ToolboxTimeout.Seconds()))
	case code == 126 && out.Len() == 0:
		head += msg(ctx, " (working folder missing)")
	}
	if cut {
		head += msg(ctx, ", output cut after 64 KiB")
	}
	return toolResult{Content: []content{{"text", head + "\n" + text}}, IsError: code != 0}, m
}

func (t *Toolboxes) chargeEUR() float64 {
	if t.ChargeEUR != nil {
		return *t.ChargeEUR
	}
	return instance.Current().Accounting.ToolboxEUR
}
