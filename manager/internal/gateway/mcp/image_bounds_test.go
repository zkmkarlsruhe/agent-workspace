// SPDX-License-Identifier: MIT

package mcp

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/gif"
	"testing"
	"testing/synctest"
)

func TestImageDecodeParallel(t *testing.T) {
	defer SetImageDecodeParallel(4)
	src := []byte(pngOf(1, 1))
	synctest.Test(t, func(t *testing.T) {
		SetImageDecodeParallel(1)
		imageDecodeSlots <- struct{}{}
		done := make(chan error, 1)
		go func() {
			release, err := acquireImageSlot(context.Background())
			if err == nil {
				_, err = imageScaled(src)
				release()
			}
			done <- err
		}()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("decode bypassed occupied semaphore")
		default:
		}
		<-imageDecodeSlots
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if len(imageDecodeSlots) != 0 {
			t.Fatal("decode leaked slot")
		}
		if _, err := imageScaled(src[:33]); err == nil {
			t.Fatal("truncated PNG accepted")
		}
		if len(imageDecodeSlots) != 0 {
			t.Fatal("failed decode leaked slot")
		}
		SetImageDecodeParallel(0)
		if imageDecodeSlots != nil {
			t.Fatal("zero did not disable semaphore")
		}
		if _, err := imageScaled(src); err != nil {
			t.Fatal(err)
		}
	})
}

func TestImagePixelLimit(t *testing.T) {
	var src bytes.Buffer
	if err := gif.Encode(&src, image.NewGray(image.Rect(0, 0, 1, 1)), nil); err != nil {
		t.Fatal(err)
	}
	b := src.Bytes()
	binary.LittleEndian.PutUint16(b[6:8], 5001)
	binary.LittleEndian.PutUint16(b[8:10], 5000)
	if _, err := imageScaled(b); !errors.Is(err, errImagePixels) {
		t.Fatalf("image above 25 MP: %v", err)
	}
}

func TestImageAnswerBounds(t *testing.T) {
	var src bytes.Buffer
	if err := gif.Encode(&src, image.NewGray(image.Rect(0, 0, 1, 1)), nil); err != nil {
		t.Fatal(err)
	}
	b := bytes.Clone(src.Bytes())
	binary.LittleEndian.PutUint16(b[6:8], 7000)
	binary.LittleEndian.PutUint16(b[8:10], 7000)
	if _, err := aiPNG(b, "test", "test"); !errors.Is(err, errImagePixels) {
		t.Fatalf("49 MP model answer: %v", err)
	}
	for name, data := range map[string][]byte{
		"header only":  []byte(pngOf(2, 2))[:33],
		"missing IEND": []byte(pngOf(2, 2))[:len(pngOf(2, 2))-12],
		"broken IDAT":  func() []byte { b := []byte(pngOf(2, 2)); b[41] ^= 0xff; return b }(),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := aiPNG(data, "test", "test"); err == nil {
				t.Fatal("invalid PNG accepted")
			}
		})
	}
}
