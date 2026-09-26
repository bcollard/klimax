package localca

import (
	"github.com/bcollard/homepki/pkg/pki"
	"github.com/bcollard/marina/internal/hostsudo"
)

// Trusted reports whether the system trusts the root. Unprivileged: on macOS
// the system pool is the platform verifier, so this reflects keychain trust.
func (s *Store) Trusted() bool {
	root, err := s.Root()
	return err == nil && pki.SystemTrusts(root) == nil
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
