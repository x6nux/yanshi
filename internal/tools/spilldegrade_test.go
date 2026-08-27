package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bodyOfSize builds a multi-line body of roughly n bytes, so the head/tail
// preview has real lines to work with.
func bodyOfSize(n int) string {
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(strings.Repeat("x", 79))
		b.WriteString("\n")
	}
	return b.String()
}

func TestDegradeToolResult_Table(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		withRoot   bool
		wantDid    bool
		wantReason string
	}{
		{
			name:       "below the minimum is left alone",
			body:       bodyOfSize(degradeMinBytes - 200),
			withRoot:   true,
			wantDid:    false,
			wantReason: "the marker text would approach the size of the body",
		},
		{
			name:       "exactly at the minimum is left alone",
			body:       strings.Repeat("y", degradeMinBytes),
			withRoot:   true,
			wantDid:    false,
			wantReason: "the boundary is <=, not <",
		},
		{
			name:       "comfortably over the minimum degrades",
			body:       bodyOfSize(DegradeMaxBytes * 8),
			withRoot:   true,
			wantDid:    true,
			wantReason: "this is the whole point",
		},
		{
			name:       "between SpillThreshold and the degrade floor still degrades",
			body:       bodyOfSize(SpillThreshold / 2),
			withRoot:   true,
			wantDid:    true,
			wantReason: "the 64 KiB spill cap never saw this size; the degrade tier is what covers it",
		},
		{
			name:       "already degraded is refused",
			body:       "[spilled: 900 lines / 90 KiB → .yanshi/tmp/spillover/a.txt]\n" + bodyOfSize(DegradeMaxBytes*4),
			withRoot:   true,
			wantDid:    false,
			wantReason: "a second pass would point at a preview, not at the original",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.withRoot {
				ctx = WithWorkRoot(ctx, t.TempDir())
			}
			got, did := DegradeToolResult(ctx, "run_tests", tc.body)
			require.Equal(t, tc.wantDid, did, tc.wantReason)
			if !did {
				assert.Equal(t, tc.body, got, "a refused degrade must return the body untouched")
				return
			}
			assert.LessOrEqual(t, len(got), DegradeMaxBytes,
				"a degraded result must fit the per-result budget")
			assert.Less(t, len(got), len(tc.body))
		})
	}
}

// TestDegradeWritesTheFullTextBeforeShrinking is the property that separates
// degradation from deletion. Without the disk copy, T4 is a truncation with a
// friendly message on it.
func TestDegradeWritesTheFullTextBeforeShrinking(t *testing.T) {
	root := t.TempDir()
	ctx := WithWorkRoot(context.Background(), root)
	body := bodyOfSize(DegradeMaxBytes * 10)

	got, did := DegradeToolResult(ctx, "shell_run", body)
	require.True(t, did)

	entries, err := os.ReadDir(filepath.Join(root, spillDir))
	require.NoError(t, err)
	require.Len(t, entries, 1, "exactly one spillover file per degrade")

	full, err := os.ReadFile(filepath.Join(root, spillDir, entries[0].Name()))
	require.NoError(t, err)
	assert.Equal(t, body, string(full), "the file must hold the ORIGINAL, not the preview")

	// And the shrunken body must point at it, by a path relative to the root
	// so fs_read can take it directly.
	assert.Contains(t, got, ".yanshi/tmp/spillover/shell_run-")
}

// TestDegradeRefusesWhenTheDiskCopyFails pins the fail-safe direction.
//
// If the spillover write fails there is no recovery pointer, so shrinking the
// text destroys it. A full window is recoverable; a silently truncated test
// log the model then reasons about is not. The unwritable root is produced by
// making the work root a FILE, so MkdirAll cannot create the spillover dir.
func TestDegradeRefusesWhenTheDiskCopyFails(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(blocked, []byte("x"), 0o600))
	ctx := WithWorkRoot(context.Background(), blocked)

	body := bodyOfSize(DegradeMaxBytes * 10)
	got, did := DegradeToolResult(ctx, "run_tests", body)

	require.False(t, did, "no disk copy means no recovery pointer, so no shrink")
	assert.Equal(t, body, got, "the full text must survive when it cannot be saved elsewhere")
}

// TestDegradeUsesTheFoldRecognisedMarker is this side of the soft coupling
// pinned from the other side by
// internal/ctxcompact::TestSpillMarkerMatchesToolsPreview.
//
// ctxcompact is a leaf and cannot import tools, so it recovers the spillover
// path by matching the preview header textually. If the degrade tier emitted a
// DIFFERENT header, every degraded result would fall back to the weaker
// tool-call-id pointer when fold later touched it — silently, because the
// fallback is safe.
func TestDegradeUsesTheFoldRecognisedMarker(t *testing.T) {
	ctx := WithWorkRoot(context.Background(), t.TempDir())
	got, did := DegradeToolResult(ctx, "run_tests", bodyOfSize(DegradeMaxBytes*10))
	require.True(t, did)

	require.True(t, strings.HasPrefix(got, spillHeaderPrefix),
		"the degraded body must OPEN with the header ctxcompact.spillPathIn looks for; got %.60q", got)
	header := got[:strings.IndexByte(got, ']')+1]
	assert.Contains(t, header, "→", "spillPathIn splits the header on the arrow")
	assert.True(t, AlreadyDegraded(got), "the marker must be self-identifying, or the idempotency check fails")
}

// TestDegradeIsIdempotent. Running the pass again on an already-degraded body
// must be a no-op: a second pass would spill a copy of the PREVIEW and hand
// back a pointer that resolves to strictly less than the first one.
func TestDegradeIsIdempotent(t *testing.T) {
	root := t.TempDir()
	ctx := WithWorkRoot(context.Background(), root)

	once, did := DegradeToolResult(ctx, "run_tests", bodyOfSize(DegradeMaxBytes*10))
	require.True(t, did)
	twice, didAgain := DegradeToolResult(ctx, "run_tests", once)

	require.False(t, didAgain)
	assert.Equal(t, once, twice)

	entries, err := os.ReadDir(filepath.Join(root, spillDir))
	require.NoError(t, err)
	assert.Len(t, entries, 1, "the second pass must not write a second file")
}

// TestDegradePreviewNeverSplitsARune. The final cut to DegradeMaxBytes is
// byte-wise, so a multi-byte body is the case that would produce invalid UTF-8
// in the transcript.
func TestDegradePreviewNeverSplitsARune(t *testing.T) {
	ctx := WithWorkRoot(context.Background(), t.TempDir())
	var b strings.Builder
	for b.Len() < DegradeMaxBytes*6 {
		b.WriteString(strings.Repeat("测试输出行", 12))
		b.WriteString("\n")
	}
	got, did := DegradeToolResult(ctx, "run_tests", b.String())
	require.True(t, did)
	assert.True(t, utf8Valid(got), "a degraded body must remain valid UTF-8")
	assert.LessOrEqual(t, len(got), DegradeMaxBytes)
}

// utf8Valid is a local check so the test does not depend on which of the two
// unicode packages the production file happens to import.
func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

// TestTruncateRunes_Table covers the byte-cut helper directly, including the
// boundaries the preview path rarely reaches.
func TestTruncateRunes_Table(t *testing.T) {
	cases := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"non-positive yields empty", "abc", 0, ""},
		{"negative yields empty", "abc", -5, ""},
		{"shorter than the cap is unchanged", "abc", 10, "abc"},
		{"exact fit is unchanged", "abc", 3, "abc"},
		{"ascii cuts cleanly", "abcdef", 4, "abcd"},
		{"multibyte cut backs off to a boundary", "测试", 4, "测"},
		{"multibyte cut mid-sequence backs off", "测试", 5, "测"},
		{"multibyte exact boundary keeps both", "测试", 6, "测试"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, truncateRunes(tc.in, tc.n))
		})
	}
}

// TestDegradeKeepRecentIsBelowFoldKeepRecent states the relationship between
// the two tiers as an assertion rather than as prose in a comment.
//
// This pass runs every iteration, so its recent window is the working set of
// ONE decision. Fold runs only under window pressure and is the last line
// before eviction, so it keeps a wider margin. If this ever inverted, the
// aggressive unconditional pass would be cutting results the conservative
// pressure-driven one was still protecting.
func TestDegradeKeepRecentIsBelowFoldKeepRecent(t *testing.T) {
	// ctxcompact.FoldKeepRecent, mirrored rather than imported: tools must not
	// depend on ctxcompact (and ctxcompact must not depend on tools — GOV1).
	const foldKeepRecent = 5
	assert.Less(t, DegradeKeepRecent, foldKeepRecent,
		"the unconditional per-round pass must protect a NARROWER window than the "+
			"pressure-driven one; inverted, it would cut what fold was still saving")
	assert.Less(t, DegradeMaxBytes, SpillThreshold,
		"a degrade ceiling near the spill cap would save nothing")
}
