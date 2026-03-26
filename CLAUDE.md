# Project: klimax

Go CLI that wraps **Lima** to manage a macOS Virtualization.framework VM running Docker, with support for multiple kind clusters, pull-through registry mirrors, and pure L3 host↔cluster routing.

---

## Workspace rules (CRITICAL)

1. **Referenced projects** — `./klimax.code-workspace` lists `../kind-on-lima`, `../lima`, `../colima` as reference folders. **Do NOT read these directories into your main context.** Spawn a sub-agent to search them when needed.
2. **Sub-agents** — Use the `Explore` agent to look up Lima/Colima internals. Extract only what you need and report back.

---

## Design principles

- **Lima Go packages** (`github.com/lima-vm/lima/v2`) — never shell out to `limactl`.
- **`vmType: vz` only** — macOS Virtualization.framework; no QEMU.
- **`up` is infrastructure-only** — it creates/starts the VM, provisions Docker, the kind network, registries, and routing rules. It does **not** create or manage kind clusters.
- **Cluster lifecycle is CLI-only** — `klimax cluster create/delete/list`. There is no cluster list in the config file.
- **All provisioning is idempotent** — safe to re-run `klimax up` at any time.
- **Pure L3 routing, no SNAT** — macOS routes `kindBridgeCIDR` to the VM's `lima0` IP; iptables exempts replies from MASQUERADE.

---

## Inspiration / references

| Project | Location | Notes |
|---|---|---|
| [colima](https://github.com/abiosoft/colima) | `../../public/abiosoft/colima` | Lima-based Docker desktop; inspired CLI design |
| [kind-on-lima](https://github.com/bcollard/kind-on-lima) | `../kind-on-lima` | Shell-script predecessor; source of networking and registry patterns |
| [lima](https://github.com/lima-vm/lima) | `../../public/lima-vm/lima` | Underlying VM library |

---

## Networking background

### Lima vzNAT topology
```
macOS host  192.168.105.1  bridge100
Lima VM     192.168.105.2  lima0
```
These are auto-assigned by Lima's vzNAT. The VM IP is reachable from the host.

### kind network
All kind clusters share a single Docker bridge network named `kind` with a user-specified subnet (default `172.30.0.0/16`). The bridge interface is `br-<id>` inside the VM.

### Pure L3 routing (Strategy 2 — no SNAT)
Docker installs: `POSTROUTING -s <bridge-cidr> ! -o <bridge-if> -j MASQUERADE`

To preserve source IPs in host→cluster replies, klimax inserts a nat exemption rule **before** Docker's MASQUERADE:
```
iptables -t nat -I POSTROUTING 1 -s <kindCIDR> -d <VM_NET> -o lima0 -j ACCEPT
```
Plus two `DOCKER-USER` forwarding rules (new+established from host, and direct lima0→br-*).

These rules are persisted via:
- `lima-no-nat-kind.service` — systemd oneshot, runs at boot (after Docker)
- `docker.service.d/99-no-nat-kind.conf` — `ExecStartPost` drop-in so rules survive Docker restarts

---

## Package layout

```
cmd/klimax/main.go                   entry point

internal/config/config.go            Config struct, LoadConfig, Validate, defaults
internal/limatemplate/template.go    builds limatype.LimaYAML (Ubuntu 25.04, portForwards, provision script)
internal/vm/vm.go                    Manager: EnsureRunning, Stop, Delete, Inspect
internal/guest/guest.go              SSH Client: Run, RunScript, WriteFile
internal/docker/network.go           EnsureKindNetwork (idempotent, CIDR comparison)
internal/registry/registry.go        EnsureRegistries, ContainerdPatches (local reg + mirrors)
internal/kind/kind.go                CreateCluster, DeleteCluster, ListClusters, DetectUsedNums, NextFreeNum
internal/routing/macos.go            EnsureRoute, DeleteRoute, RouteExists, Lima0IP
internal/routing/iptables.go         InstallNoNat, CheckNoNatRule

internal/cli/root.go                 cobra root command, persistent flags (--config, --debug)
internal/cli/up.go                   `klimax up` — infra only (VM + network + registries + routing)
internal/cli/down.go                 `klimax down`
internal/cli/destroy.go              `klimax destroy`
internal/cli/status.go               `klimax status`
internal/cli/doctor.go               `klimax doctor`
internal/cli/version.go              `klimax version`
internal/cli/cluster.go              `klimax cluster` subcommands
internal/cli/docker_env.go           `klimax docker-env`
```

---

## Configuration schema (`config.yaml`)

Cluster lifecycle is **not** in the config file. The config drives infrastructure only.

```yaml
vm:
  name: "klimax"         # Lima instance name; socket at ~/.<name>.docker.sock
  cpus: 4
  memory: "10GiB"
  disk: "40GiB"

network:
  kindBridgeCIDR: "172.30.0.0/16"   # Docker "kind" network subnet

kind:
  nodeVersion: "v1.32.0"             # kindest/node image tag (default)
  metalLBVersion: "v0.14.9"          # MetalLB manifest version (default)
  coreDNSDomains:                    # forwarded to 8.8.8.8/8.8.4.4 (default: [runlocal.dev])
    - "runlocal.dev"

registries:
  localRegistry:
    enabled: true
    port: 5000                       # kind-registry push registry
  mirrors:
    - name: "registry-dockerio"
      port: 5030
      remoteURL: "https://registry-1.docker.io"
      # username/password: optional DockerHub creds
    - name: "registry-quayio"
      port: 5010
      remoteURL: "https://quay.io"
    - name: "registry-gcrio"
      port: 5020
      remoteURL: "https://gcr.io"
```

See `config.example.yaml` for the full annotated reference.

---

## VM provision script (first boot, runs as root)

Installed by `limatemplate.Build()` as a Lima `provision.system` script:

1. Set inotify limits (`fs.inotify.max_user_watches=524288`, `max_user_instances=512`)
2. Enable `net.ipv4.ip_forward=1`
3. Install: `jq`, `iptables`, `curl`, `net-tools`, `python3`
4. Configure Docker socket permissions via `docker.socket.d/override.conf` (`SocketUser=${LIMA_CIDATA_USER}`)
5. Install Docker via `get.docker.com`
6. Install kind CLI `v0.27.0`
7. Install kubectl (latest stable)

### Docker socket forwarding

Lima `portForwards` forwards `/run/docker.sock` → `~/.<vmName>.docker.sock` on the host.

```bash
eval $(klimax docker-env)   # sets DOCKER_HOST
```

---

## Cluster creation flow (`klimax cluster create <name>`)

1. **Auto-assign num** — inspect live `<name>-control-plane` containers' port bindings (70N → num=N); find lowest free slot 1–99.
2. **Build kind cluster config** with:
   - API port `70<num>` on `0.0.0.0` (reachable from host via lima0)
   - `serviceSubnet: 10.<num>.0.0/16`, `podSubnet: 10.1<num>.0.0/16`
   - `kubeadmConfigPatches`: `topology.kubernetes.io/region` + `zone` labels
   - `containerdConfigPatches`: mirror all registries through local containers
3. **`kind create cluster`** with `--image kindest/node:<nodeVersion>`
4. **Install MetalLB** (`kubectl apply -f …/metallb-native.yaml`); wait for readiness
5. **Configure IPAddressPool**: `172.30.<num>.1–7` and `172.30.<num>.16–254`; L2Advertisement
6. **Apply `local-registry-hosting` ConfigMap** in `kube-public`
7. **Patch CoreDNS** ConfigMap to forward configured domains to `8.8.8.8`
8. **Export kubeconfig** → `~/.kube/kind/<name>.kubeconfig` with server patched from `127.0.0.1` to `lima0 IP`

---

## CLI reference

```
klimax up                              Start VM + infra (idempotent)
klimax down                            Stop VM
klimax destroy                         Delete all clusters, delete VM, remove route
klimax status                          Show VM state, clusters, route, iptables
klimax doctor                          Diagnose common issues
klimax version                         Print version

klimax docker-env                      Print: export DOCKER_HOST=unix://~/.<name>.docker.sock
klimax docker-env --unset              Print: unset DOCKER_HOST

klimax cluster create <name>           Create a kind cluster (num auto-assigned)
  --num N                              Override cluster num (1-99)
  --region europe-west1                Override topology region label
  --zone   europe-west1-b              Override topology zone label
klimax cluster delete [name]           Delete a cluster (interactive picker if no name)
klimax cluster list [-o text|json|yaml] List clusters with num, API port, kubeconfig path
klimax cluster use <name>              Print: export KUBECONFIG=~/.kube/kind/<name>.kubeconfig
```

Global flags (all commands): `-c config.yaml`, `--debug`

---

## Safety and idempotency

- `klimax up` is safe to run repeatedly — every step checks before acting.
- iptables rules are inserted only if not already present (`-C` check before `-I`).
- Registry containers are started only if not already running.
- `kind create cluster` only runs for clusters that don't exist.
- Route is deleted then re-added (no-op on the network level).
- All kubeconfigs are written atomically with `0600` permissions.

---

## Known caveats

- Docker rewrites iptables on restart → handled by `ExecStartPost` drop-in.
- macOS VPN software can conflict with the host route → `klimax doctor` warns.
- `podSubnet: 10.1<num>.0.0/16` overlaps with `serviceSubnet: 10.<num+10>.0.0/16` for num≥10. In practice keep num 1–9 per VM.
- The vzNAT subnet `192.168.105.0/24` is fixed by Lima; do not use it for kind or pods.

---

## Release

- `goreleaser` for cross-compilation and GitHub releases
- Homebrew tap: `bcollard/homebrew-klimax`
