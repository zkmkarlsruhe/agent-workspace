// SPDX-License-Identifier: MIT

package mcp

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// jpegWithOrientation encodes img and inserts an APP1 Exif segment with tag 0x0112 = o.
func jpegWithOrientation(t *testing.T, img image.Image, o uint16, bo binary.AppendByteOrder, mark string) []byte {
	var j bytes.Buffer
	if err := jpeg.Encode(&j, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	tiff := []byte(mark)
	tiff = bo.AppendUint16(tiff, 42)
	tiff = bo.AppendUint32(tiff, 8)
	tiff = bo.AppendUint16(tiff, 1)
	tiff = bo.AppendUint16(tiff, 0x0112)
	tiff = bo.AppendUint16(tiff, 3)
	tiff = bo.AppendUint32(tiff, 1)
	tiff = bo.AppendUint16(tiff, o)
	tiff = append(tiff, 0, 0, 0, 0, 0, 0)
	seg := append([]byte("Exif\x00\x00"), tiff...)
	app1 := []byte{0xFF, 0xE1, byte((len(seg) + 2) >> 8), byte(len(seg) + 2)}
	b := j.Bytes()
	return append(append(append([]byte{}, b[:2]...), append(app1, seg...)...), b[2:]...)
}

func TestImageOrientation(t *testing.T) {
	// 40x20, left half red, right half blue: landscape as stored
	src := image.NewRGBA(image.Rect(0, 0, 40, 20))
	for y := range 20 {
		for x := range 40 {
			c := color.RGBA{255, 0, 0, 255}
			if x >= 20 {
				c = color.RGBA{0, 0, 255, 255}
			}
			src.SetRGBA(x, y, c)
		}
	}
	red := func(c color.RGBA) bool { return c.R > 200 && c.B < 60 }
	for _, tc := range []struct {
		o      uint16
		bo     binary.AppendByteOrder
		mark   string
		w, h   int
		topRed bool // pixel near the top-left corner is red
	}{
		{1, binary.LittleEndian, "II", 40, 20, true},
		{3, binary.BigEndian, "MM", 40, 20, false},   // 180°: blue now left
		{6, binary.LittleEndian, "II", 20, 40, true}, // 90° cw: red half on top
		{8, binary.BigEndian, "MM", 20, 40, false},   // 90° ccw: blue half on top
	} {
		b := jpegWithOrientation(t, src, tc.o, tc.bo, tc.mark)
		if got := exifOrientation(b); got != int(tc.o) {
			t.Fatalf("orientation %d read as %d", tc.o, got)
		}
		img, err := imageScaled(b)
		if err != nil {
			t.Fatal(err)
		}
		if w, h := img.Bounds().Dx(), img.Bounds().Dy(); w != tc.w || h != tc.h {
			t.Fatalf("o=%d size %dx%d, want %dx%d", tc.o, w, h, tc.w, tc.h)
		}
		if got := red(img.RGBAAt(2, 2)); got != tc.topRed {
			t.Fatalf("o=%d top-left red=%v, want %v", tc.o, got, tc.topRed)
		}
	}
	if exifOrientation([]byte("not a jpeg")) != 1 {
		t.Fatal("non-JPEG must read as 1")
	}
}
