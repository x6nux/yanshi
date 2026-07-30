// Package imagestore is the session-level, in-memory image store for Tier G
// multimodal. It mints stable ids (img-N) for images that arrive via any of the
// five entry points, enforces format/size limits, downsamples oversized long
// edges, and evicts least-recently-used entries under a combined item/byte cap.
//
// It is deliberately dependency-free (pure stdlib decode + box-filter downscale)
// so it can be reused by the orchestrator (placeholder path) and the
// image_describe tool without pulling model or transport packages.
package imagestore

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"sync"
	"time"
)

// supportedFmts lists the mutable/identifiable formats. webp is not in stdlib;
// we detect it by extension + pass-through bytes, but skip decode/downscale.
var supportedFmts = map[string]bool{"png": true, "jpeg": true, "jpg": true, "gif": true, "webp": true}

// Config tames the store limits. Zero MaxImageBytes disables the per-image byte
// cap; zero MaxLongEdge disables downscaling. MaxItems/MaxBytes drive LRU.
type Config struct {
	MaxItems      int
	MaxBytes      int
	MaxImageBytes int
	MaxLongEdge   int
}

// Defaults applied by New when a field is zero.
const (
	defaultMaxItems      = 20
	defaultMaxBytes      = 100 << 20 // 100 MiB
	defaultMaxImageBytes = 10 << 20  // 10 MiB
	defaultMaxLongEdge   = 2048
)

// Entry is one stored image.
type Entry struct {
	ID      string
	Source  string
	Fmt     string
	W, H    int
	Bytes   []byte
	Created time.Time
}

// Store is a concurrency-safe LRU image store keyed by stable id.
type Store struct {
	mu         sync.Mutex
	cfg        Config
	next       int
	byID       map[string]*entryNode
	head, tail *entryNode // MRU=head, LRU=tail
	bytes      int
}

type entryNode struct {
	entry      Entry
	prev, next *entryNode
}

// New builds a Store with the given limits (zero fields fall back to defaults).
func New(cfg Config) *Store {
	if cfg.MaxItems == 0 {
		cfg.MaxItems = defaultMaxItems
	}
	if cfg.MaxBytes == 0 {
		cfg.MaxBytes = defaultMaxBytes
	}
	if cfg.MaxImageBytes == 0 {
		cfg.MaxImageBytes = defaultMaxImageBytes
	}
	if cfg.MaxLongEdge == 0 {
		cfg.MaxLongEdge = defaultMaxLongEdge
	}
	return &Store{cfg: cfg, byID: make(map[string]*entryNode)}
}

// Put validates + (if needed) downscales bytes, assigns the next stable id, and
// inserts the entry as most-recently-used. Returns the id or an error describing
// why the image was rejected (format/size). Evictions keep both the item count
// and total byte count within the configured caps.
func (s *Store) Put(raw []byte, source, fmtName string) (string, error) {
	fmtName = strings.ToLower(strings.TrimSpace(fmtName))
	if !supportedFmts[fmtName] {
		return "", fmt.Errorf("imagestore: unsupported format %q (want png/jpeg/webp/gif)", fmtName)
	}
	if len(raw) > s.cfg.MaxImageBytes {
		return "", fmt.Errorf("imagestore: image %d bytes exceeds %d byte limit", len(raw), s.cfg.MaxImageBytes)
	}
	w, h, data, err := normalize(raw, fmtName, s.cfg.MaxLongEdge)
	if err != nil {
		return "", fmt.Errorf("imagestore: decode/normalize: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	id := fmt.Sprintf("img-%d", s.next)
	node := &entryNode{entry: Entry{
		ID: id, Source: source, Fmt: canonicalFmt(fmtName),
		W: w, H: h, Bytes: data, Created: time.Now(),
	}}
	s.pushFront(node)
	s.byID[id] = node
	s.bytes += len(data)
	s.evict()
	return id, nil
}

// Get returns the entry by id and promotes it to most-recently-used.
func (s *Store) Get(id string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	node, ok := s.byID[id]
	if !ok {
		return Entry{}, false
	}
	s.moveToFront(node)
	return node.entry, true
}

// Placeholder renders the stable placeholder text the non-multimodal path inserts
// into the user message: [image:<id> | <source> | <W>x<H> <fmt>]. Unknown id -> "".
func (s *Store) Placeholder(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	node, ok := s.byID[id]
	if !ok {
		return ""
	}
	e := node.entry
	return fmt.Sprintf("[image:%s | %s | %dx%d %s]", e.ID, e.Source, e.W, e.H, e.Fmt)
}

// --- LRU helpers ---

func (s *Store) pushFront(n *entryNode) {
	n.prev, n.next = nil, s.head
	if s.head != nil {
		s.head.prev = n
	}
	s.head = n
	if s.tail == nil {
		s.tail = n
	}
}

func (s *Store) remove(n *entryNode) {
	if n.prev != nil {
		n.prev.next = n.next
	} else {
		s.head = n.next
	}
	if n.next != nil {
		n.next.prev = n.prev
	} else {
		s.tail = n.prev
	}
	n.prev, n.next = nil, nil
}

func (s *Store) moveToFront(n *entryNode) {
	if s.head == n {
		return
	}
	s.remove(n)
	s.pushFront(n)
}

func (s *Store) evict() {
	for (len(s.byID) > s.cfg.MaxItems || s.bytes > s.cfg.MaxBytes) && s.tail != nil {
		victim := s.tail
		s.remove(victim)
		delete(s.byID, victim.entry.ID)
		s.bytes -= len(victim.entry.Bytes)
	}
}

func canonicalFmt(f string) string {
	if f == "jpg" {
		return "jpeg"
	}
	return f
}

// normalize decodes raw (gif->first frame; webp->pass-through bytes, no decode),
// downsamples when the long edge exceeds maxLongEdge via a box filter, and
// re-encodes to PNG so the stored bytes are a self-contained raster. Returns
// (width, height, bytes, err). webp returns the original dims as 0x0 (unknown)
// since the stdlib cannot decode it.
func normalize(raw []byte, fmtName string, maxLongEdge int) (int, int, []byte, error) {
	if fmtName == "webp" {
		return 0, 0, raw, nil // stdlib has no webp decoder; store raw
	}
	img, err := decodeByFmt(raw, fmtName)
	if err != nil {
		return 0, 0, nil, err
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if maxLongEdge > 0 && (w > maxLongEdge || h > maxLongEdge) {
		img, w, h = downscale(img, maxLongEdge)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return 0, 0, nil, err
	}
	return w, h, buf.Bytes(), nil
}

func decodeByFmt(raw []byte, fmtName string) (image.Image, error) {
	switch fmtName {
	case "png":
		return png.Decode(bytes.NewReader(raw))
	case "jpeg", "jpg":
		return jpeg.Decode(bytes.NewReader(raw))
	case "gif":
		g, err := gif.DecodeAll(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		if len(g.Image) == 0 {
			return nil, errors.New("gif has no frames")
		}
		return g.Image[0], nil // first frame
	default:
		return nil, fmt.Errorf("unsupported format %q", fmtName)
	}
}

// downscale uses nearest-neighbor point sampling so the long edge <= max.
// Does NOT introduce golang.org/x/image. src bounds known.
func downscale(src image.Image, max int) (image.Image, int, int) {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	scale := 1.0
	if sw >= sh {
		scale = float64(sw) / float64(max)
	} else {
		scale = float64(sh) / float64(max)
	}
	if scale < 1 {
		scale = 1
	}
	dw, dh := int(float64(sw)/scale), int(float64(sh)/scale)
	if dw < 1 {
		dw = 1
	}
	if dh < 1 {
		dh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	boxW := float64(sw) / float64(dw)
	boxH := float64(sh) / float64(dh)
	for dy := 0; dy < dh; dy++ {
		for dx := 0; dx < dw; dx++ {
			x0 := int(float64(dx) * boxW)
			y0 := int(float64(dy) * boxH)
			dst.Set(dx, dy, src.At(b.Min.X+x0, b.Min.Y+y0))
		}
	}
	return dst, dw, dh
}
