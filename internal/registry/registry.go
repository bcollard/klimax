// Package registry manages the local Docker registry and pull-through mirror
// containers running inside the klimax Lima VM.
package registry

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/bcollard/klimax/internal/config"
	"github.com/bcollard/klimax/internal/guest"
)

const (
	registryImage   = "registry"
	registryTag     = "2"
	localRegName    = "kind-registry"
	kindNetworkName = "kind"
)

// EnsureRegistries idempotently starts the local registry and all configured
// pull-through mirrors in the guest VM, connecting them to the kind network.
func EnsureRegistries(ctx context.Context, g *guest.Client, cfg config.RegistryConfig) error {
	if cfg.LocalRegistry.Enabled {
		if err := ensureLocalRegistry(ctx, g, cfg.LocalRegistry.Port); err != nil {
			return fmt.Errorf("local registry: %w", err)
		}
	}
	for _, m := range cfg.Mirrors {
		if err := ensureMirror(ctx, g, m, cfg.CacheStorage); err != nil {
			return fmt.Errorf("mirror %q: %w", m.Name, err)
		}
	}
	return nil
}

// ensureLocalRegistry starts the push-capable local registry (kind-registry:5000).
func ensureLocalRegistry(ctx context.Context, g *guest.Client, port int) error {
	slog.Info("Ensuring local registry", "name", localRegName, "port", port)
	running, err := isContainerRunning(ctx, g, localRegName)
	if err != nil {
		return err
	}
	if running {
		slog.Info("Local registry already running", "name", localRegName)
		return connectToKindNetwork(ctx, g, localRegName)
	}
	// Remove a stopped container if present.
	if _, err := g.Run(ctx, fmt.Sprintf("docker rm -f %s 2>/dev/null || true", localRegName)); err != nil {
		return err
	}
	cmd := fmt.Sprintf(
		"docker run -d --restart=always -p 0.0.0.0:%d:%d --name %s %s:%s",
		port, port, localRegName, registryImage, registryTag,
	)
	if _, err := g.Run(ctx, cmd); err != nil {
		return fmt.Errorf("starting local registry: %w", err)
	}
	return connectToKindNetwork(ctx, g, localRegName)
}

// ensureMirror starts a pull-through cache container for the given mirror config.
func ensureMirror(ctx context.Context, g *guest.Client, m config.RegistryMirror, cacheStorage string) error {
	slog.Info("Ensuring registry mirror", "name", m.Name, "port", m.Port, "remote", m.RemoteURL)
	running, err := isContainerRunning(ctx, g, m.Name)
	if err != nil {
		return err
	}
	if running {
		slog.Info("Registry mirror already running", "name", m.Name)
		return connectToKindNetwork(ctx, g, m.Name)
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

	cmd := fmt.Sprintf(
		"docker run -d --restart=always"+
			" -v %s:/etc/docker/registry/config.yml"+
			" -v %s:/var/lib/registry"+
			" -p %d:%d"+
			" --name %s"+
			" %s:%s",
		configPath, cacheDir, m.Port, m.Port, m.Name, registryImage, registryTag,
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

// ContainerdPatches returns the containerd TOML patches for all configured registries.
// These are embedded in the kind cluster config so every kind node uses the mirrors.
func ContainerdPatches(cfg config.RegistryConfig) string {
	var sb strings.Builder

	// Local registry
	if cfg.LocalRegistry.Enabled {
		host := fmt.Sprintf("%s:%d", localRegName, cfg.LocalRegistry.Port)
		sb.WriteString(containerdMirrorEntry(host, host, cfg.LocalRegistry.Port, false, "", ""))
		// Also mirror "localhost:<port>" alias used by some tools
		sb.WriteString(containerdMirrorEntry(
			fmt.Sprintf("localhost:%d", cfg.LocalRegistry.Port),
			host, cfg.LocalRegistry.Port, false, "", ""))
	}

	// Pull-through mirrors
	for _, m := range cfg.Mirrors {
		remoteHost := remoteHostname(m.RemoteURL)
		endpoint := fmt.Sprintf("%s:%d", m.Name, m.Port)
		sb.WriteString(containerdMirrorEntry(remoteHost, endpoint, m.Port, false, m.Username, m.Password))
		// docker.io needs extra alias for "index.docker.io"
		if strings.Contains(m.RemoteURL, "docker.io") {
			sb.WriteString(containerdMirrorEntry("docker.io", endpoint, m.Port, false, m.Username, m.Password))
		}
	}

	return sb.String()
}

// containerdMirrorEntry builds a single containerd TOML mirror stanza.
func containerdMirrorEntry(mirrorHost, endpoint string, port int, tls bool, user, pass string) string {
	_ = port // port is embedded in endpoint string
	var sb strings.Builder
	scheme := "http"
	if tls {
		scheme = "https"
	}
	sb.WriteString(fmt.Sprintf(
		"  [plugins.\"io.containerd.grpc.v1.cri\".registry.mirrors.\"%s\"]\n    endpoint = [\"%s://%s\"]\n",
		mirrorHost, scheme, endpoint,
	))
	sb.WriteString(fmt.Sprintf(
		"  [plugins.\"io.containerd.grpc.v1.cri\".registry.configs.\"%s\".tls]\n    insecure_skip_verify = true\n",
		mirrorHost,
	))
	if user != "" {
		sb.WriteString(fmt.Sprintf(
			"  [plugins.\"io.containerd.grpc.v1.cri\".registry.configs.\"%s\".auth]\n    username = %q\n    password = %q\n",
			mirrorHost, user, pass,
		))
	}
	return sb.String()
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
