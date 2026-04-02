# klimax

**klimax** manages a macOS Virtualization.framework (VZ) Lima VM, installs Docker inside it, creates and manages multiple [kind](https://kind.sigs.k8s.io/) clusters, and wires up pure L3 routing from your Mac into the kind bridge subnet — no SNAT, no VPN, direct IP access to pods and LoadBalancer services.

---

## What it does

| Concern | What klimax does |
|---|---|
| VM | Creates/starts/stops/deletes a Lima VZ instance |
| Docker | Installs Docker in the VM; forwards the socket to `~/.<vmname>.docker.sock` |
| kind | Creates/deletes multiple kind clusters; each gets its own subnet slice and API port |
| Registries | Runs a local push registry (`kind-registry:5000`) + pull-through mirrors for docker.io, quay.io, gcr.io; mirror data cached persistently |
| Networking | Routes `kindBridgeCIDR` from macOS → VM via `lima0`; no SNAT so source IPs are preserved |
| MetalLB | Installed in every cluster with a dedicated IP pool slice |
| CoreDNS | Adds custom domain forwarding (e.g. `runlocal.dev`) at cluster creation |
| kubeconfig | Exports per-cluster kubeconfig to `~/.kube/klimax/<name>.kubeconfig`; auto-merges into `~/.kube/config` |

---

## Prerequisites

### On your Mac (host)

- **macOS 13 Ventura or later** — Apple Virtualization.framework is required (`vmType: vz`)
- **`sudo` access** — needed only for `klimax up` (adds macOS route) and `klimax destroy`; `klimax down` does not require sudo
- **Go 1.22+** — only if building from source; not needed for the pre-built binary

> klimax is self-contained. On first `klimax up` it automatically downloads and caches the Lima guest agent binary. No separate Lima installation required.

### Inside the VM (auto-provisioned by `klimax up`)

| Tool | Version | Purpose |
|---|---|---|
| Docker | latest via get.docker.com | Container runtime for kind and registries |
| kind | v0.27.0 | Kubernetes-in-Docker cluster manager |
| kubectl | latest stable | Cluster management from within the VM |
| jq, iptables, curl, net-tools, python3 | distro packages | Tooling for scripts and routing rules |

---

## Installation

### Homebrew (recommended)

```sh
brew tap bcollard/klimax
brew install --cask klimax
```

### Build from source

CGO is required because Lima's VM management packages link against macOS frameworks:

```sh
git clone https://github.com/bcollard/klimax
cd klimax
CGO_ENABLED=1 go build -o klimax ./cmd/klimax
sudo mv klimax /usr/local/bin/
```

### Shell completion

```sh
# zsh (add to ~/.zshrc for persistence)
source <(klimax completion zsh)

# bash
source <(klimax completion bash)

# fish
klimax completion fish > ~/.config/fish/completions/klimax.fish
```

---

## Quick start

```sh
# 1. Edit config (or accept defaults)
klimax config edit

# 2. Bring up the VM + Docker + networking + registries
klimax up

# 3. Point your shell at the VM's Docker daemon
eval $(klimax docker-env)        # current shell only
# or: klimax docker-context      # persistent Docker context

# 4. Create a kind cluster
klimax cluster create dev

# 5. Use the cluster (kubeconfig auto-merged into ~/.kube/config)
kubectl get nodes

# 6. Create a second cluster
klimax cluster create staging

# 7. List all clusters
klimax cluster list
```

After `klimax up`, the kind bridge CIDR is routed from your Mac directly to the VM. You can reach any pod IP, Service ClusterIP, or MetalLB LoadBalancer IP without port-forwarding.

---

## Configuration reference

The default config path is `~/.klimax/config.yaml`. Use `klimax config edit` to open it in your `$EDITOR`, or copy `config.example.yaml` to get started.

```yaml
# ── VM ──────────────────────────────────────────────────────────────────────
vm:
  name: "klimax"         # Lima instance name; Docker socket at ~/.<name>.docker.sock
  cpus: 4
  memory: "10GiB"
  disk: "40GiB"
  # rosetta: false       # enable Rosetta 2 for amd64 containers (ARM64 only)

# ── Networking ───────────────────────────────────────────────────────────────
network:
  kindBridgeCIDR: "172.30.0.0/16"   # routed from macOS → VM; no SNAT

  # Set to true when running alongside other Lima VMs (kind-on-lima, Rancher Desktop)
  # that also manage kind clusters. Lima mirrors every guest TCP port to 127.0.0.1 by
  # default; when two VMs both try to mirror port 7001 the connections conflict.
  # With disablePortMirroring: true, kubeconfigs use the VM's direct lima0 IP instead
  # of 127.0.0.1. ⚠ VM-level: only takes effect on new VMs (klimax destroy && up).
  # disablePortMirroring: false

# ── Kind defaults (applied to every `klimax cluster create`) ─────────────────
kind:
  nodeVersion: "v1.32.0"
  metalLBVersion: "v0.14.9"
  coreDNSDomains:
    - "runlocal.dev"        # custom zones forwarded to 8.8.8.8/8.8.4.4
  autoMergeKubeconfig: true   # merge context into ~/.kube/config after create
  autoRemoveKubeconfig: true  # remove context from ~/.kube/config after delete

# ── Registries ───────────────────────────────────────────────────────────────
registries:
  # "host" (default): cache at ~/.klimax/registry-cache/ — survives klimax destroy
  # "guest": cache inside the VM — wiped on klimax destroy
  cacheStorage: "host"

  localRegistry:
    enabled: true
    port: 5000            # push with: docker push kind-registry:5000/myimage:tag

  mirrors:
    - name: "registry-dockerio"
      port: 5030
      remoteURL: "https://registry-1.docker.io"
      # username/password: optional, avoids Docker Hub rate limits

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
klimax [--config ~/.klimax/config.yaml] [--debug] <command>
```

### VM lifecycle

| Command | Description |
|---|---|
| `klimax up` | Create/start the VM, provision Docker, set up networking and registries (idempotent) |
| `klimax down` | Stop the VM — preserves all clusters and registry cache data |
| `klimax down --remove-route` | Stop the VM and remove the macOS host route (requires sudo) |
| `klimax destroy` | Stop + delete VM, delete all clusters, remove host route |
| `klimax status` | Show VM state, clusters, route, and iptables rule presence |
| `klimax doctor` | Diagnose common issues |
| `klimax version` | Print the klimax version |
| `klimax shell` | Open an interactive SSH session in the VM |
| `klimax config edit` | Open the config file in `$VISUAL` / `$EDITOR` |

### Docker

**Option A — environment variable (current shell only)**

```sh
eval $(klimax docker-env)          # export DOCKER_HOST=unix://...
eval $(klimax docker-env --unset)  # unset DOCKER_HOST
```

**Option B — Docker context (persistent across shells)**

```sh
klimax docker-context              # create/update "klimax" context
klimax docker-context --unset      # docker context use default
```

> If `DOCKER_HOST` is set it overrides the active Docker context — use one or the other. `klimax docker-context` warns when both are active.

### Clusters

```sh
# Create
klimax cluster create <name>
klimax cluster create <name> --num 3             # pin to slot 3
klimax cluster create <name> --region us-east1 --zone us-east1-a

# Delete — interactive multi-select picker when no name given
klimax cluster delete <name>
klimax cluster delete

# List
klimax cluster list
klimax cluster list -o json
klimax cluster list -o yaml

# Kubeconfig
eval $(klimax cluster use <name>)    # print + eval KUBECONFIG export
klimax cluster merge <name>          # merge into ~/.kube/config manually

# E2E smoke test (uses current kubectl context)
klimax cluster e2e-test-nginx
klimax cluster e2e-test-nginx --cleanup   # remove nginx pod/svc only
```

The interactive delete picker supports multi-select:

```
Delete kind clusters (↑/↓ navigate · Space toggle · a=all · Enter confirm · q quit)

  [ ] dev       port 7001
  [x] staging   port 7002
  [x] prod      port 7003

  2 cluster(s) selected — press Enter to delete
```

### Per-cluster resources (auto-assigned from `--num N`)

| Resource | Value |
|---|---|
| API server host port | `700N` (e.g. `7001` for num 1) |
| Service subnet | `10.N.0.0/16` |
| Pod subnet | `10.1N.0.0/16` |
| MetalLB pool | `<kindPrefix>.N.1–7` and `<kindPrefix>.N.16–254` |
| kubeconfig | `~/.kube/klimax/<name>.kubeconfig` |
| topology labels | `topology.kubernetes.io/region=europe-westN`, `zone=europe-westN-b` |

### Registries

```sh
klimax registry clean-cache   # remove all mirror cache dirs + containers; run 'klimax up' to restart
```

Mirror cache data is stored at `~/.klimax/registry-cache/<mirror-name>/` by default (`cacheStorage: "host"`), virtiofs-mounted into the VM and bind-mounted into each registry container. Blobs survive `klimax down`/`up` cycles and even `klimax destroy`.

---

## Networking deep-dive

```
macOS host
  bridge1xx (<host-IP>, macOS-assigned)
      │  vzNAT — Apple VZNATNetworkDeviceAttachment
      ▼
Lima VZ guest
  lima0 (<guest-IP>, macOS-assigned)
  br-<id> (172.30.0.1/16)  ← Docker bridge "kind"
      │
  kind cluster nodes (172.30.N.x)
```

> vzNAT IPs are assigned by macOS and cannot be configured. klimax detects the VM's `lima0` IP at runtime — nothing is hardcoded. Multiple Lima VMs (klimax, limactl, colima…) each get a distinct IP on their own `bridge1xx`, so they coexist without conflict.

### How pure L3 routing works

1. `klimax up` adds a macOS route: `172.30.0.0/16 → <lima0-IP>` (via `sudo /sbin/route`).
2. Inside the VM, `ip_forward=1` and a systemd oneshot apply iptables rules idempotently:
   - **nat exemption** (before Docker's MASQUERADE): kind→host traffic exits `lima0` without SNAT.
   - **DOCKER-USER forward rules**: host→kind and established returns are explicitly allowed.
3. A `docker.service.d` drop-in reruns the rules after every Docker restart.

The result: `curl http://172.30.1.200/` on your Mac reaches the MetalLB VIP directly.

### kubeconfig and API server access

klimax supports two modes for kubeconfig API server addresses:

**Default (loopback mode — `network.disablePortMirroring: false`)**

Cluster API servers listen on `0.0.0.0:700N` inside the VM. Lima's hostagent automatically forwards these ports to `127.0.0.1:700N` on the host. Exported kubeconfigs point at `https://127.0.0.1:700N` — no VPN or direct vzNAT IP access required. This mode also avoids conflicts with host-based security software (e.g. endpoint agents that block direct VM IP access).

**Direct IP mode (`network.disablePortMirroring: true`)**

Use this when running klimax alongside other Lima-based VMs (kind-on-lima, Rancher Desktop) that also manage kind clusters. By default, Lima mirrors every guest TCP port to `127.0.0.1` — when two VMs both try to mirror port `7001`, they conflict.

Setting `disablePortMirroring: true` disables all Lima TCP port mirroring for the klimax VM. Kubeconfigs then use the VM's direct `lima0` IP (e.g. `192.168.64.3:700N`), which is L2-reachable from the host via vzNAT. The API server cert automatically includes the lima0 IP as a SAN.

> Note: the lima0 IP is assigned dynamically by macOS and may change on VM restart. Run `klimax cluster merge <name>` after a restart to refresh the address in `~/.kube/config`.

This is a VM-level setting — it only takes effect on new VMs (`klimax destroy && klimax up`).

### Registry mirrors

Every kind cluster is configured via containerd patches to:
- Push/pull from `kind-registry:5000` — the local push registry in the VM.
- Cache pulls from docker.io, quay.io, gcr.io transparently through pull-through mirrors, avoiding rate limits and accelerating cluster creation.

All registry containers are attached to the `kind` Docker network so cluster nodes resolve them by hostname.

---

## Running alongside Rancher Desktop, Colima, or kind-on-lima

klimax is designed to coexist with other Lima-based tools on the same Mac. Each Lima VM gets
its own vzNAT interface (`bridge1xx`) and a distinct macOS-assigned IP, so there is no
IP-level conflict between VMs.

The one friction point is **Lima's TCP port mirroring**: by default, Lima's hostagent
forwards every TCP port that a process in the VM listens on to `127.0.0.1` on the host. When
two VMs independently manage kind clusters, both hostagents try to mirror the same API-server
ports (e.g. `7001`) to `127.0.0.1` simultaneously, breaking connectivity for both.

**klimax bypasses this entirely** with a single config flag:

```yaml
# ~/.klimax/config.yaml
network:
  disablePortMirroring: true
```

With this flag:
- Lima stops forwarding any TCP port from the klimax VM to `127.0.0.1`.
- Cluster kubeconfigs use the VM's direct `lima0` IP (e.g. `https://192.168.64.3:7001`) — reachable from the host over vzNAT without any routing or VPN.
- The API server cert automatically includes the `lima0` IP as a SAN, so TLS verification works out of the box.
- Every other Lima VM (Rancher Desktop, Colima, kind-on-lima) keeps forwarding its own ports to `127.0.0.1` completely unaffected.

This is a VM-level setting — recreate the VM once to apply it (`klimax destroy && klimax up`),
then forget about it.

> The `lima0` IP is assigned dynamically by macOS and may change on VM restart.
> Run `klimax cluster merge <name>` after a restart to refresh kubeconfigs.

See [docs/klimax-vs-other-lima-based-tools.md](docs/klimax-vs-other-lima-based-tools.md) for
a detailed comparison with Rancher Desktop, Colima, and kind-on-lima.

---

## Project layout

```
klimax/
├── cmd/klimax/              # main entrypoint
├── internal/
│   ├── cli/                 # Cobra commands
│   ├── config/              # YAML schema, defaults, validation
│   ├── vm/                  # Lima instance manager + guest agent download
│   ├── limatemplate/        # Builds limatype.LimaYAML (VZ, mounts, provision script)
│   ├── guest/               # SSH client for running commands/scripts in the VM
│   ├── docker/              # Docker network management in guest
│   ├── registry/            # Local registry + pull-through mirror lifecycle
│   ├── kind/                # kind cluster create/delete/list; MetalLB, CoreDNS, kubeconfig
│   └── routing/             # iptables no-NAT rules; macOS route management
├── config.example.yaml
├── .goreleaser.yaml
└── Makefile
```

---

## License

MIT
