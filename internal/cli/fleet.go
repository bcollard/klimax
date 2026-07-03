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
		newFleetCreateCmd(),
		newFleetDeleteCmd(),
		newFleetLabelCmd(),
	)
	return cmd
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
