package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

func newRegistryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "registry",
		Short: "Manage registry mirrors",
	}
	cmd.AddCommand(newRegistryCleanCacheCmd())
	return cmd
}

func newRegistryCleanCacheCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "clean-cache",
		Short: "Remove all registry mirror cache data and their containers",
		Long: `Stops and removes all mirror containers, then deletes their cached blob data.

Run 'klimax up' afterwards to restart the registries fresh.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRegistryCleanCache(cmd.Context())
		},
	}
}

func runRegistryCleanCache(ctx context.Context) error {
	cfg, g, err := connectToRunningVM(ctx)
	if err != nil {
		return err
	}

	for _, m := range cfg.Registries.Mirrors {
		fmt.Printf("Cleaning cache for mirror %q...\n", m.Name)

		// Stop and remove the container (best-effort).
		if _, err := g.Run(ctx, fmt.Sprintf("docker rm -f %s 2>/dev/null || true", m.Name)); err != nil {
			slog.Warn("Failed to remove mirror container", "name", m.Name, "err", err)
		}

		// Delete the cache directory.
		if cfg.Registries.CacheStorage == "guest" {
			dir := "/var/lib/klimax/registry-cache/" + m.Name
			if _, err := g.Run(ctx, fmt.Sprintf("rm -rf %q", dir)); err != nil {
				slog.Warn("Failed to remove guest cache dir", "dir", dir, "err", err)
			}
		} else {
			home, _ := os.UserHomeDir()
			dir := filepath.Join(home, ".klimax", "registry-cache", m.Name)
			if err := os.RemoveAll(dir); err != nil {
				slog.Warn("Failed to remove host cache dir", "dir", dir, "err", err)
			}
		}
	}

	fmt.Println("Cache cleared. Run 'klimax up' to restart the registries.")
	return nil
}
