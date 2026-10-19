package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/XUJiahua/tunnel-mvp/internal/upgrade"
	"github.com/spf13/cobra"
	"os"
	"time"
)

func addUpgradeCommands(agent *cobra.Command, configPath *string) {
	var o upgrade.Options
	supervise := &cobra.Command{Use: "supervise", Short: "Run an Agent with locally pinned signed updates and automatic readiness rollback", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		o.Config = *configPath
		o.Output = cmd.ErrOrStderr()
		return upgrade.Supervise(cmd.Context(), o)
	}}
	supervise.Flags().Uint64Var(&o.MinimumSequence, "minimum-sequence", 0, "Local release sequence floor; persists across rollback")
	supervise.Flags().StringVar(&o.Directory, "update-dir", "", "Private directory for managed Agent binaries and journal")
	supervise.Flags().StringVar(&o.PublicKey, "release-public-key", "", "Base64 Ed25519 release public key file (pinned locally)")
	supervise.Flags().StringVar(&o.ManifestURL, "manifest-url", "", "Direct HTTPS signed manifest URL; no redirects")
	supervise.Flags().StringVar(&o.Bootstrap, "bootstrap-binary", "", "Locally trusted executable used only for a new directory")
	supervise.Flags().DurationVar(&o.Poll, "update-interval", time.Minute, "Signed update polling interval (at least 10s)")
	supervise.Flags().DurationVar(&o.ReadyTimeout, "ready-timeout", 30*time.Second, "New Agent tunnel readiness deadline (5s–5m)")
	supervise.Flags().BoolVar(&o.WatchConfig, "watch-config", false, "Pass local ACL configuration watching to the Agent child")
	supervise.PreRunE = func(cmd *cobra.Command, _ []string) error {
		for _, name := range []string{"site-id", "instance-id", "server-url", "remote-policy", "allow"} {
			if f := cmd.Flag(name); f != nil && f.Changed {
				return fmt.Errorf("supervisor Agent settings must use the config file or environment")
			}
		}
		if o.Poll < 10*time.Second || o.ReadyTimeout < 5*time.Second || o.ReadyTimeout > 5*time.Minute {
			return fmt.Errorf("invalid update interval or readiness deadline")
		}
		return nil
	}
	agent.AddCommand(supervise)
	var dir string
	rollback := &cobra.Command{Use: "rollback", Short: "Restore the verified previous Agent executable while the supervisor is stopped", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		m, e := upgrade.Open(dir, "", "")
		if e != nil {
			return e
		}
		defer m.Close()
		if e = m.Rollback(); e != nil {
			return e
		}
		return output(cmd, m.Status())
	}}
	rollback.Flags().StringVar(&dir, "update-dir", "", "Managed Agent directory (supervisor must be stopped)")
	agent.AddCommand(rollback)
}
func releaseManifestCommand() *cobra.Command {
	var input, key, outputPath string
	cmd := &cobra.Command{Use: "sign-update-manifest", Short: "Sign a prepared release manifest with a local Ed25519 private key", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		raw, e := os.ReadFile(input)
		if e != nil {
			return e
		}
		var manifest upgrade.Manifest
		if e = json.Unmarshal(raw, &manifest); e != nil {
			return e
		}
		raw, e = os.ReadFile(key)
		if e != nil {
			return e
		}
		private, e := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(raw)))
		if e != nil {
			return e
		}
		signed, e := upgrade.Sign(manifest, ed25519.PrivateKey(private))
		if e != nil {
			return e
		}
		f, e := os.OpenFile(outputPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		_, e = f.Write(signed)
		if e == nil {
			e = f.Sync()
		}
		ce := f.Close()
		if e == nil {
			e = ce
		}
		return e
	}}
	cmd.Flags().StringVar(&input, "input", "", "Manifest JSON containing sequence, version, expiry and platform artifacts")
	cmd.Flags().StringVar(&key, "private-key", "", "Base64 Ed25519 private key file (64 bytes); never uploaded")
	cmd.Flags().StringVar(&outputPath, "output", "", "New signed manifest output file (no overwrite)")
	return cmd
}

func releaseKeyCommand() *cobra.Command {
	var privatePath, publicPath string
	cmd := &cobra.Command{Use: "generate-update-key", Short: "Create a local Ed25519 release key pair without displaying private material", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if privatePath == "" || publicPath == "" || privatePath == publicPath {
			return fmt.Errorf("distinct private and public output paths required")
		}
		for _, p := range []string{privatePath, publicPath} {
			if _, e := os.Lstat(p); !os.IsNotExist(e) {
				return fmt.Errorf("key output already exists or cannot be checked")
			}
		}
		pub, key, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			return e
		}
		for _, v := range []struct {
			path string
			data []byte
		}{{privatePath, key}, {publicPath, pub}} {
			f, e := os.OpenFile(v.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if e != nil {
				return e
			}
			_, e = f.WriteString(base64.StdEncoding.EncodeToString(v.data) + "\n")
			if e == nil {
				e = f.Sync()
			}
			ce := f.Close()
			if e == nil {
				e = ce
			}
			if e != nil {
				return e
			}
		}
		return nil
	}}
	cmd.Flags().StringVar(&privatePath, "private-output", "", "New private key file (keep offline)")
	cmd.Flags().StringVar(&publicPath, "public-output", "", "New public key file to pin on Agent hosts")
	return cmd
}
