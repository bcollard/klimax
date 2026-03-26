package cli

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/bcollard/klimax/internal/config"
	"github.com/bcollard/klimax/internal/docker"
	"github.com/bcollard/klimax/internal/guest"
	"github.com/bcollard/klimax/internal/registry"
	"github.com/bcollard/klimax/internal/routing"
	"github.com/bcollard/klimax/internal/vm"
	"github.com/spf13/cobra"
)

func newUpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "up",
		Short: "Create/start the VM, provision Docker, create kind clusters, set up routing",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUp(cmd.Context())
		},
	}
}

func runUp(ctx context.Context) error {
	cfg, err := loadAndValidate()
	if err != nil {
		return err
	}

	// 1. Ensure VM is running.
	mgr := vm.New(cfg.VM.Name)
	inst, err := mgr.EnsureRunning(ctx, cfg)
	if err != nil {
		return fmt.Errorf("vm: %w", err)
	}
	slog.Info("VM is running", "name", inst.Name, "sshPort", inst.SSHLocalPort)

	// 2. Open SSH client.
	g, err := guest.NewClient(inst)
	if err != nil {
		return fmt.Errorf("guest SSH: %w", err)
	}

	// 3. Ensure kind Docker network.
	if err := docker.EnsureKindNetwork(ctx, g, cfg.Network.KindBridgeCIDR); err != nil {
		return fmt.Errorf("docker network: %w", err)
	}

	// 4. Ensure local registry + pull-through mirrors.
	if err := registry.EnsureRegistries(ctx, g, cfg.Registries); err != nil {
		return fmt.Errorf("registries: %w", err)
	}

	// 5. Detect lima0 IP (needed for routing).
	lima0IP, err := routing.Lima0IP(ctx, g)
	if err != nil {
		return fmt.Errorf("detecting lima0 IP: %w", err)
	}

	// 6. Install no-NAT rules + systemd persistence in guest.
	if err := routing.InstallNoNat(ctx, g, cfg.Network.KindBridgeCIDR); err != nil {
		return fmt.Errorf("routing rules: %w", err)
	}

	// 7. Add macOS route for kind CIDR → lima0.
	if err := routing.EnsureRoute(cfg.Network.KindBridgeCIDR, lima0IP); err != nil {
		return fmt.Errorf("macOS route: %w", err)
	}

	slog.Info("klimax up complete",
		"vm", cfg.VM.Name,
		"kindCIDR", cfg.Network.KindBridgeCIDR,
		"lima0IP", lima0IP,
		"dockerSocket", "~/."+cfg.VM.Name+".docker.sock",
	)
	fmt.Printf("\nVM ready.\n  eval $(klimax docker-env)          # use VM Docker daemon\n  klimax cluster create <name>       # create a kind cluster\n\n")
	return nil
}

func loadAndValidate() (*config.Config, error) {
	cfg, err := config.LoadConfig(configFile)
	if err != nil {
		return nil, err
	}
	if err := config.Validate(cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return cfg, nil
}
