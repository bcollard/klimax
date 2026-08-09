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
	"github.com/spf13/cobra"
)

func newShellCmd() *cobra.Command {
	var tty bool
	cmd := &cobra.Command{
		Use:     "shell [command [args...]]",
		Aliases: []string{"ssh"},
		Short:   "Open an interactive shell in the klimax VM, or run a command in it",
		Long: `With no arguments, opens an interactive SSH session in the klimax VM.

With arguments, runs that command in the VM and exits with its exit code —
stdin, stdout and stderr are passed through, so it composes in pipelines:

  klimax shell docker ps
  klimax shell -- bash -c 'kind get clusters | wc -l'
  cat script.sh | klimax shell bash -s

Use "--" before the command when it has flags of its own, so they are not
parsed as klimax flags.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runShell(cmd.Context(), args, tty)
		},
	}
	// Stop flag parsing at the first positional argument so that
	// `klimax shell docker ps -a` does not try to interpret -a.
	cmd.Flags().SetInterspersed(false)
	cmd.Flags().BoolVarP(&tty, "tty", "t", false, "Force pseudo-terminal allocation (for interactive commands)")
	return cmd
}

func runShell(ctx context.Context, args []string, tty bool) error {
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
	if tty {
		sshArgs = append([]string{"-t"}, sshArgs...)
	}
	if len(args) > 0 {
		// Quote each argument so the remote shell sees the same words the caller
		// typed (ssh concatenates remote args and lets the remote shell re-split).
		sshArgs = append(sshArgs, "--", shellQuote(args))
	}

	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh not found in PATH: %w", err)
	}

	cmd := exec.Command(sshBin, sshArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		// Propagate the remote command's exit code rather than wrapping it, so
		// callers can branch on it as they would running the command locally.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(args) > 0 {
			os.Exit(exitErr.ExitCode())
		}
		return err
	}
	return nil
}

// shellQuote renders args as a single single-quoted shell command line.
func shellQuote(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}
