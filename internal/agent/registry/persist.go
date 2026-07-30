package registry

import (
	"encoding/json"
	"os"
)

// persistenceSchemaVersion is the version number for the on-disk format.
const persistenceSchemaVersion = 1

// marshal is the JSON encoder used by snapshotLocked. It is a package-level
// var so tests can inject failures for the defensive snapshot-error rollback
// paths (json.Marshal itself never fails for Record).
var marshal = json.Marshal

// persistedState is the root JSON object written to disk.
type persistedState struct {
	SchemaVersion int      `json:"schema_version"`
	SessionBootID string   `json:"session_boot_id"`
	Agents        []Record `json:"agents"`
}

// loadState reads and decodes the persisted state file. Unknown JSON fields
// are silently ignored (future-proofing). Missing file yields an empty state
// (not an error).
func loadState(path string) (persistedState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return persistedState{SchemaVersion: persistenceSchemaVersion}, nil
		}
		return persistedState{}, err
	}
	var st persistedState
	if err := json.Unmarshal(data, &st); err != nil {
		return persistedState{}, err
	}
	if st.SchemaVersion == 0 {
		st.SchemaVersion = persistenceSchemaVersion
	}
	// Accept unknown top-level fields (they're silently ignored by
	// json.Unmarshal with no DisallowUnknownFields).
	return st, nil
}
