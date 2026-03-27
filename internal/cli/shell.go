package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/bcollard/klimax/internal/guest"
	"github.com/bcollard/klimax/internal/vm"
	"github.com/spf13/cobra"
)

func newShellCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "shell",
		Short: "Open an interactive SSH session in the klimax VM",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runShell(cmd.Context())
		},
	}
}

func runShell(ctx context.Context) error {
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

	sshArgs, err := guest.SSHArgs(inst)
	if err != nil {
		return fmt.Errorf("building SSH args: %w", err)
	}

	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh not found in PATH: %w", err)
	}

	cmd := exec.Command(sshBin, sshArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
