package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/bcollard/klimax/internal/guest"
	"github.com/bcollard/klimax/internal/kind"
	"github.com/bcollard/klimax/internal/routing"
	"github.com/bcollard/klimax/internal/vm"
	"github.com/lima-vm/lima/v2/pkg/limatype"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// statusReport is the machine-readable shape of `klimax status`. Clusters and
// IPTables are omitted (nil) when the VM is not running, since neither can be
// determined without a guest connection — VM.Status says why.
type statusReport struct {
	VM       statusVM        `json:"vm"                 yaml:"vm"`
	Route    statusRoute     `json:"route"              yaml:"route"`
	Clusters *statusClusters `json:"clusters,omitempty" yaml:"clusters,omitempty"`
	IPTables *statusIPTables `json:"iptables,omitempty" yaml:"iptables,omitempty"`
}

type statusVM struct {
	Name     string `json:"name,omitempty"     yaml:"name,omitempty"`
	Exists   bool   `json:"exists"             yaml:"exists"`
	Status   string `json:"status"             yaml:"status"`
	CPUs     int    `json:"cpus,omitempty"     yaml:"cpus,omitempty"`
	MemoryMB int64  `json:"memoryMB,omitempty" yaml:"memoryMB,omitempty"`
}

type statusRoute struct {
	CIDR    string `json:"cidr"              yaml:"cidr"`
	Present bool   `json:"present"           yaml:"present"`
	Gateway string `json:"gateway,omitempty" yaml:"gateway,omitempty"`
}

type statusClusters struct {
	Names []string `json:"names"           yaml:"names"`
	Error string   `json:"error,omitempty" yaml:"error,omitempty"`
}

type statusIPTables struct {
	NoNATExemption bool   `json:"noNatExemption"  yaml:"noNatExemption"`
	Error          string `json:"error,omitempty" yaml:"error,omitempty"`
}

func newStatusCmd() *cobra.Command {
	var outputFmt string
	cmd := &cobra.Command{
		Use:     "status",
		Aliases: []string{"st", "s"},
		Short:   "Show VM state, clusters, route, and iptables rule presence",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStatus(cmd.Context(), outputFmt)
		},
	}
	cmd.Flags().StringVarP(&outputFmt, "output", "o", "text", "Output format: text, json, yaml")
	return cmd
}

func runStatus(ctx context.Context, outputFmt string) error {
	rep, err := collectStatus(ctx)
	if err != nil {
		return err
	}

	switch outputFmt {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	case "yaml":
		return yaml.NewEncoder(os.Stdout).Encode(rep)
	default: // "text"
		printStatusText(rep)
		return nil
	}
}

// collectStatus gathers every status fact. It performs no privileged operations,
// so it is safe to call on every invocation.
func collectStatus(ctx context.Context) (*statusReport, error) {
	cfg, err := loadAndValidate()
	if err != nil {
		return nil, err
	}

	mgr := vm.New(cfg.VM.Name, KlimaxHome())
	inst, err := mgr.Inspect(ctx)
	if err != nil {
		return nil, fmt.Errorf("inspecting VM: %w", err)
	}

	rep := &statusReport{
		Route: statusRoute{CIDR: cfg.Network.KindBridgeCIDR},
	}

	if inst == nil {
		rep.VM = statusVM{Exists: false, Status: "not created"}
	} else {
		rep.VM = statusVM{
			Name:     inst.Name,
			Exists:   true,
			Status:   string(inst.Status),
			CPUs:     inst.CPUs,
			MemoryMB: inst.Memory / 1024 / 1024,
		}
	}

	if gw, ok := routing.RouteGateway(cfg.Network.KindBridgeCIDR); ok {
		rep.Route.Present = true
		rep.Route.Gateway = gw
	}

	// Clusters and iptables need a running VM.
	if inst == nil || inst.Status != limatype.StatusRunning {
		return rep, nil
	}

	g, err := guest.NewClient(inst)
	if err != nil {
		return nil, fmt.Errorf("guest SSH: %w", err)
	}

	// Names is always a slice, never nil: consumers of -o json should see [] for
	// "no clusters" rather than null.
	rep.Clusters = &statusClusters{Names: []string{}}
	if names, err := kind.ListClusters(ctx, g); err != nil {
		rep.Clusters.Error = err.Error()
	} else if names != nil {
		rep.Clusters.Names = names
	}

	rep.IPTables = &statusIPTables{}
	if ok, err := routing.CheckNoNatRule(ctx, g, cfg.Network.KindBridgeCIDR); err != nil {
		rep.IPTables.Error = err.Error()
	} else {
		rep.IPTables.NoNATExemption = ok
	}

	return rep, nil
}

// printStatusText renders the human-readable report. The wording is deliberately
// unchanged from before the machine-readable formats were added.
func printStatusText(rep *statusReport) {
	fmt.Println("=== VM ===")
	if !rep.VM.Exists {
		fmt.Println("  status: not created")
	} else {
		fmt.Printf("  name:   %s\n", rep.VM.Name)
		fmt.Printf("  status: %s\n", rep.VM.Status)
		fmt.Printf("  cpus:   %d\n", rep.VM.CPUs)
		fmt.Printf("  memory: %d MB\n", rep.VM.MemoryMB)
	}

	fmt.Println("\n=== macOS Route ===")
	if rep.Route.Present {
		fmt.Printf("  %s → present (via %s)\n", rep.Route.CIDR, rep.Route.Gateway)
	} else {
		fmt.Printf("  %s → MISSING\n", rep.Route.CIDR)
	}

	fmt.Println("\n=== Kind Clusters ===")
	switch {
	case rep.Clusters == nil:
		fmt.Println("  (VM is not running)")
		return
	case rep.Clusters.Error != "":
		fmt.Printf("  error: %v\n", rep.Clusters.Error)
	case len(rep.Clusters.Names) == 0:
		fmt.Println("  (none)")
	default:
		for _, c := range rep.Clusters.Names {
			fmt.Printf("  - %s\n", c)
		}
	}

	fmt.Println("\n=== IPTables (no-NAT rule) ===")
	switch {
	case rep.IPTables == nil:
		fmt.Println("  (VM is not running)")
	case rep.IPTables.Error != "":
		fmt.Printf("  error: %v\n", rep.IPTables.Error)
	case rep.IPTables.NoNATExemption:
		fmt.Println("  nat POSTROUTING exemption → present")
	default:
		fmt.Println("  nat POSTROUTING exemption → MISSING")
	}
}
