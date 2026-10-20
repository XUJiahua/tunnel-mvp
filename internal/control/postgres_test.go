package control

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/XUJiahua/tunnel-mvp/internal/state"
	"github.com/gorilla/websocket"
	"os"
)

func TestPostgresRuntimeFencesRealAgentStreams(t *testing.T) {
	t.Setenv("TUNNEL_TEST_STORAGE", state.Postgres)
	f := setup(t)
	dsn := os.Getenv("TUNNEL_POSTGRES_DSN")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/events" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: ready\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		if r.URL.Path == "/ws" {
			c, e := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
			if e != nil {
				return
			}
			defer c.Close()
			for {
				kind, data, e := c.ReadMessage()
				if e != nil {
					return
				}
				if e = c.WriteMessage(kind, data); e != nil {
					return
				}
			}
		}
		io.WriteString(w, "postgres proxy")
	}))
	defer up.Close()
	site, token := f.site(t, "PostgreSQL site")
	cancelAgent := f.agent(t, site, token, portOf(up.URL))
	defer cancelAgent()
	svc := f.service(t, site, "PostgreSQL app", up.URL)
	cookie, host, _ := f.grant(t, svc, f.cookie)
	res, body := f.request(t, "GET", "/", nil, cookie, host)
	if res.StatusCode != 200 || string(body) != "postgres proxy" {
		t.Fatal("PostgreSQL proxy failed", res.StatusCode, string(body))
	}
	req, _ := http.NewRequest("GET", f.h.URL+"/events", nil)
	req.Host = host
	req.AddCookie(cookie)
	stream, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer stream.Body.Close()
	if stream.StatusCode != 200 {
		t.Fatal("SSE failed", stream.StatusCode)
	}
	if _, e = io.ReadFull(stream.Body, make([]byte, 13)); e != nil {
		t.Fatal(e)
	}
	headers := http.Header{"Host": {host}, "Cookie": {cookie.String()}, "Origin": {f.s.serviceURL(svc.ID).String()}}
	ws, _, e := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(f.h.URL, "http")+"/ws", headers)
	if e != nil {
		t.Fatal(e)
	}
	defer ws.Close()
	if e = ws.WriteMessage(websocket.TextMessage, []byte("ready")); e != nil {
		t.Fatal(e)
	}
	if _, msg, e := ws.ReadMessage(); e != nil || string(msg) != "ready" {
		t.Fatal("WebSocket failed", e)
	}
	pg := f.s.store.Repository.(*state.PostgresRepository)
	db, e := sql.Open("postgres", dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	var killed bool
	e = db.QueryRow(`SELECT pg_terminate_backend(pid) FROM pg_catalog.pg_locks WHERE locktype='advisory' AND classid=1414876748 AND objid=1 AND objsubid=2 AND mode='ExclusiveLock' AND granted AND database=(SELECT oid FROM pg_database WHERE datname=current_database())`).Scan(&killed)
	if e != nil || !killed {
		t.Fatal("could not terminate runtime lease", e)
	}
	select {
	case <-pg.Failure():
	case <-time.After(8 * time.Second):
		t.Fatal("lost lease undetected")
	}
	deadline := time.Now().Add(3 * time.Second)
	for !f.s.storageFenced.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !f.s.storageFenced.Load() {
		t.Fatal("control plane did not fence")
	}
	closed := make(chan error, 1)
	go func() { _, e := stream.Body.Read(make([]byte, 1)); closed <- e }()
	select {
	case e := <-closed:
		if e == nil {
			t.Fatal("SSE survived storage failure")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SSE not canceled")
	}
	_ = ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, e = ws.ReadMessage(); e == nil {
		t.Fatal("WebSocket survived storage failure")
	}
	for _, path := range []string{"/healthz", "/api/me", "/tunnel/connect"} {
		res, _ = f.request(t, "GET", path, nil, f.cookie, "")
		if res.StatusCode != 503 {
			t.Fatalf("%s remains available: %d", path, res.StatusCode)
		}
	}
	if _, e = state.OpenPostgresRuntime(context.Background(), t.TempDir(), dsn, nil); e != state.ErrPostgresRecovery {
		t.Fatal("crash latch bypassed", e)
	}
}
