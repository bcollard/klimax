package cli

import (
	"context"
	"errors"
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
	"github.com/lima-vm/lima/v2/pkg/limatype"
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

	// A share of a directory that isn't there fails deep inside Lima's VM start,
	// so check the paths while there is still a config key to blame.
	if err := checkMountLocations(cfg); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	// 1. Ensure VM is running.
	mgr := vm.New(cfg.VM.Name, KlimaxHome())

	// An Inspect failure is not fatal here: both branches below are advisory
	// pre-flight work, and EnsureRunning re-inspects and reports properly.
	if existing, ierr := mgr.Inspect(ctx); ierr == nil {
		if existing == nil {
			// The VM doesn't exist yet, so this `up` will create it — review the
			// config for evolution (new options) and node-version drift before
			// baking anything in.
			if err := reviewConfigBeforeCreate(cfg); err != nil {
				return err
			}
		} else if err := reconcileMounts(ctx, mgr, existing, cfg); err != nil {
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

	// 3. Detect lima0 IP. Resolved before anything pulls an image: it completes
	// the no_proxy list, and dockerd must have the proxy before step 5.
	lima0IP, err := routing.Lima0IP(ctx, g)
	if err != nil {
		return fmt.Errorf("detecting lima0 IP: %w", err)
	}

	// 4. Install extra CA certificates. Before the proxy: a TLS-intercepting
	// proxy is useless without its certificate trusted, and both end in the
	// same Docker restart.
	caChanged, err := reconcileCACerts(ctx, g, cfg)
	if err != nil {
		return fmt.Errorf("ca certificates: %w", err)
	}

	// 5. Give dockerd the proxy, if one is configured.
	if err := reconcileDockerProxy(ctx, g, cfg, lima0IP, caChanged); err != nil {
		return fmt.Errorf("docker proxy: %w", err)
	}

	// The mirrors get the same proxy dockerd did, host-inherited or not.
	effectiveCfg, err := effectiveProxy(ctx, g, cfg)
	if err != nil {
		return fmt.Errorf("resolving proxy: %w", err)
	}
	// Containers cannot reach a proxy on the VM's loopback, so they get the
	// gateway-rewritten form. dockerd and guest shells keep the literal value:
	// they run in the VM's own network namespace, where loopback is correct.
	effectiveProxyEnv := effectiveCfg.ProxyEnv(lima0IP)
	containerProxyEnv := effectiveCfg.ContainerProxyEnv(lima0IP, cfg.KindBridgeGateway())

	// Put the proxy in the guest environment as well. Lima only writes what the
	// Mac's system settings say; a proxy that came from the klimax config would
	// otherwise be invisible to every SSH session — and therefore to
	// `kind create cluster`, which reads its own environment to decide what to
	// inject into each node, and to the in-guest `kubectl apply -f https://…`
	// that installs MetalLB.
	if err := reconcileGuestProxyEnv(ctx, g, cfg, effectiveProxyEnv); err != nil {
		return fmt.Errorf("guest proxy environment: %w", err)
	}

	// 6. Ensure kind Docker network.
	if err := docker.EnsureKindNetwork(ctx, g, cfg.Network.KindBridgeCIDR); err != nil {
		return fmt.Errorf("docker network: %w", err)
	}

	// 7. Ensure pull-through mirrors.
	if err := registry.EnsureRegistries(ctx, g, cfg.Registries, containerProxyEnv, cfg.VM.CACerts.Enabled()); err != nil {
		return fmt.Errorf("registries: %w", err)
	}

	// 8. Install no-NAT rules + systemd persistence in guest.
	if err := routing.InstallNoNat(ctx, g, cfg.Network.KindBridgeCIDR); err != nil {
		return fmt.Errorf("routing rules: %w", err)
	}

	// 9. Add macOS route for kind CIDR → lima0.
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

// checkMountLocations rejects a vm.mounts entry whose host directory is not
// there.
//
// Lima only warns about a non-existent mount location and carries on, handing
// Virtualization.framework a share for a path that does not exist. That surfaces
// much later as an opaque VM start failure rather than as "you typed the path
// wrong", so klimax refuses up front, naming the config key.
func checkMountLocations(cfg *config.Config) error {
	var errs []error
	for i, m := range cfg.VM.Mounts {
		expanded, err := m.Expand()
		if err != nil {
			errs = append(errs, fmt.Errorf("vm.mounts[%d].location %q: %w", i, m.Location, err))
			continue
		}
		st, err := os.Stat(expanded.Location)
		switch {
		case errors.Is(err, os.ErrNotExist):
			errs = append(errs, fmt.Errorf("vm.mounts[%d].location %q does not exist — create it or remove the entry", i, expanded.Location))
		case err != nil:
			errs = append(errs, fmt.Errorf("vm.mounts[%d].location %q is not readable: %w", i, expanded.Location, err))
		case !st.IsDir():
			errs = append(errs, fmt.Errorf("vm.mounts[%d].location %q is not a directory", i, expanded.Location))
		}
	}
	return errors.Join(errs...)
}

// reconcileMounts brings an existing VM's host-directory shares in line with
// vm.mounts.
//
// Mounts are the one Lima instance setting worth reconciling in place. Adding a
// directory is a casual, frequent thing to want, and the alternative — the
// `klimax destroy && klimax up` that vm.imageDisk and disablePortMirroring
// require — throws away every kind cluster on the VM in order to share a folder.
// A mount has no on-disk state and no guest-side migration either: Lima reads
// the list when it starts, so a stopped VM plus an edited instance config is the
// entire change.
//
// Nothing is written while the VM runs. Lima would ignore the edit until a
// restart, and a half-applied config that disagrees with the running VM is worse
// than one that plainly says "restart to apply".
func reconcileMounts(ctx context.Context, mgr *vm.Manager, inst *limatype.Instance, cfg *config.Config) error {
	limaYAML := vm.InstanceYAMLPath(inst.Dir)
	live, err := vm.ReadInstanceMounts(limaYAML)
	if err != nil {
		slog.Warn("Could not read mounts from the instance config — leaving them unchanged",
			"path", limaYAML, "err", err)
		return nil
	}
	desired := limatemplate.BuildMounts(cfg)
	if vm.MountsEqual(live, desired) {
		return nil
	}

	fmt.Printf("\nvm.mounts differs from the VM's current shares:\n  on the VM:\n%s\n  in %s:\n%s\n",
		vm.DescribeMounts(live), configFile, vm.DescribeMounts(desired))

	if inst.Status != limatype.StatusRunning {
		if err := vm.WriteInstanceMounts(limaYAML, desired); err != nil {
			return fmt.Errorf("updating mounts in %s: %w", limaYAML, err)
		}
		fmt.Printf("Applied — the VM is stopped, so the new shares take effect as it starts.\n\n")
		return nil
	}

	if !term.IsTerminal(int(os.Stdin.Fd())) {
		slog.Warn("Non-interactive: leaving the VM's mounts unchanged",
			"apply", "klimax down && klimax up")
		fmt.Println()
		return nil
	}

	fmt.Printf("Applying this needs a VM restart, which stops every kind cluster on it.\nRestart the VM now? [y/N] ")
	var answer string
	_, _ = fmt.Scanln(&answer)
	if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
		fmt.Printf("Left unchanged. Apply later with: klimax down && klimax up\n\n")
		return nil
	}

	if err := mgr.Stop(ctx); err != nil {
		return fmt.Errorf("stopping the VM to apply mounts: %w", err)
	}
	if err := vm.WriteInstanceMounts(limaYAML, desired); err != nil {
		// The VM is stopped and the config is untouched, so the next `klimax up`
		// starts it exactly as it was. Say so rather than leaving the user
		// wondering what state they are in.
		return fmt.Errorf("updating mounts in %s (the VM is stopped; 'klimax up' restarts it unchanged): %w", limaYAML, err)
	}
	// EnsureRunning re-inspects, finds the VM stopped, and starts it with the
	// new mounts.
	fmt.Printf("Mounts updated — restarting the VM.\n\n")
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

// reconcileDockerProxy writes the dockerd proxy drop-in and restarts Docker when
// it changed. Called on every `klimax up`, so editing network.proxy takes effect
// without recreating the VM — the drop-in has no on-disk or guest-side state
// beyond the file itself.
//
// Removing the proxy from the config removes the drop-in, rather than leaving
// dockerd pointed at a proxy that is no longer there.
func reconcileDockerProxy(ctx context.Context, g *guest.Client, cfg *config.Config, lima0IP string, forceRestart bool) error {
	effective, err := effectiveProxy(ctx, g, cfg)
	if err != nil {
		return err
	}
	want := limatemplate.DockerProxyDropIn(effective, lima0IP)

	// `cat` of a missing file is an error, which is the "not present" case.
	have, _ := g.Run(ctx, "cat "+limatemplate.DockerProxyDropInPath+" 2>/dev/null")
	if strings.TrimSpace(have) == strings.TrimSpace(want) && !forceRestart {
		return nil
	}
	if strings.TrimSpace(have) == strings.TrimSpace(want) {
		// Only the trust store moved; dockerd reads it at start, so it still
		// needs the restart even though the drop-in is unchanged.
		_, err := g.Run(ctx, "sudo systemctl restart docker")
		return err
	}

	if want == "" {
		slog.Info("Removing Docker proxy configuration (network.proxy is no longer set)")
		if _, err := g.Run(ctx, "sudo rm -f "+limatemplate.DockerProxyDropInPath); err != nil {
			return err
		}
	} else {
		slog.Info("Configuring Docker proxy",
			"http", effective.Network.Proxy.HTTP,
			"https", effective.Network.Proxy.HTTPS,
			"source", proxySource(cfg))
		if err := g.WriteFile(ctx, limatemplate.DockerProxyDropInPath, want); err != nil {
			return err
		}
	}

	// A drop-in only takes effect after a daemon-reload plus a restart of the
	// unit itself; systemd does not re-read Environment= on its own.
	_, err = g.Run(ctx, "sudo systemctl daemon-reload && sudo systemctl restart docker")
	return err
}

// effectiveProxy resolves the proxy klimax should apply, returning a copy of cfg
// with network.proxy filled in.
//
// An explicit http/https in the config wins. Otherwise, when inheritFromHost is
// on (the default), the values come from the guest's /etc/environment, which
// Lima has already populated from the Mac's system proxy settings. Reading them
// back is better than re-deriving them here: Lima also rewrites loopback proxy
// addresses to the gateway the guest can actually reach, which a fresh reading
// of `scutil --proxy` on the host would miss.
func effectiveProxy(ctx context.Context, g *guest.Client, cfg *config.Config) (*config.Config, error) {
	if cfg.Network.Proxy.Enabled() || !cfg.Network.Proxy.InheritsFromHost() {
		return cfg, nil
	}
	env, err := guestEnvironmentProxy(ctx, g)
	if err != nil || len(env) == 0 {
		return cfg, err
	}
	out := *cfg
	out.Network.Proxy.HTTP = env["http_proxy"]
	out.Network.Proxy.HTTPS = env["https_proxy"]
	return &out, nil
}

// guestEnvironmentProxy reads the proxy variables Lima wrote into the guest's
// /etc/environment. Values there are KEY=value, optionally quoted.
func guestEnvironmentProxy(ctx context.Context, g *guest.Client) (map[string]string, error) {
	out, err := g.Run(ctx, "cat /etc/environment 2>/dev/null || true")
	if err != nil {
		return nil, err
	}
	// Strip klimax's own block first. It is written from the config, so reading
	// it back as "the host's proxy" would make a configured proxy self-
	// sustaining: removing network.proxy would never take effect, because the
	// previous run's value would be re-inherited every time.
	return parseEnvironmentProxy(stripKlimaxEnvBlock(out)), nil
}

// stripKlimaxEnvBlock removes the #KLIMAX-START..#KLIMAX-END section, leaving
// Lima's block and anything the image shipped with.
func stripKlimaxEnvBlock(content string) string {
	start := strings.Index(content, klimaxEnvStart)
	if start < 0 {
		return content
	}
	rest := content[start:]
	end := strings.Index(rest, klimaxEnvEnd)
	if end < 0 {
		return content[:start]
	}
	return content[:start] + rest[end+len(klimaxEnvEnd):]
}

// parseEnvironmentProxy extracts the proxy variables from /etc/environment
// content. Entries are KEY=value, optionally quoted, and either spelling of the
// name may appear.
func parseEnvironmentProxy(content string) map[string]string {
	env := map[string]string{}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		if k != "http_proxy" && k != "https_proxy" {
			continue
		}
		if v = strings.Trim(strings.TrimSpace(v), `"'`); v != "" {
			env[k] = v
		}
	}
	return env
}

// proxySource names where the applied proxy came from, so the log line explains
// a proxy the user did not put in their config.
func proxySource(cfg *config.Config) string {
	if cfg.Network.Proxy.Enabled() {
		return "config"
	}
	return "macOS system settings (via Lima)"
}

// caCertDir is where update-ca-certificates looks for extra trust anchors.
const caCertDir = "/usr/local/share/ca-certificates"

// reconcileCACerts installs the configured CA certificates into the guest trust
// store and reports whether anything changed.
//
// Certificates already reached a *new* VM through Lima's caCerts at first boot.
// This runs on every `up` so that adding one to an existing VM does not require
// recreating it — matching how vm.mounts and network.proxy behave — and so that
// removing one takes effect.
func reconcileCACerts(ctx context.Context, g *guest.Client, cfg *config.Config) (bool, error) {
	want, err := cfg.VM.CACerts.Load()
	if err != nil {
		return false, err
	}

	// Only klimax-managed files are considered; the image's own trust store is
	// never touched.
	listed, _ := g.Run(ctx, "ls "+caCertDir+"/klimax-*.crt 2>/dev/null || true")
	have := map[string]bool{}
	for _, line := range strings.Fields(listed) {
		have[filepath.Base(line)] = true
	}

	changed := false
	for _, name := range config.SortedNames(want) {
		path := caCertDir + "/" + name
		if have[name] {
			existing, _ := g.Run(ctx, "cat "+path+" 2>/dev/null")
			if strings.TrimSpace(existing) == strings.TrimSpace(want[name]) {
				delete(have, name)
				continue
			}
		}
		slog.Info("Installing CA certificate", "name", name)
		if err := g.WriteFile(ctx, path, want[name]); err != nil {
			return false, err
		}
		delete(have, name)
		changed = true
	}

	// Anything left in have is klimax-managed but no longer configured.
	for name := range have {
		slog.Info("Removing CA certificate no longer in the config", "name", name)
		if _, err := g.Run(ctx, "sudo rm -f "+caCertDir+"/"+name); err != nil {
			return false, err
		}
		changed = true
	}

	if !changed {
		return false, nil
	}
	if _, err := g.Run(ctx, "sudo update-ca-certificates --fresh"); err != nil {
		return false, fmt.Errorf("updating trust store: %w", err)
	}
	return true, nil
}

// klimaxEnvStart and klimaxEnvEnd delimit klimax's own block in
// /etc/environment. Lima owns a #LIMA-START/#LIMA-END block in the same file;
// the two must not tread on each other, and klimax's block is appended last so
// its values win when both set the same name.
const (
	klimaxEnvStart = "#KLIMAX-START"
	klimaxEnvEnd   = "#KLIMAX-END"
)

// reconcileGuestProxyEnv maintains klimax's block in the guest's
// /etc/environment, so SSH sessions — and everything they launch — see the
// proxy from the klimax config.
//
// Only the proxy variables go in. This is not a general-purpose env mechanism,
// and /etc/environment is read by every login on the VM.
func reconcileGuestProxyEnv(ctx context.Context, g *guest.Client, cfg *config.Config, env map[string]string) error {
	var block strings.Builder
	if !cfg.Network.Proxy.Enabled() {
		// Host-inherited values already live in Lima's block; duplicating them
		// would fight with Lima on every boot.
		env = nil
	}
	if len(env) > 0 {
		block.WriteString(klimaxEnvStart + "\n")
		for _, k := range sortedKeys(env) {
			fmt.Fprintf(&block, "%s=%s\n", k, env[k])
		}
		block.WriteString(klimaxEnvEnd + "\n")
	}

	current, _ := g.Run(ctx, "cat /etc/environment 2>/dev/null || true")
	if extractKlimaxEnvBlock(current) == strings.TrimSuffix(block.String(), "\n") {
		return nil
	}

	slog.Info("Updating guest proxy environment", "vars", len(env))
	// sed deletes any previous klimax block; the new one is appended.
	script := fmt.Sprintf(`#!/bin/bash
set -euo pipefail
sudo sed -i '/%s/,/%s/d' /etc/environment
`, klimaxEnvStart, klimaxEnvEnd)
	if block.Len() > 0 {
		script += fmt.Sprintf("sudo tee -a /etc/environment >/dev/null <<'KLIMAX_ENV_EOF'\n%sKLIMAX_ENV_EOF\n", block.String())
	}
	return g.RunScript(ctx, "update guest proxy environment", script)
}

// extractKlimaxEnvBlock returns klimax's block from /etc/environment content,
// without a trailing newline, or "" when absent.
func extractKlimaxEnvBlock(content string) string {
	start := strings.Index(content, klimaxEnvStart)
	if start < 0 {
		return ""
	}
	end := strings.Index(content[start:], klimaxEnvEnd)
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(content[start : start+end+len(klimaxEnvEnd)])
}
