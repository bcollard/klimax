package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/bcollard/klimax/internal/config"
	"github.com/bcollard/klimax/internal/vm"
	"github.com/docker/go-units"
	"github.com/lima-vm/lima/v2/pkg/limatype"
	"github.com/lima-vm/lima/v2/pkg/limatype/filenames"
	"github.com/spf13/cobra"
)

func newDiskCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "disk",
		Short: "Manage the klimax VM disk",
	}
	cmd.AddCommand(newDiskResizeCmd())
	cmd.AddCommand(newDiskResizeImageCmd())
	return cmd
}

func newDiskResizeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resize <size>",
		Short: "Grow the VM disk (e.g. 80GiB) — applied on the next VM start",
		Long: `Grows the klimax VM disk without recreating the VM.

The new size is written to vm.disk in the klimax config and to the Lima instance
config. Lima expands the disk image on the next VM start, and the guest root
filesystem is extended during boot, so the change takes effect after:

  klimax down && klimax up

Shrinking is not supported (neither by Lima nor by the guest filesystem).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDiskResize(cmd.Context(), args[0])
		},
	}
}

func runDiskResize(ctx context.Context, size string) error {
	cfg, err := loadAndValidate()
	if err != nil {
		return err
	}

	newBytes, err := units.RAMInBytes(size)
	if err != nil {
		return fmt.Errorf("invalid size %q (use e.g. 80GiB): %w", size, err)
	}
	oldBytes, err := units.RAMInBytes(cfg.VM.Disk)
	if err != nil {
		return fmt.Errorf("current vm.disk %q in %s is not a valid size: %w", cfg.VM.Disk, configFile, err)
	}

	switch {
	case newBytes == oldBytes:
		fmt.Printf("VM disk is already %s — nothing to do.\n", cfg.VM.Disk)
		return nil
	case newBytes < oldBytes:
		return fmt.Errorf("cannot shrink the disk: current size is %s, requested %s", cfg.VM.Disk, size)
	}

	// Update the klimax config so a future 'klimax destroy && up' keeps the size.
	if err := rewriteVMDisk(configFile, size); err != nil {
		return fmt.Errorf("updating vm.disk in %s: %w", configFile, err)
	}
	fmt.Printf("Updated vm.disk in %s: %s → %s\n", configFile, cfg.VM.Disk, size)

	// Update the live instance config, which is what Lima reads on start.
	mgr := vm.New(cfg.VM.Name, KlimaxHome())
	inst, err := mgr.Inspect(ctx)
	if err != nil {
		return fmt.Errorf("inspecting VM: %w", err)
	}
	if inst == nil {
		fmt.Printf("VM does not exist yet — it will be created with a %s disk on 'klimax up'.\n", size)
		return nil
	}

	limaYAML := filepath.Join(inst.Dir, filenames.LimaYAML)
	if err := rewriteLimaDisk(limaYAML, size); err != nil {
		return fmt.Errorf("updating disk in %s: %w", limaYAML, err)
	}
	fmt.Printf("Updated disk in %s\n", limaYAML)

	if inst.Status == limatype.StatusRunning {
		fmt.Printf("\nThe VM is running. Restart it to apply the new size:\n  klimax down && klimax up\n")
	} else {
		fmt.Printf("\nStart the VM to apply the new size:\n  klimax up\n")
	}
	return nil
}

func newDiskResizeImageCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resize-image <size>",
		Short: "Grow the persistent image-cache disk (e.g. 30GiB)",
		Long: `Grows the klimax VM's persistent image-cache disk (imageDisk in the config,
mounted over /var/lib/containerd in the guest).

Unlike 'klimax disk resize' (the VM's root disk), this disk survives
'klimax destroy' by design — it holds the container image store so images
don't need re-pulling after a VM recreate. Resizing it in place preserves
that cache, instead of deleting and recreating the disk at a new size.

The VM must be stopped first (klimax down) — the backing disk file cannot be
resized while attached to a running VM. The guest filesystem is grown
automatically on the next boot, so apply with:

  klimax down
  klimax disk resize-image <size>
  klimax up

Shrinking is not supported.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDiskResizeImage(cmd.Context(), args[0])
		},
	}
}

func runDiskResizeImage(ctx context.Context, size string) error {
	cfg, err := loadAndValidate()
	if err != nil {
		return err
	}
	if cfg.VM.ImageDisk == "" {
		return fmt.Errorf("imageDisk is not set in %s — nothing to resize", configFile)
	}

	diskName := config.ImageDiskName(cfg.VM.Name)
	if err := vm.ResizeImageDisk(ctx, diskName, size); err != nil {
		return err
	}

	// Keep the config in sync so a future disk recreation (or another machine
	// applying this config) uses the same size. It may already say `size` — the
	// config is what someone edits first when they want a bigger disk, only to
	// find it had no effect on the existing one.
	if cfg.VM.ImageDisk == size {
		fmt.Printf("imageDisk in %s already reads %s — left unchanged.\n", configFile, size)
	} else {
		if err := rewriteImageDisk(configFile, size); err != nil {
			return fmt.Errorf("updating imageDisk in %s: %w", configFile, err)
		}
		fmt.Printf("Updated imageDisk in %s: %s → %s\n", configFile, cfg.VM.ImageDisk, size)
	}
	fmt.Printf("\nStart the VM to mount the grown disk:\n  klimax up\n")
	return nil
}

// imageDiskRE matches the klimax config's `vm.imageDisk` line (2-space
// nested, value optionally quoted, optional trailing comment).
var imageDiskRE = regexp.MustCompile(`(?m)^(\s+imageDisk:\s*)("?[^"\s#]+"?)(\s*(#.*)?)$`)

// rewriteImageDisk rewrites imageDisk in the klimax config file in place,
// preserving indentation and any trailing inline comment.
func rewriteImageDisk(path, size string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out, ok := replaceFirst(imageDiskRE, data, []byte(`${1}"`+size+`"${3}`))
	if !ok {
		return fmt.Errorf("no imageDisk line found — set it manually to %q", size)
	}
	return os.WriteFile(path, out, 0o600)
}

// vmDiskRE matches the klimax config's `vm.disk` line (2-space nested, value
// optionally quoted, optional trailing comment).
var vmDiskRE = regexp.MustCompile(`(?m)^(\s+disk:\s*)("?[^"\s#]+"?)(\s*(#.*)?)$`)

// rewriteVMDisk rewrites vm.disk in the klimax config file in place, preserving
// indentation and any trailing inline comment.
func rewriteVMDisk(path, size string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out, ok := replaceFirst(vmDiskRE, data, []byte(`${1}"`+size+`"${3}`))
	if !ok {
		return fmt.Errorf("no vm.disk line found — set it manually to %q", size)
	}
	return os.WriteFile(path, out, 0o600)
}

// replaceFirst applies an expansion to the first match of re only, leaving any
// later matches untouched (`disk:` is a generic enough key to be worth the care).
func replaceFirst(re *regexp.Regexp, data, expansion []byte) ([]byte, bool) {
	loc := re.FindSubmatchIndex(data)
	if loc == nil {
		return data, false
	}
	out := make([]byte, 0, len(data)+len(expansion))
	out = append(out, data[:loc[0]]...)
	out = re.Expand(out, expansion, data, loc)
	out = append(out, data[loc[1]:]...)
	return out, true
}

// limaDiskRE matches the top-level `disk:` line of a Lima instance config.
var limaDiskRE = regexp.MustCompile(`(?m)^(disk:\s*)("?[^"\s#]+"?)(\s*(#.*)?)$`)

// rewriteLimaDisk rewrites the top-level disk size in a Lima instance config,
// keeping the file's existing permissions.
func rewriteLimaDisk(path, size string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	out, ok := replaceFirst(limaDiskRE, data, []byte("${1}"+size+"${3}"))
	if !ok {
		return fmt.Errorf("no top-level disk: line found")
	}
	return os.WriteFile(path, out, fi.Mode().Perm())
}
