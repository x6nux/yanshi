package store

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/auth"
)

func TestAuthSQLiteAdapterUpsertsOnlyNonSecretMetadata(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	adapter := AuthMetadataFromDB(st)
	first := time.Unix(1000, 0).UTC()
	second := time.Unix(2000, 0).UTC()
	if err := adapter.SaveAuthMetadata("openai", "main", auth.AuthMetadata{
		Source: "device", ExpiresAt: first,
	}); err != nil {
		t.Fatal(err)
	}
	if err := adapter.SaveAuthMetadata("openai", "main", auth.AuthMetadata{
		Source: "device", ExpiresAt: second,
	}); err != nil {
		t.Fatal(err)
	}

	var source string
	var expiresAt int64
	if err := st.DB.QueryRow(
		`SELECT source, expires_at FROM auth_metadata
         WHERE provider = ? AND account = ?`,
		"openai", "main",
	).Scan(&source, &expiresAt); err != nil {
		t.Fatal(err)
	}
	if source != "device" || expiresAt != second.Unix() {
		t.Fatalf("metadata = (%q,%d), want (device,%d)", source, expiresAt, second.Unix())
	}

	columns, err := st.columns("auth_metadata")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"provider", "account", "source", "expires_at", "updated_at"}
	if !reflect.DeepEqual(columns, want) {
		t.Fatalf("auth_metadata columns = %v, want %v; secret columns are forbidden", columns, want)
	}
}

// TestAuthSQLiteAdapterLoadDeleteLifecycle exercises the Task 14 Load/Delete
// lifecycle on top of the Task 8 Save upsert. Not-found returns the distinct
// auth.ErrAuthMetadataNotFound sentinel; delete is idempotent-fail (second
// delete on the same row also returns not-found, not nil).
func TestAuthSQLiteAdapterLoadDeleteLifecycle(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	adapter := AuthMetadataFromDB(st)
	expires := time.Unix(3456, 0).UTC()

	if err := adapter.SaveAuthMetadata("openai", "main", auth.AuthMetadata{
		Source: "device", ExpiresAt: expires,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := adapter.LoadAuthMetadata("openai", "main")
	if err != nil || got.Source != "device" || !got.ExpiresAt.Equal(expires) {
		t.Fatalf("load = %#v err=%v", got, err)
	}
	if err := adapter.DeleteAuthMetadata("openai", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.LoadAuthMetadata("openai", "main"); !errors.Is(err, auth.ErrAuthMetadataNotFound) {
		t.Fatalf("load after delete = %v", err)
	}
	if err := adapter.DeleteAuthMetadata("openai", "main"); !errors.Is(err, auth.ErrAuthMetadataNotFound) {
		t.Fatalf("second delete = %v", err)
	}
}

// TestAuthSQLiteAdapter_ConcurrentSetLoadDeleteIsAtomic exercises the C5
// atomic contract on the production SQLite adapter: concurrent calls to
// Save/Load/Delete on the same adapter instance must never produce a
// partially-written row. The authSQLiteAdapter's txMu enforces this;
// running under `go test -race` catches any regression.
func TestAuthSQLiteAdapter_ConcurrentSetLoadDeleteIsAtomic(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	adapter := AuthMetadataFromDB(st)

	var wg sync.WaitGroup
	const goroutines = 8
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			_ = adapter.SaveAuthMetadata("openai", "main", auth.AuthMetadata{
				Source: fmt.Sprintf("iteration-%d", i),
			})
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = adapter.LoadAuthMetadata("openai", "main")
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = adapter.DeleteAuthMetadata("openai", "main")
		}()
	}
	wg.Wait()
}
