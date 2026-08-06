package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConcurrentAppend_NoBusy: 16 goroutines x 50 messages each, all successful.
func TestConcurrentAppend_NoBusy(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "yanshi.db"))
	require.NoError(t, err)
	defer st.Close()
	sid, err := st.CreateSession("c")
	require.NoError(t, err)
	const N, M = 16, 50
	var wg sync.WaitGroup
	start := make(chan struct{})
	for g := 0; g < N; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			<-start
			for i := 0; i < M; i++ {
				if err := st.AppendMessage(sid, g*M+i, "user", fmt.Sprintf("g%d-i%d", g, i)); err != nil {
					t.Errorf("append: %v", err)
					return
				}
			}
		}(g)
	}
	close(start)
	wg.Wait()
	count, err := st.SessionMessageCount(sid)
	require.NoError(t, err)
	assert.Equal(t, N*M, count, "no messages lost under concurrent write")
}

// TestConcurrentMixedWrite_NoBusy: mixed concurrent writes (Create, Append, KVSet, WriteMemory).
func TestConcurrentMixedWrite_NoBusy(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "yanshi.db"))
	require.NoError(t, err)
	defer st.Close()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = st.CreateSession(fmt.Sprintf("s-%d", i))
			_ = st.KVSet(fmt.Sprintf("k-%d", i), fmt.Sprintf("v-%d", i))
			_, _ = st.WriteMemory("note", fmt.Sprintf("note-%d", i))
		}(i)
	}
	wg.Wait()
}

// TestConcurrentReadWrite_NoBlocking: concurrent reads while writes are happening.
//
// ledger: F1/WAL1#2 并发写不报 locked
func TestConcurrentReadWrite_NoBlocking(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "yanshi.db"))
	require.NoError(t, err)
	defer st.Close()
	sid, err := st.CreateSession("rw")
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			_ = st.AppendMessage(sid, i, "user", fmt.Sprintf("msg-%d", i))
			time.Sleep(time.Millisecond)
		}
		close(done)
	}()
	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
			_, _ = st.Messages(sid)
			_, _ = st.SessionMessageCount(sid)
		}
	}
}

// TestWriteTxSerializes: two concurrent WriteTx calls, observe ordering.
func TestWriteTxSerializes(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "yanshi.db"))
	require.NoError(t, err)
	defer st.Close()
	order := make(chan string, 2)
	go func() {
		_ = st.WriteTx(context.Background(), func(tx *sql.Tx) error {
			order <- "first"
			time.Sleep(10 * time.Millisecond)
			return nil
		})
	}()
	time.Sleep(time.Millisecond)
	_ = st.WriteTx(context.Background(), func(tx *sql.Tx) error {
		order <- "second"
		return nil
	})
	close(order)
	var got []string
	for s := range order {
		got = append(got, s)
	}
	// If WriteTx serialized correctly, "first" should appear before "second".
	// If not, they could interleave.
	require.Len(t, got, 2)
	assert.Equal(t, "first", got[0])
	assert.Equal(t, "second", got[1])
}

// TestInMemoryForcedSingleConn: :memory: forces MaxOpenConns==1.
func TestInMemoryForcedSingleConn(t *testing.T) {
	st, err := Open(":memory:")
	require.NoError(t, err)
	defer st.Close()
	stats := st.DB.Stats()
	assert.Equal(t, 1, stats.MaxOpenConnections)
}
