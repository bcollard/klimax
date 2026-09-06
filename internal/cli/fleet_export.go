package cli

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/bcollard/klimax/internal/fleet"
	"github.com/bcollard/klimax/internal/guest"
	"github.com/bcollard/klimax/internal/kind"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// Node labels that describe klimax-managed topology or membership rather than
// user intent. They are reconstructed from dedicated manifest fields on apply,
// so they must not be echoed back into the entry's free-form `labels:` map.
var exportManagedLabels = map[string]bool{
	"topology.kubernetes.io/region": true,
	"topology.kubernetes.io/zone":   true,
	fleetLabelKey:                   true,
	"managed-by":                    true,
}

func newFleetExportCmd() *cobra.Command {
	var selector, name string
	var withNums bool
	cmd := &cobra.Command{
		Use:   "export [cluster...]",
		Short: "Write a Fleet manifest describing existing clusters",
		Long: `Reverse of 'klimax fleet create -f': reads live clusters and writes a Fleet
manifest to stdout, so an ad-hoc lab can be captured into a reproducible file.

Choose clusters by name, by label selector, or — with neither — from an
interactive picker.

  klimax fleet export dev staging > fleet.yaml
  klimax fleet export -l klimax.dev/fleet=mesh > fleet.yaml
  klimax fleet export > fleet.yaml          # interactive picker

Captured per cluster: name, num, nodeVersion, region/zone, and custom node
labels. Not captured, because live clusters do not record it: dependsOn
ordering, registry mirror selection, and addons — add those by hand if you
need them.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFleetExport(cmd.Context(), args, selector, name, withNums)
		},
	}
	cmd.Flags().StringVarP(&selector, "selector", "l", "", "Select clusters by node label selector")
	cmd.Flags().StringVar(&name, "name", "", "metadata.name for the exported fleet (default: the clusters' shared fleet label, else \"exported\")")
	cmd.Flags().BoolVar(&withNums, "nums", true, "Record each cluster's num, pinning its API port on re-apply")
	return cmd
}

func runFleetExport(ctx context.Context, args []string, selector, name string, withNums bool) error {
	if len(args) > 0 && selector != "" {
		return fmt.Errorf("pass cluster names or --selector, not both")
	}

	_, g, err := connectToRunningVM(ctx)
	if err != nil {
		return err
	}

	names, err := resolveExportTargets(ctx, g, args, selector)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return nil // picker cancelled, or nothing matched — already reported
	}

	man, err := buildFleetManifest(ctx, g, names, name, withNums)
	if err != nil {
		return err
	}

	enc := yaml.NewEncoder(os.Stdout)
	enc.SetIndent(2)
	if err := enc.Encode(man); err != nil {
		return err
	}
	return enc.Close()
}

// resolveExportTargets turns args/selector/no-input into a concrete cluster list.
// Explicit names are validated against the live set so a typo fails loudly
// rather than producing a manifest for a cluster that does not exist.
func resolveExportTargets(ctx context.Context, g *guest.Client, args []string, selector string) ([]string, error) {
	live, err := kind.ListClusters(ctx, g)
	if err != nil {
		return nil, err
	}
	if len(live) == 0 {
		fmt.Fprintln(os.Stderr, "No kind clusters found.")
		return nil, nil
	}

	switch {
	case len(args) > 0:
		liveSet := make(map[string]bool, len(live))
		for _, n := range live {
			liveSet[n] = true
		}
		var missing []string
		for _, n := range args {
			if !liveSet[n] {
				missing = append(missing, n)
			}
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("no such cluster(s): %s", strings.Join(missing, ", "))
		}
		return args, nil

	case selector != "":
		matched, err := kind.ClustersMatchingSelector(ctx, g, selector)
		if err != nil {
			return nil, err
		}
		if len(matched) == 0 {
			fmt.Fprintf(os.Stderr, "No clusters match selector %q.\n", selector)
			return nil, nil
		}
		sort.Strings(matched)
		return matched, nil

	default:
		usedNums, _ := kind.DetectUsedNums(ctx, g) // best-effort; port 0 if unknown
		numByName := make(map[string]int, len(usedNums))
		for num, n := range usedNums {
			numByName[n] = num
		}
		items := make([]pickerItem, len(live))
		for i, n := range live {
			items[i] = pickerItem{name: n, apiPort: 7000 + numByName[n]}
		}
		return runPicker("Export kind clusters to a Fleet manifest", "export", items)
	}
}

// buildFleetManifest reads each cluster's live state and assembles the manifest.
func buildFleetManifest(ctx context.Context, g *guest.Client, names []string, name string, withNums bool) (*fleet.Fleet, error) {
	usedNums, _ := kind.DetectUsedNums(ctx, g) // best-effort; num 0 if unknown
	numByName := make(map[string]int, len(usedNums))
	for num, n := range usedNums {
		numByName[n] = num
	}

	infos := make(map[string]*kind.ClusterInfo, len(names))
	for _, n := range names {
		info, err := kind.ClusterInfoFor(ctx, g, n)
		if err != nil {
			return nil, err
		}
		infos[n] = info
	}

	return assembleFleet(names, numByName, infos, name, withNums), nil
}

// assembleFleet is the pure transform from live cluster facts to a manifest.
// Split out from the guest I/O so it can be tested directly.
func assembleFleet(names []string, numByName map[string]int, infos map[string]*kind.ClusterInfo, name string, withNums bool) *fleet.Fleet {
	man := &fleet.Fleet{
		APIVersion: fleet.APIVersion,
		Kind:       fleet.Kind,
		Metadata:   fleet.Metadata{Name: name},
	}

	for _, n := range names {
		info := infos[n]
		if info == nil {
			info = &kind.ClusterInfo{}
		}
		entry := fleet.ClusterEntry{Name: n}
		if withNums {
			entry.Num = numByName[n]
		}
		entry.NodeVersion = info.KubeletVersion
		entry.Region = info.Labels["topology.kubernetes.io/region"]
		entry.Zone = info.Labels["topology.kubernetes.io/zone"]
		if custom := customLabels(info.Labels); len(custom) > 0 {
			entry.Labels = custom
		}
		man.Spec.Clusters = append(man.Spec.Clusters, entry)
	}

	if man.Metadata.Name == "" {
		man.Metadata.Name = deriveFleetName(names, infos)
	}
	return man
}

// customLabels keeps only labels a user would have set themselves: it drops the
// kubelet's own infrastructure labels and the ones klimax reconstructs from
// dedicated manifest fields.
func customLabels(labels map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range labels {
		if isInfraLabel(k) || exportManagedLabels[k] {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// deriveFleetName uses the clusters' shared klimax.dev/fleet label when they all
// agree, so re-applying an exported fleet keeps its identity. Falls back to
// "exported" when the set is mixed or unlabelled.
func deriveFleetName(names []string, infos map[string]*kind.ClusterInfo) string {
	shared := ""
	for _, n := range names {
		info := infos[n]
		if info == nil {
			return "exported"
		}
		f := info.Labels[fleetLabelKey]
		if f == "" {
			return "exported"
		}
		if shared == "" {
			shared = f
			continue
		}
		if shared != f {
			return "exported"
		}
	}
	if shared == "" {
		return "exported"
	}
	return shared
}
