package mcp

import (
	"context"
	"testing"
)

// 指纹是变更检测器：同配置稳定，任一字段翻动即不同。
//
// 变异：把 catalogFingerprint 改成恒返固定串 → 本测试的「不同」用例变红，
// 且下游 TestManagerReloadServesToolsFromCatalog 会在配置变更后照样命中
// 缓存（陈旧 schema 复活）。
func TestCatalogFingerprintChangesWithConfig(t *testing.T) {
	base := &ServerConfig{Name: "srv", Transport: TransportHTTP, URL: "http://x"}
	again := &ServerConfig{Name: "srv", Transport: TransportHTTP, URL: "http://x"}
	if catalogFingerprint(base) != catalogFingerprint(again) {
		t.Fatal("identical configs produced different fingerprints")
	}
	if catalogFingerprint(base) == catalogFingerprint(&ServerConfig{Name: "srv", Transport: TransportHTTP, URL: "http://y"}) {
		t.Fatal("a URL change did not change the fingerprint")
	}
	if catalogFingerprint(base) == catalogFingerprint(&ServerConfig{Name: "srv", Transport: TransportStdio, Command: "run.sh"}) {
		t.Fatal("a transport change did not change the fingerprint")
	}
	// allow/deny 也进指纹：过滤配置变了，旧条目不许冒充新配置的目录。
	if catalogFingerprint(base) == catalogFingerprint(&ServerConfig{Name: "srv", Transport: TransportHTTP, URL: "http://x", ToolDeny: []string{"x"}}) {
		t.Fatal("a tool_deny change did not change the fingerprint")
	}
}

// LRU 驱逐：容量+1 条后最老的不再命中，最新的仍在。
//
// 变异：删掉 catalogStore 的驱逐分支 → 本测试变红（超出容量后全都命中，
// 缓存无界增长）。
func TestCatalogLRUEvicts(t *testing.T) {
	key := func(i int) catalogKey {
		return catalogKey{server: "lru-probe", fingerprint: catalogFingerprint(&ServerConfig{Name: "lru-probe", Args: []string{}}) + string(rune('a'+i))}
	}
	for i := 0; i < catalogMaxEntries; i++ {
		catalogStore(key(i), nil)
	}
	for i := 0; i < catalogMaxEntries; i++ {
		if _, ok := catalogLookup(key(i)); !ok {
			t.Fatalf("entry %d vanished before the cache was full", i)
		}
	}
	// 触碰第 0 条使其成为最近使用，再插入新条目 —— 被驱逐的必须是第 1 条
	// 而不是刚用过的第 0 条。
	if _, ok := catalogLookup(key(0)); !ok {
		t.Fatal("entry 0 vanished while touching")
	}
	catalogStore(key(catalogMaxEntries), nil)
	if _, ok := catalogLookup(key(1)); ok {
		t.Fatal("entry 1 survived eviction; LRU order is broken")
	}
	if _, ok := catalogLookup(key(0)); !ok {
		t.Fatal("recently used entry 0 was evicted")
	}
}

// Reload 往返（Disable → Enable → startOne）在配置未变时从缓存出目录，
// 不再打 tools/list。这是缓存的生产受益路径：daemon reload 重建 Manager、
// health loop 重连，都不必为一份没变的目录重新付一次往返。
//
// 变异：把 startOne 里 catalogLookup 的命中分支删掉（恒 miss）→ 本测试
// 的 ListCount==1 断言变红（第二次 startOne 重新请求）。
func TestManagerReloadServesToolsFromCatalog(t *testing.T) {
	ts, fake := NewFakeHTTPServer([]ToolDescriptor{{ToolName: "echo"}})
	defer ts.Close()
	m := NewManager(map[string]*ServerConfig{
		"cache-hit-probe": {Name: "cache-hit-probe", Enabled: true, Transport: TransportHTTP, URL: ts.URL},
	})
	if st := m.StartAll(context.Background()); len(st) != 1 || st[0].Status != StatusReady {
		t.Fatalf("status=%+v", st)
	}
	if fake.ListCount != 1 {
		t.Fatalf("ListCount after start = %d, want 1", fake.ListCount)
	}
	_ = m.Disable(context.Background(), "cache-hit-probe")
	if err := m.Enable(context.Background(), "cache-hit-probe"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if fake.ListCount != 1 {
		t.Fatalf("reload refetched tools/list (ListCount=%d); unchanged config must be served from the catalog cache", fake.ListCount)
	}
	tools, _ := m.ListAllTools(context.Background())
	if len(tools) != 1 || tools[0].Qualified != "mcp_cache_hit_probe_echo" {
		t.Fatalf("tools after cached reload = %+v", tools)
	}
}

// 配置变更即失效：同一个 server 名、不同 env —— 新指纹 miss，真实
// tools/list 重打一次。
//
// 变异：指纹漏掉 Env 字段做不到（整份 config 哈希），等价变异是删掉
// startOne 的 catalogStore，让第二次永远 miss —— 反方向（恒 miss）由
// 上面的 reload 用例看管，本用例看管的是「恒 hit」方向：把 startOne 里
// catalogFingerprint 换成常量 → 本测试变红（ListCount 停在 1）。
func TestManagerConfigChangeInvalidatesCatalog(t *testing.T) {
	ts, fake := NewFakeHTTPServer([]ToolDescriptor{{ToolName: "echo"}})
	defer ts.Close()
	cfgOf := func() *ServerConfig {
		return &ServerConfig{Name: "cache-miss-probe", Enabled: true, Transport: TransportHTTP, URL: ts.URL}
	}
	m1 := NewManager(map[string]*ServerConfig{"cache-miss-probe": cfgOf()})
	m1.StartAll(context.Background())
	if fake.ListCount != 1 {
		t.Fatalf("ListCount after first manager = %d, want 1", fake.ListCount)
	}
	changed := cfgOf()
	changed.Env = map[string]string{"CHANGED": "1"}
	m2 := NewManager(map[string]*ServerConfig{"cache-miss-probe": changed})
	m2.StartAll(context.Background())
	if fake.ListCount != 2 {
		t.Fatalf("ListCount after config change = %d, want 2; a changed config must miss the catalog cache", fake.ListCount)
	}
}

// listChanged 失效：catalogInvalidateServer 之后同一指纹也必须 miss。
//
// 变异：把 catalogInvalidateServer 改成空操作 → 本测试变红（失效后照样
// 命中旧目录 —— W-F-28「配置变更即失效」的另一半由 server 主动宣告）。
func TestCatalogInvalidateServerForcesRefetch(t *testing.T) {
	key := catalogKey{server: "invalidate-probe", fingerprint: "fp"}
	catalogStore(key, []ToolDescriptor{{ToolName: "old"}})
	if _, ok := catalogLookup(key); !ok {
		t.Fatal("entry missing right after store")
	}
	catalogInvalidateServer("invalidate-probe")
	if _, ok := catalogLookup(key); ok {
		t.Fatal("entry survived invalidation; the server said its catalog changed")
	}
	// 其他 server 的条目不受牵连。
	other := catalogKey{server: "other-probe", fingerprint: "fp"}
	catalogStore(other, nil)
	catalogInvalidateServer("invalidate-probe")
	if _, ok := catalogLookup(other); !ok {
		t.Fatal("invalidation touched a different server's entries")
	}
}
