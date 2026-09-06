package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/bcollard/klimax/internal/config"
	"github.com/bcollard/klimax/internal/guest"
	"github.com/bcollard/klimax/internal/routing"
	"github.com/bcollard/klimax/internal/vm"
	"github.com/lima-vm/lima/v2/pkg/limatype"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// Check status values.
const (
	checkOK   = "ok"
	checkFail = "fail"
	checkWarn = "warn"
)

// Stable check IDs. These are part of the machine-readable contract — renaming
// one is a breaking change for anything consuming `klimax doctor -o json`.
const (
	checkIDHostagent   = "hostagent"
	checkIDVM          = "vm"
	checkIDRoute       = "route"
	checkIDRosettaHost = "rosetta-host"
	checkIDSSH         = "ssh"
	checkIDIPTables    = "iptables"
	checkIDIPForward   = "ip-forward"
	checkIDRosettaVM   = "rosetta-vm"
)

// doctorCheck is one diagnosis. Fixable marks the checks `--fix` can repair
// itself; everything else carries a Fix string for the operator to run.
type doctorCheck struct {
	ID       string `json:"id"                 yaml:"id"`
	Status   string `json:"status"             yaml:"status"`
	Message  string `json:"message"            yaml:"message"`
	Detail   string `json:"detail,omitempty"   yaml:"detail,omitempty"`
	Fix      string `json:"fix,omitempty"      yaml:"fix,omitempty"`
	Fixable  bool   `json:"fixable"            yaml:"fixable"`
	Fixed    *bool  `json:"fixed,omitempty"    yaml:"fixed,omitempty"`
	FixError string `json:"fixError,omitempty" yaml:"fixError,omitempty"`
}

// doctorReport is the machine-readable shape of `klimax doctor`.
type doctorReport struct {
	OK     bool          `json:"ok"     yaml:"ok"`
	Checks []doctorCheck `json:"checks" yaml:"checks"`
}

// doctorEnv carries the handles a fix needs. Guest is nil when the VM is not
// running, which is why every guest-side fix re-checks it.
type doctorEnv struct {
	cfg   *config.Config
	guest *guest.Client
}

func newDoctorCmd() *cobra.Command {
	var outputFmt string
	var fix bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose issues and print actionable fix commands",
		Long: `Diagnose common klimax problems and print a fix for each failure.

With --fix, klimax applies the repairs it can perform itself: the macOS host
route, the iptables no-NAT exemption, and guest IP forwarding. Everything else
(creating or starting the VM, installing Rosetta, clearing a stale hostagent)
stays advisory — those either need a full 'klimax up' or are too destructive to
run without you asking.

The macOS route fix shells out to sudo, exactly as 'klimax up' does.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(cmd.Context(), outputFmt, fix)
		},
	}
	cmd.Flags().StringVarP(&outputFmt, "output", "o", "text", "Output format: text, json, yaml")
	cmd.Flags().BoolVar(&fix, "fix", false, "Apply the fixes klimax can perform itself (route, iptables, IP forwarding)")
	return cmd
}

func runDoctor(ctx context.Context, outputFmt string, fix bool) error {
	rep, env, err := diagnose(ctx)
	if err != nil {
		return err
	}

	if fix {
		applyDoctorFixes(ctx, rep, env)
	}

	rep.OK = reportOK(rep)

	switch outputFmt {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	case "yaml":
		return yaml.NewEncoder(os.Stdout).Encode(rep)
	default: // "text"
		printDoctorText(rep, fix)
		return nil
	}
}

// reportOK is true when no check failed. Warnings do not fail the report — they
// flag things worth knowing that are not necessarily broken.
func reportOK(rep *doctorReport) bool {
	for _, c := range rep.Checks {
		if c.Status == checkFail {
			return false
		}
	}
	return true
}

// diagnose runs every check without changing anything.
func diagnose(ctx context.Context) (*doctorReport, *doctorEnv, error) {
	cfg, err := loadAndValidate()
	if err != nil {
		return nil, nil, err
	}
	rep := &doctorReport{}
	env := &doctorEnv{cfg: cfg}

	// Stale hostagent (binary replaced while it was running).
	if c := checkHostagent(cfg.VM.Name); c != nil {
		rep.Checks = append(rep.Checks, *c)
	}

	// VM.
	mgr := vm.New(cfg.VM.Name, KlimaxHome())
	inst, err := mgr.Inspect(ctx)
	if err != nil {
		rep.Checks = append(rep.Checks, doctorCheck{
			ID: checkIDVM, Status: checkFail,
			Message: fmt.Sprintf("Could not inspect VM: %v", err),
		})
		rep.OK = false
		return rep, env, nil
	}
	switch {
	case inst == nil:
		rep.Checks = append(rep.Checks, doctorCheck{
			ID: checkIDVM, Status: checkFail,
			Message: fmt.Sprintf("VM %q does not exist", cfg.VM.Name),
			Fix:     fmt.Sprintf("klimax up -c %s", configFile),
		})
	case inst.Status != limatype.StatusRunning:
		rep.Checks = append(rep.Checks, doctorCheck{
			ID: checkIDVM, Status: checkFail,
			Message: fmt.Sprintf("VM %q is %s (expected Running)", cfg.VM.Name, inst.Status),
			Fix:     fmt.Sprintf("klimax up -c %s", configFile),
		})
	default:
		rep.Checks = append(rep.Checks, doctorCheck{
			ID: checkIDVM, Status: checkOK,
			Message: fmt.Sprintf("VM %q is Running", cfg.VM.Name),
		})
	}

	// macOS route. Fixable only when the VM is running — the gateway is the
	// live lima0 IP, which can only be read from inside the VM.
	running := inst != nil && inst.Status == limatype.StatusRunning
	if routing.RouteExists(cfg.Network.KindBridgeCIDR) {
		rep.Checks = append(rep.Checks, doctorCheck{
			ID: checkIDRoute, Status: checkOK,
			Message: fmt.Sprintf("macOS route for %s is present", cfg.Network.KindBridgeCIDR),
		})
	} else {
		rep.Checks = append(rep.Checks, doctorCheck{
			ID: checkIDRoute, Status: checkFail,
			Message: fmt.Sprintf("macOS route for %s is missing", cfg.Network.KindBridgeCIDR),
			Fix:     fmt.Sprintf("klimax up -c %s", configFile),
			Fixable: running,
		})
	}

	// Rosetta on the host.
	rep.Checks = append(rep.Checks, checkRosettaHost(cfg.VM.Rosetta))

	if !running {
		return rep, env, nil
	}

	g, err := guest.NewClient(inst)
	if err != nil {
		rep.Checks = append(rep.Checks, doctorCheck{
			ID: checkIDSSH, Status: checkFail,
			Message: fmt.Sprintf("Cannot open SSH connection: %v", err),
		})
		return rep, env, nil
	}
	env.guest = g

	// iptables no-NAT exemption.
	ruleOK, err := routing.CheckNoNatRule(ctx, g, cfg.Network.KindBridgeCIDR)
	switch {
	case err != nil:
		rep.Checks = append(rep.Checks, doctorCheck{
			ID: checkIDIPTables, Status: checkFail,
			Message: fmt.Sprintf("Cannot check iptables rule: %v", err),
		})
	case ruleOK:
		rep.Checks = append(rep.Checks, doctorCheck{
			ID: checkIDIPTables, Status: checkOK,
			Message: fmt.Sprintf("iptables no-NAT exemption for %s is present", cfg.Network.KindBridgeCIDR),
		})
	default:
		rep.Checks = append(rep.Checks, doctorCheck{
			ID: checkIDIPTables, Status: checkFail,
			Message: fmt.Sprintf("iptables no-NAT exemption for %s is missing", cfg.Network.KindBridgeCIDR),
			Fix:     fmt.Sprintf("klimax up -c %s   (or run /usr/local/sbin/no-nat-kind.sh inside the VM)", configFile),
			Fixable: true,
		})
	}

	// Guest IP forwarding.
	fwd, err := g.Run(ctx, "cat /proc/sys/net/ipv4/ip_forward")
	switch {
	case err != nil:
		rep.Checks = append(rep.Checks, doctorCheck{
			ID: checkIDIPForward, Status: checkWarn,
			Message: fmt.Sprintf("Cannot check IP forwarding: %v", err),
		})
	case strings.TrimSpace(fwd) == "1":
		rep.Checks = append(rep.Checks, doctorCheck{
			ID: checkIDIPForward, Status: checkOK,
			Message: "IP forwarding is enabled in guest",
		})
	default:
		rep.Checks = append(rep.Checks, doctorCheck{
			ID: checkIDIPForward, Status: checkFail,
			Message: "IP forwarding is disabled in guest",
			Fix:     "sudo sysctl -w net.ipv4.ip_forward=1  (inside the VM)",
			Fixable: true,
		})
	}

	// Rosetta inside the VM.
	rosettaActive := false
	if out, rerr := g.Run(ctx, "test -e /proc/sys/fs/binfmt_misc/rosetta && echo yes || echo no"); rerr == nil {
		rosettaActive = strings.TrimSpace(out) == "yes"
	}
	switch {
	case cfg.VM.Rosetta && rosettaActive:
		rep.Checks = append(rep.Checks, doctorCheck{
			ID: checkIDRosettaVM, Status: checkOK,
			Message: "Rosetta is enabled in the VM (amd64 containers supported)",
		})
	case cfg.VM.Rosetta && !rosettaActive:
		rep.Checks = append(rep.Checks, doctorCheck{
			ID: checkIDRosettaVM, Status: checkFail,
			Message: "vm.rosetta is set but Rosetta is not active in the VM",
			Fix:     "install Rosetta on the host, then: klimax destroy -c " + configFile + " && klimax up -c " + configFile,
		})
	case rosettaActive:
		rep.Checks = append(rep.Checks, doctorCheck{
			ID: checkIDRosettaVM, Status: checkOK,
			Message: "Rosetta is active in the VM (vm.rosetta not set in config)",
		})
	default:
		rep.Checks = append(rep.Checks, doctorCheck{
			ID: checkIDRosettaVM, Status: checkOK,
			Message: "Rosetta is not enabled in the VM (vm.rosetta: false)",
		})
	}

	return rep, env, nil
}

// applyDoctorFixes repairs the failing checks marked Fixable, recording the
// outcome on each check. Fixes are best-effort and independent: one failing does
// not stop the others.
func applyDoctorFixes(ctx context.Context, rep *doctorReport, env *doctorEnv) {
	for i := range rep.Checks {
		c := &rep.Checks[i]
		if c.Status != checkFail || !c.Fixable {
			continue
		}
		var err error
		switch c.ID {
		case checkIDRoute:
			err = fixRoute(ctx, env)
		case checkIDIPTables:
			if env.guest == nil {
				err = fmt.Errorf("VM is not running")
			} else {
				err = routing.InstallNoNat(ctx, env.guest, env.cfg.Network.KindBridgeCIDR)
			}
		case checkIDIPForward:
			if env.guest == nil {
				err = fmt.Errorf("VM is not running")
			} else {
				_, err = env.guest.Run(ctx, "sudo sysctl -w net.ipv4.ip_forward=1")
			}
		default:
			continue
		}
		done := err == nil
		c.Fixed = &done
		if err != nil {
			c.FixError = err.Error()
			continue
		}
		c.Status = checkOK
		c.Message += " — fixed"
	}
}

// fixRoute adds the macOS route via the VM's live lima0 IP. It needs the guest
// because the gateway is assigned by macOS and is only discoverable in-VM.
func fixRoute(ctx context.Context, env *doctorEnv) error {
	if env.guest == nil {
		return fmt.Errorf("VM is not running")
	}
	ip, err := routing.Lima0IP(ctx, env.guest)
	if err != nil {
		return fmt.Errorf("resolving lima0 IP: %w", err)
	}
	return routing.EnsureRoute(env.cfg.Network.KindBridgeCIDR, ip)
}

// printDoctorText renders the human-readable report. The per-check wording is
// unchanged from before the machine-readable formats were added.
func printDoctorText(rep *doctorReport, fixed bool) {
	for _, c := range rep.Checks {
		switch c.Status {
		case checkOK:
			fmt.Printf("[OK]   %s\n", c.Message)
		case checkWarn:
			fmt.Printf("[WARN] %s\n", c.Message)
		default:
			fmt.Printf("[FAIL] %s\n", c.Message)
		}
		if c.Detail != "" {
			fmt.Printf("  %s\n", c.Detail)
		}
		switch {
		case c.FixError != "":
			fmt.Printf("  Fix failed: %s\n", c.FixError)
			if c.Fix != "" {
				fmt.Printf("  Fix: %s\n", c.Fix)
			}
		case c.Status != checkOK && c.Fix != "":
			fmt.Printf("  Fix: %s\n", c.Fix)
		}
	}

	if rep.OK {
		fmt.Println("\nAll checks passed.")
		return
	}
	if fixed {
		fmt.Println("\nSome checks still fail — see Fix suggestions above.")
		return
	}
	if anyFixable(rep) {
		fmt.Println("\nSome checks failed — see Fix suggestions above, or re-run with --fix.")
		return
	}
	fmt.Println("\nSome checks failed — see Fix suggestions above.")
}

// anyFixable reports whether at least one failing check could be repaired by
// --fix, so the text output only advertises the flag when it would do something.
func anyFixable(rep *doctorReport) bool {
	for _, c := range rep.Checks {
		if c.Status == checkFail && c.Fixable {
			return true
		}
	}
	return false
}

// checkRosettaHost reports whether Rosetta 2 is installed on the macOS host.
// Rosetta exists only on Apple Silicon and is required only when vm.rosetta: true
// (transparent amd64 container support). It fails only when it is a genuine
// problem — i.e. Rosetta is requested in config but not installed.
func checkRosettaHost(wanted bool) doctorCheck {
	if runtime.GOARCH != "arm64" {
		return doctorCheck{ID: checkIDRosettaHost, Status: checkOK,
			Message: "Rosetta N/A — host is not Apple Silicon"}
	}
	// The runtime file is present once `softwareupdate --install-rosetta` has run.
	if _, err := os.Stat("/Library/Apple/usr/libexec/oah/libRosettaRuntime"); err == nil {
		return doctorCheck{ID: checkIDRosettaHost, Status: checkOK,
			Message: "Rosetta 2 is installed on the host"}
	}
	if wanted {
		return doctorCheck{ID: checkIDRosettaHost, Status: checkFail,
			Message: "vm.rosetta is enabled but Rosetta 2 is not installed on the host",
			Fix:     "softwareupdate --install-rosetta --agree-to-license"}
	}
	return doctorCheck{ID: checkIDRosettaHost, Status: checkOK,
		Message: "Rosetta 2 not installed on host (only needed for vm.rosetta: true)"}
}

// checkHostagent detects a running hostagent whose on-disk binary has been
// replaced since it was launched — a common cause of "zsh: killed" when running
// subsequent klimax commands, because macOS amfid refuses the new binary.
// Returns nil when no pidfile is present.
//
// Deliberately not Fixable: the remedy kills a live process and removes its
// socket, which is too destructive to run without the operator asking.
func checkHostagent(instanceName string) *doctorCheck {
	pidFile := filepath.Join(KlimaxHome(), instanceName, "ha.pid")
	sockFile := filepath.Join(KlimaxHome(), instanceName, "ha.sock")

	data, err := os.ReadFile(pidFile)
	if err != nil {
		// No pid file — hostagent not running (or already cleaned up).
		return nil
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return &doctorCheck{ID: checkIDHostagent, Status: checkWarn,
			Message: fmt.Sprintf("hostagent pidfile %s is unreadable", pidFile),
			Fix:     "rm -f " + pidFile}
	}

	cleanup := fmt.Sprintf("kill %d && rm -f %s %s", pid, pidFile, sockFile)

	if !processAlive(pid) {
		return &doctorCheck{ID: checkIDHostagent, Status: checkWarn,
			Message: fmt.Sprintf("stale hostagent pidfile: pid %d is not running", pid),
			Detail:  "Left behind by a hostagent that exited, or by a PID from a previous boot.",
			Fix:     fmt.Sprintf("rm -f %s %s", pidFile, sockFile)}
	}

	exe, err := processExePath(pid)
	if err != nil {
		return &doctorCheck{ID: checkIDHostagent, Status: checkWarn,
			Message: fmt.Sprintf("hostagent is running (pid %d)", pid),
			Detail:  fmt.Sprintf("Could not determine its binary (%v). If klimax commands fail with 'killed', the binary may have been replaced while hostagent was running.", err),
			Fix:     cleanup}
	}

	self, _ := os.Executable()
	if sameFile(exe, self) {
		return &doctorCheck{ID: checkIDHostagent, Status: checkOK,
			Message: fmt.Sprintf("hostagent is running (pid %d)", pid)}
	}
	return &doctorCheck{ID: checkIDHostagent, Status: checkWarn,
		Message: fmt.Sprintf("hostagent (pid %d) is running a different binary than the current klimax", pid),
		Detail:  fmt.Sprintf("hostagent binary: %s\n  current binary:   %s", exe, self),
		Fix:     "klimax down  (or: " + cleanup + ")"}
}

// processAlive reports whether pid is a live process.
//
// os.FindProcess is NOT usable here: on every Unix it succeeds unconditionally,
// without checking that the process exists. Signal 0 performs the existence
// check without delivering anything; EPERM means the process is alive but owned
// by another user, which still counts as running.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// processExePath returns the on-disk path of the binary backing pid.
//
// /proc is Linux-only, so on macOS — the only platform klimax supports — it is
// never available, and the binary-replacement check this function exists for
// would silently never run. Fall back to ps, which reports the executable path
// on darwin.
func processExePath(pid int) (string, error) {
	if exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
		return exe, nil
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil {
		return "", fmt.Errorf("ps: %w", err)
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", errors.New("ps returned no command for the pid")
	}
	return path, nil
}

// sameFile compares two executable paths, resolving symlinks first: Homebrew
// installs klimax as /opt/homebrew/bin/klimax -> ../Caskroom/klimax/<ver>/klimax,
// and ps and os.Executable do not agree on which side of that link they report.
func sameFile(a, b string) bool {
	if a == b {
		return true
	}
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return ra == rb
}
