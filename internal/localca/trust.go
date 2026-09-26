package localca

import (
	"os/exec"

	"github.com/bcollard/homepki/pkg/pki"
	"github.com/bcollard/klimax/internal/hostsudo"
)

// Trusted reports whether macOS trusts the root. Unprivileged: `security
// verify-cert` evaluates the certificate against the current trust settings.
func (s *Store) Trusted() bool {
	args := pki.MacOSTrustStatusArgs(s.RootCertPath())
	return exec.Command(args[0], args[1:]...).Run() == nil
}

// Trust adds the root to the System keychain as a trusted root. Needs sudo;
// interactive=false uses `sudo -n` so an unattended run fails fast instead of
// hanging on a prompt.
func (s *Store) Trust(interactive bool) error {
	return hostsudo.Run(interactive, nil, pki.MacOSTrustInstallArgs(s.RootCertPath())...)
}

// Untrust removes the root's trust setting from the System keychain.
func (s *Store) Untrust(interactive bool) error {
	return hostsudo.Run(interactive, nil, pki.MacOSTrustUninstallArgs(s.RootCertPath())...)
}
