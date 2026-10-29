# SPDX-License-Identifier: MIT

import base64
import io
import unittest
from types import SimpleNamespace
from unittest.mock import patch

from PIL import Image
from pypdf import PdfWriter
from pypdf.generic import DictionaryObject, NameObject, DecodedStreamObject

from worker import Refused, execute, png, svg

SVG = b'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 50"><rect width="100" height="50" fill="#345678"/></svg>'


def call(operation, data, **options):
    return execute({"operation": operation, "data": base64.b64encode(data).decode(), "options": options})


def output(result, index=0):
    return base64.b64decode(result["outputs"][index]["data"])


def document():
    writer = PdfWriter()
    page = writer.add_blank_page(200, 200)
    font = DictionaryObject({NameObject("/Type"): NameObject("/Font"), NameObject("/Subtype"): NameObject("/Type1"), NameObject("/BaseFont"): NameObject("/Helvetica")})
    page[NameObject("/Resources")] = DictionaryObject({NameObject("/Font"): DictionaryObject({NameObject("/F1"): writer._add_object(font)})})
    stream = DecodedStreamObject()
    stream.set_data(b"BT /F1 12 Tf 10 150 Td (Invoice 42) Tj ET")
    page[NameObject("/Contents")] = writer._add_object(stream)
    writer.add_blank_page(200, 200)
    data = io.BytesIO()
    writer.write(data)
    return data.getvalue()


class WorkerTests(unittest.TestCase):
    def test_cutout_preserves_existing_alpha_once(self):
        def remove(image, session):
            self.assertEqual(image.mode, "RGB")
            return image.convert("RGBA")

        fake = SimpleNamespace(new_session=lambda *args, **kwargs: None, remove=remove)
        with patch.dict("sys.modules", rembg=fake):
            result = call("background_remove", png(Image.new("RGBA", (5, 5), (200, 0, 0, 128))))
        self.assertEqual(Image.open(io.BytesIO(output(result))).getpixel((2, 2))[3], 128)

    def test_svg_render_and_reedit(self):
        result = call("svg_create", SVG)
        self.assertEqual(Image.open(io.BytesIO(output(result, 1))).size, (1024, 512))
        changed = output(result).replace(b"#345678", b"#ff0000")
        edited = call("svg_edit", changed)
        self.assertEqual(Image.open(io.BytesIO(output(edited, 1))).convert("RGB").getpixel((50, 50)), (255, 0, 0))

    def test_reject_active_svg(self):
        cases = [b'<script>alert(1)</script>', b'<image href="file:///etc/passwd"/>',
                 b'<use href="#a"/>', b'<foreignObject/>', b'<style>@import "https://example.org";</style>',
                 b'<rect width="10" height="10" onload="alert(1)"/>',
                 b'<rect width="10" height="10" fill="url(https://example.org/a)"/>',
                 b'<rect width="10" height="10" style="fill:red"/>']
        for case in cases:
            with self.subTest(case=case), self.assertRaises(Refused):
                svg(SVG.replace(b'<rect width="100" height="50" fill="#345678"/>', case))
        with self.assertRaises(Exception):
            svg(b'<!DOCTYPE svg [<!ENTITY x SYSTEM "file:///etc/passwd">]>' + SVG)

    def test_huge_and_invalid_svg_dimensions(self):
        for viewbox in [b"0 0 nan 10", b"0 0 100001 10", b"0 0 -1 10"]:
            with self.assertRaises(Refused):
                svg(SVG.replace(b"0 0 100 50", viewbox))
        # Huge intrinsic dimensions cannot force a huge render allocation.
        result = call("svg_create", SVG.replace(b"viewBox", b'width="999999999" height="999999999" viewBox'))
        self.assertLess(len(output(result, 1)), 100000)

    def test_alpha_crop_resize_convert(self):
        image = Image.new("RGBA", (100, 60), (200, 0, 0, 100))
        result = call("image_transform", png(image), crop=[0, 0, 80, 40], width=40)
        edited = Image.open(io.BytesIO(output(result)))
        self.assertEqual(edited.size, (40, 20))
        self.assertEqual(edited.getpixel((5, 5))[3], 100)
        with self.assertRaises(Refused):
            call("image_transform", png(image), format="jpeg")
        with self.assertRaises(Refused):
            call("image_transform", png(image), crop=[0, 0, 101, 40])
        for options in [{"width":True}, {"height":0}, {"width":10000,"height":10000}]:
            with self.assertRaises(Refused):
                call("image_transform", png(image), **options)

    def test_trace_real_paths(self):
        image = Image.new("RGBA", (60, 40), (0, 0, 0, 0))
        image.paste((255, 0, 0, 255), (10, 10, 50, 30))
        result = call("raster_to_vector", png(image))
        self.assertIn(b"<path", output(result))
        self.assertNotIn(b"<image", output(result))
        self.assertEqual(result["metadata"]["width"], 60)

    def test_pdf_text_partial_and_page_range(self):
        result = call("pdf_to_text", document())
        self.assertEqual(result["status"], "partial")
        self.assertIn(b"Invoice 42", output(result))
        self.assertFalse(result["metadata"]["ocr"])
        self.assertFalse(result["metadata"]["pages"][1]["has_text"])
        self.assertEqual(call("pdf_to_text", document(), start_page=1, end_page=1)["status"], "complete")
        with self.assertRaisesRegex(Refused, "NO_TEXT_LAYER"):
            call("pdf_to_text", document(), start_page=2, end_page=2)


class HandwritingTests(unittest.TestCase):
    def test_pdf_explicit_page_and_rendering(self):
        with self.assertRaises(Refused):
            call('handwriting_prepare', document(), source_type='pdf')
        result = call('handwriting_prepare', document(), source_type='pdf', page=2)
        self.assertEqual(result['metadata']['document_pages'], 2)
        self.assertEqual(result['metadata']['page'], 2)
        self.assertEqual(result['metadata']['render_dpi'], 192)
        image = Image.open(io.BytesIO(output(result)))
        self.assertEqual(image.size, (534, 534))
        self.assertEqual(image.getpixel((10, 10)), (255, 255, 255))
        for page in (0, 3, True, 1.5):
            with self.assertRaises(Refused):
                call('handwriting_prepare', document(), source_type='pdf', page=page)

    def test_image_full_resolution_white_alpha(self):
        image = Image.new('RGBA', (2000, 20), (0, 0, 0, 0))
        result = call('handwriting_prepare', png(image), source_type='image')
        prepared = Image.open(io.BytesIO(output(result)))
        self.assertEqual(prepared.size, (2000, 20))
        self.assertEqual(prepared.getpixel((0, 0)), (255, 255, 255))
        self.assertEqual(result['metadata']['page'], 1)
        with self.assertRaises(Refused):
            call('handwriting_prepare', png(image), source_type='image', page=2)
        with self.assertRaises(Refused):
            call('handwriting_prepare', png(image), source_type='image', url='https://example.test')

    def test_oversized_pdf_refused_before_render(self):
        writer = PdfWriter()
        writer.add_blank_page(100000, 100000)
        data = io.BytesIO(); writer.write(data)
        with self.assertRaises(Refused):
            call('handwriting_prepare', data.getvalue(), source_type='pdf', page=1)


if __name__ == "__main__":
    unittest.main()
