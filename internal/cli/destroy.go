package cli

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/bcollard/klimax/internal/guest"
	"github.com/bcollard/klimax/internal/kind"
	"github.com/bcollard/klimax/internal/routing"
	"github.com/bcollard/klimax/internal/vm"
	"github.com/spf13/cobra"
)

func newDestroyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "destroy",
		Short: "Delete kind clusters, delete the VM, and remove the macOS route",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDestroy(cmd.Context())
		},
	}
}

func runDestroy(ctx context.Context) error {
	cfg, err := loadAndValidate()
	if err != nil {
		return err
	}

	mgr := vm.New(cfg.VM.Name, KlimaxHome())

	// Delete kind clusters first (while VM is still running).
	inst, err := mgr.Inspect(ctx)
	if err != nil {
		return fmt.Errorf("inspecting VM: %w", err)
	}
	if inst != nil && inst.Status == "Running" {
		g, err := guest.NewClient(inst)
		if err != nil {
			return fmt.Errorf("guest SSH: %w", err)
		}
		clusters, err := kind.ListClusters(ctx, g)
		if err != nil {
			slog.Warn("Could not list kind clusters (continuing)", "err", err)
		}
		for _, name := range clusters {
			if err := kind.DeleteCluster(ctx, g, name); err != nil {
				slog.Warn("Failed to delete kind cluster (continuing)", "cluster", name, "err", err)
			}
		}
	}

	// Delete the VM.
	if err := mgr.Delete(ctx); err != nil {
		return fmt.Errorf("deleting VM: %w", err)
	}

	// Remove macOS route.
	if err := routing.DeleteRoute(cfg.Network.KindBridgeCIDR); err != nil {
		slog.Warn("Failed to delete route (continuing)", "err", err)
	}

	slog.Info("klimax destroy complete", "vm", cfg.VM.Name)
	return nil
}
