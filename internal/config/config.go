package config

import (
	"errors"
	"fmt"
	"net"
	"os"

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
	Name   string `yaml:"name"`   // default: "klimax"
	CPUs   int    `yaml:"cpus"`   // default: 4
	Memory string `yaml:"memory"` // e.g. "10GiB"
	Disk   string `yaml:"disk"`   // e.g. "40GiB"
}

type NetworkConfig struct {
	// KindBridgeCIDR is the subnet for the Docker bridge network named "kind".
	KindBridgeCIDR string `yaml:"kindBridgeCIDR"` // e.g. "172.30.0.0/16"
}

// KindConfig holds global defaults used by every `klimax cluster create` invocation.
// Cluster lifecycle is managed exclusively via `klimax cluster` subcommands — there
// is no cluster list here.
type KindConfig struct {
	// NodeVersion is the kindest/node image tag used when creating clusters.
	NodeVersion string `yaml:"nodeVersion"` // e.g. "v1.32.0"
	// MetalLBVersion is the MetalLB manifest version installed in each cluster.
	MetalLBVersion string `yaml:"metalLBVersion"` // e.g. "v0.14.9"
	// CoreDNSDomains are extra DNS zones forwarded to 8.8.8.8/8.8.4.4 by CoreDNS.
	// Applied to every cluster at creation time. Default: ["runlocal.dev"]
	CoreDNSDomains []string `yaml:"coreDNSDomains"`
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
}

// RegistryConfig controls the local Docker registry and pull-through mirrors.
type RegistryConfig struct {
	LocalRegistry LocalRegistryConfig `yaml:"localRegistry"`
	Mirrors       []RegistryMirror    `yaml:"mirrors"`
}

type LocalRegistryConfig struct {
	Enabled bool `yaml:"enabled"` // default: true
	Port    int  `yaml:"port"`    // default: 5000
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
	DefaultVMName          = "klimax"
	DefaultCPUs            = 4
	DefaultMemory          = "10GiB"
	DefaultDisk            = "40GiB"
	DefaultKindCIDR        = "172.30.0.0/16"
	DefaultKindNodeVersion = "v1.32.0"
	DefaultMetalLBVersion  = "v0.14.9"
	DefaultLocalRegPort    = 5000
)

// DefaultMirrors are the pull-through registry caches enabled by default.
// Users can override the full list via config; an empty slice disables all mirrors.
var DefaultMirrors = []RegistryMirror{
	{Name: "registry-dockerio", Port: 5030, RemoteURL: "https://registry-1.docker.io"},
	{Name: "registry-quayio", Port: 5010, RemoteURL: "https://quay.io"},
	{Name: "registry-gcrio", Port: 5020, RemoteURL: "https://gcr.io"},
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
	if cfg.Kind.NodeVersion == "" {
		cfg.Kind.NodeVersion = DefaultKindNodeVersion
	}
	if cfg.Kind.MetalLBVersion == "" {
		cfg.Kind.MetalLBVersion = DefaultMetalLBVersion
	}
	if len(cfg.Kind.CoreDNSDomains) == 0 {
		cfg.Kind.CoreDNSDomains = []string{"runlocal.dev"}
	}
	// Registry defaults: local registry enabled by default.
	if !cfg.Registries.LocalRegistry.Enabled && cfg.Registries.LocalRegistry.Port == 0 {
		cfg.Registries.LocalRegistry.Enabled = true
	}
	if cfg.Registries.LocalRegistry.Port == 0 {
		cfg.Registries.LocalRegistry.Port = DefaultLocalRegPort
	}
	// If the user provided no mirrors section at all, use built-in defaults.
	if cfg.Registries.Mirrors == nil {
		cfg.Registries.Mirrors = append([]RegistryMirror(nil), DefaultMirrors...)
	}
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
	}

	return errors.Join(errs...)
}
