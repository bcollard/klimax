package cli

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var (
	configFile   string
	debug        bool
	limaLogLevel string
)

// resolveLimaLogLevel maps the --lima-log-level flag to a logrus level. Lima's
// packages log via logrus; by default klimax hides those (Error level) so only
// klimax's own logs show. --debug surfaces them at info; an explicit flag wins.
func resolveLimaLogLevel(v string, debug bool) (logrus.Level, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "":
		if debug {
			return logrus.InfoLevel, nil
		}
		return logrus.ErrorLevel, nil
	case "off", "none", "quiet", "silent":
		return logrus.PanicLevel, nil
	default:
		lvl, err := logrus.ParseLevel(v)
		if err != nil {
			return 0, fmt.Errorf("invalid --lima-log-level %q (use: trace, debug, info, warn, error, off)", v)
		}
		return lvl, nil
	}
}

// KlimaxHome returns the klimax state directory (~/.klimax).
func KlimaxHome() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".klimax")
}

// NewRootCmd builds the root cobra command for klimax.
func NewRootCmd(version string) *cobra.Command {
	defaultConfig := filepath.Join(KlimaxHome(), "config.yaml")
	root := &cobra.Command{
		Use:   "klimax",
		Short: "Lima-based VZ VM + multi-kind cluster manager",
		Long: `klimax manages a macOS Virtualization.framework (VZ) Lima VM,
installs Docker, creates and manages kind clusters, and sets up
pure L3 routing from the host into the kind bridge subnet.`,
		SilenceUsage: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			level := slog.LevelInfo
			if debug {
				level = slog.LevelDebug
			}
			slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

			// Quiet Lima's (logrus) logs by default so only klimax logs show.
			// The hostagent subcommand re-configures logrus for its JSON event
			// stream, so this does not affect VM readiness detection.
			lvl, err := resolveLimaLogLevel(limaLogLevel, debug)
			if err != nil {
				return err
			}
			logrus.SetLevel(lvl)
			return nil
		},
	}

	root.PersistentFlags().StringVarP(&configFile, "config", "c", defaultConfig, "Path to klimax config file")
	root.PersistentFlags().BoolVar(&debug, "debug", false, "Enable debug logging (also surfaces Lima logs at info)")
	root.PersistentFlags().StringVar(&limaLogLevel, "lima-log-level", "", "Show Lima VM logs at this level (trace|debug|info|warn|error|off); hidden by default")

	root.CompletionOptions.DisableDefaultCmd = true
	root.AddCommand(
		newUpCmd(),
		newDownCmd(),
		newDestroyCmd(),
		newStatusCmd(),
		newDoctorCmd(),
		newVersionCmd(version),
		newClusterCmd(),
		newFleetCmd(),
		newKubeconfigCmd(),
		newRegistryCmd(),
		newConfigCmd(),
		newShellCmd(),
		newDockerEnvCmd(),
		newDockerContextCmd(),
		newHostagentCmd(),
		newSkillCmd(),
		newCompletionCmd(root),
	)

	return root
}
