---
name: alt-text
description: "Write alt texts and image descriptions for artworks, exhibition photos and graphics, factual and without interpretation, following WCAG and BITV 2.0. Load for alt text, image description, caption, accessibility, screen reader, Alt-Text, Alternativtext, Bildbeschreibung, Bildunterschrift, Barrierefreiheit, Screenreader."
metadata:
  workspace/version: "1.2.0"
  workspace/owner: "operations"
  workspace/license: "MIT"
  workspace/tier: "optional"
  workspace/title: "Image descriptions and alt text"
  workspace/title-de: "Bildbeschreibung und Alt-Text"
  workspace/description-de: "Alternativtexte und Bildbeschreibungen für Kunstwerke, Ausstellungsfotos und Grafiken schreiben, sachlich und ohne Deutung, nach WCAG und BITV 2.0."
  workspace/category: "images"
---
# Image description and alt text

## Basis
- WCAG 2.2, success criterion 1.1.1 "Non-text Content"; in Germany binding for public bodies via BITV 2.0 and EN 301 549.
- Useful for museums: "Cooper Hewitt Guidelines for Image Description". If needed, find and link it with `workspace_web_search`.

## Three kinds
1. **Alt text** (`alt` attribute on the web, alternative text in Word): about 15 words, most important first, no "image of". Example (German): „Raumhohe Projektion aus blauen Linien, davor drei Menschen als Silhouetten“.
2. **Caption (credit)**: from the work data, not from the image. Order as in the material, usually: name, *title*, year, technique, dimensions, collection or lender, photo: name, rights.
3. **Long description**: 80 to 250 words for website, audio guide or catalogue. From whole to detail: kind of work, overall impression, layout left to right or front to back, colours, materials, size relative to the body, text in the image verbatim.
- Purely decorative images get an empty `alt=""`; say so instead of inventing a text.

## Rules
- Describe what is visible. No interpretation, no judgement ("impressive"), no assumed intention of the artist. Interpretations only verbatim from the material, with source.
- People: visible features (posture, clothing, action). Skin colour, gender or age only if important for the work and clearly visible; no names without evidence.
- Video and installation: sequence in time order, sound named separately ("Sound: deep hum").
- Colours with simple names, with a comparison if needed ("red like a fire extinguisher").

## Procedure
- View the image with the tool `workspace_image_view`, never with `read`: `path` e.g. `inbox/x.jpg`, `question` asking for the kind wanted, e.g. "Describe the image factually from left to right, with colours, materials and every visible text verbatim, without interpretation." The tool downsizes itself; convert files over 20 MB or HEIC first: `python3 -c "from PIL import Image; i=Image.open('inbox/x.tif'); i.thumbnail((2000,2000)); i.convert('RGB').save('work/x.jpg')"`.
- If the person only pasted the image into the chat, ask them to upload it as a file; copy it to `inbox/`, then view it.
- Work data (title, year, technique, rights) only from files or from the person, never derived from the image. Missing: write `[fehlt]` (English: `[missing]`).
- Many images: table with skill `xlsx`, columns file, alt text, caption, long description, check. **Never overwrite**: `python3 /etc/opencode/skills/data/scripts/next_name.py results/bildtexte.xlsx`.

## Limits
- The description comes from the photo, not from the work on site. A photo can misrepresent size, material and sound. Someone who knows the work approves the texts; say so.
