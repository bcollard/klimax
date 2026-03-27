package guest

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lima-vm/lima/v2/pkg/limatype"
	"github.com/lima-vm/lima/v2/pkg/limatype/dirnames"
	"golang.org/x/crypto/ssh"
)

// Client executes commands in the Lima guest over SSH.
type Client struct {
	address string // "host:port"
	config  *ssh.ClientConfig
}

// NewClient builds an SSH client for the given running Lima instance.
// It uses Lima's auto-generated key at $LIMA_HOME/_config/user.
func NewClient(inst *limatype.Instance) (*Client, error) {
	if inst.Status != limatype.StatusRunning {
		return nil, fmt.Errorf("instance %q is not running (status: %s)", inst.Name, inst.Status)
	}

	keyPath, err := limaPrivateKeyPath()
	if err != nil {
		return nil, err
	}
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("reading Lima SSH key %q: %w", keyPath, err)
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("parsing Lima SSH key: %w", err)
	}

	user := guestUser(inst)
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // Lima local VM, acceptable
		Timeout:         10 * time.Second,
	}

	addr := net.JoinHostPort(inst.SSHAddress, fmt.Sprintf("%d", inst.SSHLocalPort))
	return &Client{address: addr, config: cfg}, nil
}

// Run executes a single command in the guest and returns its combined stdout.
// stderr is logged at debug level.
func (c *Client) Run(ctx context.Context, cmd string) (string, error) {
	slog.Debug("guest run", "cmd", cmd)
	cl, err := c.dial()
	if err != nil {
		return "", err
	}
	defer cl.Close()

	sess, err := cl.NewSession()
	if err != nil {
		return "", fmt.Errorf("new SSH session: %w", err)
	}
	defer sess.Close()

	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr

	if err := sess.Run(cmd); err != nil {
		return "", fmt.Errorf("running %q: %w (stderr: %s)", cmd, err, stderr.String())
	}
	if s := stderr.String(); s != "" {
		slog.Debug("guest stderr", "cmd", cmd, "stderr", s)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// RunScript uploads and runs a multi-line shell script (via stdin) in the guest
// with root privileges. Lima always configures passwordless sudo for the guest
// user, so all klimax provisioning scripts can rely on this.
func (c *Client) RunScript(ctx context.Context, description, script string) error {
	slog.Info("guest script", "description", description)
	slog.Debug("guest script content", "script", script)

	cl, err := c.dial()
	if err != nil {
		return err
	}
	defer cl.Close()

	sess, err := cl.NewSession()
	if err != nil {
		return fmt.Errorf("new SSH session: %w", err)
	}
	defer sess.Close()

	var stderr bytes.Buffer
	sess.Stdin = strings.NewReader(script)
	sess.Stderr = &stderr

	if err := sess.Run("sudo bash -s"); err != nil {
		return fmt.Errorf("script %q failed: %w\nstderr: %s", description, err, stderr.String())
	}
	return nil
}

// RunScriptStream runs a multi-line shell script in the guest with root
// privileges, streaming stdout and stderr directly to the caller's terminal.
// Use this instead of RunScript when live output matters (e.g. e2e tests).
func (c *Client) RunScriptStream(ctx context.Context, script string) error {
	cl, err := c.dial()
	if err != nil {
		return err
	}
	defer cl.Close()

	sess, err := cl.NewSession()
	if err != nil {
		return fmt.Errorf("new SSH session: %w", err)
	}
	defer sess.Close()

	sess.Stdin = strings.NewReader(script)
	sess.Stdout = os.Stdout
	sess.Stderr = os.Stderr

	return sess.Run("sudo bash -s")
}

// WriteFile writes content to path in the guest using sudo.
// Removes any stale directory at the target path before writing.
func (c *Client) WriteFile(ctx context.Context, path, content string) error {
	slog.Debug("guest write file", "path", path)
	// Remove stale directory if present (may be root-owned), then write via sudo tee.
	script := fmt.Sprintf("sudo rm -rf %q && sudo tee %q <<'__KLIMAX_EOF__' > /dev/null\n%s\n__KLIMAX_EOF__\n", path, path, content)
	_, err := c.Run(ctx, script)
	return err
}

// SSHArgs returns the arguments needed to exec the system ssh binary for an
// interactive session into the given running Lima instance.
func SSHArgs(inst *limatype.Instance) ([]string, error) {
	keyPath, err := limaPrivateKeyPath()
	if err != nil {
		return nil, err
	}
	user := guestUser(inst)
	args := []string{
		"-i", keyPath,
		"-p", fmt.Sprintf("%d", inst.SSHLocalPort),
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		fmt.Sprintf("%s@%s", user, inst.SSHAddress),
	}
	return args, nil
}

// dial opens a new SSH connection. Callers are responsible for closing it.
func (c *Client) dial() (*ssh.Client, error) {
	cl, err := ssh.Dial("tcp", c.address, c.config)
	if err != nil {
		return nil, fmt.Errorf("SSH dial %s: %w", c.address, err)
	}
	return cl, nil
}

// limaPrivateKeyPath returns $LIMA_HOME/_config/user (Lima's shared SSH key).
func limaPrivateKeyPath() (string, error) {
	configDir, err := dirnames.LimaConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolving Lima config dir: %w", err)
	}
	return filepath.Join(configDir, "user"), nil
}

// guestUser returns the SSH username for the Lima guest.
// Lima defaults to the current macOS user, stored in the instance config.
func guestUser(inst *limatype.Instance) string {
	if inst.Config != nil && inst.Config.User.Name != nil && *inst.Config.User.Name != "" {
		return *inst.Config.User.Name
	}
	// fallback: Lima convention matches the host username
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "ubuntu"
}
