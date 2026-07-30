package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/secrets"
)

func TestWS_RedactsOutboundFrames(t *testing.T) {
	r := secrets.NewRedactor()
	secret := `sk-ws-"quoted"-\slash`
	r.Register(secret)

	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, err := upgrader.Upgrade(w, req, nil)
		require.NoError(t, err)
		defer raw.Close()
		wc := &wsConn{Conn: raw, redactor: r}
		wc.write(proto.NewError("leaked: " + secret))
	}))
	defer srv.Close()

	c := dial(t, "ws"+strings.TrimPrefix(srv.URL, "http"))
	defer c.Close()
	_, data, err := c.ReadMessage()
	require.NoError(t, err)
	require.NotContains(t, string(data), secret)
	require.NotContains(t, string(data), `sk-ws-\"quoted\"-\\slash`)
	require.Contains(t, string(data), "[REDACTED]")
}
