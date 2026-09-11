package registry

import (
	"strings"
	"testing"

	"github.com/bcollard/klimax/internal/config"
)

func dockerHostsTestConfig() config.RegistryConfig {
	return config.RegistryConfig{Mirrors: []config.RegistryMirror{
		{Name: "registry-dockerio", Port: 5030, RemoteURL: "https://registry-1.docker.io"},
		{Name: "registry-quayio", Port: 5010, RemoteURL: "https://quay.io"},
		{Name: "registry-us-docker-pkgdev", Port: 5040, RemoteURL: "https://us-docker.pkg.dev"},
	}}
}

func hostFor(t *testing.T, hosts []RegistryHost, name string) RegistryHost {
	t.Helper()
	for _, h := range hosts {
		if h.Host == name {
			return h
		}
	}
	t.Fatalf("no mapping for %q in %+v", name, hosts)
	return RegistryHost{}
}

// The whole point: every mirror is available to dockerd, not only Docker Hub.
// daemon.json's registry-mirrors could never express this.
func TestDockerdRegistryHostsCoversEveryMirror(t *testing.T) {
	hosts := DockerdRegistryHosts(dockerHostsTestConfig())
	if len(hosts) != 3 {
		t.Fatalf("want a mapping per mirror, got %d: %+v", len(hosts), hosts)
	}
	for _, want := range []string{"docker.io", "quay.io", "us-docker.pkg.dev"} {
		hostFor(t, hosts, want)
	}
}

// dockerd runs in the VM's network namespace, where the container name does not
// resolve. Getting this wrong fails silently: pulls keep working and nothing is
// ever cached.
func TestDockerdRegistryHostsUseLoopbackNotContainerName(t *testing.T) {
	for _, h := range DockerdRegistryHosts(dockerHostsTestConfig()) {
		if !strings.HasPrefix(h.Endpoint, "http://127.0.0.1:") {
			t.Errorf("%s endpoint %q must be loopback", h.Host, h.Endpoint)
		}
		if strings.Contains(h.Endpoint, "registry-") {
			t.Errorf("%s endpoint %q uses a container name", h.Host, h.Endpoint)
		}
	}
}

// dockerd looks the directory up by reference.Domain(ref), which is "docker.io".
// A directory named registry-1.docker.io would never be consulted.
func TestDockerdRegistryHostsKeysHubAsDockerIO(t *testing.T) {
	hosts := DockerdRegistryHosts(dockerHostsTestConfig())
	hub := hostFor(t, hosts, "docker.io")
	if hub.Endpoint != "http://127.0.0.1:5030" {
		t.Errorf("hub endpoint = %q", hub.Endpoint)
	}
	for _, h := range hosts {
		if h.Host == "registry-1.docker.io" {
			t.Error("hub keyed by its upstream hostname; dockerd will never look there")
		}
	}
}

// "us-docker.pkg.dev" contains "docker" and must not be mistaken for Hub.
func TestDockerdRegistryHostsDoesNotMisclassifyArtifactRegistry(t *testing.T) {
	ar := hostFor(t, DockerdRegistryHosts(dockerHostsTestConfig()), "us-docker.pkg.dev")
	if ar.Endpoint != "http://127.0.0.1:5040" {
		t.Errorf("endpoint = %q, want the Artifact Registry mirror", ar.Endpoint)
	}
}

// server= is the fallback containerd uses when the mirror cannot serve, so a
// mirror that is down degrades to a direct pull instead of failing it.
func TestDockerHostsTOMLKeepsUpstreamAsFallback(t *testing.T) {
	quay := hostFor(t, DockerdRegistryHosts(dockerHostsTestConfig()), "quay.io")
	body := quay.DockerHostsTOML()
	for _, want := range []string{
		`server = "https://quay.io"`,
		`[host."http://127.0.0.1:5010"]`,
		`capabilities = ["pull", "resolve"]`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("hosts.toml missing %q:\n%s", want, body)
		}
	}
}

// The reconciler only deletes files carrying this marker, so it can never take
// out a user's registry CA sitting in the same directory.
func TestDockerHostsTOMLCarriesOwnershipMarker(t *testing.T) {
	body := hostFor(t, DockerdRegistryHosts(dockerHostsTestConfig()), "quay.io").DockerHostsTOML()
	if !strings.Contains(strings.SplitN(body, "\n", 2)[0], "Managed by klimax") {
		t.Errorf("first line must mark ownership:\n%s", body)
	}
}

// Output feeds drift detection; reordering it would rewrite files every run.
func TestDockerdRegistryHostsStableOrder(t *testing.T) {
	cfg := dockerHostsTestConfig()
	rev := config.RegistryConfig{Mirrors: []config.RegistryMirror{
		cfg.Mirrors[2], cfg.Mirrors[0], cfg.Mirrors[1],
	}}
	a, b := DockerdRegistryHosts(cfg), DockerdRegistryHosts(rev)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("order-dependent: %+v vs %+v", a, b)
		}
	}
}

func TestDockerHostsPath(t *testing.T) {
	if got, want := DockerHostsPath("quay.io"), "/etc/docker/certs.d/quay.io/hosts.toml"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// The two trees are read by different daemons and must not be conflated.
func TestDockerCertsDirIsNotContainerdCertsDir(t *testing.T) {
	if strings.Contains(DockerCertsDir, "containerd") {
		t.Errorf("DockerCertsDir = %q; dockerd reads /etc/docker/certs.d", DockerCertsDir)
	}
}
