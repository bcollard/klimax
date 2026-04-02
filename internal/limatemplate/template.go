package limatemplate

import (
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"

	"github.com/bcollard/klimax/internal/config"
	"github.com/lima-vm/lima/v2/pkg/limatype"
	"github.com/lima-vm/lima/v2/pkg/ptr"
)

// Ubuntu 25.04 (plucky) cloud images.
const (
	ubuntuAMD64 = "https://cloud-images.ubuntu.com/releases/25.04/release/ubuntu-25.04-server-cloudimg-amd64.img"
	ubuntuARM64 = "https://cloud-images.ubuntu.com/releases/25.04/release/ubuntu-25.04-server-cloudimg-arm64.img"
)

// kindCLIVersion is the kind binary version installed in the VM.
// Separate from the kind node image version (specified per-cluster in config).
const kindCLIVersion = "v0.27.0"

// provisionScript runs inside the VM as root (mode: system) on first boot.
// It installs Docker, sets socket permissions, enables IP forwarding, and
// installs kind + kubectl binaries.
var provisionScript = `#!/bin/bash
set -eux -o pipefail

# Increase inotify limits for kind
sysctl -w fs.inotify.max_user_watches=524288
sysctl -w fs.inotify.max_user_instances=512
echo 'fs.inotify.max_user_watches=524288' >> /etc/sysctl.d/99-klimax.conf
echo 'fs.inotify.max_user_instances=512'  >> /etc/sysctl.d/99-klimax.conf

# Enable IP forwarding for host<->kind routing
sysctl -w net.ipv4.ip_forward=1
echo 'net.ipv4.ip_forward=1' > /etc/sysctl.d/99-klimax-forward.conf

# Install tools
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -q jq iptables curl net-tools python3

# Docker socket permissions (allows lima user to use Docker without sudo)
# Lima's guest user is always named "lima" — this is a Lima invariant.
if [ ! -e /etc/systemd/system/docker.socket.d/override.conf ]; then
  mkdir -p /etc/systemd/system/docker.socket.d
  cat > /etc/systemd/system/docker.socket.d/override.conf <<EOF
[Socket]
SocketUser=lima
EOF
fi

# sshd: evict idle sessions after 30s (10 probes × 3s) so interrupted
# klimax commands don't leave zombie sshd-session processes that exhaust
# vsock connection slots and cause "handshake failed: EOF" on new dials.
if ! grep -q 'ClientAliveInterval' /etc/ssh/sshd_config; then
  cat >> /etc/ssh/sshd_config <<EOF

# Added by klimax provisioner
ClientAliveInterval 3
ClientAliveCountMax 10
EOF
  systemctl reload ssh || true
fi

# Install Docker if not present
if ! command -v docker >/dev/null 2>&1; then
  curl -fsSL https://get.docker.com | sh
fi

# Resolve architecture for binary downloads
ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')

# Install kind CLI if not present
KIND_VERSION="` + kindCLIVersion + `"
if ! command -v kind >/dev/null 2>&1; then
  curl -fsSLo /usr/local/bin/kind \
    "https://kind.sigs.k8s.io/dl/${KIND_VERSION}/kind-linux-${ARCH}"
  chmod +x /usr/local/bin/kind
fi

# Install kubectl if not present (latest stable)
if ! command -v kubectl >/dev/null 2>&1; then
  KUBECTL_VERSION=$(curl -fsSL https://dl.k8s.io/release/stable.txt)
  curl -fsSLo /usr/local/bin/kubectl \
    "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${ARCH}/kubectl"
  chmod +x /usr/local/bin/kubectl
fi
`

// probeScript checks that Docker and kind are installed and running.
var probeScript = `#!/bin/bash
set -eux -o pipefail
if ! timeout 30s bash -c "until command -v docker >/dev/null 2>&1; do sleep 3; done"; then
  echo >&2 "docker is not installed yet"
  exit 1
fi
if ! timeout 30s bash -c "until pgrep dockerd; do sleep 3; done"; then
  echo >&2 "dockerd is not running"
  exit 1
fi
if ! command -v kind >/dev/null 2>&1; then
  echo >&2 "kind is not installed yet"
  exit 1
fi
`

// buildPortForwards returns the Lima portForwards rules for the given config.
// The Docker socket forward is always included. When DisablePortMirroring is
// true, a catch-all TCP ignore rule is prepended so Lima's hostagent does not
// auto-mirror any guest TCP port to 127.0.0.1 on the host — required when
// running alongside other Lima VMs that manage kind clusters with the same
// port numbers.
//
// Rule ordering is critical: Lima scans PortForwards from the top to detect
// the global ignoreTCP flag and stops at the first non-ignore rule.
// GuestIP must be net.IPv4zero (not nil) so Lima's ignore check also matches
// ports bound to all interfaces (0.0.0.0) inside the VM.
func buildPortForwards(cfg *config.Config) []limatype.PortForward {
	var fwds []limatype.PortForward
	if cfg.Network.DisablePortMirroring {
		fwds = append(fwds, limatype.PortForward{
			GuestIP:        net.IPv4zero,
			GuestPortRange: [2]int{1, 65535},
			Proto:          limatype.ProtoTCP,
			Ignore:         true,
		})
	}
	fwds = append(fwds, limatype.PortForward{
		GuestSocket: "/run/docker.sock",
		HostSocket:  "{{.Home}}/." + cfg.VM.Name + ".docker.sock",
	})
	return fwds
}

// Build constructs a limatype.LimaYAML from a klimax config.
// The result can be marshaled to YAML and passed to instance.Create().
func Build(cfg *config.Config) *limatype.LimaYAML {
	vzNAT := true
	systemFalse := false

	y := &limatype.LimaYAML{
		VMType: ptr.Of(limatype.VZ),
		CPUs:   ptr.Of(cfg.VM.CPUs),
		Memory: ptr.Of(cfg.VM.Memory),
		Disk:   ptr.Of(cfg.VM.Disk),

		Images: []limatype.Image{
			{File: limatype.File{Location: ubuntuAMD64, Arch: limatype.X8664}},
			{File: limatype.File{Location: ubuntuARM64, Arch: limatype.AARCH64}},
		},

		// VZ uses virtiofs for best performance
		MountType: ptr.Of(limatype.VIRTIOFS),

		// vzNAT: host reaches VM on 192.168.105.2 via lima0 / bridge100
		Networks: []limatype.Network{
			{VZNAT: &vzNAT},
		},

		// Forward the Docker socket to the host so macOS tools (docker CLI, kind) can use it.
		// Socket lands at ~/.<vmName>.docker.sock; set DOCKER_HOST=unix://$HOME/.<name>.docker.sock.
		PortForwards: buildPortForwards(cfg),

		// Disable containerd; we use Docker
		Containerd: limatype.Containerd{
			System: &systemFalse,
			User:   &systemFalse,
		},

		Provision: []limatype.Provision{
			{
				Mode:   limatype.ProvisionModeSystem,
				Script: &provisionScript,
			},
		},

		Probes: []limatype.Probe{
			{
				Script: &probeScript,
				Hint:   "Check /var/log/cloud-init-output.log in the guest",
			},
		},

		HostResolver: limatype.HostResolver{
			Hosts: map[string]string{
				"host.docker.internal": "host.lima.internal",
			},
		},
	}

	// For host cache storage, mount ~/.klimax/registry-cache into the guest via virtiofs
	// so Docker registry containers can bind-mount it at /var/lib/registry.
	// Lima mounts at the same absolute path on both sides (virtiofs convention).
	if cfg.Registries.CacheStorage == "host" {
		home, _ := os.UserHomeDir()
		cacheDir := filepath.Join(home, ".klimax", "registry-cache")
		y.Mounts = []limatype.Mount{
			{Location: cacheDir, Writable: ptr.Of(true)},
		}
	}

	if cfg.VM.Rosetta {
		if runtime.GOARCH != "arm64" {
			slog.Warn("vm.rosetta is set but host is not ARM64 — skipping Rosetta")
		} else {
			rosettaTrue := true
			y.VMOpts = limatype.VMOpts{
				limatype.VZ: limatype.VZOpts{
					Rosetta: limatype.Rosetta{
						Enabled: &rosettaTrue,
						BinFmt:  &rosettaTrue,
					},
				},
			}
		}
	}

	return y
}
