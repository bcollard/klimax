package cli

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/bcollard/klimax/internal/routing"
	"github.com/bcollard/klimax/internal/vm"
	"github.com/spf13/cobra"
)

func newDownCmd() *cobra.Command {
	var removeRoute bool
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Stop the VM",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDown(cmd.Context(), removeRoute)
		},
	}
	cmd.Flags().BoolVar(&removeRoute, "remove-route", false, "Also remove the macOS host route (requires sudo)")
	return cmd
}

func runDown(ctx context.Context, removeRoute bool) error {
	cfg, err := loadAndValidate()
	if err != nil {
		return err
	}

	if removeRoute {
		if err := routing.DeleteRoute(cfg.Network.KindBridgeCIDR); err != nil {
			slog.Warn("Failed to delete route (continuing)", "err", err)
		}
	}

	mgr := vm.New(cfg.VM.Name, KlimaxHome())
	if err := mgr.Stop(ctx); err != nil {
		return fmt.Errorf("stopping VM: %w", err)
	}

	slog.Info("klimax down complete", "vm", cfg.VM.Name)
	return nil
}
