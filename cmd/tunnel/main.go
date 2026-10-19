package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/XUJiahua/tunnel-mvp/internal/agent"
	"github.com/XUJiahua/tunnel-mvp/internal/config"
	"github.com/XUJiahua/tunnel-mvp/internal/control"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if err := run(); err != nil {
		slog.Error("tunnel stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: tunnel <server|agent> -config config.json; tunnel version")
	}
	if os.Args[1] == "version" || os.Args[1] == "--version" {
		fmt.Printf("tunnel %s (commit: %s, built: %s)\n", version, commit, date)
		return nil
	}
	f := flag.NewFlagSet(os.Args[1], flag.ContinueOnError)
	path := f.String("config", "", "JSON configuration file")
	if err := f.Parse(os.Args[2:]); err != nil {
		return err
	}
	if *path == "" {
		return fmt.Errorf("-config required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch os.Args[1] {
	case "server":
		var c config.Server
		if err := config.Load(*path, &c); err != nil {
			return err
		}
		if err := c.Validate(); err != nil {
			return err
		}
		s, err := control.New(c, os.Getenv("TUNNEL_ADMIN_PASSWORD"))
		if err != nil {
			return err
		}
		defer s.Close()
		return s.Run(ctx)
	case "agent":
		var c config.Agent
		if err := config.Load(*path, &c); err != nil {
			return err
		}
		return agent.Run(ctx, c, os.Getenv("TUNNEL_AGENT_TOKEN"))
	default:
		return fmt.Errorf("unknown command: %s", os.Args[1])
	}
}
