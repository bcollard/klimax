// Package registry manages the pull-through mirror containers running inside
// the klimax Lima VM.
package registry

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bcollard/klimax/internal/config"
	"github.com/bcollard/klimax/internal/guest"
)

const (
	registryImage   = "registry"
	registryTag     = "2"
	kindNetworkName = "kind"
)

// EnsureRegistries idempotently starts all configured pull-through mirrors in
// the guest VM, connecting them to the kind network.
func EnsureRegistries(ctx context.Context, g *guest.Client, cfg config.RegistryConfig, proxyEnv map[string]string) error {
	for _, m := range cfg.Mirrors {
		if err := ensureMirror(ctx, g, m, cfg.CacheStorage, proxyEnv); err != nil {
			return fmt.Errorf("mirror %q: %w", m.Name, err)
		}
	}
	return nil
}

// ensureMirror starts a pull-through cache container for the given mirror config.
func ensureMirror(ctx context.Context, g *guest.Client, m config.RegistryMirror, cacheStorage string, proxyEnv map[string]string) error {
	slog.Info("Ensuring registry mirror", "name", m.Name, "port", m.Port, "remote", m.RemoteURL)
	running, err := isContainerRunning(ctx, g, m.Name)
	if err != nil {
		return err
	}
	if running {
		// A running container keeps the environment it was created with, so a
		// proxy added or changed in the config would never reach it. Recreate
		// only when it actually differs — the cache lives in a volume, so a
		// recreate costs nothing but the container.
		stale, err := mirrorProxyIsStale(ctx, g, m.Name, proxyEnv)
		if err != nil {
			return err
		}
		if !stale {
			slog.Info("Registry mirror already running", "name", m.Name)
			return connectToKindNetwork(ctx, g, m.Name)
		}
		slog.Info("Recreating registry mirror to apply changed proxy settings", "name", m.Name)
	}
	// Remove a stopped container if present.
	if _, err := g.Run(ctx, fmt.Sprintf("docker rm -f %s 2>/dev/null || true", m.Name)); err != nil {
		return err
	}

	configContent := buildMirrorConfig(m)
	configPath := fmt.Sprintf("/tmp/%s-config.yml", m.Name)

	// Write the registry config file to the guest.
	if err := g.WriteFile(ctx, configPath, configContent); err != nil {
		return fmt.Errorf("writing mirror config: %w", err)
	}

	// Ensure the cache directory exists in the guest (for host mode, virtiofs makes
	// the host path available at the same absolute path inside the VM).
	cacheDir := mirrorCacheDir(m.Name, cacheStorage)
	if _, err := g.Run(ctx, fmt.Sprintf("mkdir -p %q", cacheDir)); err != nil {
		return fmt.Errorf("creating cache dir %q: %w", cacheDir, err)
	}

	// A pull-through cache reaches its upstream registry itself, so it needs the
	// proxy as much as dockerd does — and no_proxy so it does not send traffic
	// for its sibling mirrors or the cluster network to a proxy that cannot
	// route it.
	cmd := fmt.Sprintf(
		"docker run -d --restart=always"+
			" -v %s:/etc/docker/registry/config.yml"+
			" -v %s:/var/lib/registry"+
			" -p %d:%d"+
			"%s"+
			" --name %s"+
			" %s:%s",
		configPath, cacheDir, m.Port, m.Port, dockerEnvArgs(proxyEnv), m.Name, registryImage, registryTag,
	)
	if _, err := g.Run(ctx, cmd); err != nil {
		return fmt.Errorf("starting mirror %q: %w", m.Name, err)
	}
	return connectToKindNetwork(ctx, g, m.Name)
}

// mirrorCacheDir returns the cache directory path for a mirror (guest-side path for both strategies).
// For "host": Lima mounts ~/.klimax/registry-cache at the same absolute path in the guest via virtiofs.
// For "guest": a VM-local path under /var/lib/klimax/registry-cache.
func mirrorCacheDir(mirrorName, cacheStorage string) string {
	if cacheStorage == "guest" {
		return "/var/lib/klimax/registry-cache/" + mirrorName
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".klimax", "registry-cache", mirrorName)
}

// buildMirrorConfig generates a distribution/registry v2 config YAML for a pull-through mirror.
func buildMirrorConfig(m config.RegistryMirror) string {
	var sb strings.Builder
	sb.WriteString("version: 0.1\n")
	sb.WriteString("proxy:\n")
	sb.WriteString(fmt.Sprintf("  remoteurl: %s\n", m.RemoteURL))
	if m.Username != "" {
		sb.WriteString(fmt.Sprintf("  username: %s\n", m.Username))
		sb.WriteString(fmt.Sprintf("  password: %s\n", m.Password))
	}
	sb.WriteString(`log:
  fields:
    service: registry
  accesslog:
    disabled: false
storage:
  cache:
    blobdescriptor: inmemory
  filesystem:
    rootdirectory: /var/lib/registry
`)
	sb.WriteString(fmt.Sprintf("http:\n  addr: :%d\n  headers:\n    X-Content-Type-Options: [nosniff]\n", m.Port))
	sb.WriteString(`health:
  storagedriver:
    enabled: true
    interval: 10s
    threshold: 3
`)
	return sb.String()
}

// RegistryHost maps an upstream registry hostname (as referenced in image names,
// e.g. "docker.io") to the local mirror endpoint URL that serves it.
type RegistryHost struct {
	// Host is the registry as it appears in image references, and the directory
	// name under /etc/containerd/certs.d/ (e.g. "docker.io", "quay.io").
	Host string
	// Endpoint is the mirror URL containerd pulls from (e.g.
	// "http://registry-dockerio:5030").
	Endpoint string
}

// RegistryHosts returns the containerd certs.d host→mirror mappings for all
// configured registries.
//
// Modern kind node images ship containerd 2.x, which defaults
// config_path = "/etc/containerd/certs.d" and refuses the legacy
// `registry.mirrors` config.toml block ("`mirrors` cannot be set when
// `config_path` is provided"). We therefore write one hosts.toml per registry
// under certs.d on each node (see WriteHostsTOML / kind.configureRegistryMirrors)
// instead of patching containerd's config.toml.
func RegistryHosts(cfg config.RegistryConfig) []RegistryHost {
	var hosts []RegistryHost

	// Pull-through mirrors.
	for _, m := range cfg.Mirrors {
		endpoint := fmt.Sprintf("http://%s:%d", m.Name, m.Port)
		hosts = append(hosts, RegistryHost{Host: remoteHostname(m.RemoteURL), Endpoint: endpoint})
		// Images reference Docker Hub as "docker.io"; the mirror's remoteURL is
		// "registry-1.docker.io", so add the "docker.io" alias too.
		if strings.Contains(m.RemoteURL, "docker.io") {
			hosts = append(hosts, RegistryHost{Host: "docker.io", Endpoint: endpoint})
		}
	}

	return hosts
}

// HostsTOML renders the containerd certs.d hosts.toml body for a single mirror.
func (h RegistryHost) HostsTOML() string {
	// skip_verify is a no-op for http endpoints but keeps the mirror usable if a
	// future endpoint uses https with a self-signed cert.
	return fmt.Sprintf("[host.%q]\n  capabilities = [\"pull\", \"resolve\"]\n  skip_verify = true\n", h.Endpoint)
}

// remoteHostname extracts the hostname from a remote URL (e.g. "https://quay.io" → "quay.io").
func remoteHostname(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	return u.Host
}

// isContainerRunning returns true if the named container exists and is running.
func isContainerRunning(ctx context.Context, g *guest.Client, name string) (bool, error) {
	out, err := g.Run(ctx, fmt.Sprintf(
		"docker inspect -f '{{.State.Running}}' %s 2>/dev/null || true", name,
	))
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "true", nil
}

// connectToKindNetwork connects a container to the kind Docker network (idempotent).
func connectToKindNetwork(ctx context.Context, g *guest.Client, containerName string) error {
	_, err := g.Run(ctx, fmt.Sprintf(
		"docker network connect %s %s 2>/dev/null || true", kindNetworkName, containerName,
	))
	return err
}

// dockerEnvArgs renders an env map as `docker run -e` arguments, sorted so the
// command is identical run to run.
func dockerEnvArgs(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, " -e %s", shellQuote(k+"="+env[k]))
	}
	return b.String()
}

// shellQuote wraps a value in single quotes for the guest shell. Proxy URLs and
// no_proxy lists contain characters (&, ?, *) that would otherwise be
// interpreted before docker ever sees them.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// mirrorProxyIsStale reports whether a running mirror's proxy environment
// differs from what the config now asks for.
//
// Only the proxy variables are compared: the container carries plenty of other
// environment (PATH and the registry image's own defaults) that has nothing to
// do with klimax and must not trigger a recreate.
func mirrorProxyIsStale(ctx context.Context, g *guest.Client, name string, want map[string]string) (bool, error) {
	out, err := g.Run(ctx, fmt.Sprintf(
		"docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' %s 2>/dev/null || true", name))
	if err != nil {
		return false, err
	}
	have := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && isProxyVar(k) {
			have[k] = v
		}
	}
	if len(have) != len(want) {
		return true, nil
	}
	for k, v := range want {
		if have[k] != v {
			return true, nil
		}
	}
	return false, nil
}

// isProxyVar reports whether an environment variable name is one klimax manages
// for proxying, in either spelling.
func isProxyVar(k string) bool {
	switch strings.ToLower(k) {
	case "http_proxy", "https_proxy", "no_proxy":
		return true
	}
	return false
}
