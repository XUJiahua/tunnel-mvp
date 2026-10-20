package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func TestTunnelImmediateCancellationAfterHandshake(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	for range 100 {
		ctx, cancel := context.WithCancel(context.Background())
		err := connectTunnel(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil, websocket.DefaultDialer, nil, cancel)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}
}
