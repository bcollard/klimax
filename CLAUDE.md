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
- **Cluster lifecycle is CLI-only** — `klimax cluster create/delete/list`. There is no cluster list in the config file. `klimax cluster apply -f <Fleet>` creates a *fleet* declaratively, but the manifest is a **separate, ephemeral input** (like `kubectl apply -f`), never `config.yaml` — the config stays infra-only. `apply` is additive (create-if-absent, skip-if-present).
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
skill.go                             root package `klimax`; go:embed SKILL.md into var SkillMD (shipped by `klimax skill install`)
SKILL.md                             canonical Agent Skill (single source of truth; embedded into the binary)

internal/config/config.go            Config struct, LoadConfig, Validate, defaults
internal/limatemplate/template.go    builds limatype.LimaYAML (Ubuntu 25.04, portForwards, provision script)
internal/vm/vm.go                    Manager: EnsureRunning, Stop, Delete, Inspect
internal/guest/guest.go              SSH Client: Run, RunScript, RunScriptStream, WriteFile, SSHArgs
internal/docker/network.go           EnsureKindNetwork (idempotent, CIDR comparison)
internal/registry/registry.go        EnsureRegistries, RegistryHosts (local reg + mirrors → containerd certs.d hosts.toml + cache volumes)
internal/kind/kind.go                CreateCluster, DeleteCluster, ListClusters, DetectUsedNums, NextFreeNum, LabelNodes
internal/kind/query.go               ClustersMatchingSelector (kubectl -l), ClustersByFleet (klimax.dev/fleet via jq), ClusterInfoFor (nodes/version/ready/labels)
internal/kind/addons.go              InstallMetricsServer (addon installers)
internal/fleet/fleet.go              Fleet manifest: types, Parse, Validate (names-only minimal form, dependsOn DAG, cycle detection)
internal/fleet/plan.go               Resolve → Plan (num pre-assignment, defaults merge, existence marking), DeletionOrder
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
internal/cli/cluster.go              `klimax cluster` subcommands (create/delete/list/label/e2e-test-nginx; use+merge deprecated → kubeconfig)
internal/cli/kubeconfig.go           `klimax kubeconfig` (path/env/merge/remove/use) — kubeconfig helpers; `use` merges + kubectl use-context
internal/cli/cluster_apply.go        `klimax cluster apply -f`/`delete -f` — Fleet manifest: dependsOn DAG scheduler, maxParallel, skip-existing, serialized kubeconfig merge, per-cluster overrides
internal/cli/fleet.go                `klimax fleet` subcommands (list/describe/create/delete/label) — fleet membership tracked by the klimax.dev/fleet node label, not the manifest; describe curates infra labels in text, full set in json/yaml
internal/cli/registry.go             `klimax registry clean-cache`
internal/cli/skill.go                `klimax skill install|path` — install the embedded Agent Skill for AI coding tools
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
  nodeVersion: "v1.35.0"             # kindest/node image tag (default)
  metalLBVersion: "v0.15.2"          # MetalLB manifest version (default)
  customDnsResolvers:                # per-zone upstream resolvers; resolvers default to 8.8.8.8/8.8.4.4 if omitted
    # - domain: "runlocal.dev"       # example; empty by default in code
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
4. Configure Docker socket permissions via `docker.socket.d/override.conf` (`SocketUser=lima` — the guest user is pinned to `lima` via `user.name` in the Lima YAML; by default Lima derives it from the macOS host username, which would break the `SocketUser=lima` assumption on hosts whose username is a valid Linux name)
5. Install Docker via `get.docker.com`
6. Install kind CLI `v0.31.0`
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
4. **`kind create cluster`** with `--image kindest/node:<nodeVersion>`
5. **Configure registry mirrors** — write `/etc/containerd/certs.d/<host>/hosts.toml` on every node (`configureRegistryMirrors` → `registry.RegistryHosts`). containerd 2.x (kind ≥ v0.30 node images) defaults `config_path = /etc/containerd/certs.d` and **rejects** the legacy `registry.mirrors` config.toml block ("`mirrors` cannot be set when `config_path` is provided"), which silently disables the CRI plugin and hangs `kubeadm init`. No containerd restart needed — certs.d is read per-pull.
6. **Install MetalLB** (`kubectl apply -f …/metallb-native.yaml`); wait for readiness
7. **Configure IPAddressPool**: `172.30.<num>.1–7` and `172.30.<num>.16–254`; L2Advertisement
8. **Apply `local-registry-hosting` ConfigMap** in `kube-public`
9. **Patch CoreDNS** ConfigMap with per-zone upstream resolvers from `customDnsResolvers`
10. **Export kubeconfig** → `~/.kube/klimax/<name>.kubeconfig`; server set to `https://127.0.0.1:700N` (default) or `https://<lima0IP>:700N` (when `disablePortMirroring: true`)

### kubeconfig naming

`exportKubeconfig` strips the `kind-` prefix from context/cluster/user names — contexts are stored as bare `<name>` in `~/.kube/config`, not `kind-<name>`. `removeFromKubeconfig` must use the bare name accordingly.

---

## Fleet manifest (`klimax cluster apply -f`)

A declarative fleet applied via `klimax cluster apply -f <file>`. See `examples/fleet.yaml`.

- **Minimal manifest lists only names** — everything else defaults:
  ```yaml
  apiVersion: klimax.dev/v1alpha1
  kind: Fleet
  spec:
    clusters: [dev, staging]
  ```
- **`ClusterEntry` unmarshals from a bare string OR an object** (`fleet.ClusterEntry.UnmarshalYAML`) — that's what makes the names-only form work.
- **Per-cluster options**: `dependsOn`, `num`, `nodeVersion`, `region`, `zone`, `registries` (cherry-pick: `localRegistry` bool + `mirrors` — `nil`=all, `["*"]`=all, `[]`=none, `[names]`=subset, by config mirror name), `addons.metricsServer` (`enabled`/`version`/`kubeletInsecureTLS`), `labels` (map). `spec.defaults` supplies inherited values (labels are merged, entry wins).
- **Scheduler** (`internal/cli/cluster_apply.go`): builds a dependsOn DAG, creates clusters up to `spec.maxParallel` at a time (default 1 = sequential), gating each on its dependencies via per-cluster `done` channels. `strategy: FailFast` (default) stops scheduling new clusters after the first failure; `ContinueOnError` presses on.
- **Race-safety** (ties into [[project_concurrent_cluster_create]]): all nums are **pre-assigned** in `fleet.Resolve` before any create (honouring explicit nums, filling gaps around live clusters); kubeconfig merges are **serialized** behind a mutex even when creates run in parallel.
- **Additive**: existing clusters are skipped (never recreated/mutated). Mirror-name selections are validated against the config catalog up front.
- **Teardown**: `klimax cluster delete -f <file>` deletes the manifest's clusters that exist, in reverse-dependency order (`fleet.DeletionOrder`), prompting unless `--yes`.
- **Node labels** (`kind.applyNodeLabels`, applied post-create via `kubectl label nodes --all --overwrite`, admin creds so no NodeRestriction): every klimax cluster always gets `managed-by=klimax`; fleets add `klimax.dev/fleet=<metadata.name>`; `region`/`zone` are surfaced as `topology.kubernetes.io/*` (kubeadm node-labels patch); custom labels come from `-l key=value` (CLI) or `labels:`/`defaults.labels` (Fleet). Validated by `config.ValidateLabels` before any create. Existing clusters can be relabeled with `klimax cluster label <name> -l key=value` / `-l key-` (reuses `kind.LabelNodes`).
- **`fleet` command & selectors**: fleet membership is derived from the live `klimax.dev/fleet` node label (not the manifest), so `klimax fleet list/describe/delete/label <name>` operate on whatever clusters currently carry the label.
- **Adoption**: `apply`/`fleet create` only *skip* pre-existing clusters by name — it does not relabel them, so a listed cluster that isn't already a member is **not** silently pulled into the fleet. Instead it warns and lists them; re-run with `--adopt` to relabel them into the fleet (fleet label + the manifest entry's labels, via `adoptIntoFleet` → `kind.LabelNodes`). `klimax fleet adopt <fleet> <cluster>…` does the same for arbitrary existing clusters (just sets `klimax.dev/fleet`). `fleet delete <name>` and `fleet label <name>` resolve members via `kind.ClustersMatchingSelector(g, "klimax.dev/fleet=<name>")`; `fleet list` groups via `kind.ClustersByFleet`. `cluster list -l` / `cluster delete -l` take an arbitrary kubectl label selector (matched in-guest by kubectl, one call per cluster). `fleet create -f`/`delete -f` delegate to the same code as `cluster apply -f`/`delete -f`. Selectors are charset-validated (`selectorRE`) before shell interpolation.

---

## CLI reference

```
klimax up                              Start VM + infra (idempotent)
klimax down                            Stop VM (no sudo required)
klimax down --remove-route             Stop VM and remove macOS host route (requires sudo)
klimax destroy                         Delete all clusters, delete VM, remove route
klimax status                          Show VM state, clusters, route, iptables
klimax doctor                          Diagnose common issues (VM, route, iptables, IP forwarding, Rosetta host+VM state)
klimax version                         Print version
klimax shell                           Open interactive SSH session in the VM
klimax config edit                     Open config in $VISUAL / $EDITOR / nano / vi

klimax docker-env                      Print: export DOCKER_HOST=unix://~/.<name>.docker.sock
klimax docker-env --unset              Print: unset DOCKER_HOST

klimax docker-context                  Create/update "klimax" Docker context + docker context use <name>
klimax docker-context --unset          docker context use default

klimax cluster create <name>           Create a kind cluster (num auto-assigned)
  --region europe-west1                Override topology region label
  --zone   europe-west1-b              Override topology zone label
  -l, --label key=value               Extra node label (repeatable)
klimax cluster apply -f <file>         Create a fleet from a Fleet manifest (- for stdin)
  --dry-run                            Print the resolved plan (nums, DAG, options) and exit
  --max-parallel N                     Override spec.maxParallel (concurrent creations)
klimax cluster delete [name]           Delete a cluster; interactive multi-select picker if no name given
  -f <file>                            Delete the clusters listed in a Fleet manifest (reverse-dependency order)
  -l, --selector <sel>                 Delete clusters whose nodes match a label selector
  -y, --yes                            Skip the confirmation prompt
klimax cluster list                    List clusters with num, API port, kubeconfig path
  -o text|json|yaml                    Output format
  -l, --selector <sel>                 Filter by node label selector (e.g. klimax.dev/fleet=f1)
klimax cluster use <name>              DEPRECATED → 'klimax kubeconfig env <name>'
klimax cluster merge <name>            DEPRECATED → 'klimax kubeconfig merge <name>'

klimax kubeconfig path <name>          Print the cluster's kubeconfig file path
klimax kubeconfig env <name>           Print: export KUBECONFIG=~/.kube/klimax/<name>.kubeconfig
klimax kubeconfig merge <name>         Merge the cluster's context into ~/.kube/config
klimax kubeconfig remove <name>        Remove the cluster's context from ~/.kube/config
klimax kubeconfig use <name>           Merge + `kubectl config use-context <name>` (switch active context)
klimax cluster label <name>            Label an existing cluster's nodes
  -l, --label key=value               Set/overwrite a node label (repeatable)
  -l, --label key-                    Remove a node label
klimax cluster e2e-test-nginx          Deploy nginx, expose, curl — uses current kubectl context on host
  --cleanup                            Only remove nginx pod/svc (does NOT run the test)

klimax fleet create -f <file>          Create clusters from a Fleet manifest (alias of 'cluster apply -f'; --dry-run, --max-parallel)
  --adopt                              Adopt pre-existing clusters listed in the manifest into this fleet (relabel them)
klimax fleet adopt <fleet> <cluster>…  Adopt existing clusters into a fleet (sets their klimax.dev/fleet label)
klimax fleet list [-o text|json|yaml]  List fleets (grouped by klimax.dev/fleet) and their member clusters
klimax fleet describe <name>           Show a fleet's members with num, API port, kubeconfig, node count/version/readiness, labels ([-o text|json|yaml])
klimax fleet delete <name>             Delete all clusters in the named fleet (-y to skip prompt)
klimax fleet delete -f <file>          Delete the clusters listed in a Fleet manifest
klimax fleet label <name> -l key=value Apply node labels to every cluster in the fleet (key- to remove)

klimax registry clean-cache            Stop mirror containers + delete cache dirs; run 'klimax up' to restart

klimax skill install                   Install the embedded Agent Skill into ~/.claude/skills/klimax/SKILL.md
  --claude                             Target Claude Code's user skills dir (default true)
  --print                              Write the skill to stdout instead of installing
  --force, -f                          Overwrite an existing installed skill
klimax skill path                      Print the Claude Code install path for the skill

klimax completion bash|zsh|fish|powershell   Print shell completion script
```

Global flags (all commands): `-c config.yaml`, `--debug`, `--lima-log-level <level>`

**Logging:** klimax's own logs use `log/slog`; Lima's library logs use `logrus`. By default klimax raises logrus to `error` so only klimax logs (and genuine Lima errors) show — the noisy `INFO[…]`/`WARN[…]` Lima lines are hidden. `--debug` surfaces Lima at `info` (and klimax at debug); `--lima-log-level trace|debug|info|warn|error|off` overrides explicitly (`off`→panic-only). Set in `root.go` `PersistentPreRunE` (`resolveLimaLogLevel`). The `hostagent` subcommand re-sets logrus to debug/JSON in `initHostagentLogrus`, so quieting the parent never affects VM readiness detection.

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
- The macOS route is added/refreshed only when missing or pointing at a stale gateway; when it already targets the current lima0 IP, `klimax up` skips it entirely and does **not** invoke sudo (so re-running `up` on a live VM never prompts). See `routing.RouteGateway`.
- `cluster create` warns (does not block) when `kind.nodeVersion` differs from `config.DefaultKindNodeVersion` — the image the bundled kind CLI is validated against.
- **On first VM creation only**, `klimax up` reviews an existing config (`reviewConfigBeforeCreate` in `up.go`): it lists options this klimax version adds that the config doesn't set (`config.MissingKeys` — schema diff of the user's file vs the defaulted struct), and if `kind.nodeVersion` drifts from `config.DefaultKindNodeVersion` it **interactively offers to rewrite it** in the config file (`rewriteNodeVersion`, preserves comments/indent). Non-interactive (no TTY): it keeps the pinned value and only warns — never blocks. Skipped entirely when the VM already exists.
- All kubeconfigs are written atomically with `0600` permissions.

---

## Known caveats

- Docker rewrites iptables on restart → handled by `ExecStartPost` drop-in.
- MetalLB (and metrics-server) readiness waits are `300s` — their images are pulled from quay.io through the mirror on first use, which can exceed two minutes on a cold cache. On a *very* slow link even 300s can be too short (controller must pull+run before speaker's `memberlist` secret exists, then speaker pulls too); the mirror cache warms after the first successful pull. `kind create cluster --wait 5m` covers the core node images (preloaded).
- macOS VPN software can conflict with the host route → `klimax doctor` warns.
- `podSubnet: 10.1<num>.0.0/16` overlaps with `serviceSubnet: 10.<num+10>.0.0/16` for num≥10. In practice keep num 1–9 per VM.
- The vzNAT subnet is macOS-assigned and not configurable; do not overlap `kindBridgeCIDR` with it (the macOS-assigned range is typically `192.168.64.x` but may vary).
- `DOCKER_HOST` env var overrides the active Docker context — use one mechanism or the other, not both.
- Registry containers run inside the VM; `guest.WriteFile` uses `sudo tee` and `sudo rm -rf` to handle root-owned stale paths from previous failed runs.
- **Mirror names must not be hostnames.** A mirror container joins the shared `kind` Docker network; a name like `quay.io` makes Docker's embedded DNS resolve `quay.io` to the container itself, so the pull-through proxy (whose `remoteurl` is `https://quay.io`) resolves upstream to itself → connection refused → `404 manifest unknown` → containerd silently falls back to slow, unauthenticated **direct** pulls (no cache, docker.io throttling). `config.Validate` now rejects mirror names containing `.` or `:`. Renaming a mirror also requires recreating its container (`docker rm -f` the old one, then `klimax up`) since the old-named container keeps its port.
- `klimax down` does **not** remove the macOS host route by default (stale route is harmless; `klimax up` refreshes it). Use `--remove-route` to remove it explicitly.
- `network.disablePortMirroring: true` — the lima0 IP is assigned dynamically by macOS and may change on VM restart; re-run `klimax cluster merge <name>` after a restart to refresh kubeconfigs. Host-based security software (e.g. CrowdStrike) may also block TCP connections to vzNAT IPs — use the default loopback mode in that case.

---

## Keeping docs in sync

When adding or changing a user-facing command, flag, or manifest field, update **all** the doc surfaces — they drift independently:

- `README.md` (user reference) and this `CLAUDE.md`
- `config.example.yaml` and/or `examples/fleet.yaml` where a config/manifest field changed
- `SKILL.md` — the Agent Skill is **`go:embed`-ded into the binary** (shipped by `klimax skill install`). It does not update itself; after a build, verify with `klimax skill install --print`. It shipped stale once because it was easy to forget.

Verify behavior end-to-end against the live VM before cutting a release (see the live-VM testing caution: throwaway clusters, clean up).

## Release

- `goreleaser` for cross-compilation and GitHub releases
- Triggered by pushing a `vX.Y.Z` tag (`.github/workflows/release.yml`); default bump is a patch on the latest tag.
- **Tags must be annotated** — `git tag -a vX.Y.Z -m "..."`. A lightweight `git tag vX.Y.Z` fails with `fatal: no tag message?` (repo is configured to require annotated tags).
- Homebrew distribution is a **Cask**, not a Formula: `bcollard/homebrew-klimax` → `Casks/klimax.rb`. goreleaser bumps it automatically on release. Users install/upgrade with `brew upgrade --cask klimax` (or `brew reinstall --cask klimax`).
