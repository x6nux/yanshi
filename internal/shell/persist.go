package shell

import (
	"encoding/json"
	"fmt"
)

// PersistStore is the small storage interface the Manager uses to flush its
// job table at shutdown and reload it at boot. *store.Store satisfies this
// via the JobFromKV adapter below; tests substitute a sync.Map-backed fake.
type PersistStore interface {
	SaveJob(Job) error
	LoadJobs() ([]Job, error)
}

// JobFromKV adapts an internal/store.Store-shaped KV (KVGet/KVSet) to the
// PersistStore interface. Each SaveJob replaces any prior entry with the same
// ID so the table stays idempotent; LoadJobs returns the full list.
//
// We accept the KV interface inline (rather than importing internal/store) so
// this package stays a leaf with no dependency on the storage layer.
func JobFromKV(kv interface {
	KVGet(string) (string, bool, error)
	KVSet(string, string) error
}) PersistStore {
	return &kvPersist{kv: kv}
}

type kvPersist struct {
	kv interface {
		KVGet(string) (string, bool, error)
		KVSet(string, string) error
	}
}

// jobKey is the single KV key under which the entire job table is serialized.
// A versioned suffix (v1) lets future schema changes ship under a new key
// without breaking installs that still hold the old shape.
const jobKey = "security.shell.jobs.v1"

// SaveJob merges job into the persisted list. Idempotent: a prior entry with
// the same ID is replaced. The full list is re-marshalled on every call —
// the table is small (one entry per live job) so this is plenty fast.
func (p *kvPersist) SaveJob(job Job) error {
	existing, _, _ := p.kv.KVGet(jobKey)
	var list []Job
	if existing != "" {
		_ = json.Unmarshal([]byte(existing), &list)
	}
	// Replace any prior entry with the same ID so SaveJob is idempotent.
	out := make([]Job, 0, len(list)+1)
	for _, j := range list {
		if j.ID != job.ID {
			out = append(out, j)
		}
	}
	out = append(out, job)
	data, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("shell: encode jobs: %w", err)
	}
	return p.kv.KVSet(jobKey, string(data))
}

// LoadJobs returns the persisted list, or nil when the key is absent / empty
// (first boot, or all jobs cleared).
func (p *kvPersist) LoadJobs() ([]Job, error) {
	raw, ok, err := p.kv.KVGet(jobKey)
	if err != nil {
		return nil, err
	}
	if !ok || raw == "" {
		return nil, nil
	}
	var list []Job
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil, fmt.Errorf("shell: decode jobs: %w", err)
	}
	return list, nil
}
