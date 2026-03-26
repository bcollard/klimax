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
	var keepRoute bool
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Stop the VM (optionally remove the macOS route)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDown(cmd.Context(), keepRoute)
		},
	}
	cmd.Flags().BoolVar(&keepRoute, "keep-route", false, "Do not remove the macOS route on stop")
	return cmd
}

func runDown(ctx context.Context, keepRoute bool) error {
	cfg, err := loadAndValidate()
	if err != nil {
		return err
	}

	if !keepRoute {
		if err := routing.DeleteRoute(cfg.Network.KindBridgeCIDR); err != nil {
			slog.Warn("Failed to delete route (continuing)", "err", err)
		}
	}

	mgr := vm.New(cfg.VM.Name)
	if err := mgr.Stop(ctx); err != nil {
		return fmt.Errorf("stopping VM: %w", err)
	}

	slog.Info("klimax down complete", "vm", cfg.VM.Name)
	return nil
}
