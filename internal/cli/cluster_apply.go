package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/bcollard/klimax/internal/config"
	"github.com/bcollard/klimax/internal/fleet"
	"github.com/bcollard/klimax/internal/guest"
	"github.com/bcollard/klimax/internal/kind"
	"github.com/spf13/cobra"
)

func newClusterApplyCmd() *cobra.Command {
	var filename string
	var dryRun bool
	var maxParallel int
	cmd := &cobra.Command{
		Use:   "apply -f <fleet.yaml>",
		Short: "Create a fleet of kind clusters from a Fleet manifest",
		Long: `Reconcile the cluster fleet described by a Fleet manifest: create every
listed cluster that does not yet exist, honouring dependsOn ordering and running
independent clusters up to maxParallel at a time. Existing clusters are left
untouched (apply is additive).

The minimal manifest only lists cluster names:

  apiVersion: klimax.dev/v1alpha1
  kind: Fleet
  spec:
    clusters:
      - dev
      - staging`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if filename == "" {
				return errors.New("a manifest is required: klimax cluster apply -f <file>")
			}
			return runClusterApply(cmd.Context(), filename, dryRun, maxParallel)
		},
	}
	cmd.Flags().StringVarP(&filename, "filename", "f", "", "Path to a Fleet manifest (- for stdin)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print the resolved plan and exit without creating anything")
	cmd.Flags().IntVar(&maxParallel, "max-parallel", 0, "Override spec.maxParallel (concurrent cluster creations)")
	return cmd
}

func runClusterApply(ctx context.Context, filename string, dryRun bool, maxParallel int) error {
	data, err := readManifest(filename)
	if err != nil {
		return err
	}
	cs, err := fleet.Parse(data)
	if err != nil {
		return err
	}
	if err := cs.Validate(); err != nil {
		return fmt.Errorf("invalid Fleet: %w", err)
	}

	cfg, g, err := connectToRunningVM(ctx)
	if err != nil {
		return err
	}

	// Pre-flight: verify every cherry-picked mirror name exists in the config catalog.
	if err := validateMirrorSelections(cs, cfg.Registries); err != nil {
		return err
	}
	// Pre-flight: validate labels (merged with defaults, plus the fleet label).
	if cs.Metadata.Name != "" {
		if err := config.ValidateLabels(map[string]string{"klimax.dev/fleet": cs.Metadata.Name}); err != nil {
			return fmt.Errorf("metadata.name %q is not a valid label value: %w", cs.Metadata.Name, err)
		}
	}
	for _, c := range cs.Spec.Clusters {
		if err := config.ValidateLabels(c.Merged(cs.Spec.Defaults).Labels); err != nil {
			return fmt.Errorf("cluster %q: %w", c.Name, err)
		}
	}

	existing, err := kind.DetectUsedNums(ctx, g)
	if err != nil {
		return fmt.Errorf("detecting existing clusters: %w", err)
	}

	plan, err := cs.Resolve(existing)
	if err != nil {
		return err
	}
	if maxParallel > 0 {
		plan.MaxParallel = maxParallel
	}

	printPlan(plan)
	if dryRun {
		fmt.Println("\n(dry-run: no clusters created)")
		return nil
	}
	if len(plan.ToCreate()) == 0 {
		fmt.Println("\nNothing to do — all clusters already exist.")
		return nil
	}

	return executePlan(ctx, g, cfg, plan)
}

// executePlan schedules cluster creation honouring dependsOn and maxParallel.
func executePlan(ctx context.Context, g *guest.Client, cfg *config.Config, plan *fleet.Plan) error {
	failFast := plan.Strategy != fleet.StrategyContinueOnError

	var (
		mu        sync.Mutex
		mergeMu   sync.Mutex
		results   = make(map[string]error) // name → create error (nil = ok)
		anyFailed bool
	)
	done := make(map[string]chan struct{}, len(plan.Clusters))
	for _, pc := range plan.Clusters {
		done[pc.Name] = make(chan struct{})
	}
	// Existing clusters count as already-satisfied dependencies.
	for _, pc := range plan.Clusters {
		if pc.Exists {
			mu.Lock()
			results[pc.Name] = nil
			mu.Unlock()
			close(done[pc.Name])
		}
	}

	setResult := func(name string, err error) {
		mu.Lock()
		results[name] = err
		if err != nil {
			anyFailed = true
		}
		mu.Unlock()
	}

	sem := make(chan struct{}, plan.MaxParallel)
	var wg sync.WaitGroup

	for _, pc := range plan.ToCreate() {
		wg.Add(1)
		go func(pc fleet.PlannedCluster) {
			defer wg.Done()
			defer close(done[pc.Name])

			// Wait for dependencies to reach a terminal state.
			for _, d := range pc.DependsOn {
				select {
				case <-done[d]:
				case <-ctx.Done():
					setResult(pc.Name, ctx.Err())
					return
				}
			}

			// Skip if a dependency failed, or fail-fast has already tripped.
			mu.Lock()
			var skipErr error
			for _, d := range pc.DependsOn {
				if results[d] != nil {
					skipErr = fmt.Errorf("skipped: dependency %q failed", d)
					break
				}
			}
			if skipErr == nil && failFast && anyFailed {
				skipErr = errors.New("skipped: earlier cluster failed (FailFast)")
			}
			mu.Unlock()
			if skipErr != nil {
				slog.Warn("Skipping cluster", "cluster", pc.Name, "reason", skipErr)
				setResult(pc.Name, skipErr)
				return
			}

			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				setResult(pc.Name, ctx.Err())
				return
			}
			defer func() { <-sem }()

			// Re-check fail-fast after possibly queuing behind the semaphore.
			mu.Lock()
			tripped := failFast && anyFailed
			mu.Unlock()
			if tripped {
				setResult(pc.Name, errors.New("skipped: earlier cluster failed (FailFast)"))
				return
			}

			fmt.Printf("→ creating cluster %q (num %d)\n", pc.Name, pc.Num)
			err := createOne(ctx, g, cfg, plan.Name, pc, &mergeMu)
			if err != nil {
				slog.Error("Cluster creation failed", "cluster", pc.Name, "err", err)
			} else {
				fmt.Printf("✓ cluster %q ready\n", pc.Name)
			}
			setResult(pc.Name, err)
		}(pc)
	}

	wg.Wait()
	return summarize(plan, results)
}

// createOne builds the per-cluster config overrides and creates a single cluster.
func createOne(ctx context.Context, g *guest.Client, cfg *config.Config, fleetName string, pc fleet.PlannedCluster, mergeMu *sync.Mutex) error {
	// Labels: merged per-cluster/default labels, plus the fleet label.
	labels := map[string]string{}
	maps.Copy(labels, pc.Labels)
	if fleetName != "" {
		labels["klimax.dev/fleet"] = fleetName
	}

	cl := config.ClusterConfig{Name: pc.Name, Num: pc.Num, Region: pc.Region, Zone: pc.Zone, Labels: labels}

	// Per-cluster nodeVersion override.
	kindCfg := cfg.Kind
	if pc.NodeVersion != "" {
		kindCfg.NodeVersion = pc.NodeVersion
	}

	// Per-cluster registry cherry-pick.
	regCfg := applyRegistrySelect(cfg.Registries, pc.Registries)

	if err := kind.CreateCluster(ctx, g, cl, kindCfg, regCfg, cfg.Network.KindBridgeCIDR, cfg.Network.DisablePortMirroring); err != nil {
		return err
	}

	// Addons.
	if pc.Addons != nil && pc.Addons.MetricsServer != nil && pc.Addons.MetricsServer.Enabled {
		ms := pc.Addons.MetricsServer
		if err := kind.InstallMetricsServer(ctx, g, pc.Name, ms.Version, boolOrTrue(ms.KubeletInsecureTLS)); err != nil {
			return fmt.Errorf("metrics-server addon: %w", err)
		}
	}

	// Kubeconfig merge (serialized: it read-modify-writes ~/.kube/config).
	if cfg.Kind.AutoMergeKubeconfig != nil && *cfg.Kind.AutoMergeKubeconfig {
		mergeMu.Lock()
		err := runClusterMerge(pc.Name)
		mergeMu.Unlock()
		if err != nil {
			slog.Warn("Auto-merge kubeconfig failed", "cluster", pc.Name, "err", err)
		}
	}
	return nil
}

// applyRegistrySelect returns a copy of base with the cluster's cherry-picks applied.
func applyRegistrySelect(base config.RegistryConfig, sel *fleet.RegistrySelect) config.RegistryConfig {
	out := base
	if sel == nil {
		return out
	}
	if sel.LocalRegistry != nil {
		out.LocalRegistry.Enabled = *sel.LocalRegistry
	}
	if sel.Mirrors != nil {
		want := *sel.Mirrors
		if containsWildcard(want) {
			return out // all configured mirrors
		}
		byName := make(map[string]config.RegistryMirror, len(base.Mirrors))
		for _, m := range base.Mirrors {
			byName[m.Name] = m
		}
		filtered := make([]config.RegistryMirror, 0, len(want))
		for _, name := range want {
			if m, ok := byName[name]; ok {
				filtered = append(filtered, m)
			}
		}
		out.Mirrors = filtered
	}
	return out
}

// validateMirrorSelections fails early if a manifest references a mirror name
// that is not defined in the infrastructure config's catalog.
func validateMirrorSelections(cs *fleet.Fleet, reg config.RegistryConfig) error {
	known := make(map[string]bool, len(reg.Mirrors))
	for _, m := range reg.Mirrors {
		known[m.Name] = true
	}
	check := func(cluster string, sel *fleet.RegistrySelect) error {
		if sel == nil || sel.Mirrors == nil {
			return nil
		}
		for _, name := range *sel.Mirrors {
			if name == fleet.MirrorsAll {
				continue
			}
			if !known[name] {
				return fmt.Errorf("cluster %q selects unknown mirror %q (not in registries.mirrors)", cluster, name)
			}
		}
		return nil
	}
	if err := check("<defaults>", cs.Spec.Defaults.Registries); err != nil {
		return err
	}
	for _, c := range cs.Spec.Clusters {
		if err := check(c.Name, c.Registries); err != nil {
			return err
		}
	}
	return nil
}

func printPlan(plan *fleet.Plan) {
	name := plan.Name
	if name == "" {
		name = "(unnamed)"
	}
	fmt.Printf("Fleet %s — maxParallel=%d strategy=%s\n\n", name, plan.MaxParallel, plan.Strategy)
	fmt.Printf("%-24s  %4s  %-8s  %-20s  %s\n", "NAME", "NUM", "ACTION", "DEPENDS-ON", "OPTIONS")
	fmt.Printf("%-24s  %4s  %-8s  %-20s  %s\n", strings.Repeat("-", 24), "----", "--------", strings.Repeat("-", 20), "-------")
	for _, pc := range plan.Clusters {
		action := "create"
		if pc.Exists {
			action = "skip"
		}
		deps := "-"
		if len(pc.DependsOn) > 0 {
			deps = strings.Join(pc.DependsOn, ",")
		}
		fmt.Printf("%-24s  %4d  %-8s  %-20s  %s\n", pc.Name, pc.Num, action, deps, planOptions(pc))
	}
}

func planOptions(pc fleet.PlannedCluster) string {
	var opts []string
	if pc.NodeVersion != "" {
		opts = append(opts, "node="+pc.NodeVersion)
	}
	if pc.Registries != nil && pc.Registries.Mirrors != nil {
		m := *pc.Registries.Mirrors
		switch {
		case containsWildcard(m):
			opts = append(opts, "mirrors=all")
		case len(m) == 0:
			opts = append(opts, "mirrors=none")
		default:
			opts = append(opts, "mirrors="+strings.Join(m, "+"))
		}
	}
	if pc.Addons != nil && pc.Addons.MetricsServer != nil && pc.Addons.MetricsServer.Enabled {
		opts = append(opts, "metrics-server")
	}
	if len(opts) == 0 {
		return "-"
	}
	return strings.Join(opts, " ")
}

func summarize(plan *fleet.Plan, results map[string]error) error {
	var created, skipped, failed int
	var failedNames []string
	for _, pc := range plan.Clusters {
		if pc.Exists {
			continue
		}
		switch err := results[pc.Name]; {
		case err == nil:
			created++
		case strings.HasPrefix(err.Error(), "skipped:"):
			skipped++
		default:
			failed++
			failedNames = append(failedNames, pc.Name)
		}
	}
	fmt.Printf("\napply complete: %d created, %d skipped, %d failed\n", created, skipped, failed)
	if failed > 0 {
		return fmt.Errorf("%d cluster(s) failed: %s", failed, strings.Join(failedNames, ", "))
	}
	return nil
}

// runClusterDeleteFromManifest deletes every cluster listed in a Fleet
// manifest that currently exists, in reverse-dependency order (dependents first).
func runClusterDeleteFromManifest(ctx context.Context, filename string, yes bool) error {
	data, err := readManifest(filename)
	if err != nil {
		return err
	}
	cs, err := fleet.Parse(data)
	if err != nil {
		return err
	}
	if err := cs.Validate(); err != nil {
		return fmt.Errorf("invalid Fleet: %w", err)
	}

	cfg, g, err := connectToRunningVM(ctx)
	if err != nil {
		return err
	}

	live, err := kind.ListClusters(ctx, g)
	if err != nil {
		return err
	}
	liveSet := make(map[string]bool, len(live))
	for _, n := range live {
		liveSet[n] = true
	}

	// Only delete manifest clusters that actually exist, dependents first.
	var targets []string
	for _, name := range cs.DeletionOrder() {
		if liveSet[name] {
			targets = append(targets, name)
		}
	}
	if len(targets) == 0 {
		fmt.Println("Nothing to delete — none of the manifest's clusters exist.")
		return nil
	}
	if !yes && filename == "-" {
		return errors.New("refusing to prompt while reading manifest from stdin; pass --yes to confirm")
	}

	return confirmAndDeleteClusters(ctx, g, cfg, targets, yes)
}

func readManifest(filename string) ([]byte, error) {
	if filename == "-" {
		return io.ReadAll(os.Stdin)
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("reading manifest %q: %w", filename, err)
	}
	return data, nil
}

func containsWildcard(s []string) bool {
	return slices.Contains(s, fleet.MirrorsAll)
}

func boolOrTrue(p *bool) bool {
	if p == nil {
		return true
	}
	return *p
}
