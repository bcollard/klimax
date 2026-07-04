package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/bcollard/klimax/internal/kind"
	"github.com/spf13/cobra"
)

func newKubeconfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "kubeconfig",
		Aliases: []string{"kc"},
		Short:   "Manage kubeconfig for klimax clusters",
		Long: `Kubeconfig helpers for klimax clusters. Each cluster's kubeconfig is written
to ~/.kube/klimax/<name>.kubeconfig; 'merge'/'use' integrate it into the default
~/.kube/config.`,
	}
	cmd.AddCommand(
		newKubeconfigPathCmd(),
		newKubeconfigEnvCmd(),
		newKubeconfigMergeCmd(),
		newKubeconfigRemoveCmd(),
		newKubeconfigUseCmd(),
	)
	return cmd
}

func newKubeconfigPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path <name>",
		Short: "Print the path to a cluster's kubeconfig file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := kind.KindKubeconfigPath(args[0])
			warnIfKubeconfigMissing(path)
			fmt.Println(path)
			return nil
		},
	}
}

func newKubeconfigEnvCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "env <name>",
		Short: "Print 'export KUBECONFIG=<path>' for a cluster (eval to use its isolated kubeconfig)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return printKubeconfigEnv(args[0])
		},
	}
}

func newKubeconfigMergeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "merge <name>",
		Short: "Merge a cluster's context into ~/.kube/config",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClusterMerge(args[0])
		},
	}
}

func newKubeconfigRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "remove <name>",
		Aliases: []string{"rm"},
		Short:   "Remove a cluster's context/cluster/user from ~/.kube/config",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := removeFromKubeconfig(args[0]); err != nil {
				return err
			}
			fmt.Printf("Removed %q from ~/.kube/config\n", args[0])
			return nil
		},
	}
}

func newKubeconfigUseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use <name>",
		Short: "Merge a cluster and switch the active kubectl context to it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runKubeconfigUse(cmd.Context(), args[0])
		},
	}
}

// runKubeconfigUse merges the cluster into ~/.kube/config and switches the active
// kubectl context to it.
func runKubeconfigUse(ctx context.Context, name string) error {
	if err := runClusterMerge(name); err != nil {
		return err
	}
	c := exec.CommandContext(ctx, "kubectl", "config", "use-context", name)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("switching kubectl context to %q (is kubectl installed?): %w", name, err)
	}
	return nil
}

// printKubeconfigEnv prints the export command to point KUBECONFIG at a cluster's
// isolated kubeconfig file. Shared with the deprecated 'cluster use'.
func printKubeconfigEnv(name string) error {
	path := kind.KindKubeconfigPath(name)
	warnIfKubeconfigMissing(path)
	fmt.Printf("export KUBECONFIG=%s\n", path)
	return nil
}

func warnIfKubeconfigMissing(path string) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "warning: kubeconfig not found at %s\n", path)
	}
}
