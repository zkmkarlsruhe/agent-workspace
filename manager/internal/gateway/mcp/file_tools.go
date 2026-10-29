// SPDX-License-Identifier: MIT

package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/filetools"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// One prepared request at a time matches the shared worker and bounds encoded buffers.
var filePreparation = make(chan struct{}, 1)

const fileBurst = 12
const fileRefill = 10 * time.Second

type fileBucket struct {
	tokens float64
	at     time.Time
}
type fileRates struct {
	sync.Mutex
	people map[string]fileBucket
}

func (r *fileRates) allow(uid string, now time.Time) bool {
	r.Lock()
	defer r.Unlock()
	b, ok := r.people[uid]
	if !ok {
		// Fully replenished idle entries carry no rate history worth retaining.
		for id, old := range r.people {
			if now.Sub(old.at) >= fileBurst*fileRefill {
				delete(r.people, id)
			}
		}
		b = fileBucket{tokens: fileBurst, at: now}
	}
	b.tokens = min(fileBurst, b.tokens+now.Sub(b.at).Seconds()/fileRefill.Seconds())
	b.at = now
	allowed := b.tokens >= 1
	if allowed {
		b.tokens--
	}
	r.people[uid] = b
	return allowed
}

type fileArtifact struct {
	Path      string `json:"path"`
	MediaType string `json:"media_type"`
	Role      string `json:"role"`
	SHA256    string `json:"sha256"`
}

type fileResult struct {
	SchemaVersion int            `json:"schema_version"`
	Status        string         `json:"status"`
	Outputs       []fileArtifact `json:"outputs"`
	MetadataPath  string         `json:"metadata_path,omitempty"`
	Warnings      []string       `json:"warnings"`
}

func structuredResult(value any) toolResult {
	b, _ := json.Marshal(value)
	return toolResult{Content: []content{{Type: "text", Text: string(b)}}, Structured: value}
}

func digest(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// projectInput binds even absolute paths to the current session's project.
func (s *Server) projectInput(ctx context.Context, u store.User, session, file string, exts ...string) ([]byte, string, string, error) {
	if session == "" {
		return nil, "", "", fmt.Errorf("a project session is required")
	}
	dir, err := s.Files.SessionDir(ctx, u.UID, session)
	project, ok := strings.CutPrefix(dir, workspace)
	if err != nil || !ok || !lifecycle.ValidProject(project) {
		return nil, "", "", fmt.Errorf("unknown session project")
	}
	if file == "" {
		return nil, project, "", nil
	}
	p, rel, err := resolveFile(file, dir, exts...)
	if err != nil || p != project {
		return nil, "", "", fmt.Errorf("source must be a supported file in this session's project")
	}
	rc, info, err := s.Files.OpenFile(ctx, u.UID, project, rel)
	if err != nil {
		return nil, "", "", err
	}
	defer rc.Close()
	if info.Size > maxImageBytes {
		return nil, "", "", fmt.Errorf("input exceeds 20 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(rc, maxImageBytes+1))
	if err != nil || len(data) > maxImageBytes {
		return nil, "", "", fmt.Errorf("could not read bounded input")
	}
	return data, project, rel, nil
}

func (s *Server) fileTool(ctx context.Context, u store.User, session, operation string, raw json.RawMessage) (toolResult, callMeta) {
	m := callMeta{capability: "file-tools", target: operation, status: http.StatusBadRequest}
	var a struct {
		Path         string         `json:"path"`
		SVG          string         `json:"svg"`
		SourceSHA256 string         `json:"source_sha256"`
		Options      map[string]any `json:"options"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&a); err != nil {
		return errorResult(ctx, "Invalid file tool arguments: %s", err), m
	}
	if a.Options == nil {
		a.Options = map[string]any{}
	}
	ext := imageExts
	switch operation {
	case "handwriting_to_text":
		ext = append(append([]string{}, imageExts...), ".pdf")
	case "pdf_to_text":
		ext = []string{".pdf"}
	case "svg_create", "svg_edit":
		ext = []string{".svg"}
	}
	if operation != "svg_create" && a.Path == "" || operation == "svg_create" && a.Path != "" {
		return errorResult(ctx, "A source path is required except for svg_create."), m
	}
	if !s.fileRates.allow(u.UID, time.Now()) {
		m.status = http.StatusTooManyRequests
		return errorResult(ctx, "File processing rate limit reached; wait before retrying."), m
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	if s.Reserve != nil {
		release, err := s.Reserve(ctx, u.UID, 0)
		if err != nil {
			m.status = http.StatusTooManyRequests
			return errorResult(ctx, "File processing capacity is unavailable: %s", err), m
		}
		release = sync.OnceFunc(release)
		m.release = release
		// Preparation holds admission only; transcription reserves its own model cost.
		if operation == "handwriting_to_text" {
			defer release()
			ctx = context.WithValue(ctx, handwritingAdmissionKey{}, release)
		}
	}
	select {
	case filePreparation <- struct{}{}:
		defer func() { <-filePreparation }()
	case <-ctx.Done():
		m.status = http.StatusTooManyRequests
		return errorResult(ctx, "File preparation cancelled: %s", ctx.Err()), m
	}
	if ctx.Err() != nil {
		return errorResult(ctx, "File preparation cancelled: %s", ctx.Err()), m
	}
	data, project, source, err := s.projectInput(ctx, u, session, a.Path, ext...)
	if err != nil {
		return errorResult(ctx, "Cannot read source: %s", err), m
	}
	m.bytesIn = len(data)
	sourceHash := digest(data)
	if operation == "svg_edit" && (a.SourceSHA256 == "" || a.SourceSHA256 != sourceHash) {
		return errorResult(ctx, "SOURCE_CHANGED: source_sha256 must match the current SVG. Read it again before editing."), m
	}
	if operation == "svg_create" || operation == "svg_edit" {
		if len(a.SVG) == 0 || len(a.SVG) > 512<<10 {
			return errorResult(ctx, "SVG markup is required, at most 512 KiB."), m
		}
		data = []byte(a.SVG)
	} else if a.SVG != "" || a.SourceSHA256 != "" {
		return errorResult(ctx, "SVG arguments are only accepted by SVG tools."), m
	}
	var processed filetools.Result
	if operation == "handwriting_to_text" {
		processed, err = s.transcribeFile(ctx, u, session, data, strings.EqualFold(path.Ext(source), ".pdf"), a.Options)
	} else {
		processed, err = s.ProcessFiles(ctx, operation, data, a.Options)
	}
	if err != nil {
		m.status = http.StatusBadGateway
		return errorResult(ctx, "File processing failed: %s", err), m
	}
	// Validate the whole response before publishing its first file.
	want := map[string]string{"background_remove": ".png", "raster_to_vector": ".svg", "svg_create": ".svg", "svg_edit": ".svg", "pdf_to_text": ".txt", "handwriting_to_text": ".txt"}[operation]
	if operation == "image_transform" {
		format, _ := a.Options["format"].(string)
		if format == "" {
			format = "png"
		}
		want = "." + format
	}
	mimes := map[string]string{".png": "image/png", ".jpeg": "image/jpeg", ".webp": "image/webp", ".svg": "image/svg+xml", ".txt": "text/plain; charset=utf-8"}
	expected := 1
	if want == ".svg" {
		expected = 2
	}
	if len(processed.Outputs) != expected || processed.Status != "complete" && processed.Status != "partial" {
		return errorResult(ctx, "Invalid file service result."), m
	}
	for i, out := range processed.Outputs {
		ext, role := want, "result"
		if i == 1 {
			ext, role = ".png", "preview"
		}
		if out.Extension != ext || out.Role != role || out.MediaType != mimes[ext] || mimes[ext] == "" || len(out.Data) == 0 || len(out.Data) > maxImageBytes {
			return errorResult(ctx, "Invalid file service output."), m
		}
	}
	result := fileResult{SchemaVersion: 1, Status: processed.Status, Outputs: []fileArtifact{}, Warnings: processed.Warnings}
	if result.Warnings == nil {
		result.Warnings = []string{}
	}
	id, _ := uuid.NewV7()
	base := path.Join(instance.Current().Folders.Results, operation+"-"+id.String())
	partial := func(err error) (toolResult, callMeta) {
		m.status = http.StatusInternalServerError
		if len(result.Outputs) == 0 {
			return errorResult(ctx, "Could not save output: %s", err), m
		}
		result.Status = "partial"
		result.Warnings = append(result.Warnings, "Not all output files were saved: "+err.Error())
		return structuredResult(result), m
	}
	for _, out := range processed.Outputs {
		name := base
		if out.Role == "preview" {
			name += "-preview"
		}
		rel, err := s.Files.WriteNew(ctx, u.UID, project, name+out.Extension, bytes.NewReader(out.Data))
		if err != nil {
			return partial(err)
		}
		result.Outputs = append(result.Outputs, fileArtifact{Path: workspace + project + "/" + rel, MediaType: out.MediaType, Role: out.Role, SHA256: digest(out.Data)})
	}
	metadata := map[string]any{"schema_version": 1, "source": source, "source_sha256": sourceHash,
		"operation": operation, "processing": processed.Metadata, "options": a.Options, "outputs": result.Outputs, "warnings": result.Warnings, "status": result.Status}
	meta, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return partial(err)
	}
	rel, err := s.Files.WriteNew(ctx, u.UID, project, base+".metadata.json", bytes.NewReader(meta))
	if err != nil {
		return partial(err)
	}
	result.MetadataPath = workspace + project + "/" + rel
	m.status = http.StatusOK
	return structuredResult(result), m
}

func fileToolDefinition(name, description string, options map[string]any) map[string]any {
	props := map[string]any{}
	required := []string{}
	if name != "svg_create" {
		props["path"] = map[string]any{"type": "string", "maxLength": 1024, "description": "Source in this session's project; use a returned output path to chain operations."}
		required = append(required, "path")
	}
	if name == "svg_create" || name == "svg_edit" {
		props["svg"] = map[string]any{"type": "string", "maxLength": 512 << 10, "description": "Complete SVG markup with viewBox; static shapes, paths, text and gradients. No CSS, scripts, images, external references or use elements."}
		required = append(required, "svg")
	}
	if name == "svg_edit" {
		props["source_sha256"] = map[string]any{"type": "string", "pattern": "^[a-f0-9]{64}$", "description": "SHA256 of the current source bytes, as returned in outputs. Prevents edits based on a stale source."}
		required = append(required, "source_sha256")
	}
	if options != nil {
		props["options"] = map[string]any{"type": "object", "additionalProperties": false, "properties": options}
	}
	return map[string]any{"name": name, "title": name, "description": description + " Saves new project files and returns their paths, hashes, status and warnings. Originals remain unchanged. Maximum input: 20 MiB; processing: 90 seconds.",
		"annotations":  map[string]any{"readOnlyHint": false, "destructiveHint": false, "openWorldHint": false},
		"outputSchema": fileOutputSchema,
		"inputSchema":  map[string]any{"type": "object", "additionalProperties": false, "properties": props, "required": required}}
}

var fileTools = []map[string]any{
	fileToolDefinition("background_remove", "Removes a picture's background using foreground segmentation. Returns PNG with transparency. Check edges in the result.", nil),
	fileToolDefinition("raster_to_vector", "Traces a raster logo or illustration into editable SVG paths and a PNG preview. Maximum 4 megapixels. Not suitable for preserving photographic detail or editable lettering.", map[string]any{"mode": map[string]any{"type": "string", "enum": []string{"color", "binary"}}}),
	fileToolDefinition("svg_create", "Creates a vector graphic from SVG markup you author to match the user's request. Validates the static SVG and returns the SVG plus a rendered PNG preview.", nil),
	fileToolDefinition("svg_edit", "Edits an existing vector graphic using replacement SVG markup you author after reading the source. Requires its current SHA256. Returns a new SVG and preview.", nil),
	fileToolDefinition("image_transform", "Crops, resizes or converts a raster image (PNG, JPEG, WebP or GIF), not SVG. For SVG results, pass the PNG output with role=preview from svg_create, svg_edit or raster_to_vector. Crops before resizing. One dimension preserves aspect; two dimensions set exact size. JPEG refuses transparency.", map[string]any{
		"width": map[string]any{"type": "integer", "minimum": 1, "maximum": 10000}, "height": map[string]any{"type": "integer", "minimum": 1, "maximum": 10000},
		"crop":   map[string]any{"type": "array", "minItems": 4, "maxItems": 4, "items": map[string]any{"type": "integer", "minimum": 0}, "description": "left, top, right, bottom in source pixels after EXIF orientation"},
		"format": map[string]any{"type": "string", "enum": []string{"png", "jpeg", "webp"}}}),
	fileToolDefinition("pdf_to_text", "Extracts an existing PDF text layer to UTF-8 plain text with page mapping. NO OCR. Blank or scanned pages produce warnings or NO_TEXT_LAYER. Up to 100 selected pages per call.", map[string]any{
		"start_page": map[string]any{"type": "integer", "minimum": 1}, "end_page": map[string]any{"type": "integer", "minimum": 1}}),
}

var fileOutputSchema = map[string]any{
	"type": "object", "required": []string{"schema_version", "status", "outputs", "warnings"},
	"properties": map[string]any{
		"schema_version": map[string]any{"type": "integer", "const": 1},
		"status":         map[string]any{"type": "string", "enum": []string{"complete", "partial"}},
		"outputs": map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "object", "required": []string{"path", "media_type", "role", "sha256"}, "properties": map[string]any{
			"path": map[string]any{"type": "string"}, "media_type": map[string]any{"type": "string"}, "role": map[string]any{"type": "string"}, "sha256": map[string]any{"type": "string"}}}},
		"metadata_path": map[string]any{"type": "string"},
		"warnings":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	},
}
