package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/bcollard/klimax/internal/config"
	"github.com/bcollard/klimax/internal/guest"
	"github.com/bcollard/klimax/internal/kind"
	"github.com/bcollard/klimax/internal/routing"
	"github.com/bcollard/klimax/internal/vm"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func newClusterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Manage kind clusters inside the klimax VM",
	}
	cmd.AddCommand(
		newClusterCreateCmd(),
		newClusterDeleteCmd(),
		newClusterListCmd(),
		newClusterUseCmd(),
	)
	return cmd
}

// ─── create ──────────────────────────────────────────────────────────────────

func newClusterCreateCmd() *cobra.Command {
	var num int
	var region, zone string
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a new kind cluster in the running VM",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClusterCreate(cmd.Context(), args[0], num, region, zone)
		},
	}
	cmd.Flags().IntVar(&num, "num", 0, "Cluster number (1-99) for subnet/port allocation; auto-assigned if 0")
	cmd.Flags().StringVar(&region, "region", "", "topology.kubernetes.io/region label (default: europe-west<N>)")
	cmd.Flags().StringVar(&zone, "zone", "", "topology.kubernetes.io/zone label (default: europe-west<N>-b)")
	return cmd
}

func runClusterCreate(ctx context.Context, name string, num int, region, zone string) error {
	cfg, g, err := connectToRunningVM(ctx)
	if err != nil {
		return err
	}

	// Resolve num: find the lowest free slot by inspecting live cluster port bindings.
	if num == 0 {
		usedNums, err := kind.DetectUsedNums(ctx, g)
		if err != nil {
			return fmt.Errorf("detecting used cluster nums: %w", err)
		}
		num, err = kind.NextFreeNum(usedNums)
		if err != nil {
			return err
		}
		slog.Info("Auto-assigned cluster num", "name", name, "num", num)
	}

	cl := config.ClusterConfig{Name: name, Num: num, Region: region, Zone: zone}

	lima0IP, err := routing.Lima0IP(ctx, g)
	if err != nil {
		return fmt.Errorf("detecting lima0 IP: %w", err)
	}

	return kind.CreateCluster(ctx, g, cl, cfg.Kind, cfg.Registries, cfg.Network.KindBridgeCIDR, lima0IP)
}

// ─── delete ──────────────────────────────────────────────────────────────────

func newClusterDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete [name]",
		Short: "Delete a kind cluster (interactive picker when no name is given)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				return runClusterDelete(cmd.Context(), args[0])
			}
			return runClusterDeleteInteractive(cmd.Context())
		},
	}
}

func runClusterDelete(ctx context.Context, name string) error {
	_, g, err := connectToRunningVM(ctx)
	if err != nil {
		return err
	}
	return kind.DeleteCluster(ctx, g, name)
}

// runClusterDeleteInteractive shows a numbered list and lets the user pick.
func runClusterDeleteInteractive(ctx context.Context) error {
	_, g, err := connectToRunningVM(ctx)
	if err != nil {
		return err
	}

	names, err := kind.ListClusters(ctx, g)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Println("No kind clusters found.")
		return nil
	}

	fmt.Println("Select a cluster to delete:")
	for i, n := range names {
		fmt.Printf("  %d. %s\n", i+1, n)
	}
	fmt.Print("Enter number (or q to cancel): ")

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	line := strings.TrimSpace(scanner.Text())
	if line == "q" || line == "" {
		fmt.Println("Cancelled.")
		return nil
	}

	var idx int
	if _, err := fmt.Sscanf(line, "%d", &idx); err != nil || idx < 1 || idx > len(names) {
		return fmt.Errorf("invalid selection %q", line)
	}

	name := names[idx-1]
	fmt.Printf("Deleting cluster %q...\n", name)
	return kind.DeleteCluster(ctx, g, name)
}

// ─── list ────────────────────────────────────────────────────────────────────

func newClusterListCmd() *cobra.Command {
	var outputFmt string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List kind clusters running in the VM",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClusterList(cmd.Context(), outputFmt)
		},
	}
	cmd.Flags().StringVarP(&outputFmt, "output", "o", "text", "Output format: text, json, yaml")
	return cmd
}

type clusterInfo struct {
	Name           string `json:"name"           yaml:"name"`
	Num            int    `json:"num"            yaml:"num"`
	APIPort        int    `json:"apiPort"        yaml:"apiPort"`
	KubeconfigPath string `json:"kubeconfigPath" yaml:"kubeconfigPath"`
}

func runClusterList(ctx context.Context, outputFmt string) error {
	_, g, err := connectToRunningVM(ctx)
	if err != nil {
		return err
	}

	names, err := kind.ListClusters(ctx, g)
	if err != nil {
		return err
	}

	// Detect nums from live port bindings.
	usedNums, _ := kind.DetectUsedNums(ctx, g) // best-effort; num=0 if unknown
	numToName := make(map[string]int, len(usedNums))
	for n, name := range usedNums {
		numToName[name] = n
	}

	clusters := make([]clusterInfo, 0, len(names))
	for _, n := range names {
		num := numToName[n]
		clusters = append(clusters, clusterInfo{
			Name:           n,
			Num:            num,
			APIPort:        7000 + num,
			KubeconfigPath: kind.KindKubeconfigPath(n),
		})
	}

	switch outputFmt {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(clusters)
	case "yaml":
		return yaml.NewEncoder(os.Stdout).Encode(clusters)
	default: // "text"
		if len(names) == 0 {
			fmt.Println("No kind clusters found.")
			return nil
		}
		fmt.Printf("%-30s  %4s  %8s  %s\n", "NAME", "NUM", "API-PORT", "KUBECONFIG")
		fmt.Printf("%-30s  %4s  %8s  %s\n", strings.Repeat("-", 30), "----", "--------", "---------")
		for _, c := range clusters {
			fmt.Printf("%-30s  %4d  %8d  %s\n", c.Name, c.Num, c.APIPort, c.KubeconfigPath)
		}
		return nil
	}
}

// ─── use ─────────────────────────────────────────────────────────────────────

func newClusterUseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use <name>",
		Short: "Print the export command to set KUBECONFIG for the given cluster",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := kind.KindKubeconfigPath(args[0])
			if _, err := os.Stat(path); os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "warning: kubeconfig not found at %s\n", path)
			}
			fmt.Printf("export KUBECONFIG=%s\n", path)
			return nil
		},
	}
}

// ─── shared helpers ──────────────────────────────────────────────────────────

// connectToRunningVM loads config and opens an SSH client to the running VM.
func connectToRunningVM(ctx context.Context) (*config.Config, *guest.Client, error) {
	cfg, err := loadAndValidate()
	if err != nil {
		return nil, nil, err
	}

	mgr := vm.New(cfg.VM.Name)
	inst, err := mgr.Inspect(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("inspecting VM: %w", err)
	}
	if inst == nil {
		return nil, nil, errors.New("VM does not exist; run 'klimax up' first")
	}

	g, err := guest.NewClient(inst)
	if err != nil {
		return nil, nil, fmt.Errorf("opening SSH client: %w", err)
	}
	return cfg, g, nil
}
