package automation

import (
	"encoding/json"
	"errors"

	"github.com/x6nux/yanshi/internal/store"
)

// stateKey 是 KV 命名空间。automation:idem: 前缀保留给 A2Adapter 的幂等映射。
const stateKey = "automation:c1:state"

// Repository 封装 store.KVGet/KVSet 的 read-modify-write。所有调用必须在
// Manager.mu 锁内进行，以串行化 scheduler tick 与 tool CRUD 的并发写。
type Repository struct {
	store *store.Store
}

// NewRepository 构造 repo。s 为 nil 时 Load/Save 都返回错误（fail-closed）。
func NewRepository(s *store.Store) *Repository {
	return &Repository{store: s}
}

// Load 读取持久化 state。未初始化时返回零值 state（含当前 SchemaVersion）。
// 不匹配的 SchemaVersion 或损坏 JSON 都返回错误，不静默丢弃。
func (r *Repository) Load() (State, error) {
	if r == nil || r.store == nil {
		return State{}, errors.New("automation repository is not configured")
	}
	value, ok, err := r.store.KVGet(stateKey)
	if err != nil {
		return State{}, err
	}
	if !ok || value == "" {
		return State{SchemaVersion: StateSchemaVersion}, nil
	}
	var state State
	if err := json.Unmarshal([]byte(value), &state); err != nil {
		return State{}, err
	}
	if state.SchemaVersion != StateSchemaVersion {
		return State{}, errors.New("unsupported automation state schema version")
	}
	return state, nil
}

// Save 持久化 state。写前强制设置 SchemaVersion；调用方不必担心遗漏。
func (r *Repository) Save(state State) error {
	if r == nil || r.store == nil {
		return errors.New("automation repository is not configured")
	}
	state.SchemaVersion = StateSchemaVersion
	value, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return r.store.KVSet(stateKey, string(value))
}
