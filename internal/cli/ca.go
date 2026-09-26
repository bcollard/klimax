package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"regexp"
	"slices"
	"time"

	"github.com/bcollard/marina/internal/config"
	"github.com/bcollard/marina/internal/guest"
	"github.com/bcollard/marina/internal/kind"
	"github.com/bcollard/marina/internal/localca"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

// caStore is the local CA for the configured DNS domain.
func caStore(cfg *config.Config) *localca.Store {
	return localca.New(MarinaHome(), cfg.DNSDomain())
}

func isInteractive() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// reconcileLocalCA runs in `up`: creates the root on first use and trusts it in
// the System keychain. Failures are warnings — everything else `up` set up
// still works, and HTTPS to the zone is the only thing waiting on it.
//
// Turning network.dns.tls off does not delete the CA or untrust it: a root
// that disappeared would break every certificate already issued, and removing
// trust is one explicit command (`marina ca untrust`).
func reconcileLocalCA(cfg *config.Config) {
	if !cfg.TLSEnabled() {
		return
	}
	store := caStore(cfg)
	root, _, created, err := store.EnsureRoot()
	if err != nil {
		slog.Warn("Local CA: could not create or load the root", "dir", store.Dir, "err", err)
		return
	}
	if created {
		slog.Info("Created the local CA", "root", store.RootCertPath(), "constrainedTo", "."+cfg.DNSDomain())
	}
	if time.Until(root.NotAfter) < 90*24*time.Hour {
		slog.Warn("Local CA root expires soon", "notAfter", root.NotAfter.Format(time.DateOnly), "root", store.RootCertPath())
	}
	if store.Trusted() {
		return
	}
	slog.Info("Trusting the local CA in the System keychain (needs sudo, once)", "root", store.RootCertPath())
	if err := store.Trust(isInteractive()); err != nil {
		slog.Warn("Could not trust the local CA — HTTPS to the zone works with --cacert but not in browsers yet",
			"err", err, "fix", "marina ca trust")
	}
}

// localCAForCluster returns a new cluster's CA material and the node trust
// store entries to install, or nil when TLS is off or the CA cannot be read.
func localCAForCluster(cfg *config.Config, cluster string, caCerts map[string]string) (*localca.Cluster, map[string]string) {
	if !cfg.TLSEnabled() {
		return nil, caCerts
	}
	c, err := caStore(cfg).EnsureCluster(cluster)
	if err != nil {
		slog.Warn("Local CA: could not issue the cluster's certificates — the cluster works, without them",
			"cluster", cluster, "err", err, "fix", "marina ca attach "+cluster)
		return nil, caCerts
	}
	// Nodes trust the root too, so containerd can pull from a registry served
	// on a marina.internal name.
	merged := maps.Clone(caCerts)
	if merged == nil {
		merged = map[string]string{}
	}
	merged[localca.NodeCAFile] = c.RootPEM
	return c, merged
}

// installLocalCA puts a freshly created cluster's CA material into it. A
// failure is a warning, for the same reason as installLocalDNS.
func installLocalCA(ctx context.Context, g *guest.Client, c *localca.Cluster) {
	if c == nil {
		return
	}
	res, err := localca.InstallInCluster(ctx, g, c.Name, c)
	if err != nil {
		slog.Warn("Local CA: could not install the cluster's certificates", "cluster", c.Name, "err", err, "fix", "marina ca attach "+c.Name)
		return
	}
	fmt.Printf("tls: wildcard %s in Secret %s/%s\n", c.WildcardNames[0], "default", localca.WildcardSecret)
	if res.IssuerNamespace != "" {
		fmt.Printf("tls: cert-manager ClusterIssuer %q issues for %s\n", localca.IssuerName, c.WildcardNames[0])
	}
}

// removeLocalCA deletes a deleted cluster's intermediate and wildcard. Done
// whatever network.dns.tls says now: the material may predate turning it off.
func removeLocalCA(cfg *config.Config, cluster string) {
	if err := caStore(cfg).RemoveCluster(cluster); err != nil {
		slog.Warn("Could not remove the cluster's CA material", "cluster", cluster, "err", err)
	}
}

// ─── marina ca ───────────────────────────────────────────────────────────────

func newCACmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ca",
		Short: "Manage the local CA for the marina.internal zone (network.dns.tls)",
		Long: `marina runs a certificate authority for the local DNS zone: a root on the Mac,
name-constrained to .<domain> and trusted in the System keychain, an intermediate per
cluster constrained to .<cluster>.<domain>, and a *.<cluster>.<domain> wildcard in each
cluster as the Secret default/marina-wildcard-tls.`,
	}
	cmd.AddCommand(newCAStatusCmd(), newCACertCmd(), newCAAttachCmd(), newCASecretCmd(), newCATrustCmd(), newCAUntrustCmd())
	return cmd
}

type caStatusReport struct {
	Enabled  bool              `json:"enabled"            yaml:"enabled"`
	Domain   string            `json:"domain"             yaml:"domain"`
	Root     string            `json:"root,omitempty"     yaml:"root,omitempty"`
	Exists   bool              `json:"exists"             yaml:"exists"`
	Trusted  bool              `json:"trusted"            yaml:"trusted"`
	NotAfter string            `json:"notAfter,omitempty" yaml:"notAfter,omitempty"`
	Clusters []caClusterStatus `json:"clusters"           yaml:"clusters"`
	Fleets   []caClusterStatus `json:"fleets"             yaml:"fleets"`
}

type caClusterStatus struct {
	Name     string `json:"name"               yaml:"name"`
	Wildcard string `json:"wildcard"           yaml:"wildcard"`
	NotAfter string `json:"notAfter,omitempty" yaml:"notAfter,omitempty"`
}

func newCAStatusCmd() *cobra.Command {
	var outputFmt string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the root CA, whether macOS trusts it, and each cluster's wildcard",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadAndValidate()
			if err != nil {
				return err
			}
			store := caStore(cfg)
			rep := caStatusReport{Enabled: cfg.TLSEnabled(), Domain: cfg.DNSDomain(), Clusters: []caClusterStatus{}, Fleets: []caClusterStatus{}}
			if root, err := store.Root(); err == nil {
				rep.Exists, rep.Root = true, store.RootCertPath()
				rep.NotAfter = root.NotAfter.Format(time.DateOnly)
				rep.Trusted = store.Trusted()
			}
			zoneStatus := func(kind localca.Kind, name string) caClusterStatus {
				st := caClusterStatus{Name: name, Wildcard: "*." + name + "." + cfg.DNSDomain()}
				if c, err := localca.LoadWildcard(store, kind, name); err == nil {
					st.NotAfter = c.NotAfter.Format(time.DateOnly)
				}
				return st
			}
			for _, name := range store.Clusters() {
				rep.Clusters = append(rep.Clusters, zoneStatus(localca.ClusterZone, name))
			}
			for _, name := range store.Fleets() {
				rep.Fleets = append(rep.Fleets, zoneStatus(localca.FleetZone, name))
			}
			switch outputFmt {
			case "json":
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(rep)
			case "yaml":
				return yaml.NewEncoder(os.Stdout).Encode(rep)
			}
			if !rep.Enabled {
				fmt.Println("Local CA is disabled (network.dns.tls.enabled: false)")
			}
			if !rep.Exists {
				fmt.Println("No root CA yet — run: marina up")
				return nil
			}
			trust := "trusted in the System keychain"
			if !rep.Trusted {
				trust = "NOT trusted — run: marina ca trust"
			}
			fmt.Printf("root:     %s\n          constrained to .%s, expires %s, %s\n", rep.Root, rep.Domain, rep.NotAfter, trust)
			for _, c := range rep.Clusters {
				fmt.Printf("cluster:  %-16s %s  (expires %s)\n", c.Name, c.Wildcard, c.NotAfter)
			}
			for _, f := range rep.Fleets {
				fmt.Printf("fleet:    %-16s %s  (expires %s)\n", f.Name, f.Wildcard, f.NotAfter)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&outputFmt, "output", "o", "text", "Output format: text, json, yaml")
	return cmd
}

func newCACertCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cert",
		Short: "Print the root CA certificate (PEM), e.g. for a client's trust store",
		Example: `  marina ca cert > marina-root-ca.crt
  marina ca cert | kubectl create configmap marina-root-ca -n my-app --from-file=ca.crt=/dev/stdin`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadAndValidate()
			if err != nil {
				return err
			}
			b, err := os.ReadFile(caStore(cfg).RootCertPath())
			if err != nil {
				return fmt.Errorf("no root CA yet (run marina up with network.dns.tls enabled): %w", err)
			}
			_, err = os.Stdout.Write(b)
			return err
		},
	}
}

func newCAAttachCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "attach <cluster>...",
		Short: "Issue or renew a cluster's wildcard and install it (also wires a cert-manager ClusterIssuer when cert-manager is present)",
		Long: `Clusters created with network.dns.tls enabled are attached automatically. Re-run this to:
  - add a cluster created before the local CA existed,
  - renew its wildcard (re-issued when within 30 days of expiry),
  - create the marina-ca ClusterIssuer after installing cert-manager.

Installing the root in the cluster's nodes restarts their containerd.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg, g, err := connectToRunningVM(ctx)
			if err != nil {
				return err
			}
			if !cfg.TLSEnabled() {
				return errors.New("network.dns.tls.enabled is false (or network.dns is off) in the config")
			}
			reconcileLocalCA(cfg)
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
				c, err := caStore(cfg).EnsureCluster(name)
				if err == nil {
					err = kind.ConfigureCACerts(ctx, g, name, map[string]string{localca.NodeCAFile: c.RootPEM})
				}
				var res localca.InstallResult
				if err == nil {
					res, err = localca.InstallInCluster(ctx, g, name, c)
				}
				if err != nil {
					slog.Error("Attach failed", "cluster", name, "err", err)
					failed = append(failed, name)
					continue
				}
				issuer := "no cert-manager in the cluster — install it, then re-run to add the marina-ca ClusterIssuer"
				if res.IssuerNamespace != "" {
					issuer = "ClusterIssuer " + localca.IssuerName + " ready"
				}
				fmt.Printf("✓ %s: %s in default/%s (expires %s); %s\n", name, c.WildcardNames[0], localca.WildcardSecret,
					c.WildcardNotAfter.Format(time.DateOnly), issuer)
				installFleetCA(ctx, g, cfg, name, liveFleetOf(ctx, g, name))
			}
			if len(failed) > 0 {
				return fmt.Errorf("%d cluster(s) not attached: %v", len(failed), failed)
			}
			return nil
		},
	}
}

var namespaceRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

func newCASecretCmd() *cobra.Command {
	var namespace string
	var fleetSecret bool
	cmd := &cobra.Command{
		Use:   "secret <cluster>",
		Short: "Copy the cluster's wildcard Secret into another namespace",
		Long: `A Secret can only be mounted from its own namespace, and an Ingress reads its TLS
Secret from the Ingress's namespace. This copies default/marina-wildcard-tls there
(--fleet: default/marina-fleet-wildcard-tls, the cluster's fleet's wildcard).
Re-run after 'marina ca attach' renews the wildcard.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !namespaceRE.MatchString(namespace) {
				return fmt.Errorf("invalid namespace %q", namespace)
			}
			_, g, err := connectToRunningVM(cmd.Context())
			if err != nil {
				return err
			}
			secret := localca.WildcardSecret
			if fleetSecret {
				secret = localca.FleetWildcardSecret
			}
			if err := localca.CopySecret(cmd.Context(), g, args[0], secret, namespace); err != nil {
				return err
			}
			fmt.Printf("✓ %s/%s\n", namespace, secret)
			return nil
		},
	}
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", "Target namespace (required)")
	cmd.Flags().BoolVar(&fleetSecret, "fleet", false, "Copy the fleet wildcard (marina-fleet-wildcard-tls) instead of the cluster's")
	_ = cmd.MarkFlagRequired("namespace")
	return cmd
}

func newCATrustCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "trust",
		Short: "Trust the root CA in the macOS System keychain (sudo)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadAndValidate()
			if err != nil {
				return err
			}
			store := caStore(cfg)
			if _, _, _, err := store.EnsureRoot(); err != nil {
				return err
			}
			if store.Trusted() {
				fmt.Println("Already trusted.")
				return nil
			}
			return store.Trust(isInteractive())
		},
	}
}

func newCAUntrustCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "untrust",
		Short: "Remove the root CA's trust setting from the macOS System keychain (sudo)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadAndValidate()
			if err != nil {
				return err
			}
			return caStore(cfg).Untrust(isInteractive())
		},
	}
}
