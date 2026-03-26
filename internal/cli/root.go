package cli

import (
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

var (
	configFile string
	debug      bool
)

// NewRootCmd builds the root cobra command for klimax.
func NewRootCmd(version string) *cobra.Command {
	root := &cobra.Command{
		Use:   "klimax",
		Short: "Lima-based VZ VM + multi-kind cluster manager",
		Long: `klimax manages a macOS Virtualization.framework (VZ) Lima VM,
installs Docker, creates and manages kind clusters, and sets up
pure L3 routing from the host into the kind bridge subnet.`,
		SilenceUsage: true,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			level := slog.LevelInfo
			if debug {
				level = slog.LevelDebug
			}
			slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
		},
	}

	root.PersistentFlags().StringVarP(&configFile, "config", "c", "config.yaml", "Path to klimax config file")
	root.PersistentFlags().BoolVar(&debug, "debug", false, "Enable debug logging")

	root.AddCommand(
		newUpCmd(),
		newDownCmd(),
		newDestroyCmd(),
		newStatusCmd(),
		newDoctorCmd(),
		newVersionCmd(version),
		newClusterCmd(),
		newDockerEnvCmd(),
	)

	return root
}
