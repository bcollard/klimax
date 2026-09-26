package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"text/tabwriter"

	"github.com/bcollard/klimax/internal/config"
	"github.com/bcollard/klimax/internal/guest"
	"github.com/bcollard/klimax/internal/kind"
	"github.com/bcollard/klimax/internal/localdns"
	"github.com/bcollard/klimax/internal/routing"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

// localDNSRouting is the part of network.dns the guest routing script needs;
// the zero value turns the DNS rules off.
func localDNSRouting(cfg *config.Config) routing.LocalDNS {
	if !cfg.DNSEnabled() {
		return routing.LocalDNS{}
	}
	return routing.LocalDNS{ServerIP: cfg.DNSServerIP(), Domain: cfg.DNSDomain()}
}

// withLocalDNSForward returns kindCfg with the local zone added to the zones a
// new cluster's CoreDNS forwards, so pods resolve the same names the Mac does.
// A copy: the caller's slice is shared with the loaded config.
func withLocalDNSForward(cfg *config.Config, kindCfg config.KindConfig) config.KindConfig {
	if !cfg.DNSEnabled() {
		return kindCfg
	}
	kindCfg.CustomDNSResolvers = append(slices.Clone(kindCfg.CustomDNSResolvers), localdns.ClusterForward(cfg))
	return kindCfg
}

// installLocalDNS gives a freshly created cluster its ExternalDNS.
//
// A failure is reported, not returned: the cluster itself is fine, and failing
// `cluster create` over a DNS addon — typically a slow image pull — would leave
// the user deleting a working cluster to retry.
func installLocalDNS(ctx context.Context, g *guest.Client, cfg *config.Config, cluster string) {
	if !cfg.DNSEnabled() {
		return
	}
	if err := localdns.InstallExternalDNS(ctx, g, cfg, cluster); err != nil {
		slog.Warn("Local DNS: ExternalDNS did not become ready — the cluster works, its Services just have no names yet",
			"cluster", cluster, "err", err, "fix", "klimax dns attach "+cluster)
		return
	}
	fmt.Printf("dns: LoadBalancer Services resolve as %s\n", cfg.DNSNameExample(cluster))
}

// deleteCluster deletes a cluster and its DNS records. ExternalDNS goes down
// with the cluster, so it never removes its own records; without the purge a
// deleted cluster's names keep resolving to VIPs nothing answers on.
func deleteCluster(ctx context.Context, g *guest.Client, cfg *config.Config, name string) error {
	if err := kind.DeleteCluster(ctx, g, name); err != nil {
		return err
	}
	if cfg.DNSEnabled() {
		if err := localdns.PurgeCluster(ctx, g, cfg, name); err != nil {
			slog.Warn("Could not remove the cluster's DNS records", "cluster", name, "err", err)
		}
	}
	removeLocalCA(cfg, name)
	return nil
}

// reconcileHostResolver keeps /etc/resolver in line with network.dns. A
// failure is a warning: everything else `up` set up still works, and the fix is
// one command.
func reconcileHostResolver(cfg *config.Config) {
	interactive := term.IsTerminal(int(os.Stdin.Fd()))
	if err := localdns.EnsureHostResolver(cfg, interactive); err != nil {
		if !cfg.DNSEnabled() {
			slog.Warn("Could not remove the klimax resolver file", "err", err)
			return
		}
		slog.Warn("Could not write the macOS resolver file — names resolve inside the VM and clusters, not yet on the Mac",
			"path", localdns.ResolverPath(cfg), "err", err,
			"fix", "run 'klimax up' from a terminal, which can answer the sudo prompt")
	}
}

// ─── klimax dns ──────────────────────────────────────────────────────────────

func newDNSCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dns",
		Short: "Inspect and manage the local DNS zone for LoadBalancer Services (network.dns)",
	}
	cmd.AddCommand(newDNSListCmd(), newDNSAttachCmd())
	return cmd
}

func newDNSListCmd() *cobra.Command {
	var outputFmt string
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the names published in the local DNS zone",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, g, err := connectToRunningVM(cmd.Context())
			if err != nil {
				return err
			}
			if !cfg.DNSEnabled() {
				return errors.New("network.dns.enabled is false in the config")
			}
			recs, err := localdns.ListRecords(cmd.Context(), g, cfg)
			if err != nil {
				return err
			}
			if recs == nil {
				recs = []localdns.Record{} // [] rather than null in -o json
			}
			switch outputFmt {
			case "json":
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(recs)
			case "yaml":
				return yaml.NewEncoder(os.Stdout).Encode(recs)
			}
			if len(recs) == 0 {
				fmt.Printf("No names published under %s yet.\n", cfg.DNSDomain())
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tIP")
			for _, r := range recs {
				fmt.Fprintf(w, "%s\t%s\n", r.Name, r.IP)
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVarP(&outputFmt, "output", "o", "text", "Output format: text, json, yaml")
	return cmd
}

func newDNSAttachCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "attach <cluster>...",
		Short: "Publish an existing cluster's Services in the local DNS zone (installs ExternalDNS + the CoreDNS forward)",
		Long: `Clusters created with network.dns enabled are attached automatically. Use this for
clusters created before the feature was on, or to retry after a failed install.

Re-applies the cluster's CoreDNS config (your kind.customDnsResolvers plus the local
zone) and restarts CoreDNS, which briefly interrupts in-cluster DNS.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg, g, err := connectToRunningVM(ctx)
			if err != nil {
				return err
			}
			if !cfg.DNSEnabled() {
				return errors.New("network.dns.enabled is false in the config")
			}
			live, err := kind.ListClusters(ctx, g)
			if err != nil {
				return err
			}
			var failed []string
			for _, name := range args {
				if !slices.Contains(live, name) {
					slog.Error("No such cluster", "cluster", name)
					failed = append(failed, name)
					continue
				}
				if err := attachCluster(ctx, g, cfg, name); err != nil {
					slog.Error("Attach failed", "cluster", name, "err", err)
					failed = append(failed, name)
					continue
				}
				fmt.Printf("✓ %s: Services resolve as %s\n", name, cfg.DNSNameExample(name))
			}
			if len(failed) > 0 {
				return fmt.Errorf("%d cluster(s) not attached: %v", len(failed), failed)
			}
			return nil
		},
	}
}

func attachCluster(ctx context.Context, g *guest.Client, cfg *config.Config, name string) error {
	resolvers := withLocalDNSForward(cfg, cfg.Kind).CustomDNSResolvers
	if err := kind.ApplyCoreDNSPatch(ctx, g, name, resolvers); err != nil {
		return fmt.Errorf("CoreDNS forward: %w", err)
	}
	return localdns.InstallExternalDNS(ctx, g, cfg, name)
}
