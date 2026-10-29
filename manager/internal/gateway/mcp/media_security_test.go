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
	"image/color"
	"image/gif"
	"image/png"
	"io"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/gateway/llm"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

type countedMediaFiles struct {
	Files
	reads atomic.Int64
}

func (f *countedMediaFiles) OpenFile(ctx context.Context, uid, project, rel string) (io.ReadCloser, lifecycle.File, error) {
	f.reads.Add(1)
	return f.Files.OpenFile(ctx, uid, project, rel)
}

func TestFileToolAdmissionBeforeSourceRead(t *testing.T) {
	s, f := mediaServer(t, fakeProcess)
	f.files["bericht/source.png"] = pngOf(2, 2)
	counted := &countedMediaFiles{Files: f}
	s.Files = counted
	s.Reserve = func(context.Context, string, float64) (func(), error) { return nil, llm.ErrBusy }
	res, meta := s.fileTool(context.Background(), store.User{UID: "u1"}, "ses_abcdefgh12", "image_transform", json.RawMessage(`{"path":"source.png"}`))
	if !res.IsError || meta.status != 429 || counted.reads.Load() != 0 {
		t.Fatalf("busy request read input: %+v reads=%d", res, counted.reads.Load())
	}
}

func TestImageCancellationBeforeRead(t *testing.T) {
	s, f := mediaServer(t, fakeProcess)
	s.CanEdit = true
	f.files["bericht/source.png"] = pngOf(2, 2)
	counted := &countedMediaFiles{Files: f}
	s.Files = counted
	var providerCalls atomic.Int64
	s.Generate = func(context.Context, store.User, string, string, string, []byte) ([]byte, string, error) {
		providerCalls.Add(1)
		return []byte(pngOf(2, 2)), "fixture", nil
	}
	s.Look = func(context.Context, store.User, string, string, []byte) (string, error) {
		providerCalls.Add(1)
		return "image", nil
	}
	defer SetImageDecodeParallel(4)
	synctest.Test(t, func(t *testing.T) {
		SetImageDecodeParallel(1)
		imageDecodeSlots <- struct{}{}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan toolResult, 8)
		for i := 0; i < 8; i++ {
			go func() {
				var res toolResult
				if i%2 == 0 {
					res, _ = s.imageGenerate(ctx, store.User{UID: "u1"}, "ses_abcdefgh12", json.RawMessage(`{"prompt":"edit","source_image":"source.png"}`))
				} else {
					res, _ = s.imageView(ctx, store.User{UID: "u1"}, "ses_abcdefgh12", json.RawMessage(`{"path":"source.png"}`))
				}
				done <- res
			}()
		}
		synctest.Wait()
		if counted.reads.Load() != 0 {
			t.Fatal("queued image requests read sources")
		}
		cancel()
		synctest.Wait()
		if len(done) != 8 {
			t.Fatalf("cancelled requests remain queued: %d", len(done))
		}
		for i := 0; i < 8; i++ {
			if res := <-done; !res.IsError {
				t.Fatal("cancelled request succeeded")
			}
		}
		if counted.reads.Load() != 0 || providerCalls.Load() != 0 {
			t.Fatal("cancelled requests performed work")
		}
		<-imageDecodeSlots
	})
}

func TestImageAdmissionDeadlineBeforeRead(t *testing.T) {
	s, f := mediaServer(t, fakeProcess)
	s.CanEdit = true
	counted := &countedMediaFiles{Files: f}
	s.Files = counted
	defer SetImageDecodeParallel(4)
	synctest.Test(t, func(t *testing.T) {
		SetImageDecodeParallel(1)
		imageDecodeSlots <- struct{}{}
		start := time.Now()
		res, _ := s.imageGenerate(context.Background(), store.User{UID: "u1"}, "ses_abcdefgh12", json.RawMessage(`{"prompt":"edit","source_image":"source.png"}`))
		if !res.IsError || time.Since(start) != 3*time.Minute || counted.reads.Load() != 0 {
			t.Fatalf("admission not deadline-bound: %+v elapsed=%v reads=%d", res, time.Since(start), counted.reads.Load())
		}
		<-imageDecodeSlots
	})
}

func TestImageCapacityHeldThroughResultSave(t *testing.T) {
	s, f := mediaServer(t, fakeProcess)
	defer SetImageDecodeParallel(4)
	SetImageDecodeParallel(1)
	s.Generate = func(context.Context, store.User, string, string, string, []byte) ([]byte, string, error) {
		if len(imageDecodeSlots) != 1 {
			t.Fatal("provider input not bounded by image capacity")
		}
		return []byte(pngOf(2, 2)), "fixture", nil
	}
	s.Files = slotCheckedFiles{Files: f, t: t}
	res, _ := s.imageGenerate(context.Background(), store.User{UID: "u1"}, "ses_abcdefgh12", json.RawMessage(`{"prompt":"create"}`))
	if res.IsError || len(imageDecodeSlots) != 0 {
		t.Fatalf("result or slot leak: %+v", res)
	}
}

type slotCheckedFiles struct {
	Files
	t *testing.T
}

func (f slotCheckedFiles) WriteNew(ctx context.Context, uid, project, rel string, r io.Reader) (string, error) {
	if len(imageDecodeSlots) != 1 {
		f.t.Fatal("image capacity released before result was saved")
	}
	return f.Files.WriteNew(ctx, uid, project, rel, r)
}

func TestImageProviderInvalidPNGNotPublished(t *testing.T) {
	s, f := mediaServer(t, fakeProcess)
	oversized := []byte(pngOf(2, 2))
	binary.BigEndian.PutUint32(oversized[16:20], 50000)
	binary.BigEndian.PutUint32(oversized[20:24], 50000)
	binary.BigEndian.PutUint32(oversized[29:33], crc32.ChecksumIEEE(oversized[12:29]))
	for name, data := range map[string][]byte{"header only": []byte(pngOf(2, 2))[:33], "oversized": oversized} {
		t.Run(name, func(t *testing.T) {
			s.Generate = func(context.Context, store.User, string, string, string, []byte) ([]byte, string, error) {
				return data, "fixture", nil
			}
			res, meta := s.imageGenerate(context.Background(), store.User{UID: "u1"}, "ses_abcdefgh12", json.RawMessage(`{"prompt":"create"}`))
			if !res.IsError || meta.status != 502 || len(f.written) != 0 {
				t.Fatalf("invalid output published: %+v writes=%d", res, len(f.written))
			}
		})
	}
	if _, err := aiPNG(oversized, "test", "test"); !errors.Is(err, errImagePixels) {
		t.Fatalf("oversized PNG: %v", err)
	}
	src := []byte(pngOf(2, 2))
	var tagged bytes.Buffer
	tagged.Write(src[:33])
	pngChunk(&tagged, "tEXt", "Keep\x00value")
	tagged.Write(src[33:])
	const label = "KI-generiert mit fixture"
	out, err := aiPNG(tagged.Bytes(), label, "trainedAlgorithmicMedia")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("Keep\x00value")) {
		t.Fatal("upstream metadata retained")
	}
	for _, want := range []string{"Software\x00", "Comment\x00\x00\x00\x00\x00" + label,
		"XML:com.adobe.xmp\x00", "digitalsourcetype/trainedAlgorithmicMedia", label + "</rdf:li>"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("generated metadata missing: %q", want)
		}
	}
	if _, err := png.Decode(bytes.NewReader(out)); err != nil {
		t.Fatal(err)
	}
}

func TestImageViewSessionProjectBoundary(t *testing.T) {
	s, f := mediaServer(t, fakeProcess)
	f.files["other/private.png"] = pngOf(2, 2)
	f.files["bericht/source.png"] = pngOf(2, 2)
	calls := 0
	s.Look = func(context.Context, store.User, string, string, []byte) (string, error) {
		calls++
		return "image", nil
	}
	for _, test := range []struct{ session, path string }{
		{"ses_abcdefgh12", "/workspace/other/private.png"}, {"", "/workspace/bericht/source.png"}, {"ses_missing12", "/workspace/bericht/source.png"},
	} {
		raw, _ := json.Marshal(map[string]string{"path": test.path})
		res, _ := s.imageView(context.Background(), store.User{UID: "u1"}, test.session, raw)
		if !res.IsError {
			t.Fatalf("foreign/unbound source allowed: %+v", test)
		}
	}
	if calls != 0 {
		t.Fatal("refused source reached provider")
	}
	res, _ := s.imageView(context.Background(), store.User{UID: "u1"}, "ses_abcdefgh12", json.RawMessage(`{"path":"/workspace/bericht/source.png"}`))
	if res.IsError || calls != 1 {
		t.Fatalf("same-project source rejected: %+v", res)
	}
}

func TestImageEditGIFFrames(t *testing.T) {
	s, f := mediaServer(t, fakeProcess)
	s.CanEdit = true
	calls := 0
	s.Generate = func(context.Context, store.User, string, string, string, []byte) ([]byte, string, error) {
		calls++
		return []byte(pngOf(2, 2)), "fixture", nil
	}

	palette := color.Palette{color.Black, color.White}
	frame := image.NewPaletted(image.Rect(0, 0, 2, 2), palette)
	for _, n := range []int{1, 2, 100} {
		frames := make([]*image.Paletted, n)
		for i := range frames {
			frames[i] = frame
		}
		var data bytes.Buffer
		if err := gif.EncodeAll(&data, &gif.GIF{Image: frames, Delay: make([]int, n)}); err != nil {
			t.Fatal(err)
		}
		f.files["bericht/input.gif"] = data.String()
		res, _ := s.imageGenerate(context.Background(), store.User{UID: "u1"}, "ses_abcdefgh12", json.RawMessage(`{"prompt":"edit","source_image":"input.gif"}`))
		if res.IsError != (n > 1) || calls != 1 {
			t.Fatalf("%d frames: %+v provider calls=%d", n, res, calls)
		}
		if n == 1 {
			truncated := data.Bytes()[:data.Len()-1]
			if _, err := imagePrepared(truncated, 0, false); err == nil {
				t.Fatal("missing GIF trailer accepted")
			}
			extended := append(bytes.Clone(truncated), 0x21, 0xfe, 2, 0x2c, 0x3b, 0, 0x3b)
			if _, err := imagePrepared(extended, 0, false); err != nil {
				t.Fatal(err)
			}
			for _, tail := range [][]byte{{0x21}, {0x21, 0xfe, 4, 1}, {0x10, 0x3b}} {
				malformed := append(bytes.Clone(truncated), tail...)
				if _, err := imagePrepared(malformed, 0, false); err == nil {
					t.Fatalf("malformed GIF tail accepted: %x", tail)
				}
			}
		}
	}
}

func TestImageCancellationAfterProviderDoesNotPublish(t *testing.T) {
	s, f := mediaServer(t, fakeProcess)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Generate = func(context.Context, store.User, string, string, string, []byte) ([]byte, string, error) {
		cancel()
		return []byte(pngOf(2, 2)), "fixture", nil
	}
	res, _ := s.imageGenerate(ctx, store.User{UID: "u1"}, "ses_abcdefgh12", json.RawMessage(`{"prompt":"create"}`))
	if !res.IsError || len(f.written) != 0 || len(imageDecodeSlots) != 0 {
		t.Fatalf("cancelled result published or slot leaked: %+v writes=%d", res, len(f.written))
	}
}
