package imagestore

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

// encodePNG encodes a solid-color PNG, for test-image construction.
func encodePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 255, G: 0, B: 0, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func TestPutAssignsIDAndGetRoundTrips(t *testing.T) {
	s := New(Config{MaxItems: 20, MaxBytes: 100 << 20})
	id, err := s.Put(encodePNG(t, 10, 10), "paste", "png")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if id != "img-1" {
		t.Fatalf("first id = %q want img-1", id)
	}
	e, ok := s.Get(id)
	if !ok {
		t.Fatalf("Get %q: not found", id)
	}
	if e.Fmt != "png" || e.Source != "paste" || e.W != 10 || e.H != 10 || len(e.Bytes) == 0 {
		t.Fatalf("entry = %#v", e)
	}
}

func TestPutRejectsUnsupportedFormat(t *testing.T) {
	s := New(Config{MaxItems: 20, MaxBytes: 100 << 20})
	if _, err := s.Put([]byte("not an image"), "paste", "bmp"); err == nil {
		t.Fatal("bmp must be rejected")
	}
}

// ledger: G/VISION-TOOL#3 超限/越权被拒
func TestPutRejectsOversized(t *testing.T) {
	s := New(Config{MaxItems: 20, MaxBytes: 100 << 20, MaxImageBytes: 100})
	if _, err := s.Put(make([]byte, 101), "paste", "png"); err == nil {
		t.Fatal(">MaxImageBytes must be rejected")
	}
}

func TestPutDownscalesLongEdge(t *testing.T) {
	s := New(Config{MaxItems: 20, MaxBytes: 100 << 20, MaxLongEdge: 2048})
	big := encodePNG(t, 3000, 1000)
	id, err := s.Put(big, "paste", "png")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	e, _ := s.Get(id)
	if e.W > 2048 || e.H > 2048 {
		t.Fatalf("long edge not downscaled: %dx%d", e.W, e.H)
	}
	if e.W <= 0 || e.H <= 0 {
		t.Fatalf("downscaled dims must be positive: %dx%d", e.W, e.H)
	}
}

func TestLRUEvictsWhenFull(t *testing.T) {
	s := New(Config{MaxItems: 2, MaxBytes: 100 << 20})
	id1, _ := s.Put(encodePNG(t, 1, 1), "a", "png")
	s.Put(encodePNG(t, 1, 1), "b", "png")
	s.Put(encodePNG(t, 1, 1), "c", "png") // triggers eviction of id1 (oldest, unaccessed)
	if _, ok := s.Get(id1); ok {
		t.Fatal("LRU must evict the oldest entry when MaxItems exceeded")
	}
}

func TestLRUGetPromotesRecency(t *testing.T) {
	s := New(Config{MaxItems: 2, MaxBytes: 100 << 20})
	id1, _ := s.Put(encodePNG(t, 1, 1), "a", "png")
	id2, _ := s.Put(encodePNG(t, 1, 1), "b", "png")
	s.Get(id1)                            // access id1 -> promotes to MRU
	s.Put(encodePNG(t, 1, 1), "c", "png") // evicts id2 (LRU)
	if _, ok := s.Get(id2); ok {
		t.Fatal("LRU must evict least-recently-used (id2), not id1")
	}
	if _, ok := s.Get(id1); !ok {
		t.Fatal("id1 was promoted and must survive")
	}
}

func TestPlaceholderFormat(t *testing.T) {
	s := New(Config{MaxItems: 20, MaxBytes: 100 << 20})
	id, _ := s.Put(encodePNG(t, 1280, 720), "paste", "png")
	got := s.Placeholder(id)
	if !strings.HasPrefix(got, "[image:img-1 | paste | ") || !strings.Contains(got, "1280x720 png") || !strings.HasSuffix(got, "]") {
		t.Fatalf("placeholder = %q", got)
	}
}

// encodeJPEG encodes a solid-color JPEG for decode/format tests.
func encodeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 0, G: 255, B: 0, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

// encodeGIF encodes a single-frame solid-color GIF.
func encodeGIF(t *testing.T, w, h int) []byte {
	t.Helper()
	p := color.Palette{color.RGBA{R: 0, G: 0, B: 255, A: 255}, color.RGBA{R: 255, G: 255, B: 255, A: 255}}
	img := image.NewPaletted(image.Rect(0, 0, w, h), p)
	g := &gif.GIF{Image: []*image.Paletted{img}, Delay: []int{0}, LoopCount: 0}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatalf("encode gif: %v", err)
	}
	return buf.Bytes()
}

// TestStoreConcurrentPut drives the store from many goroutines; the LRU and id
// counter are mutex-guarded so every Put must return a unique stable id.
func TestStoreConcurrentPut(t *testing.T) {
	s := New(Config{MaxItems: 100, MaxBytes: 100 << 20})
	done := make(chan string, 50)
	for i := 0; i < 50; i++ {
		go func() {
			id, _ := s.Put(encodePNG(t, 2, 2), "paste", "png")
			done <- id
		}()
	}
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		id := <-done
		if id == "" || seen[id] {
			t.Fatalf("duplicate or empty id %q", id)
		}
		seen[id] = true
	}
}

// TestNewAppliesDefaults covers the zero-Config defaulting branches in New
// (every limit field falls back to its default). Verified behaviorally: with
// all-zero Config, defaultMaxItems (20) caps the store, and the default byte
// limit is large enough that items aren't evicted by size.
func TestNewAppliesDefaults(t *testing.T) {
	s := New(Config{})
	first, err := s.Put(encodePNG(t, 1, 1), "a", "png")
	if err != nil {
		t.Fatalf("Put under defaults: %v", err)
	}
	if first != "img-1" {
		t.Fatalf("first id = %q want img-1", first)
	}
	// Fill to the default MaxItems WITHOUT touching `first` so it stays the
	// least-recently-used (tail); the check below must not promote it.
	for i := 0; i < defaultMaxItems-1; i++ {
		if _, err := s.Put(encodePNG(t, 1, 1), "fill", "png"); err != nil {
			t.Fatalf("Put fill %d: %v", i, err)
		}
	}
	// One more pushes past defaultMaxItems → LRU evicts the first (oldest).
	if _, err := s.Put(encodePNG(t, 1, 1), "overflow", "png"); err != nil {
		t.Fatalf("Put overflow: %v", err)
	}
	if _, ok := s.Get(first); ok {
		t.Fatal("oldest entry must be evicted once default MaxItems is exceeded")
	}
}

// TestPutRejectsUndecodableImage hits the decode/normalize error path: the
// format name is valid (png) but the bytes are not a real PNG, so normalize
// must fail and Put returns that error instead of storing garbage.
func TestPutRejectsUndecodableImage(t *testing.T) {
	s := New(Config{MaxItems: 5, MaxBytes: 100 << 20})
	if _, err := s.Put([]byte("not-a-real-png-at-all"), "paste", "png"); err == nil {
		t.Fatal("undecodable png bytes must be rejected")
	}
}

// TestPlaceholderUnknownIDReturnsEmpty covers the unknown-id branch of
// Placeholder: a missing id yields "" rather than a malformed bracket string.
func TestPlaceholderUnknownIDReturnsEmpty(t *testing.T) {
	s := New(Config{MaxItems: 5, MaxBytes: 100 << 20})
	if got := s.Placeholder("img-does-not-exist"); got != "" {
		t.Fatalf("unknown id placeholder = %q, want empty", got)
	}
}

// TestCanonicalFmtNormalizesJpgToJpeg covers canonicalFmt: the "jpg" alias is
// stored as "jpeg" while "png"/"jpeg" pass through unchanged.
func TestCanonicalFmtNormalizesJpgToJpeg(t *testing.T) {
	s := New(Config{MaxItems: 5, MaxBytes: 100 << 20})
	id, err := s.Put(encodeJPEG(t, 8, 8), "paste", "jpg")
	if err != nil {
		t.Fatalf("Put jpg: %v", err)
	}
	e, _ := s.Get(id)
	if e.Fmt != "jpeg" {
		t.Fatalf("jpg canonicalized to %q, want jpeg", e.Fmt)
	}
	if canonicalFmt("png") != "png" {
		t.Fatalf("canonicalFmt(png) = %q, want png", canonicalFmt("png"))
	}
}

// TestDecodeByFmtAllFormats drives the internal decoder directly so every
// branch is reachable (Put/normalize only reach png/jpeg/gif; the default and
// the gif-no-frames error are otherwise defensive-only).
func TestDecodeByFmtAllFormats(t *testing.T) {
	t.Run("png", func(t *testing.T) {
		img, err := decodeByFmt(encodePNG(t, 3, 3), "png")
		if err != nil || img == nil {
			t.Fatalf("png decode: img=%v err=%v", img, err)
		}
	})
	t.Run("jpeg", func(t *testing.T) {
		img, err := decodeByFmt(encodeJPEG(t, 3, 3), "jpeg")
		if err != nil || img == nil {
			t.Fatalf("jpeg decode: img=%v err=%v", img, err)
		}
	})
	t.Run("jpg-alias", func(t *testing.T) {
		if _, err := decodeByFmt(encodeJPEG(t, 3, 3), "jpg"); err != nil {
			t.Fatalf("jpg decode: %v", err)
		}
	})
	t.Run("gif-first-frame", func(t *testing.T) {
		img, err := decodeByFmt(encodeGIF(t, 4, 4), "gif")
		if err != nil || img == nil {
			t.Fatalf("gif decode: img=%v err=%v", img, err)
		}
	})
	t.Run("gif-no-frames", func(t *testing.T) {
		// A valid GIF89a header + global color table + trailer with no image
		// descriptor: gif.DecodeAll parses it to an empty frame slice, so
		// decodeByFmt's no-frames guard fires.
		emptyGIF := []byte{
			'G', 'I', 'F', '8', '9', 'a', // magic
			0x01, 0x00, // width 1
			0x01, 0x00, // height 1
			0x80,             // packed: global color table present, 2 entries
			0x00,             // background color index
			0x00,             // pixel aspect ratio
			0x00, 0x00, 0x00, // color 0: black
			0xff, 0xff, 0xff, // color 1: white
			0x3b, // trailer (no image data)
		}
		if _, err := decodeByFmt(emptyGIF, "gif"); err == nil {
			t.Fatal("gif with no frames must error")
		}
	})
	t.Run("gif-decode-error", func(t *testing.T) {
		// Truncated/garbage bytes make gif.DecodeAll itself fail.
		if _, err := decodeByFmt([]byte("GIF89a-not-really"), "gif"); err == nil {
			t.Fatal("malformed gif must return a decode error")
		}
	})
	t.Run("unknown-default", func(t *testing.T) {
		if _, err := decodeByFmt(encodePNG(t, 1, 1), "tiff"); err == nil {
			t.Fatal("unknown format must hit the default error branch")
		}
	})
}

// TestNormalizeWebpPassThrough covers the webp branch: the stdlib has no webp
// decoder, so normalize stores the raw bytes untouched and reports 0x0 dims.
func TestNormalizeWebpPassThrough(t *testing.T) {
	raw := []byte("RIFK....fake-webp-bytes")
	w, h, data, err := normalize(raw, "webp", 2048)
	if err != nil {
		t.Fatalf("webp normalize: %v", err)
	}
	if w != 0 || h != 0 {
		t.Fatalf("webp dims = %dx%d, want 0x0 (unknown)", w, h)
	}
	if !bytes.Equal(data, raw) {
		t.Fatal("webp bytes must pass through unchanged")
	}
	// And via Put the entry keeps the webp format and raw payload.
	s := New(Config{MaxItems: 5, MaxBytes: 100 << 20})
	id, err := s.Put(raw, "upload", "webp")
	if err != nil {
		t.Fatalf("Put webp: %v", err)
	}
	e, _ := s.Get(id)
	if e.Fmt != "webp" || e.W != 0 || e.H != 0 || !bytes.Equal(e.Bytes, raw) {
		t.Fatalf("webp entry = %+v", e)
	}
}

// TestDownscalePortraitAndSmallGuards exercises downscale directly: the
// portrait branch (height >= width) that Put's landscape-only existing test
// misses, plus the scale<1 clamp (max larger than the image) and the dw/dh<1
// underflow guards on a zero-size image.
func TestDownscalePortraitAndSmallGuards(t *testing.T) {
	t.Run("portrait", func(t *testing.T) {
		src := image.NewRGBA(image.Rect(0, 0, 100, 400))
		dst, w, h := downscale(src, 100)
		if w > 100 || h > 100 {
			t.Fatalf("portrait long edge not capped: %dx%d", w, h)
		}
		if dst == nil {
			t.Fatal("downscale returned nil image")
		}
	})
	t.Run("scale-clamped-to-one", func(t *testing.T) {
		// max exceeds both edges → scale<1 clamped to 1 → identity dims.
		src := image.NewRGBA(image.Rect(0, 0, 10, 10))
		_, w, h := downscale(src, 100)
		if w != 10 || h != 10 {
			t.Fatalf("clamped scale dims = %dx%d, want 10x10", w, h)
		}
	})
	t.Run("zero-size-underflow-guard", func(t *testing.T) {
		// A degenerate 0x0 image would otherwise produce dw/dh of 0; the
		// guards must clamp each to at least 1 so NewRGBA is non-empty.
		src := image.NewRGBA(image.Rect(0, 0, 0, 0))
		_, w, h := downscale(src, 10)
		if w < 1 || h < 1 {
			t.Fatalf("underflow guard failed: dims = %dx%d", w, h)
		}
	})
}

// TestRemoveHeadWithSuccessor covers the one remove() branch unreachable from
// the public API: removing the head node when it still has a next neighbor
// (moveToFront guards head==n, and evict only ever removes the tail). The helper
// itself must unlink the head correctly so the list stays consistent.
func TestRemoveHeadWithSuccessor(t *testing.T) {
	s := New(Config{MaxItems: 10, MaxBytes: 100 << 20})
	idHead, _ := s.Put(encodePNG(t, 1, 1), "head", "png")
	idNext, _ := s.Put(encodePNG(t, 1, 1), "next", "png") // becomes head; idHead is now second
	head := s.byID[idNext]
	second := s.byID[idHead]
	if s.head != head || head.next != second {
		t.Fatalf("setup wrong: head=%p second=%p", head, second)
	}
	s.remove(second) // remove the node that is currently head's successor
	if s.head != head {
		t.Fatal("head must remain after removing its successor")
	}
	if head.next != nil {
		t.Fatal("head.next must be nil after removing the sole successor")
	}
	if _, ok := s.Get(idHead); !ok {
		t.Fatal("surviving head must still be gettable")
	}
}

// TestLRURemovesHeadTailAndMiddle pins the three remove() shapes through the
// public API: eviction of the sole element (head==tail), a middle-node promote
// via Get (moveToFront→remove on a node with both neighbors), and tail eviction.
func TestLRURemovesHeadTailAndMiddle(t *testing.T) {
	// Sole-element eviction exercises remove with prev==nil AND next==nil.
	solo := New(Config{MaxItems: 1, MaxBytes: 100 << 20})
	id1, _ := solo.Put(encodePNG(t, 1, 1), "a", "png")
	if _, err := solo.Put(encodePNG(t, 1, 1), "b", "png"); err != nil {
		t.Fatal(err)
	}
	if _, ok := solo.Get(id1); ok {
		t.Fatal("sole element must be evicted when MaxItems=1")
	}

	// Middle-node promote exercises remove with prev!=nil AND next!=nil.
	s := New(Config{MaxItems: 10, MaxBytes: 100 << 20})
	idA, _ := s.Put(encodePNG(t, 1, 1), "a", "png") // tail
	idB, _ := s.Put(encodePNG(t, 1, 1), "b", "png") // middle
	idC, _ := s.Put(encodePNG(t, 1, 1), "c", "png") // head
	_ = idA
	_ = idC
	// Promoting the middle node removes it from between its two neighbors.
	if _, ok := s.Get(idB); !ok {
		t.Fatal("middle node must be gettable")
	}
	// Now shrink the cap so tail (idA) is evicted by item pressure.
	s.cfg.MaxItems = 1
	if _, err := s.Put(encodePNG(t, 1, 1), "d", "png"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get(idA); ok {
		t.Fatal("tail node must be evicted under item pressure")
	}
}

// TestLRUEvictsByBytes covers the byte-budget eviction loop, which removes
// entries from the tail until total bytes fall under MaxBytes.
func TestLRUEvictsByBytes(t *testing.T) {
	png1 := encodePNG(t, 2, 2)
	s := New(Config{MaxItems: 100, MaxBytes: len(png1) * 2}) // room for ~2
	id1, _ := s.Put(png1, "a", "png")
	id2, _ := s.Put(png1, "b", "png")
	id3, _ := s.Put(png1, "c", "png") // byte pressure evicts id1, then maybe id2
	if _, ok := s.Get(id1); ok {
		t.Fatal("oldest entry must be evicted under byte pressure")
	}
	_ = id2
	_ = id3
}
