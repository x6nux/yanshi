// Package tui tests for styles.go (render cache bounds + helpers).
package tui

import (
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/muesli/termenv"
)

// TestEntryRenderCacheCap proves the entryRenderCache is bounded (review I5):
// once the cap is reached, new keys skip caching (counter stops growing) but
// already-cached keys still hit and the render still runs. Without this cap a
// long-running session combined with width-bucketed keys (resize the terminal
// and every historical entry fans out to a fresh key per column count) could
// grow the sync.Map without limit.
func TestEntryRenderCacheCap(t *testing.T) {
	resetEntryRenderCacheForTest()
	// Ensure the cap is restored even if a future tweak lowers it for this
	// test. Production cap is 1024; we use the live value here.
	cap := entryCacheCap

	// Fill the cache to the cap with distinct keys; each must store.
	for i := int64(0); i < cap; i++ {
		key := uint64(0x1_0000_0000) ^ uint64(i) // avoid colliding with real fnv64 keys
		got := cachedEntryRender(key, func() string { return "v" + strconv.FormatInt(i, 10) })
		if got != "v"+strconv.FormatInt(i, 10) {
			t.Fatalf("首次 miss 应返回 render()，key=%d：got %q", i, got)
		}
	}
	if got := entryCacheCount.Load(); got != cap {
		t.Fatalf("填满到 cap 后计数应 = %d，实际 %d", cap, got)
	}

	// Past the cap: new keys must NOT be stored, but the render still runs and
	// the returned value is correct.
	overflowKey := uint64(0x2_0000_0000)
	got := cachedEntryRender(overflowKey, func() string { return "overflow" })
	if got != "overflow" {
		t.Fatalf("溢出 key 应仍返回 render()，got %q", got)
	}
	if got := entryCacheCount.Load(); got != cap {
		t.Fatalf("溢出 key 后计数应保持 cap=%d（不再增长），实际 %d", cap, got)
	}

	// The overflow key is not in the map: a second call re-runs render (does
	// not hit). Verify by counting render invocations.
	calls := int32(0)
	cachedEntryRender(overflowKey, func() string {
		atomic.AddInt32(&calls, 1)
		return "again"
	})
	if calls != 1 {
		t.Fatalf("溢出 key 未缓存：第二次调用应触发 render()，实际 calls=%d", calls)
	}

	// Existing entries (under cap) still hit: re-load key 0 and confirm the
	// render function is NOT invoked.
	hitCalls := int32(0)
	got = cachedEntryRender(uint64(0x1_0000_0000)^0, func() string {
		atomic.AddInt32(&hitCalls, 1)
		return "should-not-run"
	})
	if hitCalls != 0 {
		t.Fatalf("已缓存 key 应命中，render 不应被调用，实际 calls=%d", hitCalls)
	}
	if got != "v0" {
		t.Fatalf("已缓存 key 应返回缓存值 v0，实际 %q", got)
	}
}

// TestPaletteHuesEmit24BitUnderTrueColor proves the Palette mechanical
// replacement's actual purpose: a REAL production style var (roleUser, not
// an ad-hoc one — contrast capability_test.go's
// TestApplyColorProfile_TrueColorEmits24Bit, which predates this conversion
// and deliberately used an ad-hoc hex style because at that commit every
// production var was still ANSI-256-index-declared) now emits a genuine
// 24-bit "38;2;r;g;b" sequence under COLORTERM=truecolor. Before the palette
// const conversion in styles.go, roleUser was
// lipgloss.NewStyle().Foreground(lipgloss.Color("39")) — an already-8-bit-
// typed ANSIColor/ANSI256Color, which termenv.Profile.Convert never upgrades
// (only downgrades) — so this assertion would fail against the pre-Palette
// declaration.
func TestPaletteHuesEmit24BitUnderTrueColor(t *testing.T) {
	withColorProfile(t, termenv.TrueColor)
	out := roleUser.Render("x")
	// hueCyan = "#00afff" = rgb(0,175,255).
	if !strings.Contains(out, "38;2;0;175;255") {
		t.Fatalf("TrueColor: roleUser (production style) should emit a genuine 24-bit sequence for hueCyan (#00afff), got %q", out)
	}
}

// TestPaletteHuesUnchangedUnderANSI256 proves the palette conversion is
// behavior-preserving under the profile this file hardcoded before W-E-01
// (ANSI256): each hue constant's hex value round-trips to the exact
// ANSI-256 index it replaced (see styles.go's palette const block doc
// comment for the 6×6×6 cube math), so production output under the default
// profile is byte-identical to before the conversion.
func TestPaletteHuesUnchangedUnderANSI256(t *testing.T) {
	withColorProfile(t, termenv.ANSI256)
	cases := []struct {
		name string
		out  string
		want string // expected "38;5;N" substring
	}{
		{"roleUser (was 39)", roleUser.Render("x"), "38;5;39"},
		{"roleAsst (was 213)", roleAsst.Render("x"), "38;5;213"},
		{"toolName (was 123)", toolName.Render("x"), "38;5;123"},
		{"okStyle (was 42)", okStyle.Render("x"), "38;5;42"},
		{"errStyle (was 203)", errStyle.Render("x"), "38;5;203"},
		{"warnStyle (was 179)", warnStyle.Render("x"), "38;5;179"},
		{"warnStylePerm (was 196)", warnStylePerm.Render("x"), "38;5;196"},
		{"thinkingLiveStyle (was 117)", thinkingLiveStyle.Render("x"), "38;5;117"},
	}
	for _, c := range cases {
		if !strings.Contains(c.out, c.want) {
			t.Errorf("%s: ANSI256 profile should still produce %q, got %q", c.name, c.want, c.out)
		}
	}
	// selPaletteStyle sets Background, not Foreground (was ANSI-256 "24").
	if bg := selPaletteStyle.Render("x"); !strings.Contains(bg, "48;5;24") {
		t.Errorf("selPaletteStyle (was bg 24): ANSI256 profile should still produce \"48;5;24\", got %q", bg)
	}
}
