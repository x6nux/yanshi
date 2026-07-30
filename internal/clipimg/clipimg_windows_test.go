//go:build windows

package clipimg

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// withFakeRunner swaps the package-level commandOutput seam for the duration of
// the test and restores it via t.Cleanup. The fake observes the exact command
// the platform reader would invoke and returns canned output, exercising
// platformReader.ReadImage without spawning a real PowerShell process.
func withFakeRunner(t *testing.T, wantName string, fn func(ctx context.Context, name string, args ...string) ([]byte, error)) {
	t.Helper()
	orig := commandOutput
	commandOutput = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name != wantName {
			t.Fatalf("platform reader invoked %q; want %q", name, wantName)
		}
		return fn(ctx, name, args...)
	}
	t.Cleanup(func() { commandOutput = orig })
}

// TestWindowsReadImageReturnsImage exercises the success path: PowerShell
// returns PNG bytes (clipboard held an image) and the reader surfaces them with
// fmt "png" and ok=true.
func TestWindowsReadImageReturnsImage(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	withFakeRunner(t, "powershell", func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if len(args) == 0 || args[0] != "-NoProfile" {
			t.Fatalf("unexpected args: %v", args)
		}
		return png, nil
	})
	data, fmtName, ok := platformReader{}.ReadImage(context.Background())
	if !ok {
		t.Fatal("non-empty output must yield ok=true")
	}
	if fmtName != "png" {
		t.Fatalf("fmt = %q, want png", fmtName)
	}
	if !bytes.Equal(data, png) {
		t.Fatalf("data = %v, want %v", data, png)
	}
}

// TestWindowsReadImageNoImageWhenOutputEmpty is the no-image case: PowerShell
// exits cleanly but prints nothing (clipboard holds text or is empty). This is
// ok=false, NOT an error — the keystroke must fall through to text paste.
func TestWindowsReadImageNoImageWhenOutputEmpty(t *testing.T) {
	withFakeRunner(t, "powershell", func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return nil, nil // exit 0, no stdout → no image on clipboard
	})
	_, _, ok := platformReader{}.ReadImage(context.Background())
	if ok {
		t.Fatal("empty subprocess output must yield ok=false")
	}
}

// TestWindowsReadImageCommandError covers the error path: the subprocess itself
// fails (PowerShell missing, script error, non-zero exit). That too is ok=false
// so a broken clipboard backend degrades to text paste instead of erroring.
func TestWindowsReadImageCommandError(t *testing.T) {
	withFakeRunner(t, "powershell", func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return nil, errors.New("powershell: term not recognized")
	})
	_, _, ok := platformReader{}.ReadImage(context.Background())
	if ok {
		t.Fatal("subprocess error must yield ok=false")
	}
}

// TestWindowsReadImageRespectsContextCancellation confirms the context is
// threaded through the seam; a canceled context surfaces as an error from the
// (fake) subprocess and therefore ok=false.
func TestWindowsReadImageRespectsContextCancellation(t *testing.T) {
	withFakeRunner(t, "powershell", func(ctx context.Context, name string, args ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, ok := platformReader{}.ReadImage(ctx)
	if ok {
		t.Fatal("canceled context must yield ok=false")
	}
}
