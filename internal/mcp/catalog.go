package mcp

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
)

// 工具目录缓存（W-F-28）：key 是「连接身份 + 配置指纹」。连接身份是
// server 名（一个名字在一份配置里就是一个连接）；指纹是整份 ServerConfig
// 的哈希 —— 配置的任何字段变了（命令、参数、env、URL、超时、allow/deny），
// 指纹就不同，旧条目天然失效，不需要失效逻辑去追每一种配置变更。
//
// OAuth 的 ClientSecret 不进指纹（json:"-"），它本来也不参与 schema；
// bearer 参与，因为换凭据意味着换连接。
//
// 进程内的 LRU：同一进程里 Manager 会被重建（daemon reload 重建组合根）、
// server 会被 Disable/Enable/Reload —— 这些往返原本每次都要一次 tools/list
// 往返；缓存命中时跳过。跨进程不持久化：schema 无所谓跨重启复用，冷启动
// 一次往返是可接受的价格，持久化会引入陈旧条目的失效问题，不值。
//
// 缓存的是 advertised 工具（过滤前的全集）：allow/deny 过滤发生在注册处，
// 这样同一个缓存条目对任何过滤配置都成立 —— 过滤配置本身也进了指纹，双保险。

const catalogMaxEntries = 64

// catalogKey identifies one cache entry: the connection identity (server
// name) plus the fingerprint of the config that produced the entry.
type catalogKey struct {
	server      string
	fingerprint string
}

type catalogEntry struct {
	key   catalogKey
	tools []ToolDescriptor
}

var (
	catalogMu    sync.Mutex
	catalogOrder = list.New() // front = most recently used
	catalogItems = map[catalogKey]*list.Element{}
)

// catalogFingerprint hashes the config into a change detector. Deterministic
// across processes and runs (sha256 of the canonical JSON encoding), so the
// same config always hits and any field flip misses.
func catalogFingerprint(cfg *ServerConfig) string {
	b, err := json.Marshal(cfg)
	if err != nil {
		// ServerConfig marshals unconditionally in practice; a failure names
		// itself instead of silently colliding every server onto one key.
		return "unmarshalable:" + cfg.Name
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

// catalogLookup returns the cached advertised tools for key, marking the
// entry recently used.
func catalogLookup(key catalogKey) ([]ToolDescriptor, bool) {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	el, ok := catalogItems[key]
	if !ok {
		return nil, false
	}
	catalogOrder.MoveToFront(el)
	entry := el.Value.(*catalogEntry)
	return entry.tools, true
}

// catalogStore caches the advertised tools under key, evicting the least
// recently used entry when the cache is full.
func catalogStore(key catalogKey, tools []ToolDescriptor) {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	if el, ok := catalogItems[key]; ok {
		catalogOrder.MoveToFront(el)
		el.Value.(*catalogEntry).tools = tools
		return
	}
	catalogOrder.PushFront(&catalogEntry{key: key, tools: tools})
	catalogItems[key] = catalogOrder.Front()
	if len(catalogItems) > catalogMaxEntries {
		if oldest := catalogOrder.Back(); oldest != nil {
			delete(catalogItems, oldest.Value.(*catalogEntry).key)
			catalogOrder.Remove(oldest)
		}
	}
}

// catalogInvalidateServer drops every entry for the server, whatever config
// produced it. The tools/list_changed notification goes through here: the
// server just said its catalog changed, so no fingerprint can be trusted
// anymore and the next fetch must be a real round trip.
func catalogInvalidateServer(server string) {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	for el := catalogOrder.Front(); el != nil; {
		next := el.Next()
		if el.Value.(*catalogEntry).key.server == server {
			delete(catalogItems, el.Value.(*catalogEntry).key)
			catalogOrder.Remove(el)
		}
		el = next
	}
}
