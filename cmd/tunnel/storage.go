package main

import (
	"fmt"
	"os"

	"github.com/XUJiahua/tunnel-mvp/internal/config"
	"github.com/XUJiahua/tunnel-mvp/internal/state"
	"github.com/spf13/cobra"
)

func storageCommand(path *string) *cobra.Command {
	root := &cobra.Command{Use: "storage", Short: "Offline state inspection, migration and portable backup/restore"}
	root.PersistentFlags().String("data-dir", "data", "Persistent state directory; stop Server before maintenance")
	root.PersistentFlags().String("storage", "json", "Backend: json or sqlite (or TUNNEL_STORAGE)")
	effective := func(cmd *cobra.Command) (config.Server, error) { return config.ServerFrom(*path, cmd.Flags()) }
	inspect := &cobra.Command{Use: "inspect", Short: "Validate existing state and print counts without credentials", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := effective(cmd)
		if err != nil {
			return err
		}
		summary, err := state.Inspect(c.DataDir, c.Storage)
		if err != nil {
			return err
		}
		return output(cmd, summary)
	}}
	var exportPath string
	export := &cobra.Command{Use: "export", Short: "Write a checksummed portable backup without overwriting files", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := effective(cmd)
		if err != nil {
			return err
		}
		summary, err := state.Export(c.DataDir, c.Storage, exportPath)
		if err != nil {
			return err
		}
		return output(cmd, summary)
	}}
	export.Flags().StringVar(&exportPath, "output", "", "New private backup file (contains credential hashes)")
	_ = export.MarkFlagRequired("output")
	var from, to string
	migrate := &cobra.Command{Use: "migrate", Short: "Create SQLite from legacy JSON without changing the source", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if from != state.JSON || to != state.SQLite {
			return fmt.Errorf("supported migration is --from json --to sqlite; use export/restore for other conversions")
		}
		c, err := effective(cmd)
		if err != nil {
			return err
		}
		summary, err := state.MigrateJSON(c.DataDir)
		if err != nil {
			return err
		}
		return output(cmd, summary)
	}}
	migrate.Flags().StringVar(&from, "from", "json", "Source backend (json)")
	migrate.Flags().StringVar(&to, "to", "sqlite", "Target backend (sqlite)")
	var restorePath string
	restore := &cobra.Command{Use: "restore", Short: "Restore a verified backup into a fresh data directory", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := effective(cmd)
		if err != nil {
			return err
		}
		summary, err := state.Restore(c.DataDir, c.Storage, restorePath)
		if err != nil {
			return err
		}
		return output(cmd, summary)
	}}
	restore.Flags().StringVar(&restorePath, "input", "", "Checksummed portable backup file")
	_ = restore.MarkFlagRequired("input")
	var validatePath string
	validate := &cobra.Command{Use: "validate", Short: "Verify a backup's format, checksum and resource references", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		summary, err := state.ValidateBackup(validatePath)
		if err != nil {
			return err
		}
		return output(cmd, summary)
	}}
	validate.Flags().StringVar(&validatePath, "input", "", "Backup file to validate")
	_ = validate.MarkFlagRequired("input")
	var recoveryUsername string
	recovery := &cobra.Command{Use: "reset-admin-password", Short: "Offline reset of an existing administrator; password uses TUNNEL_RECOVERY_PASSWORD", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := effective(cmd)
		if err != nil {
			return err
		}
		summary, err := state.ResetAdminPassword(c.DataDir, c.Storage, recoveryUsername, os.Getenv("TUNNEL_RECOVERY_PASSWORD"))
		if err != nil {
			return err
		}
		return output(cmd, summary)
	}}
	recovery.Flags().StringVar(&recoveryUsername, "username", "", "Existing administrator username")
	_ = recovery.MarkFlagRequired("username")
	var exportOpsPath, restoreOpsPath, validateOpsPath string
	exportOps := &cobra.Command{Use: "export-operations", Short: "Offline verified archive of audit history and monitoring database", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := effective(cmd)
		if err != nil {
			return err
		}
		summary, err := state.ExportOperations(c.DataDir, exportOpsPath)
		if err != nil {
			return err
		}
		return output(cmd, summary)
	}}
	exportOps.Flags().StringVar(&exportOpsPath, "output", "", "New private .tar.gz operational archive")
	_ = exportOps.MarkFlagRequired("output")
	restoreOps := &cobra.Command{Use: "restore-operations", Short: "Restore operational archive without overwriting; resumable after interruption", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := effective(cmd)
		if err != nil {
			return err
		}
		summary, err := state.RestoreOperations(c.DataDir, restoreOpsPath)
		if err != nil {
			return err
		}
		return output(cmd, summary)
	}}
	restoreOps.Flags().StringVar(&restoreOpsPath, "input", "", "Verified operational archive; use the same file to resume")
	_ = restoreOps.MarkFlagRequired("input")
	validateOps := &cobra.Command{Use: "validate-operations", Short: "Check archive entries, checksums, gzip footer and monitoring integrity", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		summary, err := state.ValidateOperations(validateOpsPath)
		if err != nil {
			return err
		}
		return output(cmd, summary)
	}}
	validateOps.Flags().StringVar(&validateOpsPath, "input", "", "Operational archive to validate")
	_ = validateOps.MarkFlagRequired("input")
	root.AddCommand(exportOps, restoreOps, validateOps)
	root.AddCommand(inspect, export, migrate, restore, validate, recovery)
	return root
}
