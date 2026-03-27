package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

func newDockerContextCmd() *cobra.Command {
	var unset bool
	cmd := &cobra.Command{
		Use:   "docker-context",
		Short: "Create/switch to the klimax Docker context",
		Long: `Creates (or updates) a Docker context pointing to the klimax VM socket
and switches to it. Use --unset to switch back to the default context.

  klimax docker-context          # create context + docker context use klimax
  klimax docker-context --unset  # docker context use default`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDockerContext(cmd.Context(), unset)
		},
	}
	cmd.Flags().BoolVar(&unset, "unset", false, "Switch back to the default Docker context")
	return cmd
}

func runDockerContext(ctx context.Context, unset bool) error {
	cfg, err := loadAndValidate()
	if err != nil {
		return err
	}

	if unset {
		if err := dockerContext(ctx, "use", "default"); err != nil {
			return fmt.Errorf("switching to default context: %w", err)
		}
		fmt.Println("Switched to Docker context \"default\"")
		return nil
	}

	contextName := cfg.VM.Name
	socketHost := fmt.Sprintf("unix://%s/.%s.docker.sock", mustHomeDir(), cfg.VM.Name)

	// Create or update the context.
	if contextExists(ctx, contextName) {
		if err := dockerContext(ctx, "update", contextName, "--docker", "host="+socketHost); err != nil {
			return fmt.Errorf("updating Docker context %q: %w", contextName, err)
		}
	} else {
		if err := dockerContext(ctx, "create", contextName, "--docker", "host="+socketHost); err != nil {
			return fmt.Errorf("creating Docker context %q: %w", contextName, err)
		}
	}

	if err := dockerContext(ctx, "use", contextName); err != nil {
		return fmt.Errorf("switching to Docker context %q: %w", contextName, err)
	}
	fmt.Printf("Switched to Docker context %q (%s)\n", contextName, socketHost)
	if os.Getenv("DOCKER_HOST") != "" {
		fmt.Println("Warning: DOCKER_HOST is set in your shell and overrides the active context.")
		fmt.Println("  Run: eval $(klimax docker-env --unset)")
	}
	return nil
}

// contextExists reports whether a Docker context with the given name exists.
func contextExists(ctx context.Context, name string) bool {
	cmd := exec.CommandContext(ctx, "docker", "context", "inspect", name, "--format", "{{.Name}}")
	return cmd.Run() == nil
}

// dockerContext runs a `docker context <args>` command.
// stdout is suppressed (we print our own messages); stderr is captured and
// included in the returned error if the command fails.
func dockerContext(ctx context.Context, args ...string) error {
	all := append([]string{"context"}, args...)
	cmd := exec.CommandContext(ctx, "docker", all...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}
