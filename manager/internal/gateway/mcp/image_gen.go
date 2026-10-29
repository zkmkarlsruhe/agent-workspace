// SPDX-License-Identifier: MIT

package mcp

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"image"
	"image/png"
	"maps"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/gateway/llm"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// Generate makes one image for the person (llm.Gateway.Image over the
// configured models, budget held there); sourceImage set = edit that PNG.
type Generate func(ctx context.Context, u store.User, session, prompt, size string, sourceImage []byte) (img []byte, model string, err error)

const maxPrompt = 4000

var imageAspects = map[string]string{"square": "1024x1024", "landscape": "1536x1024", "portrait": "1024x1536"}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// kiName is the file name of a generated image: the wish as a slug, always
// ending in -ki (labelling duty, EU AI Act Art. 50).
func aiFileName(wish string, now time.Time) string {
	wish = strings.ToLower(strings.TrimSpace(wish))
	wish = strings.TrimSuffix(wish, ".png")
	wish = strings.NewReplacer("ä", "ae", "ö", "oe", "ü", "ue", "ß", "ss").Replace(wish)
	slug := strings.Trim(nonSlug.ReplaceAllString(wish, "-"), "-")
	if len(slug) > 60 {
		slug = strings.TrimRight(slug[:60], "-")
	}
	slug = strings.TrimSuffix(slug, "-ki")
	if slug == "" {
		slug = "bild-" + now.Format("2006-01-02-1504")
	}
	return slug + "-ki.png"
}

func (s *Server) imageGenerate(ctx context.Context, u store.User, session string, raw json.RawMessage) (toolResult, callMeta) {
	m := callMeta{capability: "image", target: "image_generate", status: http.StatusBadRequest}
	var a struct {
		Prompt      string `json:"prompt"`
		Filename    string `json:"filename"`
		Format      string `json:"aspect"`
		SourceImage string `json:"source_image"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || strings.TrimSpace(a.Prompt) == "" || len(a.Prompt) > maxPrompt || len(a.Filename) > 200 {
		return errorResult(ctx, "Invalid parameters for image_generate: prompt is missing or longer than 4000 characters."), m
	}
	if a.Format == "" {
		a.Format = "landscape"
	}
	size := imageAspects[a.Format]
	if size == "" {
		return errorResult(ctx, "aspect must be square, landscape or portrait."), m
	}
	if session == "" {
		return errorResult(ctx, "image_generate needs a session in a project."), m
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	release, err := acquireImageSlot(ctx)
	if err != nil {
		return errorResult(ctx, "Image processing cancelled: %s", err), m
	}
	defer release()
	dir, err := s.Files.SessionDir(ctx, u.UID, session)
	project, ok := strings.CutPrefix(dir, workspace)
	if err != nil || !ok || !lifecycle.ValidProject(project) {
		s.Log.WarnContext(ctx, "image_generate: session dir", "uid", u.UID, "err", err)
		return errorResult(ctx, "The project folder of this session is unknown."), m
	}
	var sourceImage []byte
	source := "trainedAlgorithmicMedia"
	if strings.TrimSpace(a.SourceImage) != "" {
		if !s.CanEdit {
			return errorResult(ctx, "Editing an existing image is not available right now, only creating new images. Leave out source_image."), m
		}
		raw, _, _, err := s.projectInput(ctx, u, session, a.SourceImage, imageExts...)
		if err != nil {
			return errorResult(ctx, "Cannot read source: %s", err), m
		}
		m.bytesIn = len(raw)
		if err := ctx.Err(); err != nil {
			return errorResult(ctx, "Image processing cancelled: %s", err), m
		}
		img, err := imagePrepared(raw, 0, false)
		if errors.Is(err, errAnimatedGIF) {
			return errorResult(ctx, "Animated GIF edits are not supported; select a frame first."), m
		}
		if err != nil {
			return errorResult(ctx, "Cannot decode edit source: %s", err), m
		}
		if err := ctx.Err(); err != nil {
			return errorResult(ctx, "Image processing cancelled: %s", err), m
		}
		var b bytes.Buffer
		if err := png.Encode(&b, img); err != nil || b.Len() > maxImageBytes {
			return errorResult(ctx, "Encoded edit source exceeds 20 MiB or could not be encoded."), m
		}
		sourceImage, source = b.Bytes(), "compositeWithTrainedAlgorithmicMedia"
	}
	if err := ctx.Err(); err != nil {
		return errorResult(ctx, "Image processing cancelled: %s", err), m
	}
	out, model, err := s.Generate(ctx, u, session, strings.TrimSpace(a.Prompt), size, sourceImage)
	switch {
	case errors.Is(err, llm.ErrDayCap):
		m.status = http.StatusTooManyRequests
		return errorResult(ctx, "Daily budget reached, it continues tomorrow."), m
	case errors.Is(err, llm.ErrBusy):
		m.status = http.StatusTooManyRequests
		return errorResult(ctx, "Too many requests at once, please wait a moment."), m
	case errors.Is(err, llm.ErrImageUncertain):
		m.status = http.StatusBadGateway
		return errorResult(ctx, "IMAGE_OUTCOME_UNKNOWN: The provider may have processed this request. No fallback was started. Do not repeat it automatically."), m
	case err != nil:
		s.Log.WarnContext(ctx, "image_generate: generate", "uid", u.UID, "err", err)
		m.status = http.StatusBadGateway
		return errorResult(ctx, "The image model is not reachable right now or refused the description. Try again later or change the description."), m
	}
	if err := ctx.Err(); err != nil {
		return errorResult(ctx, "Image processing cancelled: %s", err), m
	}
	m.target = model
	now := time.Now()
	labelled, err := aiPNG(out, msg(ctx, "AI-generated with %s, [[name]], %s", model, now.Format("2006-01-02")), source)
	if err != nil || len(labelled) > maxImageBytes {
		s.Log.WarnContext(ctx, "image_generate: answer", "uid", u.UID, "model", model, "bytes", len(out), "err", err)
		m.status = http.StatusBadGateway
		return errorResult(ctx, "The image model returned no usable image. Try again later."), m
	}
	if err := ctx.Err(); err != nil {
		return errorResult(ctx, "Image processing cancelled: %s", err), m
	}
	rel, err := s.Files.WriteNew(ctx, u.UID, project, instance.Current().Folders.Results+"/"+aiFileName(a.Filename, now), bytes.NewReader(labelled))
	if err != nil {
		return s.fileError(ctx, err, &m), m
	}
	m.status = http.StatusOK
	full := workspace + project + "/" + rel
	return toolResult{
		Content: textResult(ctx, "Image created: %s (%s in the project, model %s). It is AI-generated: label it as AI-generated when passing it on or publishing it (EU AI Act Art. 50), e.g. with \"AI-generated with %s\". The label is also in the file's metadata. Download it in the portal under the project.",
			full, rel, model, model),
		Structured: map[string]any{"schema_version": 1, "status": "complete", "outputs": []fileArtifact{{Path: full, MediaType: "image/png", Role: "result", SHA256: digest(labelled)}}, "warnings": []string{}, "image": full, "file": rel, "model": model, "bytes": len(labelled)},
	}, m
}

// kiPNG returns img as PNG with the labelling text chunks right after IHDR:
// Software, Comment and XMP with the IPTC DigitalSourceType. Other formats
// are re-encoded too; upstream ancillary chunks are discarded.
func aiPNG(img []byte, comment, sourceType string) ([]byte, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(img))
	if err != nil {
		return nil, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxImagePixels/cfg.Height {
		return nil, errImagePixels
	}
	decoded, _, err := image.Decode(bytes.NewReader(img))
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := png.Encode(&b, decoded); err != nil {
		return nil, err
	}
	img = b.Bytes()
	const ihdrEnd = 8 + 8 + 13 + 4 // signature, IHDR length+type, IHDR data, CRC
	xmp := `<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">` +
		`<rdf:Description xmlns:Iptc4xmpExt="http://iptc.org/std/Iptc4xmpExt/2008-02-29/" xmlns:dc="http://purl.org/dc/elements/1.1/" ` +
		`Iptc4xmpExt:DigitalSourceType="http://cv.iptc.org/newscodes/digitalsourcetype/` + sourceType + `">` +
		`<dc:description><rdf:Alt><rdf:li xml:lang="x-default">` + xmlEscape(comment) + `</rdf:li></rdf:Alt></dc:description>` +
		`</rdf:Description></rdf:RDF></x:xmpmeta>`
	var out bytes.Buffer
	out.Write(img[:ihdrEnd])
	pngChunk(&out, "tEXt", "Software\x00"+instance.Value("name"))
	pngChunk(&out, "iTXt", "Comment\x00\x00\x00\x00\x00"+comment)
	pngChunk(&out, "iTXt", "XML:com.adobe.xmp\x00\x00\x00\x00\x00"+xmp)
	out.Write(img[ihdrEnd:])
	return out.Bytes(), nil
}

func pngChunk(w *bytes.Buffer, typ, data string) {
	binary.Write(w, binary.BigEndian, uint32(len(data)))
	crc := crc32.NewIEEE()
	crc.Write([]byte(typ + data))
	w.WriteString(typ + data)
	binary.Write(w, binary.BigEndian, crc.Sum32())
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

// imageGenTool lists source_image only when an edit model is configured.
func imageGenTool(canEdit bool) map[string]any {
	t := maps.Clone(imageGenDef)
	if !canEdit {
		schema := maps.Clone(t["inputSchema"].(map[string]any))
		props := maps.Clone(schema["properties"].(map[string]any))
		delete(props, "source_image")
		schema["properties"], t["inputSchema"] = props, schema
	}
	return t
}

var imageGenDef = map[string]any{
	"name":  "image_generate",
	"title": "Create image",
	"description": "Creates an image from a description with an AI image model and saves it as PNG in [[folders.results]]/ (file name ends in -ki, labelled as AI-generated in the metadata). " +
		"Only when the person explicitly asks. Do not depict real, recognisable persons unless the person explicitly asks, and no third-party logos or brands. Make the description concrete: subject, style, colours, light, viewpoint; English often gives better results.",
	"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": false, "openWorldHint": true},
	"inputSchema": map[string]any{
		"type":     "object",
		"required": []string{"prompt"},
		"properties": map[string]any{
			"prompt":       map[string]any{"type": "string", "minLength": 1, "maxLength": maxPrompt, "description": "What the image should show, concretely with subject, style, colours and light"},
			"filename":     map[string]any{"type": "string", "maxLength": 200, "description": "short name without extension, e.g. poster-summer-party; -ki and .png are added; empty = bild-<date>"},
			"aspect":       map[string]any{"type": "string", "enum": []string{"square", "landscape", "portrait"}, "default": "landscape"},
			"source_image": map[string]any{"type": "string", "maxLength": 1024, "description": "Image in this session's project as the starting image, changed according to the description, e.g. [[folders.inbox]]/photo.jpg; empty = new image. Animated GIFs are not supported."},
		},
	},
}

func imageEditTool() map[string]any {
	t := imageGenTool(true)
	t["name"], t["title"] = "image_edit", "Edit image"
	t["description"] = "Changes the content of a source image according to a prompt. Preserves the source; saves a new PNG and returns its path. Use image_transform for exact crops, resizing and conversion. The edit provider controls the output dimensions. Animated GIFs are refused; select a frame first."
	schema := maps.Clone(t["inputSchema"].(map[string]any))
	schema["required"] = []string{"prompt", "source_image"}
	t["inputSchema"] = schema
	return t
}
