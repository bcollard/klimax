package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/bcollard/klimax/internal/config"
	"github.com/bcollard/klimax/internal/guest"
	"github.com/bcollard/klimax/internal/kind"
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
		newClusterMergeCmd(),
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

	return kind.CreateCluster(ctx, g, cl, cfg.Kind, cfg.Registries, cfg.Network.KindBridgeCIDR)
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

// ─── merge ───────────────────────────────────────────────────────────────────

func newClusterMergeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "merge <name>",
		Short: "Merge cluster kubeconfig into ~/.kube/config",
		Long: `Adds the cluster's context, cluster, and user entries into the default
kubeconfig (~/.kube/config) so that kubectx/kubens can switch to it.

A backup of the existing ~/.kube/config is written to ~/.kube/config.bak
before any modifications are made.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClusterMerge(args[0])
		},
	}
}

func runClusterMerge(name string) error {
	srcPath := kind.KindKubeconfigPath(name)
	if _, err := os.Stat(srcPath); os.IsNotExist(err) {
		return fmt.Errorf("kubeconfig not found at %s — run 'klimax cluster create %s' first", srcPath, name)
	}

	srcData, err := os.ReadFile(srcPath)
	if err != nil {
		return fmt.Errorf("reading cluster kubeconfig: %w", err)
	}
	var src kubeconfigFile
	if err := yaml.Unmarshal(srcData, &src); err != nil {
		return fmt.Errorf("parsing cluster kubeconfig: %w", err)
	}

	// Load or initialise the default kubeconfig.
	home, _ := os.UserHomeDir()
	dstPath := filepath.Join(home, ".kube", "config")
	var dst kubeconfigFile
	dstData, err := os.ReadFile(dstPath)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(dstData, &dst); err != nil {
			return fmt.Errorf("parsing ~/.kube/config: %w", err)
		}
		// Backup before any modification.
		bakPath := dstPath + ".bak"
		if err := os.WriteFile(bakPath, dstData, 0o600); err != nil {
			return fmt.Errorf("writing backup %s: %w", bakPath, err)
		}
		slog.Info("Backup written", "path", bakPath)
	case os.IsNotExist(err):
		dst = kubeconfigFile{APIVersion: "v1", Kind: "Config"}
		if err := os.MkdirAll(filepath.Dir(dstPath), 0o750); err != nil {
			return fmt.Errorf("creating ~/.kube: %w", err)
		}
	default:
		return fmt.Errorf("reading ~/.kube/config: %w", err)
	}

	dst.Clusters = mergeKubeconfigEntries(dst.Clusters, src.Clusters)
	dst.Contexts = mergeKubeconfigEntries(dst.Contexts, src.Contexts)
	dst.Users = mergeKubeconfigEntries(dst.Users, src.Users)

	out, err := yaml.Marshal(&dst)
	if err != nil {
		return fmt.Errorf("marshaling merged kubeconfig: %w", err)
	}
	if err := os.WriteFile(dstPath, out, 0o600); err != nil {
		return fmt.Errorf("writing ~/.kube/config: %w", err)
	}

	// Report the context name(s) that were merged in.
	contextName := "kind-" + name // kind's default naming convention
	if len(src.Contexts) > 0 {
		if n, ok := src.Contexts[0]["name"].(string); ok {
			contextName = n
		}
	}
	slog.Info("Kubeconfig merged", "context", contextName, "dst", dstPath)
	fmt.Printf("Merged context %q into %s\n  kubectx %s\n", contextName, dstPath, contextName)
	return nil
}

// kubeconfigFile is a minimal kubeconfig representation for faithful YAML round-trips.
type kubeconfigFile struct {
	APIVersion     string                   `yaml:"apiVersion"`
	Kind           string                   `yaml:"kind"`
	Clusters       []map[string]interface{} `yaml:"clusters"`
	Contexts       []map[string]interface{} `yaml:"contexts"`
	Users          []map[string]interface{} `yaml:"users"`
	CurrentContext string                   `yaml:"current-context,omitempty"`
	Preferences    interface{}              `yaml:"preferences,omitempty"`
}

// mergeKubeconfigEntries merges src into dst, overwriting entries with the same "name".
func mergeKubeconfigEntries(dst, src []map[string]interface{}) []map[string]interface{} {
	idx := make(map[string]int, len(dst))
	for i, e := range dst {
		if n, ok := e["name"].(string); ok {
			idx[n] = i
		}
	}
	for _, e := range src {
		n, _ := e["name"].(string)
		if i, exists := idx[n]; exists {
			dst[i] = e
		} else {
			idx[n] = len(dst)
			dst = append(dst, e)
		}
	}
	return dst
}

// ─── shared helpers ──────────────────────────────────────────────────────────

// connectToRunningVM loads config and opens an SSH client to the running VM.
func connectToRunningVM(ctx context.Context) (*config.Config, *guest.Client, error) {
	cfg, err := loadAndValidate()
	if err != nil {
		return nil, nil, err
	}

	mgr := vm.New(cfg.VM.Name, KlimaxHome())
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
