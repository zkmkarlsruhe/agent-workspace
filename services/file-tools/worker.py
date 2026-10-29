# SPDX-License-Identifier: MIT

"""Bounded file transforms. JSON on stdin/stdout; no paths or URLs from callers."""

import base64
import hashlib
import importlib.metadata
import io
import json
import math
import re
import sys
import xml.etree.ElementTree as ET

from defusedxml import ElementTree as SafeET
from PIL import Image, ImageChops, ImageOps

MAX_BYTES = 20 << 20
MAX_PIXELS = 25_000_000
MAX_SVG = 512 << 10
Image.MAX_IMAGE_PIXELS = MAX_PIXELS
NS = "http://www.w3.org/2000/svg"
ET.register_namespace("", NS)


class Refused(ValueError):
    pass


def integer(value, low, high, name):
    if type(value) is not int or not low <= value <= high:
        raise Refused(f"{name} must be an integer between {low} and {high}.")
    return value


def raster(data):
    image = Image.open(io.BytesIO(data))
    if image.width * image.height > MAX_PIXELS:
        raise Refused("Image exceeds 25 megapixels.")
    if getattr(image, "n_frames", 1) != 1:
        raise Refused("Animated images are not supported; select a frame first.")
    return ImageOps.exif_transpose(image).convert("RGBA")


def png(image):
    out = io.BytesIO()
    image.save(out, format="PNG")
    return out.getvalue()


def artifact(data, extension, media_type, role="result"):
    if len(data) > MAX_BYTES:
        raise Refused("Output exceeds 20 MiB; reduce the requested size or page range.")
    return {"data": base64.b64encode(data).decode(), "extension": extension,
            "media_type": media_type, "role": role}


# A deliberately static subset: no CSS, links, images, scripts, animation or use recursion.
ELEMENTS = set("svg g defs path rect circle ellipse line polyline polygon text tspan title desc linearGradient radialGradient stop clipPath".split())
ATTRIBUTES = set("id version width height viewBox preserveAspectRatio x y x1 y1 x2 y2 cx cy r rx ry d points transform fill fill-opacity fill-rule stroke stroke-width stroke-opacity stroke-linecap stroke-linejoin stroke-miterlimit stroke-dasharray stroke-dashoffset opacity clip-path clip-rule font-family font-size font-weight font-style text-anchor dominant-baseline dx dy offset stop-color stop-opacity gradientUnits gradientTransform spreadMethod fx fy".split())


def svg(data):
    if len(data) > MAX_SVG:
        raise Refused("SVG exceeds 512 KiB.")
    root = SafeET.fromstring(data, forbid_dtd=True, forbid_entities=True, forbid_external=True)
    if root.tag not in ("svg", f"{{{NS}}}svg"):
        raise Refused("Expected an SVG root element.")
    ids, references, shapes = set(), set(), 0
    nodes = list(root.iter())
    if len(nodes) > 10000:
        raise Refused("SVG exceeds 10000 elements.")
    for node in nodes:
        tag = node.tag.removeprefix(f"{{{NS}}}")
        if tag not in ELEMENTS or (node is not root and tag == "svg"):
            raise Refused(f"Unsupported SVG element: {tag}. Use static vector shapes.")
        node.tag = f"{{{NS}}}{tag}"
        shapes += tag in {"path", "rect", "circle", "ellipse", "line", "polyline", "polygon", "text"}
        for key, value in node.attrib.items():
            if key not in ATTRIBUTES or "\\" in value or any(c in value for c in "<>@"):
                raise Refused(f"Unsupported SVG attribute: {key}.")
            if key == "id":
                if not re.fullmatch(r"[A-Za-z_][\w.-]{0,100}", value) or value in ids:
                    raise Refused("SVG IDs must be unique simple names.")
                ids.add(value)
            if "url" in value.lower():
                match = re.fullmatch(r"url\(#([A-Za-z_][\w.-]{0,100})\)", value)
                if key not in {"fill", "stroke", "clip-path"} or not match:
                    raise Refused("Only local paint and clip references are supported.")
                references.add(match[1])
            if len(value) > 100000:
                raise Refused("SVG attribute exceeds the size limit.")
    if not shapes or not references <= ids:
        raise Refused("SVG needs vector shapes and valid local references.")
    # Avoid cyclic clipping; gradients cannot reference other gradients in this subset.
    for clip in root.iter(f"{{{NS}}}clipPath"):
        if any("clip-path" in node.attrib for node in clip.iter()):
            raise Refused("Nested clipping references are not supported.")
    viewbox = root.get("viewBox", "").replace(",", " ").split()
    if len(viewbox) != 4:
        raise Refused("SVG requires viewBox with four numbers.")
    try:
        x, y, w, h = map(float, viewbox)
    except ValueError as exc:
        raise Refused("Invalid SVG viewBox.") from exc
    if not all(math.isfinite(v) and abs(v) <= 100000 for v in (x, y, w, h)) or min(w, h) <= 0:
        raise Refused("Invalid SVG viewBox dimensions.")
    # Fix intrinsic dimensions to the viewBox, preventing unbounded renderer allocation.
    root.set("width", str(w))
    root.set("height", str(h))
    clean = ET.tostring(root, encoding="utf-8", xml_declaration=True)
    import cairosvg

    preview = cairosvg.svg2png(bytestring=clean, output_width=max(1, round(1024 * w / max(w, h))),
                              output_height=max(1, round(1024 * h / max(w, h))))
    return clean, preview, {"vector_elements": shapes, "width": w, "height": h}


def execute(request):
    operation = request["operation"]
    data = base64.b64decode(request.get("data", ""), validate=True)
    if len(data) > MAX_BYTES:
        raise Refused("Input exceeds 20 MiB.")
    options = request.get("options", {})
    allowed = {"image_transform": {"crop", "width", "height", "format"},
               "raster_to_vector": {"mode"}, "pdf_to_text": {"start_page", "end_page"},
               "handwriting_prepare": {"page", "source_type"}}
    if not isinstance(options, dict) or set(options) - allowed.get(operation, set()):
        raise Refused("Unknown options for this operation.")
    warnings, details, outputs = [], {}, []
    status = "complete"
    engine = ""
    if operation in {"svg_create", "svg_edit"}:
        clean, preview, details = svg(data)
        outputs = [artifact(clean, ".svg", "image/svg+xml"), artifact(preview, ".png", "image/png", "preview")]
        engine = "CairoSVG"
    elif operation == "raster_to_vector":
        import vtracer

        image = raster(data)
        if image.width * image.height > 4_000_000:
            raise Refused("Vectorization accepts at most 4 megapixels. Resize explicitly first.")
        mode = options.get("mode", "color")
        if mode not in {"color", "binary"}:
            raise Refused("mode must be color or binary.")
        config = vtracer.Config(clustering="bw" if mode == "binary" else "color-cluster", mode="spline")
        text = config.convert_bytes(png(image))
        # VTracer uses pixel dimensions without a viewBox.
        root = SafeET.fromstring(text)
        root.set("viewBox", f"0 0 {image.width} {image.height}")
        clean, preview, details = svg(ET.tostring(root))
        outputs = [artifact(clean, ".svg", "image/svg+xml"), artifact(preview, ".png", "image/png", "preview")]
        warnings.append("Tracing approximates the source. Compare the preview; lettering becomes paths, not editable text.")
        engine = "vtracer"
    elif operation == "background_remove":
        from rembg import new_session, remove

        image = raster(data)
        session = new_session("birefnet-general-lite", providers=["CPUExecutionProvider"])
        # Apply the original alpha once; rembg also multiplies alpha for RGBA input.
        result = remove(image.convert("RGB"), session=session).convert("RGBA")
        result.putalpha(ImageChops.multiply(result.getchannel("A"), image.getchannel("A")))
        outputs = [artifact(png(result), ".png", "image/png")]
        details = {"model": "birefnet-general-lite", "width": result.width, "height": result.height}
        alpha_min, alpha_max = result.getchannel("A").getextrema()
        details.update(alpha_min=alpha_min, alpha_max=alpha_max)
        if alpha_min == 255:
            warnings.append("No transparent pixels were produced. Check the foreground selection.")
        if alpha_max == 0:
            warnings.append("No visible foreground was produced. Check the source and segmentation.")
        engine = "rembg"
    elif operation == "image_transform":
        image = raster(data)
        if "crop" in options:
            crop = options["crop"]
            if not isinstance(crop, list) or len(crop) != 4:
                raise Refused("crop must contain left, top, right, bottom.")
            left, top, right, bottom = [integer(v, 0, 100000, "crop coordinate") for v in crop]
            if not (left < right <= image.width and top < bottom <= image.height):
                raise Refused("Crop is outside the image.")
            image = image.crop((left, top, right, bottom))
        if "width" in options or "height" in options:
            for name in ("width", "height"):
                if name in options:
                    integer(options[name], 1, 10000, name)
            w = options.get("width") or max(1, round(image.width * options["height"] / image.height))
            h = options.get("height") or max(1, round(image.height * w / image.width))
            if w * h > MAX_PIXELS:
                raise Refused("Output exceeds 25 megapixels.")
            image = image.resize((w, h), Image.Resampling.LANCZOS)
        fmt = options.get("format", "png")
        if fmt not in {"png", "jpeg", "webp"}:
            raise Refused("format must be png, jpeg or webp.")
        if fmt == "jpeg":
            if image.getchannel("A").getextrema()[0] < 255:
                raise Refused("JPEG cannot preserve transparency. Use PNG or WebP.")
            image = image.convert("RGB")
        out = io.BytesIO()
        image.save(out, format=fmt.upper(), **({"quality": 95} if fmt != "png" else {}))
        outputs = [artifact(out.getvalue(), "." + fmt, "image/" + fmt)]
        details = {"width": image.width, "height": image.height}
        engine = "Pillow"
    elif operation == "handwriting_prepare":
        import pypdfium2 as pdfium

        kind = options.get("source_type")
        if kind == "pdf":
            with pdfium.PdfDocument(data) as document:
                count = len(document)
                if not 1 <= count <= 1000:
                    raise Refused("PDF must contain 1 to 1000 pages; split larger documents first.")
                if "page" not in options:
                    raise Refused(f"Select an explicit page (1 to {count}); one page per call.")
                number = integer(options["page"], 1, count, "page")
                page = document[number - 1]
                try:
                    width, height = page.get_size()
                    w, h = math.ceil(width * 192 / 72), math.ceil(height * 192 / 72)
                    if min(w, h) <= 0 or w * h > MAX_PIXELS:
                        raise Refused("Rendered page exceeds 25 megapixels; crop or split explicitly.")
                    bitmap = page.render(scale=192 / 72)
                    try:
                        image = bitmap.to_pil().convert("RGBA")
                    finally:
                        bitmap.close()
                finally:
                    page.close()
            details = {"page": number, "document_pages": count, "render_dpi": 192}
        elif kind == "image":
            integer(options.get("page", 1), 1, 1, "page")
            image = raster(data)
            details = {"page": 1, "document_pages": 1, "exif_orientation_applied": True}
        else:
            raise Refused("source_type must be image or pdf.")
        background = Image.new("RGBA", image.size, "white")
        background.alpha_composite(image)
        prepared = png(background.convert("RGB"))
        outputs = [artifact(prepared, ".png", "image/png")]
        details.update(width=image.width, height=image.height,
                       source_type=kind, prepared_sha256=hashlib.sha256(prepared).hexdigest())
        engine = "pypdfium2" if kind == "pdf" else "Pillow"
    elif operation == "pdf_to_text":
        from pypdf import PdfReader

        reader = PdfReader(io.BytesIO(data))
        if reader.is_encrypted:
            raise Refused("Encrypted PDFs are not supported. Upload a decrypted copy.")
        count = len(reader.pages)
        if count > 1000:
            raise Refused("PDF exceeds 1000 pages. Split the document first.")
        start = integer(options.get("start_page", 1), 1, count, "start_page")
        end = integer(options.get("end_page", min(count, start + 99)), start, count, "end_page")
        if "end_page" not in options and count - start + 1 > 100:
            raise Refused("Select a range of at most 100 pages; this document needs multiple calls.")
        if end - start + 1 > 100:
            raise Refused("Select at most 100 pages.")
        chunks, pages, offset = [], [], 0
        for number in range(start, end + 1):
            page = reader.pages[number - 1]
            text = page.extract_text(extraction_mode="layout") or ""
            if not text.strip():
                warnings.append(f"Page {number} has no extractable text. It may be blank or require OCR; OCR was not run.")
                status = "partial"
            chunk = text + ("\n\f\n" if number < end else "")
            pages.append({"page": number, "start_char": offset, "end_char": offset + len(text), "has_text": bool(text.strip())})
            offset += len(chunk)
            chunks.append(chunk)
        if not any(p["has_text"] for p in pages):
            raise Refused("NO_TEXT_LAYER: No text extracted. Pages are blank or require OCR; use handwriting_to_text when offered.")
        outputs = [artifact("".join(chunks).encode(), ".txt", "text/plain; charset=utf-8")]
        details = {"pages": pages, "document_pages": count, "ocr": False}
        engine = "pypdf"
    else:
        raise Refused("Unknown operation.")
    return {"status": status, "outputs": outputs, "warnings": warnings,
            "metadata": {"operation": operation, "input_sha256": hashlib.sha256(data).hexdigest(),
                         "engine": engine, "version": importlib.metadata.version(engine), **details}}


if __name__ == "__main__":
    try:
        result = execute(json.load(sys.stdin))
    except Exception as exc:
        result = {"error": str(exc)[:500], "status": "error"}
    json.dump(result, sys.stdout, ensure_ascii=False)
