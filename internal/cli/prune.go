package cli

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bcollard/klimax/internal/config"
	"github.com/bcollard/klimax/internal/vm"
	"github.com/docker/go-units"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// pruneTarget is one removable path with a reason and its size on disk.
type pruneTarget struct {
	path   string
	reason string
	size   int64
}

func newPruneCmd() *cobra.Command {
	var dryRun, downloads, yes bool
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove klimax's reclaimable cached files",
		Long: `Removes files klimax can safely re-create:

  • superseded Lima guest agents in ~/.klimax/share/lima (the one matching the
    Lima version THIS klimax binary is built against is kept — running prune from
    a different klimax build removes that build's agent, which is re-downloaded
    on its next 'klimax up')
  • registry cache directories under ~/.klimax/registry-cache that no longer
    correspond to a configured mirror (e.g. after renaming or removing one)

With --downloads, it also clears Lima's image download cache. That cache lives in
the OS cache directory and is shared with any other Lima instances on this host
(limactl, Colima, Rancher Desktop), so clearing it makes their next start
re-download base images too.

Live registry caches for configured mirrors are never touched — use
'klimax registry clean-cache' for those.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPrune(dryRun, downloads, yes)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show what would be removed without removing it")
	cmd.Flags().BoolVar(&downloads, "downloads", false, "Also clear Lima's shared image download cache")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip the confirmation prompt")
	return cmd
}

func runPrune(dryRun, downloads, yes bool) error {
	cfg, err := loadAndValidate()
	if err != nil {
		return err
	}

	targets := collectPruneTargets(cfg, downloads)
	if len(targets) == 0 {
		fmt.Println("Nothing to prune.")
		return nil
	}

	printPruneTargets(targets)

	if dryRun {
		fmt.Println("\n--dry-run: nothing removed.")
		return nil
	}

	if !yes {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return fmt.Errorf("refusing to remove %d path(s) non-interactively; pass --yes (or --dry-run)", len(targets))
		}
		fmt.Printf("\nRemove these %d path(s)? [y/N] ", len(targets))
		var answer string
		_, _ = fmt.Scanln(&answer)
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			fmt.Println("Aborted.")
			return nil
		}
	}

	var removed int64
	for _, t := range targets {
		if err := os.RemoveAll(t.path); err != nil {
			fmt.Printf("  ✗ %s: %v\n", abbreviateHome(t.path), err)
			continue
		}
		removed += t.size
	}
	fmt.Printf("Reclaimed %s.\n", units.BytesSize(float64(removed)))
	return nil
}

// printPruneTargets lists the targets grouped by reason, largest first within
// each group.
func printPruneTargets(targets []pruneTarget) {
	var total int64
	var order []string
	groups := map[string][]pruneTarget{}
	for _, t := range targets {
		if _, seen := groups[t.reason]; !seen {
			order = append(order, t.reason)
		}
		groups[t.reason] = append(groups[t.reason], t)
		total += t.size
	}

	fmt.Println("Reclaimable:")
	for _, reason := range order {
		g := groups[reason]
		sort.Slice(g, func(i, j int) bool { return g[i].size > g[j].size })
		var groupSize int64
		for _, t := range g {
			groupSize += t.size
		}
		fmt.Printf("  %s (%s):\n", reason, units.BytesSize(float64(groupSize)))
		for _, t := range g {
			fmt.Printf("    %-10s  %s\n", units.BytesSize(float64(t.size)), abbreviateHome(t.path))
		}
	}
	fmt.Printf("\nTotal: %s\n", units.BytesSize(float64(total)))
}

// abbreviateHome shortens $HOME to ~ for display.
func abbreviateHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if rest, ok := strings.CutPrefix(path, home+string(os.PathSeparator)); ok {
		return "~" + string(os.PathSeparator) + rest
	}
	return path
}

// collectPruneTargets lists the removable paths, largest first.
func collectPruneTargets(cfg *config.Config, downloads bool) []pruneTarget {
	var targets []pruneTarget

	// 1. Superseded guest agents. EnsureGuestAgent version-stamps the filename,
	//    so bumping the Lima module leaves the previous download behind.
	agentDir := filepath.Join(KlimaxHome(), "share", "lima")
	keep := filepath.Base(vm.GuestAgentCachePath(KlimaxHome()))
	if entries, err := os.ReadDir(agentDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || e.Name() == keep {
				continue
			}
			p := filepath.Join(agentDir, e.Name())
			targets = append(targets, pruneTarget{
				path:   p,
				reason: fmt.Sprintf("superseded Lima guest agents (keeping %s)", keep),
				size:   pathSize(p),
			})
		}
	}

	// 2. Registry cache dirs with no matching mirror in the config.
	configured := make(map[string]bool, len(cfg.Registries.Mirrors))
	for _, m := range cfg.Registries.Mirrors {
		configured[m.Name] = true
	}
	cacheRoot := filepath.Join(KlimaxHome(), "registry-cache")
	if entries, err := os.ReadDir(cacheRoot); err == nil {
		for _, e := range entries {
			if !e.IsDir() || configured[e.Name()] {
				continue
			}
			p := filepath.Join(cacheRoot, e.Name())
			targets = append(targets, pruneTarget{
				path:   p,
				reason: "caches for mirrors that are no longer configured",
				size:   pathSize(p),
			})
		}
	}

	// 3. Lima's shared download cache — opt-in, since other Lima instances use it.
	if downloads {
		if ucd, err := os.UserCacheDir(); err == nil {
			p := filepath.Join(ucd, "lima", "download")
			if _, err := os.Stat(p); err == nil {
				targets = append(targets, pruneTarget{
					path:   p,
					reason: "Lima image download cache — shared with other Lima instances, re-downloaded on demand",
					size:   pathSize(p),
				})
			}
		}
	}

	sort.Slice(targets, func(i, j int) bool { return targets[i].size > targets[j].size })
	return targets
}

// pathSize returns the total size of a file or directory tree, ignoring errors
// (an unreadable entry simply contributes nothing).
func pathSize(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // best-effort sizing
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}
