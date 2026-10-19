package remotedialer

import (
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestConcurrentHeartbeatAndDataWriter(t *testing.T) {
	finished := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(rw, r, nil)
		if err != nil {
			finished <- err
			return
		}
		defer conn.Close()
		wrapper := newWSConn(conn)
		written := make(chan error, 1)
		go func() {
			for range 1000 {
				if err := wrapper.WriteMessage(websocket.BinaryMessage, time.Now().Add(time.Second), []byte("data")); err != nil {
					written <- err
					return
				}
			}
			written <- nil
		}()
		for {
			kind, reader, err := wrapper.NextReader()
			if err != nil {
				finished <- err
				return
			}
			io.Copy(io.Discard, reader)
			if kind == websocket.TextMessage {
				finished <- <-written
				return
			}
		}
	}))
	defer server.Close()
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			_, reader, err := client.NextReader()
			if err != nil {
				return
			}
			io.Copy(io.Discard, reader)
		}
	}()
	for range 1000 {
		if err = client.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if err = client.WriteMessage(websocket.TextMessage, []byte("done")); err != nil {
		t.Fatal(err)
	}
	if err = <-finished; err != nil {
		t.Fatal(err)
	}
	client.Close()
	<-readDone
}
func TestTimedOutWriteReleasesWriter(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(rw, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		<-release
	}))
	defer server.Close()
	defer close(release)
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	wrapper := newWSConn(client)
	err = wrapper.WriteMessage(websocket.BinaryMessage, time.Now().Add(30*time.Millisecond), make([]byte, 32<<20))
	if err == nil {
		t.Fatal("blocked writer ignored deadline")
	}
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) {
		if wrapper.TryLock() {
			wrapper.Unlock()
			return
		}
		runtime.Gosched()
	}
	t.Fatal("timed-out write left a background writer holding the mutex")
}
