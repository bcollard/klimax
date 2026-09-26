// Package hostsudo runs the few privileged commands klimax needs on the Mac:
// the resolver file for the local DNS zone and the keychain trust for the local
// CA. (The host route has its own sudoers-backed path in internal/routing.)
package hostsudo

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Run executes args under sudo.
//
// interactive=false (launchd, CI) uses `sudo -n`: it succeeds with cached
// credentials or a NOPASSWD rule and otherwise fails fast with a message,
// instead of hanging on a password prompt nobody can answer. sudo reads the
// password from the terminal, not stdin, so stdin stays free for data.
func Run(interactive bool, stdin io.Reader, args ...string) error {
	full := args
	if !interactive {
		full = append([]string{"-n"}, args...)
	}
	cmd := exec.Command("sudo", full...)
	switch {
	case stdin != nil:
		cmd.Stdin = stdin
	case interactive:
		cmd.Stdin = os.Stdin
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sudo %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
