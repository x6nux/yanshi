package clipimg

import (
	"context"
	"testing"
)

func TestReadReturnsFalseWhenBackendReportsNoImage(t *testing.T) {
	r := NewWithBackend(stubBackend{ok: false})
	if _, _, ok := r.Read(context.Background()); ok {
		t.Fatal("Read must return ok=false when the backend reports no image")
	}
}

func TestReadReturnsBytesWhenBackendHasImage(t *testing.T) {
	r := NewWithBackend(stubBackend{ok: true, data: []byte{1, 2, 3}, fmt: "png"})
	data, fmtName, ok := r.Read(context.Background())
	if !ok || fmtName != "png" || len(data) != 3 {
		t.Fatalf("Read = %d bytes, %q, ok=%v", len(data), fmtName, ok)
	}
}

// TestNewReturnsReaderWithPlatformBackend covers the production constructor: it
// must return a non-nil Reader whose backend is the platform implementation. We
// do NOT call Read on it here because that would hit the real OS clipboard
// (PowerShell on Windows) and is therefore integration-only.
func TestNewReturnsReaderWithPlatformBackend(t *testing.T) {
	r := New()
	if r == nil || r.backend == nil {
		t.Fatal("New must return a Reader with a non-nil platform backend")
	}
}

// TestReadFormatsPropagateThroughReader confirms the format string reported by
// the backend is what the Reader surfaces verbatim (the cross-platform seam
// must not rewrite it), for both png and the empty-no-image case.
func TestReadFormatsPropagateThroughReader(t *testing.T) {
	for _, tc := range []struct {
		name string
		stub stubBackend
	}{
		{"png", stubBackend{ok: true, data: []byte{9, 9}, fmt: "png"}},
		{"none", stubBackend{ok: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewWithBackend(tc.stub)
			data, fmtName, ok := r.Read(context.Background())
			if ok != tc.stub.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.stub.ok)
			}
			if ok && fmtName != "png" {
				t.Fatalf("fmt = %q, want png", fmtName)
			}
			if ok && len(data) != len(tc.stub.data) {
				t.Fatalf("data len = %d, want %d", len(data), len(tc.stub.data))
			}
		})
	}
}

// TestDefaultRunnerDelegatesToExec exercises the production default of the
// commandOutput seam: it must run the named subprocess and return its stdout.
// Using `go env GOOS` keeps the test cross-platform and dependency-free (the
// test binary itself runs under `go test`, so `go` is always on PATH). This is
// NOT a clipboard integration test — it only verifies the seam delegates to
// exec.CommandContext; the clipboard-specific PowerShell/osascript logic lives
// in platformReader.ReadImage and is covered by the platform test files.
func TestDefaultRunnerDelegatesToExec(t *testing.T) {
	out, err := commandOutput(context.Background(), "go", "env", "GOOS")
	if err != nil {
		t.Fatalf("default seam failed to run `go env GOOS`: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("default seam returned empty stdout for `go env GOOS`")
	}
}

type stubBackend struct {
	ok   bool
	data []byte
	fmt  string
}

func (s stubBackend) ReadImage(_ context.Context) ([]byte, string, bool) {
	return s.data, s.fmt, s.ok
}
