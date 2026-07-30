// Package tui tests for styles.go (render cache bounds + helpers).
package tui

import (
	"strconv"
	"sync/atomic"
	"testing"
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
