package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bcollard/marina/internal/vm"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// marina was called klimax before v1.0. `marina migrate` moves an existing
// klimax installation over. It is a clean break, not a compatibility layer:
// the klimax VM and its clusters are deleted and re-created under marina, and
// only what is expensive to rebuild is carried over — the config and the
// registry cache.
//
// What is deliberately NOT carried over:
//   - the image disk (~/.klimax/_disks/klimax-img): Lima decides whether a data
//     disk needs formatting by looking for the ext4 label lima-<disk name>, so a
//     disk renamed to marina-img would be reformatted on first boot. Images come
//     back from the registry cache.
//   - the local CA (~/.klimax/pki): its root is name-constrained to
//     .klimax.internal and cannot issue for marina.internal.
const (
	legacyName       = "klimax"
	legacyAutostart  = "dev.klimax.autostart"
	legacyDNSDomain  = "klimax.internal"
	legacyHomeSubdir = ".klimax"
)

func legacyHome() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, legacyHomeSubdir)
}

// legacyInstallPresent reports whether a klimax installation exists that has
// not been migrated: a klimax config, and no marina config yet.
func legacyInstallPresent() bool {
	_, errOld := os.Stat(filepath.Join(legacyHome(), "config.yaml"))
	_, errNew := os.Stat(filepath.Join(MarinaHome(), "config.yaml"))
	return errOld == nil && errors.Is(errNew, os.ErrNotExist)
}

func newMigrateCmd() *cobra.Command {
	var yes, dryRun bool
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Move a klimax installation (~/.klimax) over to marina",
		Long: `marina was called klimax before v1.0. This moves an existing installation over:

  1. deletes the klimax VM and its clusters (re-create them with marina afterwards)
  2. copies ~/.klimax/config.yaml to ~/.marina/config.yaml, renaming the VM
     "klimax" → "marina" and the DNS zone klimax.internal → marina.internal
  3. moves the registry cache, so no image is downloaded again
  4. removes the klimax launchd agent

Nothing else under ~/.klimax is touched; remove it yourself once marina works.
Safe to re-run: each step is skipped when already done.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMigrate(cmd.Context(), yes, dryRun)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Delete the klimax VM without asking")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show what would be done, change nothing")
	return cmd
}

func runMigrate(ctx context.Context, yes, dryRun bool) error {
	oldHome, newHome := legacyHome(), MarinaHome()
	if _, err := os.Stat(oldHome); err != nil {
		return fmt.Errorf("no klimax installation at %s", oldHome)
	}
	step := func(format string, a ...any) { fmt.Printf("→ "+format+"\n", a...) }
	if dryRun {
		step("dry run — nothing is changed")
	}

	oldCfgPath := filepath.Join(oldHome, "config.yaml")
	oldVM := legacyVMName(oldCfgPath)

	// 1. The klimax VM. Lima reads LIMA_HOME on every call, so pointing it at
	// the old home is enough to manage the old instance with this binary —
	// which matters, because `brew upgrade` has already removed klimax's.
	if err := deleteLegacyVM(ctx, oldHome, oldVM, yes, dryRun, step); err != nil {
		return err
	}

	// 2. Config.
	newCfgPath := filepath.Join(newHome, "config.yaml")
	switch _, err := os.Stat(newCfgPath); {
	case err == nil:
		step("config: %s already exists — left as is", newCfgPath)
	case errors.Is(err, os.ErrNotExist):
		data, err := os.ReadFile(oldCfgPath)
		if err != nil {
			step("config: no %s — marina up will write a default one", oldCfgPath)
			break
		}
		step("config: %s → %s (VM %q → %q, %s → marina.internal)", oldCfgPath, newCfgPath, oldVM, rewrittenVMName(oldVM), legacyDNSDomain)
		if !dryRun {
			if err := os.MkdirAll(newHome, 0o750); err != nil {
				return err
			}
			if err := os.WriteFile(newCfgPath, []byte(rewriteLegacyConfig(string(data))), 0o600); err != nil {
				return err
			}
		}
	default:
		return err
	}

	// 3. Registry cache. Same filesystem, so a rename: instant, whatever its size.
	oldCache, newCache := filepath.Join(oldHome, "registry-cache"), filepath.Join(newHome, "registry-cache")
	if _, err := os.Stat(oldCache); err == nil {
		if _, err := os.Stat(newCache); err == nil {
			step("registry cache: %s already exists — left both in place", newCache)
		} else {
			step("registry cache: %s → %s", oldCache, newCache)
			if !dryRun {
				if err := os.MkdirAll(newHome, 0o750); err != nil {
					return err
				}
				if err := os.Rename(oldCache, newCache); err != nil {
					return fmt.Errorf("moving the registry cache: %w", err)
				}
			}
		}
	}

	// 4. launchd agent: it would start the klimax binary, which no longer exists.
	home, _ := os.UserHomeDir()
	plist := filepath.Join(home, "Library", "LaunchAgents", legacyAutostart+".plist")
	hadAutostart := false
	if _, err := os.Stat(plist); err == nil {
		hadAutostart = true
		step("autostart: removing the klimax launchd agent (%s)", legacyAutostart)
		if !dryRun {
			_ = exec.Command("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), legacyAutostart)).Run()
			if err := os.Remove(plist); err != nil {
				return err
			}
		}
	}

	if dryRun {
		fmt.Println("\nDry run — re-run without --dry-run to apply. Then:")
	} else {
		fmt.Println("\nDone. Next:")
	}
	fmt.Println("  marina up                        # new VM; reuses the registry cache")
	fmt.Println("  marina cluster create <name>     # or: marina fleet create -f <file>  (apiVersion: marina.run/v1alpha1)")
	if hadAutostart {
		fmt.Println("  marina autostart install")
	}
	fmt.Println("  marina docker-context            # then: docker context rm klimax")
	fmt.Println("  marina skill install --force     # replaces the klimax Agent Skill")
	if _, err := os.Stat(filepath.Join(oldHome, "pki")); err == nil {
		fmt.Printf("  sudo security remove-trusted-cert -d %s   # the old klimax CA\n",
			filepath.Join(oldHome, "pki", legacyDNSDomain, "root.crt"))
	}
	fmt.Println("\nHostnames change from *.klimax.internal to *.marina.internal, and the fleet")
	fmt.Println("label from klimax.dev/fleet to marina.run/fleet. Remove ~/.klimax once marina works.")
	return nil
}

// deleteLegacyVM stops and deletes the klimax VM, if it still exists.
func deleteLegacyVM(ctx context.Context, oldHome, name string, yes, dryRun bool, step func(string, ...any)) error {
	prev, hadPrev := os.LookupEnv("LIMA_HOME")
	if err := os.Setenv("LIMA_HOME", oldHome); err != nil {
		return err
	}
	defer func() {
		if hadPrev {
			_ = os.Setenv("LIMA_HOME", prev)
		} else {
			_ = os.Unsetenv("LIMA_HOME")
		}
	}()

	mgr := vm.New(name, oldHome)
	inst, err := mgr.Inspect(ctx)
	if err != nil {
		return fmt.Errorf("inspecting the klimax VM: %w", err)
	}
	if inst == nil {
		step("klimax VM: already gone")
		return nil
	}
	step("klimax VM %q (%s): delete it, with every cluster on it", name, inst.Status)
	if dryRun {
		return nil
	}
	if !yes {
		if !isInteractive() {
			return errors.New("the klimax VM still exists: re-run with --yes to delete it (its clusters go with it)")
		}
		fmt.Print("Delete the klimax VM and its clusters now? [y/N] ")
		var answer string
		_, _ = fmt.Scanln(&answer)
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			return errors.New("aborted — nothing was changed")
		}
	}
	if err := mgr.Stop(ctx); err != nil {
		slog.Warn("Stopping the klimax VM failed — deleting anyway", "err", err)
	}
	return mgr.Delete(ctx)
}

// legacyVMName reads vm.name from the klimax config; "klimax" when unset.
func legacyVMName(cfgPath string) string {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return legacyName
	}
	var c struct {
		VM struct {
			Name string `yaml:"name"`
		} `yaml:"vm"`
	}
	if yaml.Unmarshal(data, &c) != nil || c.VM.Name == "" {
		return legacyName
	}
	return c.VM.Name
}

// rewrittenVMName is the VM name marina uses: the default name is renamed, a
// custom one is kept.
func rewrittenVMName(old string) string {
	if old == legacyName {
		return "marina"
	}
	return old
}

var legacyVMNameLine = regexp.MustCompile(`(?m)^(\s*name:\s*)"?klimax"?(\s*(#.*)?)$`)

// rewriteLegacyConfig adapts a klimax config for marina, line by line so the
// user's comments and layout survive: the default VM name and the DNS zone.
// Anything else is valid as-is — the schema did not change.
func rewriteLegacyConfig(s string) string {
	s = legacyVMNameLine.ReplaceAllString(s, `${1}"marina"${2}`)
	return strings.ReplaceAll(s, legacyDNSDomain, "marina.internal")
}
