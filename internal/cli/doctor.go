package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bcollard/klimax/internal/guest"
	"github.com/bcollard/klimax/internal/routing"
	"github.com/bcollard/klimax/internal/vm"
	"github.com/lima-vm/lima/v2/pkg/limatype"
	"github.com/spf13/cobra"
)

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose issues and print actionable fix commands",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(cmd.Context())
		},
	}
}

func runDoctor(ctx context.Context) error {
	cfg, err := loadAndValidate()
	if err != nil {
		return err
	}

	ok := true

	// Check for stale hostagent process (binary replaced while hostagent was running).
	checkHostagent(cfg.VM.Name)

	// Check VM
	mgr := vm.New(cfg.VM.Name, KlimaxHome())
	inst, err := mgr.Inspect(ctx)
	if err != nil {
		fmt.Printf("[FAIL] Could not inspect VM: %v\n", err)
		return nil
	}
	if inst == nil {
		fmt.Printf("[FAIL] VM %q does not exist\n", cfg.VM.Name)
		fmt.Printf("  Fix: klimax up -c %s\n", configFile)
		ok = false
	} else if inst.Status != limatype.StatusRunning {
		fmt.Printf("[FAIL] VM %q is %s (expected Running)\n", cfg.VM.Name, inst.Status)
		fmt.Printf("  Fix: klimax up -c %s\n", configFile)
		ok = false
	} else {
		fmt.Printf("[OK]   VM %q is Running\n", cfg.VM.Name)
	}

	// Check macOS route
	if routing.RouteExists(cfg.Network.KindBridgeCIDR) {
		fmt.Printf("[OK]   macOS route for %s is present\n", cfg.Network.KindBridgeCIDR)
	} else {
		fmt.Printf("[FAIL] macOS route for %s is missing\n", cfg.Network.KindBridgeCIDR)
		fmt.Printf("  Fix: klimax up -c %s\n", configFile)
		ok = false
	}

	// Guest checks only if VM is running
	if inst != nil && inst.Status == limatype.StatusRunning {
		g, err := guest.NewClient(inst)
		if err != nil {
			fmt.Printf("[FAIL] Cannot open SSH connection: %v\n", err)
			ok = false
		} else {
			// Check iptables no-NAT rule
			ruleOK, err := routing.CheckNoNatRule(ctx, g, cfg.Network.KindBridgeCIDR)
			if err != nil {
				fmt.Printf("[FAIL] Cannot check iptables rule: %v\n", err)
				ok = false
			} else if ruleOK {
				fmt.Printf("[OK]   iptables no-NAT exemption for %s is present\n", cfg.Network.KindBridgeCIDR)
			} else {
				fmt.Printf("[FAIL] iptables no-NAT exemption for %s is missing\n", cfg.Network.KindBridgeCIDR)
				fmt.Printf("  Fix: klimax up -c %s   (or run /usr/local/sbin/no-nat-kind.sh inside the VM)\n", configFile)
				ok = false
			}

			// Check IP forwarding
			fwd, err := g.Run(ctx, "cat /proc/sys/net/ipv4/ip_forward")
			if err != nil {
				fmt.Printf("[WARN] Cannot check IP forwarding: %v\n", err)
			} else if fwd == "1" {
				fmt.Println("[OK]   IP forwarding is enabled in guest")
			} else {
				fmt.Println("[FAIL] IP forwarding is disabled in guest")
				fmt.Println("  Fix: sudo sysctl -w net.ipv4.ip_forward=1  (inside the VM)")
				ok = false
			}
		}
	}

	if ok {
		fmt.Println("\nAll checks passed.")
	} else {
		fmt.Println("\nSome checks failed — see Fix suggestions above.")
	}
	return nil
}

// checkHostagent detects a running hostagent whose on-disk binary has been
// replaced since it was launched — a common cause of "zsh: killed" when running
// subsequent klimax commands.
func checkHostagent(instanceName string) {
	pidFile := filepath.Join(KlimaxHome(), instanceName, "ha.pid")
	data, err := os.ReadFile(pidFile)
	if err != nil {
		// No pid file — hostagent not running (or already cleaned up).
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return
	}

	// Check the binary path the process is running from.
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		// /proc is Linux; on macOS use the pid file as a proxy.
		// If the pid file exists the hostagent is probably still running.
		if _, err := os.FindProcess(pid); err == nil {
			fmt.Printf("[WARN] hostagent is running (pid %d)\n", pid)
			fmt.Printf("  If klimax commands fail with 'killed', the binary may have been replaced while hostagent was running.\n")
			fmt.Printf("  Fix: kill %d && rm -f %s %s\n", pid,
				filepath.Join(KlimaxHome(), instanceName, "ha.pid"),
				filepath.Join(KlimaxHome(), instanceName, "ha.sock"))
		}
		return
	}

	self, _ := os.Executable()
	if exe != self {
		fmt.Printf("[WARN] hostagent (pid %d) is running a different binary than the current klimax\n", pid)
		fmt.Printf("  hostagent binary: %s\n", exe)
		fmt.Printf("  current binary:   %s\n", self)
		fmt.Printf("  Fix: klimax down  (or: kill %d && rm -f %s %s)\n", pid,
			filepath.Join(KlimaxHome(), instanceName, "ha.pid"),
			filepath.Join(KlimaxHome(), instanceName, "ha.sock"))
	} else {
		fmt.Printf("[OK]   hostagent is running (pid %d)\n", pid)
	}
}
