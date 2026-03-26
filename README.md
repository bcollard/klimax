# klimax

**klimax** manages a macOS Virtualization.framework (VZ) Lima VM, installs Docker inside it, creates and manages multiple [kind](https://kind.sigs.k8s.io/) clusters, and wires up pure L3 routing from your Mac into the kind bridge subnet — no SNAT, no VPN, direct IP access to pods and LoadBalancer services.

---

## What it does

| Concern | What klimax does |
|---|---|
| VM | Creates/starts/stops/deletes a Lima VZ instance |
| Docker | Installs Docker in the VM; forwards the socket to `~/.<vmname>.docker.sock` |
| kind | Creates/deletes multiple kind clusters; each gets its own subnet slice and API port |
| Registries | Runs a local push registry (`kind-registry:5000`) + pull-through mirrors for docker.io, quay.io, gcr.io |
| Networking | Routes `kindBridgeCIDR` from macOS → VM via `lima0`; no SNAT so source IPs are preserved |
| MetalLB | Installed in every cluster with a dedicated IP pool slice |
| CoreDNS | Adds custom domain forwarding (e.g. `runlocal.dev`) at cluster creation |
| kubeconfig | Exports per-cluster kubeconfig to `~/.kube/kind/<name>.kubeconfig` |

---

## Prerequisites

### On your Mac (host)

- **macOS 13 Ventura or later** — Apple Virtualization.framework is required (`vmType: vz`)
- **`sudo` access** — klimax calls `sudo /sbin/route` to add/remove the kind CIDR route; `/sbin/route` is built into macOS, nothing to install
- **Go 1.22+** — only if building from source; not needed for the pre-built binary

> **Lima is used as a Go library** (`github.com/lima-vm/lima/v2`), not as a CLI. You do **not** need `limactl` installed.

### Inside the VM (auto-provisioned by `klimax up`)

klimax's provision script installs everything the VM needs on first boot — you don't install any of these yourself:

| Tool | Version | Purpose |
|---|---|---|
| Docker | latest via get.docker.com | Container runtime for kind and registries |
| kind | v0.27.0 | Kubernetes-in-Docker cluster manager |
| kubectl | latest stable | Cluster management from within the VM |
| jq, iptables, curl, net-tools, python3 | distro packages | Tooling for scripts and routing rules |

---

## Installation

### Homebrew (recommended)

klimax is distributed as a pre-built binary via a Homebrew Cask:

```sh
brew tap bcollard/klimax
brew install --cask klimax
```

### Build from source

CGO is required because Lima's VM management packages link against macOS frameworks at build time:

```sh
git clone https://github.com/bcollard/klimax
cd klimax
CGO_ENABLED=1 go build -o klimax ./cmd/klimax
sudo mv klimax /usr/local/bin/
```

---

## Quick start

```sh
# 1. Create a config file
cp config.example.yaml config.yaml
# Edit config.yaml to taste (or leave defaults)

# 2. Bring up the VM + Docker + networking
klimax up

# 3. Point your shell at the VM's Docker daemon
eval $(klimax docker-env)
docker ps   # runs inside the VM

# 4. Create a kind cluster
klimax cluster create dev

# 5. Use the cluster
eval $(klimax cluster use dev)
kubectl get nodes

# 6. Create a second cluster
klimax cluster create staging

# 7. List all clusters
klimax cluster list
```

After `klimax up`, the kind bridge CIDR is routed from your Mac directly to the VM. You can reach any pod IP, Service ClusterIP, or MetalLB LoadBalancer IP without port-forwarding.

---

## Configuration reference

Copy `config.example.yaml` to `config.yaml` (the default path). All fields have sensible defaults.

```yaml
# ── VM ──────────────────────────────────────────────────────────────────────
vm:
  name: "klimax"         # Lima instance name; Docker socket at ~/.<name>.docker.sock
  cpus: 4
  memory: "10GiB"
  disk: "40GiB"

# ── Networking ───────────────────────────────────────────────────────────────
network:
  # Subnet for the Docker bridge network named "kind".
  # macOS routes this CIDR to the VM's lima0 IP for pure L3 access (no SNAT).
  kindBridgeCIDR: "172.30.0.0/16"

# ── Kind defaults (applied to every `klimax cluster create`) ─────────────────
kind:
  nodeVersion: "v1.32.0"       # kindest/node image tag
  metalLBVersion: "v0.14.9"    # MetalLB manifest version
  coreDNSDomains:
    - "runlocal.dev"            # custom zones forwarded to 8.8.8.8/8.8.4.4

# ── Registries ───────────────────────────────────────────────────────────────
registries:
  localRegistry:
    enabled: true
    port: 5000

  mirrors:
    - name: "registry-dockerio"
      port: 5030
      remoteURL: "https://registry-1.docker.io"
      # username: "your-dockerhub-username"   # optional, avoids rate limits
      # password: "your-dockerhub-password"

    - name: "registry-quayio"
      port: 5010
      remoteURL: "https://quay.io"

    - name: "registry-gcrio"
      port: 5020
      remoteURL: "https://gcr.io"
```

> Cluster lifecycle is managed exclusively via `klimax cluster` subcommands — there is no cluster list in the config file.

---

## CLI reference

```
klimax [--config config.yaml] [--debug] <command>
```

Global flags apply to every command: `-c / --config` (default: `config.yaml`) and `--debug`.

### Help

```sh
klimax help                  # list all commands
klimax help cluster          # describe the cluster subcommand group
klimax help cluster create   # describe a specific subcommand
klimax <command> --help      # same, inline
```

### VM lifecycle

| Command | Description |
|---|---|
| `klimax up` | Create/start the VM, provision Docker, set up networking and registries |
| `klimax down` | Stop the VM (preserves clusters and data) |
| `klimax destroy` | Delete clusters, delete VM, remove routes |
| `klimax status` | Show VM and cluster status |
| `klimax doctor` | Diagnose common issues (routes, Docker socket, etc.) |
| `klimax version` | Print the klimax version |

### Docker

```sh
eval $(klimax docker-env)          # activate: export DOCKER_HOST=unix://...
eval $(klimax docker-env --unset)  # deactivate
```

### Clusters

```sh
# Create a cluster (num auto-assigned; drives API port and subnet slice)
klimax cluster create <name>
klimax cluster create <name> --num 3          # pin to num 3
klimax cluster create <name> --region us-east1 --zone us-east1-a

# Delete a cluster (interactive picker when no name given)
klimax cluster delete <name>
klimax cluster delete

# List clusters
klimax cluster list                 # text table (default)
klimax cluster list -o json
klimax cluster list -o yaml

# Print export command to set KUBECONFIG
klimax cluster use <name>
eval $(klimax cluster use dev)
```

### Per-cluster resources (auto-assigned from `--num N`)

| Resource | Value |
|---|---|
| API server host port | `700N` (e.g. `7001` for num 1) |
| Service subnet | `10.N.0.0/16` |
| Pod subnet | `10.1N.0.0/16` |
| MetalLB pool | `<kindPrefix>.N.1–7`, `<kindPrefix>.N.16–254` |
| kubeconfig | `~/.kube/kind/<name>.kubeconfig` |
| topology labels | `topology.kubernetes.io/region=europe-westN`, `zone=europe-westN-b` |

---

## Networking deep-dive

```
macOS host
  bridge100 (192.168.105.1)
      │  vzNAT link
      ▼
Lima VZ guest
  lima0 (192.168.105.2)
  br-<id> (172.30.0.1/16)  ← Docker bridge "kind"
      │
  kind cluster nodes (172.30.N.x)
```

### How pure L3 routing works

1. `klimax up` adds a macOS route: `172.30.0.0/16 → 192.168.105.2` (via `route -n add`).
2. Inside the VM, `ip_forward=1` and a systemd oneshot apply iptables rules idempotently:
   - **nat exemption** (inserted before Docker's MASQUERADE): traffic from the kind CIDR destined for the host exits `lima0` without SNAT — so reply packets carry the pod/LB IP as source, not the VM IP.
   - **DOCKER-USER forward rules**: host→kind and established returns are explicitly allowed.
3. A `docker.service.d` drop-in reruns the script after every Docker restart (Docker rewrites iptables on start).

The result: `curl http://172.30.1.200/` on your Mac reaches the MetalLB VIP directly, and the TCP reply comes back from `172.30.1.200`, not from `192.168.105.2`.

### Local registry and mirrors

Every kind cluster is configured (via containerd patches) to:
- Push/pull to `kind-registry:5000` — the local registry container in the VM.
- Transparently cache pulls from docker.io, quay.io, and gcr.io through pull-through mirror containers, avoiding rate limits and speeding up cluster creation.

All registry containers are attached to the `kind` Docker network so cluster nodes resolve them by hostname.

---

## Project layout

```
klimax/
├── cmd/klimax/              # main entrypoint
├── internal/
│   ├── cli/                 # Cobra commands (up, down, destroy, status, doctor, cluster, docker-env)
│   ├── config/              # YAML schema, defaults, validation
│   ├── vm/                  # Lima instance manager (create/start/stop/delete/inspect)
│   ├── limatemplate/        # Builds limatype.LimaYAML (VZ, portForwards, provision scripts)
│   ├── guest/               # SSH client for running commands/scripts in the VM
│   ├── docker/              # Docker network management in guest
│   ├── registry/            # Local registry + pull-through mirror lifecycle
│   ├── kind/                # kind cluster create/delete/list; MetalLB, CoreDNS, kubeconfig
│   └── routing/             # iptables no-NAT rules in guest; macOS route management
├── .github/
│   └── workflows/
│       ├── ci.yml           # build + test + vet + goreleaser check on push/PR
│       └── release.yml      # goreleaser release triggered on v*.*.* tags
├── .goreleaser.yaml         # GoReleaser config: darwin/amd64+arm64, CGO=1, Homebrew Cask tap
├── .gitignore
├── config.example.yaml
├── LICENSE
└── Makefile
```

---

## License

MIT
