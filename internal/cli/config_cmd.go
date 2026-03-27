package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/bcollard/klimax/internal/config"
	"github.com/spf13/cobra"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage klimax configuration",
	}
	cmd.AddCommand(newConfigEditCmd())
	return cmd
}

func newConfigEditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "edit",
		Short: "Open the klimax config file in $EDITOR",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfigEdit()
		},
	}
}

func runConfigEdit() error {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = os.Getenv("VISUAL")
	}
	if editor == "" {
		for _, e := range []string{"nano", "vi"} {
			if _, err := exec.LookPath(e); err == nil {
				editor = e
				break
			}
		}
	}
	if editor == "" {
		return fmt.Errorf("no editor found: set $EDITOR or $VISUAL")
	}

	if _, err := os.Stat(configFile); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(configFile), 0o750); err != nil {
			return fmt.Errorf("creating config dir: %w", err)
		}
		if err := config.WriteDefaultConfig(configFile); err != nil {
			return fmt.Errorf("creating default config: %w", err)
		}
		fmt.Printf("Created default config at %s\n", configFile)
	}

	cmd := exec.Command(editor, configFile)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
