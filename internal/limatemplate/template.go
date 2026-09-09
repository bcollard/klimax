package limatemplate

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"

	"github.com/bcollard/klimax/internal/config"
	"github.com/lima-vm/lima/v2/pkg/limatype"
	"github.com/lima-vm/lima/v2/pkg/ptr"
)

// Ubuntu 26.04 LTS (resolute) cloud images.
//
// LTS is deliberate: interim releases (24.10, 25.04, 25.10) are supported for
// only 9 months, and 25.04 had already gone EOL — a VM built from it stopped
// receiving security updates. The /releases/26.04/release/ URLs redirect to
// the current point release, so a re-created VM picks up the latest respin.
const (
	ubuntuAMD64 = "https://cloud-images.ubuntu.com/releases/26.04/release/ubuntu-26.04-server-cloudimg-amd64.img"
	ubuntuARM64 = "https://cloud-images.ubuntu.com/releases/26.04/release/ubuntu-26.04-server-cloudimg-arm64.img"
)

// buildProvision returns the provision entries, in execution order.
//
// When a persistent image disk is configured, its setup must land *before* the
// main script — that script installs Docker, and /var/lib/containerd has to
// already be the data disk so the image store is populated there rather than on
// the VM's root disk.
func buildProvision(cfg *config.Config) []limatype.Provision {
	var provisions []limatype.Provision
	if cfg.VM.ImageDisk != "" {
		provisions = append(provisions, imageDiskProvisions(config.ImageDiskName(cfg.VM.Name))...)
	}
	return append(provisions, limatype.Provision{
		Mode:   limatype.ProvisionModeSystem,
		Script: &provisionScript,
	})
}

// dataFile returns a `provision: data` entry writing content to path.
//
// Lima applies these in boot.sh (before the provision.system scripts) and
// re-applies them on every boot, creating parent directories as needed. Every
// ProvisionData field is dereferenced unconditionally by Lima's cidata builder,
// so all of them must be set.
func dataFile(path, content, permissions string) limatype.Provision {
	return limatype.Provision{
		Mode: limatype.ProvisionModeData,
		ProvisionData: limatype.ProvisionData{
			Content:     ptr.Of(content),
			Overwrite:   ptr.Of(true),
			Owner:       ptr.Of("root:root"),
			Path:        ptr.Of(path),
			Permissions: ptr.Of(permissions),
		},
	}
}

// imageDiskScript mounts the Lima data disk at /var/lib/containerd.
//
// It deliberately does NOT reuse Lima's own /mnt/lima-<name> mount. Lima mounts
// additionalDisks from /var/lib/cloud/scripts/per-boot/00-lima.boot.sh, which
// cloud-final.service runs — and on Ubuntu cloud-final.service is ordered
// After=multi-user.target. docker.service is wanted by multi-user.target, so
// Lima's disk mount always happens *after* Docker has started. Anything ordered
// before Docker that waits for that mount deadlocks outright:
//
//	cloud-final After multi-user.target
//	multi-user.target waits for docker.service
//	docker.service waits for this unit
//	this unit waits for cloud-final's mount
//
// (Measured: systemctl list-jobs showed cloud-final, multi-user, docker and
// containerd all "waiting" behind this unit's "running" job.)
//
// An /etc/fstab bind is no better — systemd's fstab generator has no ordering
// edge to Lima's plain mount(8), so on restart it binds the still-empty
// directory on the root filesystem and Lima mounts the real disk over the top.
// Docker then runs against an empty image store while the real one sits hidden
// underneath. (Measured: bind source was /dev/vda1[/mnt/...] not /dev/vdb1.)
//
// So: find the partition by its filesystem label and mount it directly, early,
// from a oneshot ordered Before= containerd/docker (which Require= it). Lima's
// later mount of the same device at /mnt/lima-<name> is harmless — two
// mountpoints for one filesystem. First-boot formatting is still Lima's job:
// its boot script runs before the provision scripts, so the label already
// exists by the time any of this is installed.
//
// If the disk never appears the unit fails and takes Docker with it, which is
// far better than silently starting on an empty store and re-pulling the world.
func imageDiskProvisions(diskName string) []limatype.Provision {
	mountScript := fmt.Sprintf(`#!/bin/bash
set -euo pipefail

DEV="/dev/disk/by-label/lima-%s"
TARGET="/var/lib/containerd"

# udev may not have published the by-label symlink yet this early in boot.
for _ in $(seq 1 60); do
  [ -b "${DEV}" ] && break
  sleep 1
done
if [ ! -b "${DEV}" ]; then
  echo "klimax: image disk ${DEV} not found; refusing to start Docker on an empty image store" >&2
  exit 1
fi
real=$(readlink -f "${DEV}")

mkdir -p "${TARGET}"

# Already mounted from the right device? Just make sure the filesystem fills
# the block device (a no-op unless 'klimax disk resize-image' grew it since
# last boot — resize2fs is safe to run online and safe to run when there's
# nothing to grow). Mounted from anything else (e.g. the root filesystem) —
# unmount before taking over.
if mountpoint -q "${TARGET}"; then
  have=$(findmnt -no SOURCE "${TARGET}")
  case "${have}" in
    "${real}"*)
      resize2fs "${real}"
      exit 0
      ;;
  esac
  umount "${TARGET}"
fi

mount "${DEV}" "${TARGET}"
resize2fs "${real}"
`, diskName)

	unit := `[Unit]
Description=Mount the klimax persistent image disk at /var/lib/containerd
Before=containerd.service docker.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/sbin/klimax-image-disk.sh

[Install]
WantedBy=multi-user.target
`

	// containerd/docker do not exist yet on first boot (the main provision script
	// installs Docker); systemd picks these drop-ins up when the units appear.
	dropIn := `[Unit]
Requires=klimax-image-disk.service
After=klimax-image-disk.service
`

	// systemctl still needs a script: `data` writes files, not enablement
	// symlinks. Mounting here too means the Docker install in the next provision
	// script populates the data disk rather than the root disk.
	enableScript := `#!/bin/bash
set -eux -o pipefail
systemctl daemon-reload
systemctl enable klimax-image-disk.service
/usr/local/sbin/klimax-image-disk.sh
`

	return []limatype.Provision{
		dataFile("/usr/local/sbin/klimax-image-disk.sh", mountScript, "0755"),
		dataFile("/etc/systemd/system/klimax-image-disk.service", unit, "0644"),
		dataFile("/etc/systemd/system/containerd.service.d/10-klimax-image-disk.conf", dropIn, "0644"),
		dataFile("/etc/systemd/system/docker.service.d/10-klimax-image-disk.conf", dropIn, "0644"),
		{Mode: limatype.ProvisionModeSystem, Script: &enableScript},
	}
}

// KindCLIVersion is the kind binary version installed in the VM.
// Separate from the kind node image version (specified per-cluster in config).
// Its default/validated kindest/node image tag is config.DefaultKindNodeVersion —
// keep the two in sync when bumping kind.
const KindCLIVersion = "v0.32.0"

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
# The guest user is pinned to "lima" via user.name in the Lima YAML (see Build()).
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
if ! grep -q '^ClientAliveInterval' /etc/ssh/sshd_config; then
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
KIND_VERSION="` + KindCLIVersion + `"
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
	if cfg.Network.PortMirroringDisabled() {
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

		// Force the guest username to "lima". By default Lima derives the guest
		// user from the macOS host username (osutil.LimaUser), only falling back
		// to "lima" when the host name is an invalid Linux username. Pinning it
		// here makes the name deterministic so the SocketUser=lima drop-in below
		// (and any other "lima" assumptions) hold on every host.
		User: limatype.User{Name: ptr.Of("lima")},

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

		Provision: buildProvision(cfg),

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

	// Attach the persistent container image store disk. Lima partitions and
	// formats it on first attach, then mounts it at /mnt/lima-<name> on every
	// boot; imageDiskScript binds it over /var/lib/containerd.
	// The disk itself is created host-side by vm.EnsureImageDisk before start.
	if cfg.VM.ImageDisk != "" {
		y.AdditionalDisks = []limatype.Disk{
			{
				Name:   config.ImageDiskName(cfg.VM.Name),
				Format: ptr.Of(true),
				FSType: ptr.Of("ext4"),
			},
		}
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
