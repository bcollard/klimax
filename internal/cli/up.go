package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/bcollard/klimax/internal/config"
	"github.com/bcollard/klimax/internal/docker"
	"github.com/bcollard/klimax/internal/guest"
	"github.com/bcollard/klimax/internal/registry"
	"github.com/bcollard/klimax/internal/routing"
	"github.com/bcollard/klimax/internal/vm"
	"github.com/spf13/cobra"
)

func newUpCmd() *cobra.Command {
	var showVMLogs bool
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Create/start the VM, provision Docker, create kind clusters, set up routing",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUp(cmd.Context(), showVMLogs)
		},
	}
	cmd.Flags().BoolVar(&showVMLogs, "show-vm-logs", false, "Stream Lima host-agent logs and cloud-init progress to stderr during startup")
	return cmd
}

func runUp(ctx context.Context, showVMLogs bool) error {
	if err := ensureConfig(); err != nil {
		return err
	}
	cfg, err := loadAndValidate()
	if err != nil {
		return err
	}

	// 1. Ensure VM is running.
	mgr := vm.New(cfg.VM.Name, KlimaxHome())
	inst, err := mgr.EnsureRunning(ctx, cfg, showVMLogs)
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

// ensureConfig creates a default config file at configFile if it does not exist.
func ensureConfig() error {
	if _, err := os.Stat(configFile); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(configFile), 0o750); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}
	if err := config.WriteDefaultConfig(configFile); err != nil {
		return fmt.Errorf("writing default config: %w", err)
	}
	fmt.Printf("No config found — created default config at %s\n", configFile)
	fmt.Printf("Edit it to customise VM resources, then re-run 'klimax up'.\n\n")
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
