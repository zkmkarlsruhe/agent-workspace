// SPDX-License-Identifier: MIT

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"
	"slices"
	"strings"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/convert"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/gateway/llm"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// Files is the part of lifecycle.Service md_to_pdf needs: reads and writes run
// as uid 1000 in the person's own container and refuse symlinks.
type Files interface {
	OpenFile(ctx context.Context, uid, project, rel string) (io.ReadCloser, lifecycle.File, error)
	WriteNew(ctx context.Context, uid, project, rel string, r io.Reader) (string, error)
	SessionDir(ctx context.Context, uid, sessionID string) (string, error)
}

const workspace = "/workspace/"

const PDFChargeEUR = 0.005

var errPath = errors.New("path")

// resolveFile maps the file argument to project and project-relative path.
// Relative paths are relative to the session folder /workspace/<project>;
// the extension (lower case) must be one of exts.
func resolveFile(file, sessionDir string, exts ...string) (project, rel string, err error) {
	file = strings.TrimSpace(file)
	if strings.HasPrefix(file, "/") {
		if path.Clean(file) != file || !strings.HasPrefix(file, workspace) {
			return "", "", errPath
		}
		project, rel, _ = strings.Cut(strings.TrimPrefix(file, workspace), "/")
	} else {
		dir := strings.TrimPrefix(sessionDir, workspace)
		if dir == sessionDir || strings.Contains(dir, "/") {
			return "", "", errPath
		}
		project, rel = dir, strings.TrimPrefix(file, "./")
	}
	if !lifecycle.ValidProject(project) || rel == "" || path.Clean(rel) != rel || strings.ContainsRune(rel, 0) ||
		!slices.Contains(exts, strings.ToLower(path.Ext(rel))) {
		return "", "", errPath
	}
	for _, seg := range strings.Split(rel, "/") {
		if strings.HasPrefix(seg, ".") { // also "..", .git, .project
			return "", "", errPath
		}
	}
	return project, rel, nil
}

func (s *Server) mdToPDF(ctx context.Context, u store.User, session string, raw json.RawMessage) (toolResult, callMeta) {
	m := callMeta{capability: "convert", target: "gotenberg", status: http.StatusBadRequest}
	var a struct {
		File string `json:"file"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || strings.TrimSpace(a.File) == "" {
		return errorResult(ctx, "Invalid parameters for md_to_pdf: file is missing."), m
	}
	dir := ""
	if !strings.HasPrefix(strings.TrimSpace(a.File), "/") {
		if session == "" {
			return errorResult(ctx, "Relative path without a session: give the absolute path, e.g. /workspace/<project>/[[folders.results]]/report.md."), m
		}
		var err error
		if dir, err = s.Files.SessionDir(ctx, u.UID, session); err != nil {
			s.Log.WarnContext(ctx, "md_to_pdf: session dir", "uid", u.UID, "err", err)
			return errorResult(ctx, "The project folder of this session is unknown: give the absolute path, e.g. /workspace/<project>/[[folders.results]]/report.md."), m
		}
	}
	project, rel, err := resolveFile(a.File, dir, ".md")
	if err != nil {
		return errorResult(ctx, "file must be a .md file in the project, e.g. [[folders.results]]/report.md or /workspace/<project>/[[folders.results]]/report.md; no dot folders, no \"..\"."), m
	}
	charge := PDFChargeEUR
	if s.PDFChargeEUR != nil {
		charge = *s.PDFChargeEUR
	}
	if s.Reserve != nil {
		release, err := s.Reserve(ctx, u.UID, charge)
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
		m.release = release
	}
	m.costEUR = charge
	rc, f, err := s.Files.OpenFile(ctx, u.UID, project, rel)
	if err != nil {
		return s.fileError(ctx, err, &m), m
	}
	m.bytesIn = int(f.Size)
	src, err := io.ReadAll(io.LimitReader(rc, convert.MaxMarkdown+1))
	rc.Close()
	if err != nil {
		return s.fileError(ctx, err, &m), m
	}
	if len(src) > convert.MaxMarkdown {
		m.status = http.StatusRequestEntityTooLarge
		return errorResult(ctx, "The file is too large for the PDF conversion (at most 4 MiB)."), m
	}
	pdf, err := s.PDF(ctx, src, path.Base(rel))
	if err != nil {
		s.Log.WarnContext(ctx, "md_to_pdf: convert", "uid", u.UID, "err", err)
		m.status = http.StatusBadGateway
		return errorResult(ctx, "The PDF conversion is not reachable right now. Try again later; the .md file is kept."), m
	}
	out, err := s.Files.WriteNew(ctx, u.UID, project, strings.TrimSuffix(rel, path.Ext(rel))+".pdf", bytes.NewReader(pdf))
	if err != nil {
		return s.fileError(ctx, err, &m), m
	}
	m.status = http.StatusOK
	full := workspace + project + "/" + out
	return toolResult{
		Content:    textResult(ctx, "PDF created: %s (%s in the project). Download it in the portal under the project.", full, out),
		Structured: map[string]any{"pdf": full, "file": out, "bytes": len(pdf)},
	}, m
}

func (s *Server) fileError(ctx context.Context, err error, m *callMeta) toolResult {
	switch {
	case errors.Is(err, lifecycle.ErrNotFound), errors.Is(err, lifecycle.ErrInvalid):
		m.status = http.StatusNotFound
		return errorResult(ctx, "The file does not exist (or it is a link).")
	case errors.Is(err, lifecycle.ErrQuota):
		m.status = http.StatusInsufficientStorage
		return errorResult(ctx, "The storage is full. Free some space first, then try again.")
	case errors.Is(err, lifecycle.ErrExists):
		m.status = http.StatusConflict
		return errorResult(ctx, "There are already 50 versions of this file. Clean up old versions.")
	}
	s.Log.WarnContext(ctx, "md_to_pdf: file", "err", err)
	m.status = http.StatusInternalServerError
	return errorResult(ctx, "The file could not be read or written.")
}

var mdToPDFTool = map[string]any{
	"name":        "md_to_pdf",
	"title":       "Save as PDF",
	"description": "Turns a Markdown file (.md) of the project into a PDF (A4, [[organization]] template) and saves it next to it: report.md becomes report.pdf; if that exists, report_v2.pdf and so on. Returns the path of the new PDF. Links stay clickable, images from the web are not loaded.",
	"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": false, "openWorldHint": false},
	"inputSchema": map[string]any{
		"type":     "object",
		"required": []string{"file"},
		"properties": map[string]any{
			"file": map[string]any{"type": "string", "maxLength": 1024, "description": "Path of the .md file, relative to the project folder ([[folders.results]]/report.md) or absolute (/workspace/<project>/[[folders.results]]/report.md)"},
		},
	},
}
