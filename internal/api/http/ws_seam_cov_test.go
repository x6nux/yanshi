package http

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/vcs"
)

// newWSPair creates a WS pair: a server-side *wsConn and a client-side
// *websocket.Conn. The server-side handler stores the upgraded wsConn into the
// returned channel so the test can read it after the client dials.
// The cleanup function closes both sides and shuts down the test server.
func newWSPair(t *testing.T) (*wsConn, *websocket.Conn, func()) {
	t.Helper()
	serverCh := make(chan *wsConn, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverCh <- &wsConn{Conn: raw}
		// Keep the connection alive (reads absorbed; writes unaffected).
		for {
			if _, _, err := raw.ReadMessage(); err != nil {
				return
			}
		}
	}))

	dialer := websocket.Dialer{}
	client, _, err := dialer.Dial("ws://"+srv.Listener.Addr().String()+"/ws", nil)
	require.NoError(t, err)
	server := <-serverCh

	cleanup := func() {
		client.Close()
		srv.Close()
	}
	return server, client, cleanup
}

// TestCov_RestoreTurn_VcsNil covers the "VCS disabled" early return branch (98-101).
func TestCov_RestoreTurn_VcsNil(t *testing.T) {
	wc, client, cleanup := newWSPair(t)
	defer cleanup()

	handleRestoreTurn(&Server{}, wc, nil, "", "")

	_, msg, err := client.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(msg), "restore_turn")

	_, msg, err = client.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(msg), "done")
}

// TestCov_RestoreTurn_HeadMismatch covers the "head changed since listing"
// branch (110-116) by creating a real VCS+repo and passing a mismatched
// confirmedHead.
func TestCov_RestoreTurn_HeadMismatch(t *testing.T) {
	db, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	vv := vcs.New(db, t.TempDir())
	repoID, err := vv.InitRepo(t.TempDir())
	require.NoError(t, err)

	wc, client, cleanup := newWSPair(t)
	defer cleanup()

	s := &Server{vcs: vv, repoID: repoID}
	handleRestoreTurn(s, wc, nil, "seam-id", "fake-head-different")

	_, msg, err := client.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(msg), "head changed")

	_, msg, err = client.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(msg), "done")
}

// TestCov_RestoreTurn_FindSeamError covers the seam-not-found branch (119-124)
// by passing a valid VCS+repo (head check passes) but a non-existent seamID.
func TestCov_RestoreTurn_FindSeamError(t *testing.T) {
	db, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	vv := vcs.New(db, t.TempDir())
	repoID, err := vv.InitRepo(t.TempDir())
	require.NoError(t, err)

	wc, client, cleanup := newWSPair(t)
	defer cleanup()

	s := &Server{vcs: vv, repoID: repoID}
	// Pass the actual fullHead as confirmedHead so the head-mismatch check
	// passes, then "no-such-seam" triggers FindSeam → sql.ErrNoRows.
	handleRestoreTurn(s, wc, nil, "no-such-seam", s.fullHead())

	_, msg, err := client.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(msg), "seam not found")

	_, msg, err = client.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(msg), "done")
}

// TestCov_RestoreTurn_MissingConfirmedHead covers the "confirmed_head is
// empty" branch (105-108). A non-nil vcs+repoID+seamID is required to pass the
// 98 early-exit, but no vcs methods are called before the branch returns.
func TestCov_RestoreTurn_MissingConfirmedHead(t *testing.T) {
	wc, client, cleanup := newWSPair(t)
	defer cleanup()

	s := &Server{vcs: &vcs.VCS{}, repoID: "proj"}
	handleRestoreTurn(s, wc, nil, "seam-id", "")

	_, msg, err := client.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(msg), "confirmed_head")
}

// TestCov_ListSeams_StoreError covers the ListSeams error branch at
// ws_seam.go:63-66 by closing the DB before the call.
func TestCov_ListSeams_StoreError(t *testing.T) {
	db, err := store.Open(":memory:")
	require.NoError(t, err)
	db.Close()

	wc, client, cleanup := newWSPair(t)
	defer cleanup()

	s := &Server{vcs: vcs.New(db, t.TempDir()), repoID: "test-repo"}
	cs := &connSession{sessionID: "test-session"}
	handleListSeams(s, wc, cs)

	_, msg, err := client.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(msg), "list_seams")
}
