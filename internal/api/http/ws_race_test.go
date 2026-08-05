package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/tools"
)

func TestWSConnWrite_ConcurrentNoRace(t *testing.T) {
	var serverConn *wsConn
	ready := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		raw, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverConn = &wsConn{Conn: raw}
		close(ready)
		for {
			_, _, err := raw.ReadMessage()
			if err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	u := "ws" + srv.URL[4:]
	dialer := websocket.DefaultDialer
	client, _, err := dialer.DialContext(context.Background(), u, nil)
	require.NoError(t, err)
	defer client.Close()

	<-ready

	const n = 16
	const m = 50
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < m; j++ {
				f := proto.ServerFrame{Type: "test", Text: "race-test"}
				serverConn.write(f)
			}
		}(i)
	}
	wg.Wait()
}

func TestPermTracker_RegisterTakeDeliverConcurrent(t *testing.T) {
	pt := newPermTracker()
	ch := make(chan tools.PermissionDecision, 1)

	id := pt.newID()
	pt.register(id, ch, tools.PermissionRequest{}, guard.ModeDefault)

	const n = 20
	var wg sync.WaitGroup
	ids := make([]string, n)
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ids[idx] = pt.newID()
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		pt.deliver(id, tools.PermissionAllow, guard.ModeDefault)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		if taken, ok := pt.take(id); ok {
			select {
			case <-taken.ch:
			default:
			}
		}
	}()
	wg.Wait()

	seen := map[string]bool{}
	for _, x := range ids {
		if x == "" {
			continue
		}
		if seen[x] {
			t.Fatalf("dup id: %s", x)
		}
		seen[x] = true
	}

	_, _ = pt.take("nonexistent")
	pt.deliver("nonexistent", tools.PermissionAllow, guard.ModeDefault)
}

func TestConnSession_ConcurrentFrameInterleaving(t *testing.T) {
	_, s := newSessionTestServer(t)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	c := dial(t, dialWSURL(t, ts))
	defer c.Close()

	frameCh := make(chan proto.ClientFrame, 16)
	var writeWG sync.WaitGroup
	writeWG.Add(1)
	go func() {
		defer writeWG.Done()
		for cf := range frameCh {
			_ = c.WriteJSON(cf)
		}
	}()

	for i := 0; i < 40; i++ {
		frameCh <- proto.NewUserMessage("interleave turn")
		frameCh <- proto.ClientFrame{Type: "set_mode", Mode: "yolo"}
		frameCh <- proto.ClientFrame{Type: "set_mode", Mode: "default"}
		frameCh <- proto.NewCancel()
	}
	close(frameCh)
	writeWG.Wait()

	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		if _, _, err := c.ReadMessage(); err != nil {
			break
		}
	}
}
