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
macOS host  <host-side IP>   bridge1xx   (macOS-assigned, e.g. 192.168.64.1)
Lima VM     <guest-side IP>  lima0       (macOS-assigned, e.g. 192.168.64.2)
```
vzNAT uses Apple's `VZNATNetworkDeviceAttachment`. IPs are assigned by macOS and **cannot be specified**. Lima does not store the guest IP in its `Instance` struct — it must be discovered at runtime from inside the VM.

klimax does this correctly: `routing.Lima0IP()` SSHs into the VM and reads `ip -o -4 addr show lima0`. No IP is hardcoded anywhere in klimax.

**Multiple Lima VMs (limactl, colima, etc.) are safe:** each vzNAT VM gets a distinct IP from macOS on its own `bridge1xx` interface. klimax's macOS route always targets the specific IP of the klimax VM, so other VMs don't interfere.

Note: the `192.168.105.x` subnet is Lima's **socket_vmnet** range (shared/bridged mode) — entirely different from vzNAT.

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
internal/guest/guest.go              SSH Client: Run, RunScript, RunScriptStream, WriteFile, SSHArgs
internal/docker/network.go           EnsureKindNetwork (idempotent, CIDR comparison)
internal/registry/registry.go        EnsureRegistries, ContainerdPatches (local reg + mirrors + cache volumes)
internal/kind/kind.go                CreateCluster, DeleteCluster, ListClusters, DetectUsedNums, NextFreeNum
internal/routing/macos.go            EnsureRoute, DeleteRoute, RouteExists, Lima0IP
internal/routing/iptables.go         InstallNoNat, CheckNoNatRule

internal/vm/guestagent.go            EnsureGuestAgent — downloads & caches lima-guestagent from GitHub releases

internal/cli/root.go                 cobra root command, persistent flags (--config, --debug)
internal/cli/up.go                   `klimax up` — infra only (VM + network + registries + routing)
internal/cli/down.go                 `klimax down` [--remove-route]
internal/cli/destroy.go              `klimax destroy`
internal/cli/status.go               `klimax status`
internal/cli/doctor.go               `klimax doctor`
internal/cli/version.go              `klimax version`
internal/cli/shell.go                `klimax shell` — interactive SSH session into the VM
internal/cli/config_cmd.go           `klimax config edit` — opens config in $VISUAL / $EDITOR
internal/cli/cluster.go              `klimax cluster` subcommands (create/delete/list/use/merge/e2e-test-nginx)
internal/cli/registry.go             `klimax registry clean-cache`
internal/cli/completion.go           `klimax completion bash|zsh|fish|powershell`
internal/cli/docker_env.go           `klimax docker-env` — prints DOCKER_HOST export (current shell only)
internal/cli/docker_context.go       `klimax docker-context` — creates/switches Docker context (persistent)
internal/cli/hostagent.go            `klimax hostagent` — hidden; Lima spawns this as a detached daemon
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
  rosetta: false         # Rosetta 2 for amd64 containers; ARM64 only

network:
  kindBridgeCIDR: "172.30.0.0/16"   # Docker "kind" network subnet
  disablePortMirroring: false        # true: disable Lima TCP port mirroring; kubeconfigs use VM's lima0 IP directly
                                     # Use when coexisting with other Lima VMs (kind-on-lima, Rancher Desktop)
                                     # that manage kind clusters — prevents API-server port conflicts on 127.0.0.1.
                                     # ⚠ VM-level: only takes effect on new VMs (klimax destroy && up).

kind:
  nodeVersion: "v1.32.0"             # kindest/node image tag (default)
  metalLBVersion: "v0.14.9"          # MetalLB manifest version (default)
  coreDNSDomains:                    # forwarded to 8.8.8.8/8.8.4.4 (default: [runlocal.dev])
    - "runlocal.dev"
  autoMergeKubeconfig: true          # merge context into ~/.kube/config after cluster create (default: true)
  autoRemoveKubeconfig: true         # remove context from ~/.kube/config after cluster delete (default: true)

registries:
  cacheStorage: "host"               # "host" (default): ~/.klimax/registry-cache/ via virtiofs, survives destroy
                                     # "guest": inside VM, wiped on destroy
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
4. Configure Docker socket permissions via `docker.socket.d/override.conf` (`SocketUser=lima` — Lima's guest user is always `lima`)
5. Install Docker via `get.docker.com`
6. Install kind CLI `v0.27.0`
7. Install kubectl (latest stable)

### Docker socket forwarding

Lima `portForwards` forwards `/run/docker.sock` → `~/.<vmName>.docker.sock` on the host.

Two ways to use it:
- `eval $(klimax docker-env)` — sets `DOCKER_HOST` in the current shell
- `klimax docker-context` — creates/updates a named Docker context (persistent across shells); conflicts with `DOCKER_HOST` if both are set

### Self-contained guest agent

Lima requires a small Linux binary (`lima-guestagent`) uploaded into the VM at startup for port forwarding. `internal/vm/guestagent.go` (`EnsureGuestAgent`) downloads it from Lima's GitHub release on first `klimax up` and caches it at `~/.klimax/share/lima/lima-guestagent.Linux-<arch>.gz`. No separate Lima installation required.

The downloaded version is matched to the Lima Go module version at runtime via `runtime/debug.ReadBuildInfo()`. The release asset uses `uname -m` naming: `Darwin-arm64` / `Darwin-x86_64` (not `Darwin-amd64`).

### lima-version file

Lima writes a `lima-version` file (mode `0o444`) into the instance dir during `instance.Create()` with version `"<unknown>"` when no ldflags are set. klimax fixes this in `vm.create()` by calling `os.Remove()` then `os.WriteFile()` with the actual Lima module version read via `runtime/debug.ReadBuildInfo()`. `os.WriteFile` alone silently fails on a read-only file — the Remove step is required.

### hostagent subprocess

Lima's `instance.StartWithPaths()` spawns `os.Executable() hostagent INSTANCE --socket ... --guestagent ...` as a detached daemon. `internal/cli/hostagent.go` implements this hidden subcommand (ports Lima's `cmd/limactl/hostagent.go`). It must configure **logrus JSON formatter** — Lima's event watcher parses JSON log lines to detect VM readiness. `runtime.LockOSThread()` is required when `--run-gui` is set (VZ on macOS).

### Binary replacement safety

Replacing `/usr/local/bin/klimax` while the hostagent is running causes macOS `amfid` to kill subsequent klimax execs. `make dev-install` aborts if a hostagent process is detected. `klimax doctor` also warns with the kill+cleanup fix command.

---

## Cluster creation flow (`klimax cluster create <name>`)

1. **Auto-assign num** — inspect live `<name>-control-plane` containers' port bindings (70N → num=N); find lowest free slot 1–99.
2. **Resolve API server address** — `127.0.0.1` by default; when `network.disablePortMirroring: true`, resolves the VM's live `lima0` IP via SSH (`routing.Lima0IP`) to embed in the cert SANs.
3. **Build kind cluster config** with:
   - API port `70<num>` on `0.0.0.0`
   - `serviceSubnet: 10.<num>.0.0/16`, `podSubnet: 10.1<num>.0.0/16`
   - `kubeadmConfigPatches`: `topology.kubernetes.io/region` + `zone` labels; when `disablePortMirroring`, also a `ClusterConfiguration` patch adding the lima0 IP to `apiServer.certSANs` (plus `127.0.0.1` for intra-VM kubectl calls)
   - `containerdConfigPatches`: mirror all registries through local containers
4. **`kind create cluster`** with `--image kindest/node:<nodeVersion>`
5. **Install MetalLB** (`kubectl apply -f …/metallb-native.yaml`); wait for readiness
6. **Configure IPAddressPool**: `172.30.<num>.1–7` and `172.30.<num>.16–254`; L2Advertisement
7. **Apply `local-registry-hosting` ConfigMap** in `kube-public`
8. **Patch CoreDNS** ConfigMap to forward configured domains to `8.8.8.8`
9. **Export kubeconfig** → `~/.kube/klimax/<name>.kubeconfig`; server set to `https://127.0.0.1:700N` (default) or `https://<lima0IP>:700N` (when `disablePortMirroring: true`)

### kubeconfig naming

`exportKubeconfig` strips the `kind-` prefix from context/cluster/user names — contexts are stored as bare `<name>` in `~/.kube/config`, not `kind-<name>`. `removeFromKubeconfig` must use the bare name accordingly.

---

## CLI reference

```
klimax up                              Start VM + infra (idempotent)
klimax down                            Stop VM (no sudo required)
klimax down --remove-route             Stop VM and remove macOS host route (requires sudo)
klimax destroy                         Delete all clusters, delete VM, remove route
klimax status                          Show VM state, clusters, route, iptables
klimax doctor                          Diagnose common issues
klimax version                         Print version
klimax shell                           Open interactive SSH session in the VM
klimax config edit                     Open config in $VISUAL / $EDITOR / nano / vi

klimax docker-env                      Print: export DOCKER_HOST=unix://~/.<name>.docker.sock
klimax docker-env --unset              Print: unset DOCKER_HOST

klimax docker-context                  Create/update "klimax" Docker context + docker context use <name>
klimax docker-context --unset          docker context use default

klimax cluster create <name>           Create a kind cluster (num auto-assigned)
  --num N                              Override cluster num (1-99)
  --region europe-west1                Override topology region label
  --zone   europe-west1-b              Override topology zone label
klimax cluster delete [name]           Delete a cluster; interactive multi-select picker if no name given
klimax cluster list [-o text|json|yaml] List clusters with num, API port, kubeconfig path
klimax cluster use <name>              Print: export KUBECONFIG=~/.kube/klimax/<name>.kubeconfig
klimax cluster merge <name>            Merge cluster context into ~/.kube/config
klimax cluster e2e-test-nginx          Deploy nginx, expose, curl — uses current kubectl context on host
  --cleanup                            Only remove nginx pod/svc (does NOT run the test)

klimax registry clean-cache            Stop mirror containers + delete cache dirs; run 'klimax up' to restart

klimax completion bash|zsh|fish|powershell   Print shell completion script
```

Global flags (all commands): `-c config.yaml`, `--debug`

---

## Registry cache persistence

Mirror registry containers (`registry-dockerio`, `registry-quayio`, `registry-gcrio`) are started with `-v <cacheDir>:/var/lib/registry`. The cache dir location depends on `registries.cacheStorage`:

- **`host`** (default): `~/.klimax/registry-cache/<name>/` on the macOS host, virtiofs-mounted into the VM at the same absolute path. Survives `klimax destroy`. Lima mount is added to the instance at creation time in `limatemplate.Build()`.
- **`guest`**: `/var/lib/klimax/registry-cache/<name>/` inside the VM. Persists across `klimax down`/`up`, wiped on `klimax destroy`.

> Changing `cacheStorage` after instance creation requires `klimax destroy && klimax up`.

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
- The vzNAT subnet is macOS-assigned and not configurable; do not overlap `kindBridgeCIDR` with it (the macOS-assigned range is typically `192.168.64.x` but may vary).
- `DOCKER_HOST` env var overrides the active Docker context — use one mechanism or the other, not both.
- Registry containers run inside the VM; `guest.WriteFile` uses `sudo tee` and `sudo rm -rf` to handle root-owned stale paths from previous failed runs.
- `klimax down` does **not** remove the macOS host route by default (stale route is harmless; `klimax up` refreshes it). Use `--remove-route` to remove it explicitly.
- `network.disablePortMirroring: true` — the lima0 IP is assigned dynamically by macOS and may change on VM restart; re-run `klimax cluster merge <name>` after a restart to refresh kubeconfigs. Host-based security software (e.g. CrowdStrike) may also block TCP connections to vzNAT IPs — use the default loopback mode in that case.

---

## Release

- `goreleaser` for cross-compilation and GitHub releases
- Homebrew tap: `bcollard/homebrew-klimax`
