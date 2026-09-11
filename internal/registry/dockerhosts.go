package registry

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/bcollard/klimax/internal/config"
)

// DockerCertsDir is where dockerd looks for per-registry hosts.toml files.
//
// This is NOT /etc/containerd/certs.d. dockerd has its own tree, hardcoded in
// moby as registry.CertsDir() (daemon/pkg/registry/config.go), and reads it via
// containerd's config.ConfigureHosts in daemon.RegistryHosts
// (daemon/hosts.go). Pointing the kind nodes' certs.d at dockerd, or vice
// versa, silently does nothing — the two daemons never read each other's tree.
const DockerCertsDir = "/etc/docker/certs.d"

// DockerdRegistryHosts returns the host→mirror mappings to install for dockerd,
// so `docker pull` and `docker compose` are served by the pull-through caches.
//
// This is RegistryHosts with two differences, both of which matter:
//
//  1. The endpoint is 127.0.0.1:<port>, not the container name. dockerd runs in
//     the VM's network namespace, where Docker's embedded DNS does not resolve
//     a container name at all. The kind nodes are containers on the kind
//     network, so for them the name is the only thing that works.
//  2. Docker Hub is keyed as "docker.io" only. dockerd looks up the directory
//     by reference.Domain(ref), which is "docker.io" — never
//     "registry-1.docker.io", which is merely the configured upstream.
//
// Requires dockerd to use the containerd image store: moby wires
// daemon.RegistryHosts in only under usesSnapshotter (daemon/daemon.go). With
// the classic graphdriver store, lookupV2Endpoints applies mirrors to Docker
// Hub alone, which is what daemon.json's registry-mirrors still covers.
func DockerdRegistryHosts(cfg config.RegistryConfig) []RegistryHost {
	var hosts []RegistryHost
	for _, m := range cfg.Mirrors {
		endpoint := fmt.Sprintf("http://127.0.0.1:%d", m.Port)
		host := remoteHostname(m.RemoteURL)
		if strings.Contains(m.RemoteURL, "docker.io") {
			host = "docker.io"
		}
		hosts = append(hosts, RegistryHost{Host: host, Endpoint: endpoint, Upstream: m.RemoteURL})
	}
	// Stable order so drift detection does not see a change that is not there.
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].Host < hosts[j].Host })
	return hosts
}

// DockerHostsTOML renders the hosts.toml body dockerd reads for one registry.
//
// `server` is the upstream, and the [host.…] entry is tried first: containerd's
// loader documents the mirror list as "tried first" with server as the
// fallback, so a mirror that is down degrades to a direct pull rather than
// failing it.
func (h RegistryHost) DockerHostsTOML() string {
	server := h.Upstream
	if server == "" {
		server = "https://" + h.Host
	}
	return fmt.Sprintf(
		"# Managed by klimax. Edits are overwritten by `klimax up`.\n"+
			"server = %q\n\n[host.%q]\n  capabilities = [\"pull\", \"resolve\"]\n",
		server, h.Endpoint)
}

// DockerHostsPath is the file backing one registry's dockerd mirror config.
func DockerHostsPath(host string) string {
	return path.Join(DockerCertsDir, host, "hosts.toml")
}
