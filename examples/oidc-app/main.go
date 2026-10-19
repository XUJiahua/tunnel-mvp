package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var c Config
	listen := flag.String("listen", "127.0.0.1:9090", "Private application listen address")
	flag.StringVar(&c.PublicURL, "public-url", os.Getenv("OIDC_APP_PUBLIC_URL"), "Public Tunnel application URL, including mount path")
	flag.StringVar(&c.Issuer, "issuer", os.Getenv("OIDC_APP_ISSUER"), "OIDC issuer (HTTPS except loopback)")
	flag.StringVar(&c.ClientID, "client-id", os.Getenv("OIDC_APP_CLIENT_ID"), "Dedicated application OIDC client ID")
	flag.Parse()
	c.ClientSecret = os.Getenv("OIDC_APP_CLIENT_SECRET")
	app, e := NewApp(ctx, c)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	server := &http.Server{Addr: *listen, Handler: app.Handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 64 << 10, BaseContext: func(net.Listener) context.Context { return ctx }}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
	case e := <-done:
		if e != nil && e != http.ErrServerClosed {
			fmt.Fprintln(os.Stderr, "Application server failed:", e)
			os.Exit(1)
		}
	}
}
