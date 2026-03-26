package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newDockerEnvCmd() *cobra.Command {
	var unset bool
	cmd := &cobra.Command{
		Use:   "docker-env",
		Short: "Print the export command to point DOCKER_HOST at the klimax VM",
		Long: `Prints shell export commands that configure your terminal to use the
Docker daemon running inside the klimax VM.

  eval $(klimax docker-env)        # activate
  eval $(klimax docker-env --unset) # deactivate`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDockerEnv(unset)
		},
	}
	cmd.Flags().BoolVar(&unset, "unset", false, "Print commands to unset the Docker environment")
	return cmd
}

func runDockerEnv(unset bool) error {
	cfg, err := loadAndValidate()
	if err != nil {
		return err
	}

	socketPath := fmt.Sprintf("%s/.%s.docker.sock", mustHomeDir(), cfg.VM.Name)

	if unset {
		fmt.Println("unset DOCKER_HOST")
		fmt.Println("unset DOCKER_TLS_VERIFY")
		fmt.Println("# Run this command to deactivate klimax docker environment:")
		fmt.Println("#   eval $(klimax docker-env --unset)")
		return nil
	}

	fmt.Printf("export DOCKER_HOST=unix://%s\n", socketPath)
	fmt.Println("# Run this command to configure your shell:")
	fmt.Println("#   eval $(klimax docker-env)")
	return nil
}

func mustHomeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "~"
	}
	return h
}
