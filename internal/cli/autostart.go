package cli

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

const (
	// autostartLabel is the launchd job label (reverse-DNS of klimax.dev, matching
	// the klimax.dev/* label namespace used for cluster nodes).
	autostartLabel = "dev.klimax.autostart"
)

func newAutostartCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "autostart",
		Short: "Manage a launchd agent that starts the klimax VM at login",
		Long: `Installs a per-user launchd agent that runs 'klimax up' when you log in.

'klimax up' needs root only to add the macOS host route, and launchd cannot answer
a password prompt — so install the sudoers snippet first, or the VM will come up
without the route:

  klimax sudoers | sudo tee /etc/sudoers.d/klimax >/dev/null && sudo chmod 0440 /etc/sudoers.d/klimax

Output is appended to ~/.klimax/logs/autostart.log.`,
	}
	cmd.AddCommand(
		newAutostartInstallCmd(),
		newAutostartUninstallCmd(),
		newAutostartStatusCmd(),
	)
	return cmd
}

func newAutostartInstallCmd() *cobra.Command {
	var print bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install and load the launchd agent",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return runAutostartInstall(print) },
	}
	cmd.Flags().BoolVar(&print, "print", false, "Write the launchd plist to stdout instead of installing it")
	return cmd
}

func newAutostartUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "uninstall",
		Aliases: []string{"remove"},
		Short:   "Unload and remove the launchd agent",
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, args []string) error { return runAutostartUninstall() },
	}
}

func newAutostartStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether the launchd agent is installed and loaded",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return runAutostartStatus() },
	}
}

// autostartPlistPath returns ~/Library/LaunchAgents/<label>.plist.
func autostartPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", autostartLabel+".plist"), nil
}

// plistString renders s as an XML-escaped <string> value, so paths containing
// characters like & or < cannot produce a malformed plist.
func plistString(s string) string {
	var buf bytes.Buffer
	if err := xml.EscapeText(&buf, []byte(s)); err != nil {
		// EscapeText only fails if the writer fails; a bytes.Buffer cannot.
		return s
	}
	return buf.String()
}

// autostartPlist renders the launchd job. The klimax binary path and config path
// are baked in so the agent behaves like the invocation that installed it.
func autostartPlist(binPath, cfgPath, logPath string) string {
	binPath, cfgPath, logPath = plistString(binPath), plistString(cfgPath), plistString(logPath)
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>up</string>
    <string>--config</string>
    <string>%s</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>
  <key>ProcessType</key>
  <string>Interactive</string>
</dict>
</plist>
`, autostartLabel, binPath, cfgPath, logPath, logPath)
}

func runAutostartInstall(printOnly bool) error {
	plistPath, err := autostartPlistPath()
	if err != nil {
		return err
	}
	binPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving the klimax binary path: %w", err)
	}
	binPath, err = filepath.Abs(binPath)
	if err != nil {
		return fmt.Errorf("resolving the klimax binary path: %w", err)
	}
	cfgPath, err := filepath.Abs(configFile)
	if err != nil {
		return fmt.Errorf("resolving the config path: %w", err)
	}

	logDir := filepath.Join(KlimaxHome(), "logs")
	logPath := filepath.Join(logDir, "autostart.log")

	if printOnly {
		fmt.Print(autostartPlist(binPath, cfgPath, logPath))
		return nil
	}

	if err := os.MkdirAll(logDir, 0o750); err != nil {
		return fmt.Errorf("creating log directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o750); err != nil {
		return fmt.Errorf("creating LaunchAgents directory: %w", err)
	}
	if err := os.WriteFile(plistPath, []byte(autostartPlist(binPath, cfgPath, logPath)), 0o644); err != nil { //nolint:gosec // launchd requires a world-readable plist
		return fmt.Errorf("writing %s: %w", plistPath, err)
	}

	// Replace any previously loaded copy, then load the new one.
	domain := "gui/" + strconv.Itoa(os.Getuid())
	_ = exec.Command("launchctl", "bootout", domain+"/"+autostartLabel).Run()
	if out, err := exec.Command("launchctl", "bootstrap", domain, plistPath).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap failed: %w\n%s", err, strings.TrimSpace(string(out)))
	}

	fmt.Printf("Installed launchd agent %s\n  plist:  %s\n  runs:   %s up --config %s\n  logs:   %s\n",
		autostartLabel, plistPath, binPath, cfgPath, logPath)
	fmt.Printf("\nThe VM starts at your next login (it was also started now, if it wasn't already).\n")
	fmt.Printf("Check that the host route can be added without a password, or the VM will start without it:\n  klimax sudoers --check\n")
	return nil
}

func runAutostartUninstall() error {
	plistPath, err := autostartPlistPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(plistPath); errors.Is(err, os.ErrNotExist) {
		fmt.Println("Autostart agent is not installed.")
		return nil
	}

	// A job that is not currently loaded is not a failure — removing the plist is
	// what actually uninstalls it.
	domain := "gui/" + strconv.Itoa(os.Getuid())
	if out, err := exec.Command("launchctl", "bootout", domain+"/"+autostartLabel).CombinedOutput(); err != nil {
		slog.Debug("launchctl bootout", "err", err, "output", strings.TrimSpace(string(out)))
	}
	if err := os.Remove(plistPath); err != nil {
		return fmt.Errorf("removing %s: %w", plistPath, err)
	}
	fmt.Printf("Removed launchd agent %s (%s)\n", autostartLabel, plistPath)
	return nil
}

func runAutostartStatus() error {
	plistPath, err := autostartPlistPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(plistPath); err != nil {
		fmt.Printf("Autostart: not installed\n  install with: klimax autostart install\n")
		return nil
	}
	fmt.Printf("Autostart: plist present at %s\n", plistPath)

	domain := "gui/" + strconv.Itoa(os.Getuid())
	out, err := exec.Command("launchctl", "print", domain+"/"+autostartLabel).CombinedOutput()
	if err != nil {
		fmt.Printf("  launchd:  not loaded (run 'klimax autostart install' to load it)\n")
		return nil
	}
	fmt.Printf("  launchd:  loaded\n")
	// `launchctl print` repeats some keys for the job's nested endpoints; report
	// only the job's own (first) value for each.
	seen := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		t := strings.TrimSpace(line)
		for _, key := range []string{"state = ", "last exit code = ", "runs = "} {
			if strings.HasPrefix(t, key) && !seen[key] {
				seen[key] = true
				fmt.Printf("  %s\n", t)
			}
		}
	}
	fmt.Printf("  logs:     %s\n", filepath.Join(KlimaxHome(), "logs", "autostart.log"))
	return nil
}
