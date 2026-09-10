package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bcollard/klimax/internal/config"
	"github.com/bcollard/klimax/internal/docker"
	"github.com/bcollard/klimax/internal/guest"
	"github.com/bcollard/klimax/internal/hostres"
	"github.com/bcollard/klimax/internal/limatemplate"
	"github.com/bcollard/klimax/internal/registry"
	"github.com/bcollard/klimax/internal/routing"
	"github.com/bcollard/klimax/internal/vm"
	"github.com/docker/go-units"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func newUpCmd() *cobra.Command {
	var showVMLogs bool
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Create/start the VM, provision Docker, create kind clusters, set up routing",
		RunE: func(cmd *cobra.Command, args []string) error {
			if showVMLogs {
				raiseLimaLogLevelForVMLogs(cmd)
			}
			return runUp(cmd.Context(), showVMLogs)
		},
	}
	cmd.Flags().BoolVar(&showVMLogs, "show-vm-logs", false, "Stream Lima host-agent logs and cloud-init boot progress to stderr during startup (implies --lima-log-level info)")
	return cmd
}

func runUp(ctx context.Context, showVMLogs bool) error {
	if err := ensureConfig(); err != nil {
		return err
	}
	cfg, err := loadAndValidate()
	if err != nil {
		return err
	}
	slog.Info("Using config", "path", configFile)

	warnOverCommittedResources(cfg)

	// 1. Ensure VM is running.
	mgr := vm.New(cfg.VM.Name, KlimaxHome())

	// If the VM doesn't exist yet, this `up` will create it — review the config
	// for evolution (new options) and node-version drift before baking anything in.
	if existing, ierr := mgr.Inspect(ctx); ierr == nil && existing == nil {
		if err := reviewConfigBeforeCreate(cfg); err != nil {
			return err
		}
	}

	// The image-store disk must exist before the instance starts: Lima fails to
	// start an instance whose additionalDisks reference a missing disk.
	if cfg.VM.ImageDisk != "" {
		diskName := config.ImageDiskName(cfg.VM.Name)
		if err := vm.EnsureImageDisk(ctx, diskName, cfg.VM.ImageDisk); err != nil {
			return fmt.Errorf("image disk: %w", err)
		}
		warnImageDiskDrift(diskName, cfg.VM.ImageDisk)
	}

	inst, err := mgr.EnsureRunning(ctx, cfg, showVMLogs)
	if err != nil {
		return fmt.Errorf("vm: %w", err)
	}
	slog.Info("VM is running", "name", inst.Name, "sshPort", inst.SSHLocalPort)

	// 2. Open SSH client.
	g, err := guest.NewClient(inst)
	if err != nil {
		return fmt.Errorf("guest SSH: %w", err)
	}

	// 3. Ensure kind Docker network.
	if err := docker.EnsureKindNetwork(ctx, g, cfg.Network.KindBridgeCIDR); err != nil {
		return fmt.Errorf("docker network: %w", err)
	}

	// 4. Ensure pull-through mirrors.
	if err := registry.EnsureRegistries(ctx, g, cfg.Registries); err != nil {
		return fmt.Errorf("registries: %w", err)
	}

	// 5. Detect lima0 IP (needed for routing).
	lima0IP, err := routing.Lima0IP(ctx, g)
	if err != nil {
		return fmt.Errorf("detecting lima0 IP: %w", err)
	}

	// 6. Install no-NAT rules + systemd persistence in guest.
	if err := routing.InstallNoNat(ctx, g, cfg.Network.KindBridgeCIDR); err != nil {
		return fmt.Errorf("routing rules: %w", err)
	}

	// 7. Add macOS route for kind CIDR → lima0.
	if err := routing.EnsureRoute(cfg.Network.KindBridgeCIDR, lima0IP); err != nil {
		return fmt.Errorf("macOS route: %w", err)
	}

	slog.Info("klimax up complete",
		"vm", cfg.VM.Name,
		"kindCIDR", cfg.Network.KindBridgeCIDR,
		"lima0IP", lima0IP,
		"dockerSocket", "~/."+cfg.VM.Name+".docker.sock",
	)
	fmt.Printf("\nVM ready.\n  eval $(klimax docker-env)          # use VM Docker daemon\n  klimax cluster create <name>       # create a kind cluster\n\n")
	return nil
}

// reviewConfigBeforeCreate runs just before a VM is created. It surfaces config
// evolution (options added in newer klimax versions that the user's config
// doesn't set) and, if the pinned kind.nodeVersion drifts from this version's
// default (matched to the bundled kind CLI), interactively offers to update it.
func reviewConfigBeforeCreate(cfg *config.Config) error {
	// 1. New options available but not set (config likely predates this version).
	if raw, err := os.ReadFile(configFile); err == nil {
		if missing, err := config.MissingKeys(raw); err == nil && len(missing) > 0 {
			fmt.Printf("Note: %s does not set these options available in this klimax version (defaults apply):\n  %s\n"+
				"  What each option does: %s\n"+
				"  Annotated example:     %s\n\n",
				configFile, strings.Join(missing, ", "), config.ConfigDocsURL, config.ExampleConfigURL)
		}
	}

	// 2. kind.nodeVersion drift vs the current default.
	if cfg.Kind.NodeVersion == config.DefaultKindNodeVersion {
		return nil
	}
	fmt.Printf("⚠ Your config pins kind.nodeVersion=%q, but this klimax version's default is %q\n"+
		"  (matched to the bundled kind CLI %s). A mismatched node version is unsupported and\n"+
		"  can make cluster creation fail.\n",
		cfg.Kind.NodeVersion, config.DefaultKindNodeVersion, limatemplate.KindCLIVersion)

	if !term.IsTerminal(int(os.Stdin.Fd())) {
		slog.Warn("Non-interactive: keeping the pinned nodeVersion. Edit the config or re-run interactively to update it.",
			"pinned", cfg.Kind.NodeVersion, "default", config.DefaultKindNodeVersion)
		return nil
	}

	fmt.Printf("Update kind.nodeVersion to %s in %s before creating the VM? [y/N] ", config.DefaultKindNodeVersion, configFile)
	var answer string
	_, _ = fmt.Scanln(&answer)
	if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
		fmt.Println("Keeping the pinned nodeVersion.")
		return nil
	}
	if err := rewriteNodeVersion(configFile, config.DefaultKindNodeVersion); err != nil {
		return fmt.Errorf("updating nodeVersion in %s: %w", configFile, err)
	}
	cfg.Kind.NodeVersion = config.DefaultKindNodeVersion
	fmt.Printf("Updated kind.nodeVersion to %s.\n\n", config.DefaultKindNodeVersion)
	return nil
}

// rewriteNodeVersion rewrites the kind.nodeVersion value in a config file in
// place, preserving indentation and any trailing inline comment.
func rewriteNodeVersion(path, newVersion string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	re := regexp.MustCompile(`(?m)^(\s*nodeVersion:\s*)("?[^"\s#]+"?)(\s*(#.*)?)$`)
	if !re.Match(data) {
		return fmt.Errorf("no nodeVersion line found")
	}
	out := re.ReplaceAll(data, []byte(`${1}"`+newVersion+`"${3}`))
	return os.WriteFile(path, out, 0o600)
}

// ensureConfig creates a default config file at configFile if it does not exist.
func ensureConfig() error {
	if _, err := os.Stat(configFile); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(configFile), 0o750); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}
	if err := config.WriteDefaultConfig(configFile); err != nil {
		return fmt.Errorf("writing default config: %w", err)
	}
	fmt.Printf("No config found — created default config at %s\n", configFile)
	fmt.Printf("Edit it to customise VM resources, then re-run 'klimax up'.\n\n")
	return nil
}

func loadAndValidate() (*config.Config, error) {
	cfg, err := config.LoadConfig(configFile)
	if err != nil {
		return nil, err
	}
	if err := config.Validate(cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return cfg, nil
}

// warnImageDiskDrift reports a mismatch between the configured vm.imageDisk size
// and the disk that actually exists.
//
// EnsureImageDisk only *creates* the disk; it never resizes one that is already
// there. So raising vm.imageDisk in the config has no effect on an existing VM,
// silently — which is exactly how a disk ends up full while the config claims it
// is much larger. Say so, and point at the command that fixes it.
func warnImageDiskDrift(diskName, want string) {
	wantBytes, err := units.RAMInBytes(want)
	if err != nil {
		return // config validation already reports a bad size
	}
	liveBytes, exists, err := vm.ImageDiskSize(diskName)
	if err != nil || !exists {
		return // nothing to compare against
	}
	switch {
	case liveBytes < wantBytes:
		slog.Warn("Image disk is smaller than vm.imageDisk in the config — the size in the config only applies when the disk is first created",
			"live", units.BytesSize(float64(liveBytes)), "configured", want,
			"fix", "klimax down && klimax disk resize-image "+want+" && klimax up")
	case liveBytes > wantBytes:
		slog.Warn("Image disk is larger than vm.imageDisk in the config — the config value is stale and would apply only to a freshly created disk",
			"live", units.BytesSize(float64(liveBytes)), "configured", want)
	}
}

// warnOverCommittedResources reports where the config asks the Mac for more than
// it has. Advisory only: the VM may still work (disks are sparse, and macOS will
// swap rather than refuse), and refusing to start over a heuristic would be worse
// than a slow VM. Each warning carries the specific key to change.
func warnOverCommittedResources(cfg *config.Config) {
	res := hostres.ReadFor(KlimaxHome())
	warnings := hostres.CheckResources(res, cfg.VM.CPUs, cfg.VM.Memory, cfg.VM.Disk, cfg.VM.ImageDisk)
	if len(warnings) == 0 {
		return
	}
	for _, w := range warnings {
		slog.Warn(w.Message, "hint", w.Hint)
	}
	fmt.Printf("  Adjust with: klimax config edit   (reference: %s)\n\n", config.ConfigDocsURL)
}

// raiseLimaLogLevelForVMLogs makes --show-vm-logs do what its name says.
//
// The flag enables Lima's cloud-init progress reporting, but Lima emits those
// lines through logrus at Info, and klimax deliberately quiets logrus to Error
// (see resolveLimaLogLevel). So on its own the flag turned the reporting on and
// then swallowed it — you also had to pass --debug, which nobody would guess.
//
// An explicit --lima-log-level still wins: someone who named a level meant it,
// including a quieter one.
func raiseLimaLogLevelForVMLogs(cmd *cobra.Command) {
	if f := cmd.Root().PersistentFlags().Lookup("lima-log-level"); f != nil && f.Changed {
		return
	}
	if logrus.GetLevel() < logrus.InfoLevel {
		logrus.SetLevel(logrus.InfoLevel)
	}
}
