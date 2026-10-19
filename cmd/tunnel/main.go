package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/XUJiahua/tunnel-mvp/internal/agent"
	"github.com/XUJiahua/tunnel-mvp/internal/config"
	"github.com/XUJiahua/tunnel-mvp/internal/control"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmd := newRootCommand()
	cmd.SetArgs(legacyArgs(os.Args[1:]))
	if err := cmd.ExecuteContext(ctx); err != nil {
		slog.Error("tunnel stopped", "error", err)
		os.Exit(1)
	}
}
func legacyArgs(args []string) []string {
	out := append([]string{}, args...)
	for i, a := range out {
		if a == "--" {
			break
		}
		if a == "-config" {
			out[i] = "--config"
		} else if strings.HasPrefix(a, "-config=") {
			out[i] = "-" + a
		}
	}
	return out
}
func output(cmd *cobra.Command, v any) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
func logging(c config.Logging) {
	levels := map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError}
	options := &slog.HandlerOptions{Level: levels[c.LogLevel]}
	if c.LogFormat == "json" {
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, options)))
		logrus.SetFormatter(&logrus.JSONFormatter{})
	} else {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, options)))
		logrus.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})
	}
	level, _ := logrus.ParseLevel(c.LogLevel)
	logrus.SetLevel(level)
}
func newRootCommand() *cobra.Command {
	var path, level, format string
	build := fmt.Sprintf("%s (commit: %s, built: %s)", version, commit, date)
	root := &cobra.Command{Use: "tunnel", Short: "Access private web applications through outbound site tunnels", Version: build, SilenceUsage: true, SilenceErrors: true}
	root.SetVersionTemplate("tunnel {{.Version}}\n")
	root.PersistentFlags().StringVarP(&path, "config", "c", "", "JSON/YAML config file (or TUNNEL_CONFIG)")
	root.PersistentFlags().StringVar(&level, "log-level", "info", "Log level: debug, info, warn, error (or TUNNEL_LOG_LEVEL)")
	root.PersistentFlags().StringVar(&format, "log-format", "text", "Log format: text, json (or TUNNEL_LOG_FORMAT)")
	server := &cobra.Command{Use: "server", Short: "Run the public control plane and access gateway", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := config.ServerFrom(path, cmd.Flags())
		if err != nil {
			return err
		}
		logging(c.Logging)
		check, _ := cmd.Flags().GetBool("check-config")
		if check {
			return output(cmd, c)
		}
		s, err := control.New(c, os.Getenv("TUNNEL_ADMIN_PASSWORD"))
		if err != nil {
			return err
		}
		defer s.Close()
		return s.Run(cmd.Context())
	}}
	server.Flags().Int("probe-interval", 60, "Automatic HEAD probe interval in seconds; 0 disables")
	server.Flags().Int("audit-max-mb", 20, "Audit file rotation size in MiB")
	server.Flags().Int("audit-backups", 5, "Number of rotated audit files to retain")
	server.Flags().String("listen", ":8080", "HTTP listen address")
	server.Flags().String("public-url", "http://localhost:8080", "Public console origin")
	server.Flags().String("access-url", "http://apps.localhost:8080", "Application domain origin")
	server.Flags().String("storage", "json", "State storage backend: json or sqlite")
	server.Flags().String("data-dir", "data", "Persistent state and audit directory")
	server.Flags().String("admin-username", "admin", "First-start administrator username")
	server.Flags().Bool("check-config", false, "Validate and print effective configuration without starting")
	agentCommand := &cobra.Command{Use: "agent", Short: "Connect an internal site to the public control plane", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := config.AgentFrom(path, cmd.Flags())
		if err != nil {
			return err
		}
		logging(c.Logging)
		if _, err = agent.NewPolicy(c.Allow); err != nil {
			return err
		}
		check, _ := cmd.Flags().GetBool("check-config")
		if check {
			return output(cmd, c)
		}
		watch, _ := cmd.Flags().GetBool("watch-config")
		if watch {
			if path == "" && os.Getenv("TUNNEL_CONFIG") == "" {
				return fmt.Errorf("--watch-config requires --config or TUNNEL_CONFIG")
			}
			return agent.RunWatched(cmd.Context(), c, os.Getenv("TUNNEL_AGENT_TOKEN"), func() (config.Agent, error) { return config.AgentFrom(path, cmd.Flags()) })
		}
		return agent.Run(cmd.Context(), c, os.Getenv("TUNNEL_AGENT_TOKEN"))
	}}
	agentCommand.PersistentFlags().String("server-url", "http://localhost:8080", "Public control plane origin")
	agentCommand.PersistentFlags().String("site-id", "", "Registered site ID")
	agentCommand.PersistentFlags().String("allow", "", "Agent ACL rules as a JSON array (or TUNNEL_ALLOW)")
	agentCommand.Flags().Bool("watch-config", false, "Reload valid ACL file changes every 2s; reconnects the tunnel")
	agentCommand.Flags().Bool("check-config", false, "Validate and print effective configuration without starting")
	var target string
	diagnose := &cobra.Command{Use: "diagnose", Short: "Test a target using local Agent DNS/ACL/TCP/HTTP (no token needed)", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := config.AgentFrom(path, cmd.Flags())
		if err != nil {
			return err
		}
		logging(c.Logging)
		result, err := agent.Diagnose(cmd.Context(), c, target)
		if err != nil {
			return err
		}
		if err = output(cmd, result); err != nil {
			return err
		}
		if !result.Reachable {
			return fmt.Errorf("%s: %s", result.Failure.Code, result.Failure.Message)
		}
		return nil
	}}
	diagnose.Flags().StringVar(&target, "target", "", "HTTP/HTTPS origin to diagnose")
	_ = diagnose.MarkFlagRequired("target")
	agentCommand.AddCommand(diagnose)
	root.AddCommand(storageCommand(&path), server, agentCommand, &cobra.Command{Use: "version", Short: "Print build version", Args: cobra.NoArgs, Run: func(cmd *cobra.Command, _ []string) { fmt.Fprintln(cmd.OutOrStdout(), "tunnel "+build) }})
	return root
}
