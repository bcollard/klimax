package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bcollard/klimax/internal/hostres"
	"github.com/lima-vm/lima/v2/pkg/localpathutil"
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
	// ImageDisk, when non-empty (e.g. "10GiB"), provisions a separate Lima data
	// disk mounted over the guest's container image store (/var/lib/containerd).
	// Named Lima disks live in $LIMA_HOME/_disks/<vm>-img and survive
	// `klimax destroy`, so kindest/node, registry:2 and locally built images are
	// not lost when the VM is re-created (a locally built image is the only kind
	// no registry mirror can restore).
	// Empty = disabled (the image store lives on the VM's root disk).
	// ⚠ Lima instance config: only takes effect on new VMs (klimax destroy && up).
	ImageDisk string `yaml:"imageDisk"`
	// Mounts are host directories shared into the guest over virtiofs.
	// Empty by default: klimax shares nothing but its own registry cache.
	// Changing this list is applied by `klimax up` (see vm.ReconcileMounts) —
	// unlike imageDisk, it does not need the VM recreated.
	Mounts []Mount `yaml:"mounts"`
}

// Mount shares a host directory with the guest over virtiofs.
//
// Lima mounts the directory at the *same absolute path* inside the VM unless
// MountPoint overrides it, and that convention is what makes `docker run -v`
// work with host paths: the Docker daemon runs in the guest and resolves a bind
// source there, so the path has to exist on both sides.
//
// Nothing is shared unless it is listed here. Without a matching entry, a bind
// like `-v /Users/you/conf:/etc/app` does not fail — dockerd simply creates the
// missing source path in the guest and the container sees an EMPTY directory.
type Mount struct {
	// Location is the host directory to share. A leading "~" is expanded;
	// relative paths are rejected.
	Location string `yaml:"location"`
	// MountPoint is the guest path. Defaults to Location, which is the only
	// value that makes host-path `-v` binds resolve — set it only when the
	// guest deliberately needs the directory somewhere else.
	// There is no "~" expansion for guest paths.
	MountPoint string `yaml:"mountPoint,omitempty"`
	// Writable lets the guest, and any container binding this path, write back
	// to the host directory. Defaults to false, matching Lima: a read-only
	// share is the safer thing to hand a container by accident.
	Writable bool `yaml:"writable,omitempty"`
}

// Expand resolves a leading "~" in Location and makes it absolute.
// MountPoint is left untouched: it names a guest path, which the host home
// directory has nothing to do with.
func (m Mount) Expand() (Mount, error) {
	loc, err := localpathutil.Expand(m.Location)
	if err != nil {
		return m, err
	}
	m.Location = filepath.Clean(loc)
	return m, nil
}

// GuestPath is where the mount appears inside the VM.
// Call it on an Expanded mount: with no explicit MountPoint the guest path is
// the host path, so an unexpanded "~/projects" would answer literally that.
func (m Mount) GuestPath() string {
	if m.MountPoint != "" {
		return m.MountPoint
	}
	return m.Location
}

// maxLimaDiskNameLen is the longest Lima disk name that survives round-tripping
// through an ext4 volume label.
//
// Lima formats a data disk with label "lima-<name>" and then decides, on EVERY
// boot, whether the disk needs first-time setup by testing for
// /dev/disk/by-label/lima-<name>. ext4 labels are capped at 16 bytes, so any
// name longer than 11 chars is silently truncated by mkfs — the by-label path
// Lima looks for then never exists and it REFORMATS the disk on every boot,
// destroying the image store it was meant to preserve.
//
// Found the hard way: "klimax-images" produced label "lima-klimax-imag".
const maxLimaDiskNameLen = 16 - len("lima-")

// ImageDiskName is the Lima data disk holding the container image store for the
// named VM. Lima disks are namespaced globally rather than per-instance, so the
// VM name is embedded to keep multiple klimax VMs from colliding.
//
// The result is always <= maxLimaDiskNameLen chars; long VM names fall back to a
// hashed suffix so distinct VMs still get distinct disks.
//
// Lives here rather than in internal/vm so that internal/limatemplate can use it
// without importing internal/vm (which imports limatemplate).
func ImageDiskName(vmName string) string {
	if name := vmName + "-img"; len(name) <= maxLimaDiskNameLen {
		return name
	}
	sum := sha256.Sum256([]byte(vmName))
	return vmName[:4] + "-" + hex.EncodeToString(sum[:])[:6]
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
	NodeVersion string `yaml:"nodeVersion"` // e.g. "v1.36.1"
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
	DefaultVMName = "klimax"
	// FallbackCPUs and FallbackMemory apply only when the host's CPU count or
	// RAM cannot be read. Normally both are scaled to the machine — see
	// hostres.DefaultCPUs / DefaultMemoryBytes.
	FallbackCPUs   = 4
	FallbackMemory = "8GiB"
	// DefaultDisk is the VM root disk. It is deliberately smaller than it used
	// to be: with DefaultImageDisk set, container images live on their own disk
	// (mounted over /var/lib/containerd), so the root disk only carries the OS,
	// Docker metadata and volumes. Both are sparse — the apparent size is not
	// what they consume on the Mac.
	DefaultDisk = "20GiB"
	// DefaultImageDisk enables the persistent container image store by default.
	// It survives `klimax destroy`, so recreating the VM no longer re-pulls
	// every image — and a locally built image, which no registry mirror can
	// restore, is no longer lost.
	DefaultImageDisk = "30GiB"
	DefaultKindCIDR  = "172.30.0.0/16"
	// DefaultKindNodeVersion is the kindest/node image the bundled kind CLI
	// (limatemplate.KindCLIVersion) is built and validated against. Keep the two
	// in sync; overriding nodeVersion away from this is unsupported (see the
	// warning emitted by kind.CreateCluster).
	DefaultKindNodeVersion = "v1.36.1"
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
	// CPUs and memory scale to the machine: a fixed default either wastes a big
	// Mac or over-commits a small one. Explicit config values always win.
	host := hostres.Read()
	if cfg.VM.CPUs == 0 {
		if n := hostres.DefaultCPUs(host.CPUs); n > 0 {
			cfg.VM.CPUs = n
		} else {
			cfg.VM.CPUs = FallbackCPUs
		}
	}
	if cfg.VM.Memory == "" {
		if b := hostres.DefaultMemoryBytes(host.MemoryBytes); b > 0 {
			cfg.VM.Memory = hostres.RoundedGiB(b)
		} else {
			cfg.VM.Memory = FallbackMemory
		}
	}
	if cfg.VM.Disk == "" {
		cfg.VM.Disk = DefaultDisk
	}
	if cfg.VM.ImageDisk == "" {
		cfg.VM.ImageDisk = DefaultImageDisk
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
// Documentation URLs surfaced in CLI output. The example config is not present
// on a Homebrew install — only in the repo — so point at the hosted copy rather
// than a filename the user cannot open.
const (
	// ExampleConfigURL is the annotated reference config.
	ExampleConfigURL = "https://github.com/bcollard/klimax/blob/main/config.example.yaml"
	// ConfigDocsURL is the configuration reference on the docs site.
	ConfigDocsURL = "https://klimax.dev/docs/configuration.html"
)

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

	header := "# klimax configuration — edit to customise, then re-run 'klimax up'\n# Reference: " + ConfigDocsURL + "\n\n"
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

// reservedGuestPaths are guest paths a mount must not land on. The first group
// is Lima's own list (limayaml.Validate rejects them outright); klimax adds the
// image-store mountpoint, where a host share would hide the container images the
// vm.imageDisk data disk exists to preserve.
var reservedGuestPaths = []string{
	"/", "/bin", "/dev", "/etc", "/home", "/opt", "/sbin", "/tmp", "/usr", "/var",
	"/var/lib/containerd",
}

// validateMounts checks vm.mounts for the mistakes that are cheap to catch here
// and expensive to debug later. It deliberately does no filesystem I/O — that a
// location actually exists is checked at `up` time, where the filesystem is real
// (see cli.checkMountLocations).
func validateMounts(mounts []Mount) []error {
	var errs []error
	seen := make(map[string]int, len(mounts))

	for i, m := range mounts {
		if strings.TrimSpace(m.Location) == "" {
			errs = append(errs, fmt.Errorf("vm.mounts[%d].location must not be empty", i))
			continue
		}
		// Reject relative paths *before* expansion. localpathutil.Expand would
		// happily resolve "projects" against the current working directory, so
		// the same config would mean different things depending on where klimax
		// was run from.
		if !filepath.IsAbs(m.Location) && !localpathutil.IsTildePath(m.Location) {
			errs = append(errs, fmt.Errorf("vm.mounts[%d].location %q must be an absolute path or start with \"~/\"", i, m.Location))
			continue
		}
		expanded, err := m.Expand()
		if err != nil {
			errs = append(errs, fmt.Errorf("vm.mounts[%d].location %q cannot be expanded: %w", i, m.Location, err))
			continue
		}
		if m.MountPoint != "" && !filepath.IsAbs(m.MountPoint) {
			// Lima does not tilde-expand guest paths, so "~/x" would become a
			// literal directory named "~" in the guest.
			errs = append(errs, fmt.Errorf("vm.mounts[%d].mountPoint %q must be an absolute guest path (no \"~\" expansion in the guest)", i, m.MountPoint))
			continue
		}

		guest := filepath.Clean(expanded.GuestPath())
		if slices.Contains(reservedGuestPaths, guest) {
			errs = append(errs, fmt.Errorf("vm.mounts[%d] would mount over the guest system path %q — pick a different mountPoint", i, guest))
			continue
		}
		if prev, dup := seen[guest]; dup {
			// Lima silently merges same-mountPoint entries, last writable wins.
			// Two entries claiming one guest path is always a config mistake.
			errs = append(errs, fmt.Errorf("vm.mounts[%d] and vm.mounts[%d] both mount at guest path %q", prev, i, guest))
			continue
		}
		seen[guest] = i
	}
	return errs
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

	errs = append(errs, validateMounts(cfg.VM.Mounts)...)

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
