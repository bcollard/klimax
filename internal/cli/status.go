package cli

import (
	"context"
	"fmt"

	"github.com/bcollard/klimax/internal/guest"
	"github.com/bcollard/klimax/internal/kind"
	"github.com/bcollard/klimax/internal/routing"
	"github.com/bcollard/klimax/internal/vm"
	"github.com/lima-vm/lima/v2/pkg/limatype"
	"github.com/spf13/cobra"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show VM state, clusters, route, and iptables rule presence",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStatus(cmd.Context())
		},
	}
}

func runStatus(ctx context.Context) error {
	cfg, err := loadAndValidate()
	if err != nil {
		return err
	}

	mgr := vm.New(cfg.VM.Name)
	inst, err := mgr.Inspect(ctx)
	if err != nil {
		return fmt.Errorf("inspecting VM: %w", err)
	}

	// VM section
	fmt.Println("=== VM ===")
	if inst == nil {
		fmt.Println("  status: not created")
	} else {
		fmt.Printf("  name:   %s\n", inst.Name)
		fmt.Printf("  status: %s\n", inst.Status)
		fmt.Printf("  cpus:   %d\n", inst.CPUs)
		fmt.Printf("  memory: %d MB\n", inst.Memory/1024/1024)
	}

	// Route section
	fmt.Println("\n=== macOS Route ===")
	if routing.RouteExists(cfg.Network.KindBridgeCIDR) {
		fmt.Printf("  %s → present\n", cfg.Network.KindBridgeCIDR)
	} else {
		fmt.Printf("  %s → MISSING\n", cfg.Network.KindBridgeCIDR)
	}

	// Clusters + iptables (only if VM is running)
	if inst == nil || inst.Status != limatype.StatusRunning {
		fmt.Println("\n=== Kind Clusters ===")
		fmt.Println("  (VM is not running)")
		return nil
	}

	g, err := guest.NewClient(inst)
	if err != nil {
		return fmt.Errorf("guest SSH: %w", err)
	}

	fmt.Println("\n=== Kind Clusters ===")
	clusters, err := kind.ListClusters(ctx, g)
	if err != nil {
		fmt.Printf("  error: %v\n", err)
	} else if len(clusters) == 0 {
		fmt.Println("  (none)")
	} else {
		for _, c := range clusters {
			fmt.Printf("  - %s\n", c)
		}
	}

	fmt.Println("\n=== IPTables (no-NAT rule) ===")
	ok, err := routing.CheckNoNatRule(ctx, g, cfg.Network.KindBridgeCIDR)
	if err != nil {
		fmt.Printf("  error: %v\n", err)
	} else if ok {
		fmt.Println("  nat POSTROUTING exemption → present")
	} else {
		fmt.Println("  nat POSTROUTING exemption → MISSING")
	}

	return nil
}
