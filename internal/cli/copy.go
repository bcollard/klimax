package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/bcollard/klimax/internal/guest"
	"github.com/bcollard/klimax/internal/vm"
	"github.com/lima-vm/lima/v2/pkg/limatype"
	"github.com/spf13/cobra"
)

// guestPathPrefix is the prefix marking a path as living inside the VM.
const guestPathPrefix = "vm:"

func newCopyCmd() *cobra.Command {
	var recursive bool
	cmd := &cobra.Command{
		Use:     "copy <source>... <destination>",
		Aliases: []string{"cp"},
		Short:   "Copy files between the host and the klimax VM",
		Long: `Copies files to or from the klimax VM over SSH.

Prefix the VM side with "vm:" (the VM's configured name also works). Exactly one
side of the copy must carry the prefix:

  klimax copy ./script.sh vm:/tmp/script.sh
  klimax copy vm:/etc/containerd/certs.d/hosts.toml ./hosts.toml
  klimax copy -r ./manifests vm:/tmp/manifests

Multiple sources are allowed when the destination is a directory.`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCopy(cmd.Context(), args, recursive)
		},
	}
	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false, "Copy directories recursively")
	return cmd
}

func runCopy(ctx context.Context, args []string, recursive bool) error {
	cfg, err := loadAndValidate()
	if err != nil {
		return err
	}

	mgr := vm.New(cfg.VM.Name, KlimaxHome())
	inst, err := mgr.Inspect(ctx)
	if err != nil {
		return fmt.Errorf("inspecting VM: %w", err)
	}
	if inst == nil {
		return errors.New("VM does not exist; run 'klimax up' first")
	}
	if inst.Status != limatype.StatusRunning {
		return fmt.Errorf("VM is not running (status: %s); run 'klimax up' first", inst.Status)
	}

	opts, userHost, err := guest.SCPArgs(inst)
	if err != nil {
		return fmt.Errorf("building scp args: %w", err)
	}

	sources := args[:len(args)-1]
	dest := args[len(args)-1]

	resolved := make([]string, 0, len(args))
	guestSides := 0
	for _, p := range append(append([]string{}, sources...), dest) {
		path, isGuest := splitGuestPath(p, cfg.VM.Name)
		if isGuest {
			guestSides++
			resolved = append(resolved, userHost+":"+path)
			continue
		}
		resolved = append(resolved, path)
	}

	switch {
	case guestSides == 0:
		return fmt.Errorf("no %q prefix found — one side of the copy must be a VM path (e.g. %s/tmp/file)", guestPathPrefix, guestPathPrefix)
	case guestSides == len(resolved):
		return errors.New("both sides are VM paths; use 'klimax shell cp ...' to copy within the VM")
	}

	scpBin, err := exec.LookPath("scp")
	if err != nil {
		return fmt.Errorf("scp not found in PATH: %w", err)
	}

	scpArgs := opts
	if recursive {
		scpArgs = append(scpArgs, "-r")
	}
	scpArgs = append(scpArgs, resolved...)

	cmd := exec.Command(scpBin, scpArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("scp: %w", err)
	}
	return nil
}

// splitGuestPath strips a guest-path prefix ("vm:" or "<vmName>:") from p,
// reporting whether the path refers to the VM.
func splitGuestPath(p, vmName string) (string, bool) {
	for _, prefix := range []string{guestPathPrefix, vmName + ":"} {
		if rest, ok := strings.CutPrefix(p, prefix); ok {
			return rest, true
		}
	}
	return p, false
}
