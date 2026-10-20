package agent

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/rancher/remotedialer"
)

// connectTunnel owns shutdown ordering. The upstream convenience client calls
// Session.Close before its Serve goroutine has necessarily initialized pings,
// racing pingCancel/WaitGroup when cancellation follows a successful handshake.
func connectTunnel(ctx context.Context, target string, headers http.Header, dialer *websocket.Dialer, localDialer remotedialer.Dialer, onConnected func()) error {
	ws, response, err := dialer.DialContext(ctx, target, headers)
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err != nil {
		if response != nil {
			return fmt.Errorf("tunnel handshake %s: %w", response.Status, err)
		}
		return err
	}
	serveCtx, cancel := context.WithCancel(ctx)
	session := remotedialer.NewClientSessionWithDialer(func(proto, address string) bool { return proto == "tcp" || proto == "tcp4" || proto == "tcp6" }, ws, localDialer)
	done := make(chan error, 1)
	go func() { _, err := session.Serve(serveCtx); done <- err }()
	onConnected()
	served := false
	select {
	case <-ctx.Done():
		err = nil
	case err = <-done:
		served = true
	}
	cancel()
	ws.Close() // Interrupt NextReader before joining the receiver.
	if !served {
		<-done
	}
	session.Close() // Serve can no longer race startPings with stopPings.
	return err
}
