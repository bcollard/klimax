package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/bcollard/klimax/internal/kind"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// fleetLabelKey is the node label that records which fleet a cluster belongs to.
const fleetLabelKey = "klimax.dev/fleet"

func newFleetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fleet",
		Short: "Manage fleets of kind clusters (grouped by the klimax.dev/fleet label)",
		Long: `A fleet is a set of kind clusters created from a Fleet manifest. Members are
tracked by the klimax.dev/fleet=<name> node label, so fleet operations work on
live clusters regardless of the original manifest.`,
	}
	cmd.AddCommand(
		newFleetListCmd(),
		newFleetDescribeCmd(),
		newFleetCreateCmd(),
		newFleetDeleteCmd(),
		newFleetLabelCmd(),
	)
	return cmd
}

// ─── fleet describe ────────────────────────────────────────────────────────────

func newFleetDescribeCmd() *cobra.Command {
	var outputFmt string
	cmd := &cobra.Command{
		Use:     "describe <name>",
		Aliases: []string{"desc", "show"},
		Short:   "Show a fleet's member clusters with their num, ports, nodes, and labels",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFleetDescribe(cmd.Context(), args[0], outputFmt)
		},
	}
	cmd.Flags().StringVarP(&outputFmt, "output", "o", "text", "Output format: text, json, yaml")
	return cmd
}

type fleetMember struct {
	Name           string            `json:"name"           yaml:"name"`
	Num            int               `json:"num"            yaml:"num"`
	APIPort        int               `json:"apiPort"        yaml:"apiPort"`
	KubeconfigPath string            `json:"kubeconfigPath" yaml:"kubeconfigPath"`
	Nodes          int               `json:"nodes"          yaml:"nodes"`
	K8sVersion     string            `json:"k8sVersion"     yaml:"k8sVersion"`
	Ready          bool              `json:"ready"          yaml:"ready"`
	Labels         map[string]string `json:"labels"         yaml:"labels"`
}

type fleetDescription struct {
	Name    string        `json:"name"     yaml:"name"`
	Members []fleetMember `json:"members"  yaml:"members"`
}

func runFleetDescribe(ctx context.Context, name, outputFmt string) error {
	_, g, err := connectToRunningVM(ctx)
	if err != nil {
		return err
	}
	names, err := kind.ClustersMatchingSelector(ctx, g, fleetLabelKey+"="+name)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return fmt.Errorf("no clusters found in fleet %q", name)
	}
	sort.Strings(names)

	usedNums, _ := kind.DetectUsedNums(ctx, g) // best-effort
	numByName := make(map[string]int, len(usedNums))
	for num, n := range usedNums {
		numByName[n] = num
	}

	desc := fleetDescription{Name: name}
	for _, n := range names {
		info, err := kind.ClusterInfoFor(ctx, g, n)
		if err != nil {
			return err
		}
		num := numByName[n]
		desc.Members = append(desc.Members, fleetMember{
			Name:           n,
			Num:            num,
			APIPort:        7000 + num,
			KubeconfigPath: kind.KindKubeconfigPath(n),
			Nodes:          info.NodeCount,
			K8sVersion:     info.KubeletVersion,
			Ready:          info.Ready,
			Labels:         info.Labels,
		})
	}

	switch outputFmt {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(desc)
	case "yaml":
		return yaml.NewEncoder(os.Stdout).Encode(desc)
	default:
		fmt.Printf("Fleet %q — %d cluster(s)\n", desc.Name, len(desc.Members))
		for _, m := range desc.Members {
			ready := "NotReady"
			if m.Ready {
				ready = "Ready"
			}
			fmt.Printf("\n▸ %s\n", m.Name)
			fmt.Printf("    num:        %d\n", m.Num)
			fmt.Printf("    apiPort:    %d\n", m.APIPort)
			fmt.Printf("    kubeconfig: %s\n", m.KubeconfigPath)
			fmt.Printf("    nodes:      %d (%s, %s)\n", m.Nodes, m.K8sVersion, ready)
			fmt.Printf("    labels:     %s\n", formatLabels(m.Labels))
		}
		return nil
	}
}

// infraLabelPrefixes are the standard read-only node labels kubelet sets. They
// are hidden from the `describe` text view (JSON/YAML output keeps everything).
// topology.kubernetes.io/* is intentionally NOT hidden — klimax sets region/zone.
var infraLabelPrefixes = []string{
	"kubernetes.io/",
	"beta.kubernetes.io/",
	"node-role.kubernetes.io/",
	"node.kubernetes.io/",
}

// formatLabels renders the klimax/custom labels as sorted "k=v" pairs, hiding
// the standard Kubernetes infrastructure node labels.
func formatLabels(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		if isInfraLabel(k) {
			continue
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return "-"
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, k+"="+labels[k])
	}
	return strings.Join(pairs, ", ")
}

func isInfraLabel(key string) bool {
	for _, p := range infraLabelPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// ─── fleet create ──────────────────────────────────────────────────────────────

func newFleetCreateCmd() *cobra.Command {
	var filename string
	var dryRun bool
	var maxParallel int
	cmd := &cobra.Command{
		Use:   "create -f <fleet.yaml>",
		Short: "Create the clusters described by a Fleet manifest",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if filename == "" {
				return errors.New("a manifest is required: klimax fleet create -f <file>")
			}
			return runClusterApply(cmd.Context(), filename, dryRun, maxParallel)
		},
	}
	cmd.Flags().StringVarP(&filename, "filename", "f", "", "Path to a Fleet manifest (- for stdin)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print the resolved plan and exit without creating anything")
	cmd.Flags().IntVar(&maxParallel, "max-parallel", 0, "Override spec.maxParallel (concurrent cluster creations)")
	return cmd
}

// ─── fleet delete ──────────────────────────────────────────────────────────────

func newFleetDeleteCmd() *cobra.Command {
	var filename string
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete [name]",
		Short: "Delete a fleet's clusters (by fleet name, or from a Fleet manifest)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if filename != "" {
				if len(args) > 0 {
					return errors.New("cannot combine a fleet name with -f")
				}
				return runClusterDeleteFromManifest(cmd.Context(), filename, yes)
			}
			if len(args) != 1 {
				return errors.New("a fleet name (or -f <manifest>) is required")
			}
			return runFleetDeleteByName(cmd.Context(), args[0], yes)
		},
	}
	cmd.Flags().StringVarP(&filename, "filename", "f", "", "Delete the clusters listed in a Fleet manifest (- for stdin)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip the confirmation prompt")
	return cmd
}

func runFleetDeleteByName(ctx context.Context, name string, yes bool) error {
	cfg, g, err := connectToRunningVM(ctx)
	if err != nil {
		return err
	}
	targets, err := kind.ClustersMatchingSelector(ctx, g, fleetLabelKey+"="+name)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		fmt.Printf("No clusters found in fleet %q\n", name)
		return nil
	}
	return confirmAndDeleteClusters(ctx, g, cfg, targets, yes)
}

// ─── fleet label ───────────────────────────────────────────────────────────────

func newFleetLabelCmd() *cobra.Command {
	var labels []string
	cmd := &cobra.Command{
		Use:   "label <name> -l key=value",
		Short: "Apply node labels to every cluster in a fleet",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFleetLabel(cmd.Context(), args[0], labels)
		},
	}
	cmd.Flags().StringArrayVarP(&labels, "label", "l", nil, "Label to set (key=value) or remove (key-); repeatable")
	return cmd
}

func runFleetLabel(ctx context.Context, name string, specs []string) error {
	kubeArgs, err := parseLabelSpecs(specs)
	if err != nil {
		return err
	}
	_, g, err := connectToRunningVM(ctx)
	if err != nil {
		return err
	}
	members, err := kind.ClustersMatchingSelector(ctx, g, fleetLabelKey+"="+name)
	if err != nil {
		return err
	}
	if len(members) == 0 {
		return fmt.Errorf("no clusters found in fleet %q", name)
	}
	for _, c := range members {
		if err := kind.LabelNodes(ctx, g, c, kubeArgs); err != nil {
			return fmt.Errorf("labeling cluster %q: %w", c, err)
		}
	}
	fmt.Printf("Labeled %d cluster(s) in fleet %q\n", len(members), name)
	return nil
}

// ─── fleet list ────────────────────────────────────────────────────────────────

func newFleetListCmd() *cobra.Command {
	var outputFmt string
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List fleets and their member clusters",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFleetList(cmd.Context(), outputFmt)
		},
	}
	cmd.Flags().StringVarP(&outputFmt, "output", "o", "text", "Output format: text, json, yaml")
	return cmd
}

type fleetInfo struct {
	Name     string   `json:"name"     yaml:"name"`
	Clusters []string `json:"clusters" yaml:"clusters"`
}

func runFleetList(ctx context.Context, outputFmt string) error {
	_, g, err := connectToRunningVM(ctx)
	if err != nil {
		return err
	}
	byFleet, err := kind.ClustersByFleet(ctx, g)
	if err != nil {
		return err
	}

	// Only real fleets (skip clusters with no klimax.dev/fleet label).
	names := make([]string, 0, len(byFleet))
	for name := range byFleet {
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	fleets := make([]fleetInfo, 0, len(names))
	for _, name := range names {
		members := byFleet[name]
		sort.Strings(members)
		fleets = append(fleets, fleetInfo{Name: name, Clusters: members})
	}

	switch outputFmt {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(fleets)
	case "yaml":
		return yaml.NewEncoder(os.Stdout).Encode(fleets)
	default:
		if len(fleets) == 0 {
			fmt.Println("No fleets found.")
			return nil
		}
		fmt.Printf("%-24s  %s\n", "FLEET", "CLUSTERS")
		fmt.Printf("%-24s  %s\n", strings.Repeat("-", 24), strings.Repeat("-", 30))
		for _, f := range fleets {
			fmt.Printf("%-24s  %s\n", f.Name, strings.Join(f.Clusters, ", "))
		}
		return nil
	}
}
