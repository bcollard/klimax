package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the klimax user-facing configuration schema.
type Config struct {
	VM         VMConfig       `yaml:"vm"`
	Network    NetworkConfig  `yaml:"network"`
	Kind       KindConfig     `yaml:"kind"`
	Registries RegistryConfig `yaml:"registries"`
}

type VMConfig struct {
	Name    string `yaml:"name"`    // default: "klimax"
	CPUs    int    `yaml:"cpus"`    // default: 4
	Memory  string `yaml:"memory"`  // e.g. "10GiB"
	Disk    string `yaml:"disk"`    // e.g. "40GiB"
	Rosetta bool   `yaml:"rosetta"` // enable Rosetta 2 for amd64 containers (ARM64 only)
}

type NetworkConfig struct {
	// KindBridgeCIDR is the subnet for the Docker bridge network named "kind".
	KindBridgeCIDR string `yaml:"kindBridgeCIDR"` // e.g. "172.30.0.0/16"
	// DisablePortMirroring prevents Lima from auto-mirroring guest TCP ports to
	// 127.0.0.1 on the host. Defaults to true. Enabled by default so klimax
	// coexists cleanly with other Lima-based VMs (kind-on-lima, Rancher Desktop)
	// that manage kind clusters with overlapping port numbers — otherwise both VMs
	// race to mirror the same port (e.g. 7001) to 127.0.0.1 and confuse each
	// other's tooling.
	// When true, kubeconfigs use the VM's direct lima0 IP instead of 127.0.0.1,
	// and the API server cert includes the lima0 IP as a SAN. Set to false to
	// force loopback (127.0.0.1) addressing — e.g. if host security software
	// (CrowdStrike) blocks TCP connections to vzNAT IPs.
	// nil = default (true).
	// ⚠ Lima instance config: only takes effect on new VMs (klimax destroy && up).
	DisablePortMirroring *bool `yaml:"disablePortMirroring"`
}

// CustomDNSResolver forwards a DNS zone to one or more upstream resolvers via CoreDNS.
type CustomDNSResolver struct {
	// Domain is the DNS zone to forward (e.g. "runlocal.dev", "corp.internal").
	Domain string `yaml:"domain"`
	// Resolvers is the list of upstream nameservers for this zone.
	// Defaults to ["8.8.8.8", "8.8.4.4"] when omitted.
	Resolvers []string `yaml:"resolvers,omitempty"`
}

// KindConfig holds global defaults used by every `klimax cluster create` invocation.
// Cluster lifecycle is managed exclusively via `klimax cluster` subcommands — there
// is no cluster list here.
type KindConfig struct {
	// NodeVersion is the kindest/node image tag used when creating clusters.
	NodeVersion string `yaml:"nodeVersion"` // e.g. "v1.35.0"
	// MetalLBVersion is the MetalLB manifest version installed in each cluster.
	MetalLBVersion string `yaml:"metalLBVersion"` // e.g. "v0.16.1"
	// CustomDNSResolvers are extra DNS zones forwarded to custom upstream resolvers
	// by CoreDNS. Resolvers default to ["8.8.8.8", "8.8.4.4"] when omitted per entry.
	// Applied to every cluster at creation time. Empty by default (no extra zones).
	CustomDNSResolvers []CustomDNSResolver `yaml:"customDnsResolvers"`
	// AutoMergeKubeconfig merges the new cluster's context into ~/.kube/config
	// automatically after creation. Default: true.
	AutoMergeKubeconfig *bool `yaml:"autoMergeKubeconfig"`
	// AutoRemoveKubeconfig removes the cluster's context/cluster/user entries
	// from ~/.kube/config automatically on deletion. Default: true.
	AutoRemoveKubeconfig *bool `yaml:"autoRemoveKubeconfig"`
}

// ClusterConfig is an internal type used by the `kind` package and `cluster` CLI.
// It is not part of the user-facing YAML schema.
type ClusterConfig struct {
	Name string
	// Num drives subnet and API-server port allocation (1–99).
	// Auto-assigned as the lowest free slot when 0.
	Num    int
	Region string // topology.kubernetes.io/region label; default: europe-west<N>
	Zone   string // topology.kubernetes.io/zone label;   default: europe-west<N>-b
	// Labels are extra node labels applied to every node at creation. klimax
	// always adds managed-by=klimax on top of these.
	Labels map[string]string
}

// RegistryConfig controls the pull-through registry mirrors.
type RegistryConfig struct {
	Mirrors []RegistryMirror `yaml:"mirrors"`
	// CacheStorage controls where mirror registry data is persisted.
	// "host" (default): bind-mounted from ~/.klimax/registry-cache on the macOS host via virtiofs.
	// "guest": stored inside the VM at /var/lib/klimax/registry-cache (wiped on destroy).
	CacheStorage string `yaml:"cacheStorage"`
}

// RegistryMirror describes a pull-through cache container to run in the VM.
type RegistryMirror struct {
	Name      string `yaml:"name"`
	Port      int    `yaml:"port"`
	RemoteURL string `yaml:"remoteURL"`
	// Username/Password are optional; used for authenticated upstreams (e.g. Docker Hub).
	Username string `yaml:"username,omitempty"`
	Password string `yaml:"password,omitempty"`
}

// defaults applied when fields are zero-valued.
const (
	DefaultVMName   = "klimax"
	DefaultCPUs     = 4
	DefaultMemory   = "10GiB"
	DefaultDisk     = "40GiB"
	DefaultKindCIDR = "172.30.0.0/16"
	// DefaultKindNodeVersion is the kindest/node image the bundled kind CLI
	// (limatemplate.KindCLIVersion) is built and validated against. Keep the two
	// in sync; overriding nodeVersion away from this is unsupported (see the
	// warning emitted by kind.CreateCluster).
	DefaultKindNodeVersion = "v1.35.0"
	DefaultMetalLBVersion  = "v0.16.1"
)

// DefaultMirrors are the pull-through registry caches enabled by default.
// Users can override the full list via config; an empty slice disables all mirrors.
var DefaultMirrors = []RegistryMirror{
	{Name: "registry-dockerio", Port: 5030, RemoteURL: "https://registry-1.docker.io"},
	{Name: "registry-quayio", Port: 5010, RemoteURL: "https://quay.io"},
	{Name: "registry-gcrio", Port: 5020, RemoteURL: "https://gcr.io"},
	{Name: "registry-us-docker-pkgdev", Port: 5040, RemoteURL: "https://us-docker.pkg.dev"},
	{Name: "registry-us-central1-docker-pkgdev", Port: 5050, RemoteURL: "https://us-central1-docker.pkg.dev"},
}

// LoadConfig reads and parses a klimax YAML config file, applying defaults.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %q: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %q: %w", path, err)
	}
	applyDefaults(&cfg)
	return &cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.VM.Name == "" {
		cfg.VM.Name = DefaultVMName
	}
	if cfg.VM.CPUs == 0 {
		cfg.VM.CPUs = DefaultCPUs
	}
	if cfg.VM.Memory == "" {
		cfg.VM.Memory = DefaultMemory
	}
	if cfg.VM.Disk == "" {
		cfg.VM.Disk = DefaultDisk
	}
	if cfg.Network.KindBridgeCIDR == "" {
		cfg.Network.KindBridgeCIDR = DefaultKindCIDR
	}
	if cfg.Network.DisablePortMirroring == nil {
		cfg.Network.DisablePortMirroring = boolPtr(true)
	}
	if cfg.Kind.NodeVersion == "" {
		cfg.Kind.NodeVersion = DefaultKindNodeVersion
	}
	if cfg.Kind.MetalLBVersion == "" {
		cfg.Kind.MetalLBVersion = DefaultMetalLBVersion
	}
	// Fill default resolvers for any entry that omits them.
	for i := range cfg.Kind.CustomDNSResolvers {
		if len(cfg.Kind.CustomDNSResolvers[i].Resolvers) == 0 {
			cfg.Kind.CustomDNSResolvers[i].Resolvers = []string{"8.8.8.8", "8.8.4.4"}
		}
	}
	if cfg.Kind.AutoMergeKubeconfig == nil {
		cfg.Kind.AutoMergeKubeconfig = boolPtr(true)
	}
	if cfg.Kind.AutoRemoveKubeconfig == nil {
		cfg.Kind.AutoRemoveKubeconfig = boolPtr(true)
	}
	// If the user provided no mirrors section at all, use built-in defaults.
	if cfg.Registries.Mirrors == nil {
		cfg.Registries.Mirrors = append([]RegistryMirror(nil), DefaultMirrors...)
	}
	if cfg.Registries.CacheStorage == "" {
		cfg.Registries.CacheStorage = "host"
	}
}

// WriteDefaultConfig writes a default config file to path with sensible defaults.
// The directory must already exist or be created by the caller.
func WriteDefaultConfig(path string) error {
	cfg := &Config{}
	applyDefaults(cfg)

	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2) // 2-space indent (matches config.example.yaml)
	if err := enc.Encode(cfg); err != nil {
		return fmt.Errorf("marshaling default config: %w", err)
	}
	_ = enc.Close()

	// yaml encoding drops struct comments, so splice a cautionary note in above
	// the nodeVersion field (indented to match its 2-space nesting under kind:).
	note := "  # ⚠ nodeVersion is matched to the kind CLI bundled in the VM.\n" +
		"  # Changing it to another kindest/node tag is UNSUPPORTED and may cause cluster\n" +
		"  # creation to fail or hang (kubeadm/containerd errors), or other unexpected\n" +
		"  # behaviour. 'klimax cluster create' logs a warning when this is overridden.\n"
	body := strings.Replace(buf.String(), "  nodeVersion:", note+"  nodeVersion:", 1)

	header := "# klimax configuration — edit to customise, then re-run 'klimax up'\n# See https://github.com/bcollard/klimax for full documentation.\n\n"
	return os.WriteFile(path, []byte(header+body), 0o600)
}

func boolPtr(b bool) *bool { return &b }

// PortMirroringDisabled reports whether Lima port mirroring should be disabled.
// It defaults to true (the field is nil-defaulted to true in applyDefaults; a
// nil pointer here — e.g. a Config built without applyDefaults — is also treated
// as the default).
func (n NetworkConfig) PortMirroringDisabled() bool {
	return n.DisablePortMirroring == nil || *n.DisablePortMirroring
}

// sanitizeMirrorName strips a hostname-like string down to a safe container-name
// suffix (alphanumerics and '-') for use in a validation hint.
func sanitizeMirrorName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Validate checks the config for correctness.
func Validate(cfg *Config) error {
	var errs []error

	if cfg.VM.CPUs < 1 {
		errs = append(errs, fmt.Errorf("vm.cpus must be >= 1, got %d", cfg.VM.CPUs))
	}
	if cfg.VM.Name == "" {
		errs = append(errs, errors.New("vm.name must not be empty"))
	}

	if _, _, err := net.ParseCIDR(cfg.Network.KindBridgeCIDR); err != nil {
		errs = append(errs, fmt.Errorf("network.kindBridgeCIDR %q is not a valid CIDR: %w", cfg.Network.KindBridgeCIDR, err))
	}

	for _, m := range cfg.Registries.Mirrors {
		if m.Name == "" || m.Port == 0 || m.RemoteURL == "" {
			errs = append(errs, fmt.Errorf("registry mirror missing name/port/remoteURL: %+v", m))
		}
		// A mirror container joins the shared "kind" Docker network. If its name
		// looks like a hostname (contains a dot), Docker's embedded DNS resolves
		// that hostname to the container itself — so the pull-through proxy can no
		// longer reach the real upstream (it resolves to itself → 404 → containerd
		// silently falls back to slow, unauthenticated direct pulls). Forbid it.
		if strings.Contains(m.Name, ".") || strings.Contains(m.Name, ":") {
			errs = append(errs, fmt.Errorf("registry mirror name %q must not look like a hostname (no '.' or ':') — it would shadow its upstream on the kind Docker network; use e.g. %q",
				m.Name, "registry-"+sanitizeMirrorName(m.Name)))
		}
	}

	if s := cfg.Registries.CacheStorage; s != "host" && s != "guest" {
		errs = append(errs, fmt.Errorf("registries.cacheStorage must be \"host\" or \"guest\", got %q", s))
	}

	return errors.Join(errs...)
}
