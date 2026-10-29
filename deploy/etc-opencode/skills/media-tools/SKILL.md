---
name: media-tools
description: "Create and edit images or SVG logos, remove backgrounds, trace raster graphics, crop and resize. Load for Bild, Logo, SVG, Vektor, freistellen, vectorize, background removal."
metadata:
  workspace/version: "1.0.0"
  workspace/owner: "operations"
  workspace/license: "MIT"
  workspace/title: "Images and vectors"
  workspace/title-de: "Bilder und Vektoren"
  workspace/description-de: "Einzelwerkzeuge für Bilder und editierbare Vektorgrafiken verketten."
---
# Images and vectors

Use the offered MCP tools directly. Tool names carry the configured server prefix.
Their outputs are project files, usable as the next tool's input. Use the returned
path, not a guessed filename. Never overwrite the source.

- `image_generate`: new raster image from a description.
- `image_edit`: change image content; pass `source_image` and the requested change.
- `background_remove`: foreground segmentation with an alpha output.
- `image_transform`: exact crop, resize or format conversion. One dimension preserves
  aspect ratio; two set exact dimensions. JPEG cannot preserve transparency.
  Input must be PNG, JPEG, WebP or GIF. For an SVG result, use its returned PNG
  output with `role: preview`; do not pass the `.svg` path to this raster tool.
- `raster_to_vector`: trace an existing logo or illustration into SVG paths.
  It approximates detail and turns lettering into paths, not editable text.
- `svg_create`: author static SVG markup matching the request, then validate and save
  it through this tool. This is the direct route for geometric logos and diagrams.
- `svg_edit`: read the existing SVG, change its markup, and pass the complete new
  SVG plus `source_sha256` from its previous tool result. For uploaded SVGs, compute
  the SHA256 of the original bytes. A changed source must be read again.

SVG requires `viewBox`. Use shapes, paths, groups, text, gradients and local clipping.
No embedded images, stylesheets, scripts, `use`, animation or external resources.
Translate simple style declarations into presentation attributes. Unsupported
features are rejected, not silently removed. Keep unrequested shapes and labels.

Check `status`, `warnings` and saved files before continuing. SVG operations return
a PNG preview: inspect it with the offered image viewer. Check real paths, lettering,
clipping, proportions and colors. For cutouts inspect the edges and transparency.
File validity alone does not prove visual quality.

Read metadata only when needed; do not paste base64 images or entire documents into
the conversation. A timeout ends the local file worker. Image provider timeouts can
leave an uncertain remote outcome: do not blindly repeat a paid request.

Example: trace a supplied logo, inspect the SVG preview, then change the requested
color with `svg_edit`. Do not add background removal or raster generation unless
the source or the requested result needs it.
