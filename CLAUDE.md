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
internal/limatemplate/template.go    builds limatype.LimaYAML (Ubuntu 26.04 LTS, portForwards, provision script)
internal/vm/vm.go                    Manager: EnsureRunning, Stop, Delete, Inspect
internal/guest/guest.go              SSH Client: Run, RunScript, RunScriptStream, WriteFile, SSHArgs
internal/docker/network.go           EnsureKindNetwork (idempotent, CIDR comparison)
internal/registry/registry.go        EnsureRegistries, RegistryHosts (pull-through mirrors → containerd certs.d hosts.toml + cache volumes)
internal/kind/kind.go                CreateCluster, DeleteCluster, ListClusters, DetectUsedNums, NextFreeNum, LabelNodes
internal/kind/query.go               ClustersMatchingSelector (kubectl -l), ClustersByFleet (klimax.dev/fleet via jq), ClusterInfoFor (nodes/version/ready/labels)
internal/kind/addons.go              InstallMetricsServer (addon installers)
internal/fleet/fleet.go              Fleet manifest: types, Parse, Validate (names-only minimal form, dependsOn DAG, cycle detection)
internal/fleet/plan.go               Resolve → Plan (num pre-assignment, defaults merge, existence marking), DeletionOrder
internal/routing/macos.go            EnsureRoute, DeleteRoute, RouteExists, Lima0IP
internal/routing/iptables.go         InstallNoNat, CheckNoNatRule

internal/vm/guestagent.go            EnsureGuestAgent — downloads & caches lima-guestagent from GitHub releases
internal/vm/disk.go                  EnsureImageDisk / ResizeImageDisk — the persistent Lima data disk for the container image store
internal/vm/mounts.go                Read/WriteInstanceMounts (yaml-node surgery on the instance config), NormalizeMounts, MountsEqual — vm.mounts reconciliation
internal/config/proxy.go             ProxyConfig, NoProxy/NoProxyString/ProxyEnv — the computed no_proxy list
internal/hostres/hostres.go          Read/ReadFor (host CPU, RAM, free disk), DefaultCPUs + DefaultMemoryBytes (host-scaled
                                     defaults), CheckResources (over-commit warnings). Its own package because
                                     internal/vm imports internal/config, so config cannot import vm.
internal/hostres/hostmem_darwin.go   hostMemoryBytes via sysctl hw.memsize (build-tagged; the package still builds for linux)

internal/cli/root.go                 cobra root command, persistent flags (--config, --debug)
internal/cli/up.go                   `klimax up` — infra only (VM + network + registries + routing)
internal/cli/down.go                 `klimax down` [--remove-route]
internal/cli/destroy.go              `klimax destroy`
internal/cli/status.go               `klimax status` — collectStatus() → statusReport, rendered as text/json/yaml (host mounts read from the instance config, so they show on a stopped VM)
internal/cli/doctor.go               `klimax doctor` — diagnose() → []doctorCheck, `--fix` applies the Fixable ones
internal/cli/fleet_export.go         `klimax fleet export` — live clusters → Fleet manifest (args, -l selector, or picker)
internal/cli/version.go              `klimax version`
internal/cli/shell.go                `klimax shell` — interactive SSH session, or non-interactive command runner (args → remote command, exit code propagated)
internal/cli/copy.go                 `klimax copy` — scp between host and VM (`vm:`/`<vmName>:` marks the guest side)
internal/cli/sudoers.go              `klimax sudoers` — emits/checks the NOPASSWD rules for the two /sbin/route commands
internal/cli/disk.go                 `klimax disk resize` — grows vm.disk + the Lima instance config (applied on next start)
                                     `klimax disk resize-image` — grows the vm.imageDisk data disk in place (VM must be stopped)
internal/cli/prune.go                `klimax prune` — removes superseded guest agents, orphaned registry caches, (opt-in) Lima download cache
internal/cli/autostart.go            `klimax autostart` — launchd agent (dev.klimax.autostart) running `klimax up` at login
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

> **Defaults are applied on every load, not baked into the file.**
> `config.applyDefaults` runs inside `LoadConfig`, so any key *absent* from the
> file is recomputed each run — which is what lets `vm.cpus`/`vm.memory` scale to
> whichever Mac reads it. A key *present* in the file always wins and is never
> recomputed. `WriteDefaultConfig` (used when no config exists) is the exception:
> it computes once and writes the numbers in as literal values, so a generated
> config is a **pinned** config and does not adapt if copied to another machine.


Cluster lifecycle is **not** in the config file. The config drives infrastructure only.

```yaml
vm:
  name: "klimax"         # Lima instance name; socket at ~/.<name>.docker.sock
  cpus: 8                # default: 3/4 of the host's cores, min 2 (10-core -> 8)
  memory: "20GiB"        # default: half the host's RAM, min 2GiB
                         # (16GiB -> 8GiB, 32GiB -> 16GiB, 64GiB -> 32GiB)
  disk: "20GiB"          # default. Root disk only carries the OS, Docker metadata
                         # and volumes — images live on imageDisk. Both are sparse.
  rosetta: false         # Rosetta 2 for amd64 containers; ARM64 only
  imageDisk: "30GiB"     # default: persistent Lima data disk mounted over
                         # /var/lib/containerd so the image store survives `destroy`.
                         # Always on — applyDefaults re-fills an empty value, so
                         # there is no config route back to the root disk.
                         # ⚠ VM-level: new VMs only. Resize an existing one with
                         # `klimax disk resize-image`.
  mounts: []             # host dirs shared into the guest over virtiofs; empty by default.
                         # Each appears at the SAME absolute path in the VM, which is
                         # what makes a host-path `docker -v` bind resolve.
                         #   - location: "~/projects"   ("~" expanded; must exist)
                         #     writable: true           (default false, matching Lima)
                         #     mountPoint: "/srv/x"     (optional; differing guest path
                         #                               breaks `-v <host path>`)
                         # NOT VM-level: `klimax up` offers a restart to apply.

network:
  kindBridgeCIDR: "172.30.0.0/16"   # Docker "kind" network subnet
  disablePortMirroring: true         # default true: disable Lima TCP port mirroring; kubeconfigs use VM's lima0 IP directly
                                     # Coexists with other Lima VMs (kind-on-lima, Rancher Desktop) that manage
                                     # kind clusters — prevents API-server port conflicts on 127.0.0.1.
                                     # Set false to force loopback (127.0.0.1) — e.g. host security software
                                     # (CrowdStrike) blocking vzNAT IPs.
                                     # ⚠ VM-level: only takes effect on new VMs (klimax destroy && up).

kind:
  nodeVersion: "v1.36.1"             # kindest/node image tag (default)
  metalLBVersion: "v0.16.1"          # MetalLB manifest version (default)
  customDnsResolvers:                # per-zone upstream resolvers; resolvers default to 8.8.8.8/8.8.4.4 if omitted
    # - domain: "runlocal.dev"       # example; empty by default in code
  autoMergeKubeconfig: true          # merge context into ~/.kube/config after cluster create (default: true)
  autoRemoveKubeconfig: true         # remove context from ~/.kube/config after cluster delete (default: true)

registries:
  cacheStorage: "host"               # "host" (default): ~/.klimax/registry-cache/ via virtiofs, survives destroy
                                     # "guest": inside VM, wiped on destroy
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
    - name: "registry-us-docker-pkgdev"
      port: 5040
      remoteURL: "https://us-docker.pkg.dev"
    - name: "registry-us-central1-docker-pkgdev"
      port: 5050
      remoteURL: "https://us-central1-docker.pkg.dev"
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
6. Install kind CLI `v0.32.0`
7. Install kubectl (latest stable)

### Docker socket forwarding

Lima `portForwards` forwards `/run/docker.sock` → `~/.<vmName>.docker.sock` on the host.

Two ways to use it:
- `eval $(klimax docker-env)` — sets `DOCKER_HOST` in the current shell
- `klimax docker-context` — creates/updates a named Docker context (persistent across shells); conflicts with `DOCKER_HOST` if both are set

### Self-contained guest agent

Lima requires a small Linux binary (`lima-guestagent`) uploaded into the VM at startup for port forwarding. `internal/vm/guestagent.go` (`EnsureGuestAgent`) downloads it from Lima's GitHub release on first `klimax up` and caches it at `~/.klimax/share/lima/lima-guestagent.Linux-<arch>-<limaVer>.gz`. The cache filename is **version-stamped**, so bumping the `lima/v2` module re-downloads the matching guest agent (guest and host agents must be the same version). No separate Lima installation required.

The downloaded version is matched to the Lima Go module version at runtime via `runtime/debug.ReadBuildInfo()`. The release asset uses `uname -m` naming: `Darwin-arm64` / `Darwin-x86_64` (not `Darwin-amd64`).

### lima-version file

Lima writes a `lima-version` file (mode `0o444`) into the instance dir during `instance.Create()` with version `"<unknown>"` when no ldflags are set. klimax fixes this in `vm.create()` by calling `os.Remove()` then `os.WriteFile()` with the actual Lima module version read via `runtime/debug.ReadBuildInfo()`. `os.WriteFile` alone silently fails on a read-only file — the Remove step is required.

### hostagent subprocess

Lima's `instance.StartWithPaths()` spawns `os.Executable() hostagent INSTANCE --socket ... --guestagent ...` as a detached daemon. `internal/cli/hostagent.go` implements this hidden subcommand (ports Lima's `cmd/limactl/hostagent.go`). It must configure **logrus JSON formatter** — Lima's event watcher parses JSON log lines to detect VM readiness. `runtime.LockOSThread()` is required when `--run-gui` is set (VZ on macOS).

### Building locally: the binary must be codesigned

`go build` alone produces a binary that **cannot start a VM**. Virtualization.framework
refuses it with a misleading error that says nothing about signing:

```
Error Domain=VZErrorDomain Code=2 "Invalid virtual machine configuration.
The process doesn't have the com.apple.security.virtualization entitlement."
```

`make build` handles this; a bare `go build -o /tmp/klimax ./cmd/klimax` does not:

```sh
codesign --sign - --entitlements entitlements.plist --force <binary>
```

Read-only commands (`status`, `doctor`, `cluster list`, `fleet export`,
`disk resize-image`) work fine unsigned, so an unsigned build can look healthy
right up until it tries to boot the VM.

### Binary replacement safety

Replacing `/usr/local/bin/klimax` while the hostagent is running causes macOS `amfid` to kill subsequent klimax execs. `make dev-install` aborts if a hostagent process is detected. `klimax doctor` also warns with the kill+cleanup fix command.

---

## Cluster creation flow (`klimax cluster create <name>`)

1. **Auto-assign num** — inspect live `<name>-control-plane` containers' port bindings (70N → num=N); find lowest free slot 1–99.
2. **Resolve API server address** — by default (`network.disablePortMirroring: true`) resolves the VM's live `lima0` IP via SSH (`routing.Lima0IP`) to embed in the cert SANs; with `disablePortMirroring: false` it uses `127.0.0.1`.
3. **Build kind cluster config** with:
   - API port `70<num>` on `0.0.0.0`
   - `serviceSubnet: 10.<num>.0.0/16`, `podSubnet: 10.1<num>.0.0/16`
   - `kubeadmConfigPatches`: `topology.kubernetes.io/region` + `zone` labels; when `disablePortMirroring` (the default), also a `ClusterConfiguration` patch adding the lima0 IP to `apiServer.certSANs` (plus `127.0.0.1` for intra-VM kubectl calls)
4. **`kind create cluster`** with `--image kindest/node:<nodeVersion>`
5. **Configure registry mirrors** — write `/etc/containerd/certs.d/<host>/hosts.toml` on every node (`configureRegistryMirrors` → `registry.RegistryHosts`). containerd 2.x (kind ≥ v0.30 node images) defaults `config_path = /etc/containerd/certs.d` and **rejects** the legacy `registry.mirrors` config.toml block ("`mirrors` cannot be set when `config_path` is provided"), which silently disables the CRI plugin and hangs `kubeadm init`. No containerd restart needed — certs.d is read per-pull.
6. **Install MetalLB** (`kubectl apply -f …/metallb-native.yaml`); wait for readiness
7. **Configure IPAddressPool**: `172.30.<num>.1–7` and `172.30.<num>.16–254`; L2Advertisement
8. **Patch CoreDNS** ConfigMap with per-zone upstream resolvers from `customDnsResolvers`
9. **Export kubeconfig** → `~/.kube/klimax/<name>.kubeconfig`; server set to `https://<lima0IP>:700N` by default (`disablePortMirroring: true`) or `https://127.0.0.1:700N` (when `disablePortMirroring: false`)

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
- **Per-cluster options**: `dependsOn`, `num`, `nodeVersion`, `region`, `zone`, `registries` (cherry-pick: `mirrors` — `nil`=all, `["*"]`=all, `[]`=none, `[names]`=subset, by config mirror name), `addons.metricsServer` (`enabled`/`version`/`kubeletInsecureTLS`), `labels` (map). `spec.defaults` supplies inherited values (labels are merged, entry wins).
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
klimax status                          Show VM state, host mounts, clusters, route, iptables
  -o text|json|yaml                    Output format (json/yaml for tooling; `clusters.names` is always a list)
klimax doctor                          Diagnose common issues (VM, route, iptables, IP forwarding, Rosetta host+VM state)
  -o text|json|yaml                    Output format; each check has a stable `id`, `status`, `fixable`
  --fix                                Apply the repairs klimax can perform: route, iptables, IP forwarding.
                                       VM creation/start, Rosetta install and hostagent cleanup stay advisory.
klimax version                         Print version
klimax shell                           Open interactive SSH session in the VM
klimax shell <cmd> [args...]           Run a command in the VM (stdin/stdout passed through, exit code propagated)
  -t, --tty                            Force pseudo-terminal allocation
klimax copy <src>... <dst>             Copy files host↔VM; prefix the VM side with `vm:` (or the VM's name)
  -r, --recursive                      Copy directories
klimax config edit                     Open config in $VISUAL / $EDITOR / nano / vi

klimax disk resize <size>              Grow the VM disk (e.g. 80GiB); rewrites vm.disk + instance lima.yaml, applied on next start
klimax disk resize-image <size>        Grow the persistent image-cache disk (vm.imageDisk), preserving the cached images.
                                       VM must be stopped: the backing file cannot be resized while attached.
                                       Lima's own boot script grows the partition + ext4 on the next start.
                                       ⚠ vm.imageDisk in the config applies only at disk *creation* — EnsureImageDisk
                                       never resizes an existing disk, so `up` warns when the two drift.
klimax prune                           Remove reclaimable caches (superseded guest agents, orphaned registry-cache dirs)
  --dry-run                            Report without removing
  --downloads                          Also clear Lima's shared image download cache (~/Library/Caches/lima/download)
  -y, --yes                            Skip the confirmation prompt (required when non-interactive)
klimax sudoers                         Print sudoers rules so `up` never prompts for the host route
  --check                              Inspect `sudo -l` output for both NOPASSWD route rules
klimax autostart install               Install + load the launchd agent (--print writes the plist to stdout)
klimax autostart uninstall             Unload + remove it
klimax autostart status                Report plist presence and launchd state

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
klimax fleet export [cluster...]       Write a Fleet manifest for live clusters (reverse of `fleet create -f`)
  -l, --selector <sel>                 Select by node label selector instead of names
  --name <fleet>                       metadata.name (default: shared klimax.dev/fleet label, else "exported")
  --nums                               Record each cluster's num, pinning API ports on re-apply (default true)
                                       No names and no selector → interactive picker.
                                       Not captured (not recoverable from live state): dependsOn, registries, addons.
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

## Registry mirrors reach dockerd and the clusters differently

Cluster pulls and `docker pull` take completely different paths, and only one of
them can use every mirror.

| Consumer | Mechanism | Which mirrors it can use |
|---|---|---|
| kind nodes (cluster pulls) | `/etc/containerd/certs.d/<host>/hosts.toml`, written per node by `kind.configureRegistryMirrors` | **all of them** — containerd supports per-registry mirrors |
| dockerd (`docker pull`, `docker compose`) | `registry-mirrors` in `/etc/docker/daemon.json`, written by `cli.reconcileDockerDaemonConfig` | **Docker Hub only** |

`registry-mirrors` has always been Hub-specific; there is no per-registry
equivalent. So `docker pull quay.io/...` on the VM goes direct, while the same
image pulled by a cluster is cached. Hub is where rate limits actually bite, so
the partial fix is still worth having — but do not document it as if it covered
everything.

> **containerd's `certs.d` is not a way around this.** dockerd resolves
> registries with its own client and never reads it, even with the containerd
> snapshotter enabled. Verified on Docker 29: a `certs.d` entry for `quay.io`
> on the VM left `docker pull quay.io/...` going direct, with zero bytes added
> to the mirror cache.

### The endpoint must be 127.0.0.1, not the container name

`registry.HubMirrorEndpoint` returns `http://127.0.0.1:<port>`, deliberately not
the `registry-dockerio` name the kind nodes use. A kind node is a container on
the `kind` network, where Docker's embedded DNS resolves that name; **dockerd
runs in the VM's host namespace, where it does not resolve at all**. Measured:
`curl registry-dockerio:5030` from the VM returns nothing, `127.0.0.1:5030`
returns 200.

Getting this wrong fails silently in the worst way — `docker info` lists the
mirror, and every pull ignores it.

### daemon.json is merged, never overwritten

`registry.MergeDaemonConfig` sets only the `registry-mirrors` key.
`daemon.json` is a file users edit (`insecure-registries`, `log-driver`,
`default-address-pools`), and klimax has no business discarding that to set one
field. A file that does not parse is reported and left alone rather than
replaced.

---

## HTTP proxy support

`network.proxy` configures dockerd, the registry mirrors, and (indirectly) the
kind nodes. Most of the chain already exists upstream; klimax fills two gaps.

**What Lima and kind already do:**

- Lima reads the Mac's system proxy (`propagateProxyEnv`, on by default) and
  writes it into the guest's `/etc/environment` `#LIMA-START` block.
- klimax's SSH commands inherit it: `/etc/pam.d/sshd` has `session required
  pam_env.so` and sshd runs with `UsePAM yes`.
- So `kind create cluster`, run over SSH, sees the proxy — and kind injects
  `ENV HTTP_PROXY/HTTPS_PROXY/NO_PROXY` into every node it creates. The node
  side is free.

**What klimax must add:**

1. **A dockerd systemd drop-in** (`30-klimax-proxy.conf`). `/etc/environment` is
   applied by `pam_env`, which covers login sessions only — systemd services
   never read it. dockerd is what pulls `kindest/node` and `registry:2`, so
   without the drop-in a proxied host fails at the first pull while `curl` from
   a shell works.
2. **Proxy env on the mirror containers.** A pull-through cache reaches its
   upstream itself. `ensureMirror` compares the running container's proxy vars
   against the config and recreates it when they differ — a running container
   keeps the environment it was created with, so a config change would otherwise
   never reach it.

**The computed `no_proxy`** (`config.NoProxy`) is the part that makes this worth
building in: the user does not know the bridge CIDR, the mirror names, or the
subnets kind will allocate, and sending cluster-internal traffic to a corporate
proxy turns every in-cluster call into a timeout. It always contains
`localhost`, `127.0.0.1`, `::1`, `.svc`, `.cluster.local`, `10.0.0.0/8`
(service + pod subnets), `network.kindBridgeCIDR`, a `/24` derived from the live
lima0 IP, and every mirror name — then the user's own `noProxy` entries.

> `10.0.0.0/8` is deliberate. klimax allocates `10.<num>.0.0/16` and
> `10.1<num>.0.0/16` per cluster, so enumerating them is impractical. On a
> corporate network that also uses 10/8 those hosts bypass the proxy too, which
> is normally correct — and the alternative breaks every cluster.

`inheritFromHost` (default true) is resolved by reading the values back out of
the guest's `/etc/environment` rather than re-running `scutil --proxy` on the
host: Lima also rewrites loopback proxy addresses to a gateway the guest can
reach, and re-deriving them would lose that.

Reconciled on every `klimax up` (`cli.reconcileDockerProxy`), like `vm.mounts`
and unlike `vm.imageDisk` — a drop-in has no state beyond the file, so changing
the proxy never needs the VM recreated. Removing `network.proxy` removes the
drop-in.

---

## CA certificates (`vm.caCerts`)

For a proxy that terminates TLS, or a private registry with a self-signed chain.
Without them every pull fails with `x509: certificate signed by unknown
authority` even when `network.proxy` is correct — the proxy is reachable, its
certificate simply is not trusted.

**Three separate trust stores** have to be reached, which is the whole
complication:

| Consumer | How it gets the CA |
|---|---|
| The VM (dockerd) | Lima `caCerts` → cloud-init `ca_certs` at first boot; `cli.reconcileCACerts` on later `up` runs |
| Registry mirrors | The guest bundle `/etc/ssl/certs/ca-certificates.crt` is bind-mounted read-only (Debian and Alpine both read that path) |
| kind nodes | `kind.configureCACerts` writes the PEM into each node and runs `update-ca-certificates`, then restarts containerd — a node is a container with its own store |

Lima's `caCerts` is required rather than merely convenient: cloud-init installs
Docker from `get.docker.com` during first boot, which fails TLS verification
behind an intercepting proxy before any klimax provisioning runs.

`config.CACerts.Load` validates the PEM up front and refuses a file containing a
private key — pointing at the wrong file otherwise surfaces much later as a pull
failure that looks like a network problem.

### Loopback proxies and containers

A proxy on the VM's loopback is unreachable from inside a container, where
`127.0.0.1` is the container's own. `config.ContainerProxyEnv` rewrites a
loopback proxy host to the kind bridge gateway for container-facing consumers;
dockerd and guest shells keep the literal value, since they run in the VM's
network namespace. Lima does the same rewrite for the guest.

This matters more than it sounds: `registry:2` **panics at startup** when its
upstream is unreachable, and with `--restart=always` that becomes a crash loop
across every mirror.

### Why klimax's own env block is stripped before inheriting

`network.proxy` is written into a `#KLIMAX-START`/`#KLIMAX-END` block in the
guest's `/etc/environment`, so `kind create cluster` (and therefore every node)
and in-guest `kubectl apply -f https://…` see it — Lima's block only ever
carries what macOS system settings say.

`cli.guestEnvironmentProxy` strips that block before reading the file for the
`inheritFromHost` path. Without it, a configured proxy becomes self-sustaining:
the next run re-inherits the previous run's value, and removing `network.proxy`
never takes effect.

---

## Host mounts vs. disks — two different mechanisms

`mountType: virtiofs` in `limatemplate.Build()` governs **`y.Mounts` only** — the
host-directory shares. It has nothing to do with storage:

| klimax config | Lima field | Guest | Mechanism |
|---|---|---|---|
| `vm.disk` | `disk` | `/dev/vda1` → `/` | virtio-blk block device, ext4 |
| `vm.imageDisk` | `additionalDisks` | `/dev/vdb1` → `/var/lib/containerd` | virtio-blk block device, ext4 |
| `registries.cacheStorage: host` | `mounts[]` | same absolute path | **virtiofs** |
| `vm.mounts` | `mounts[]` | same absolute path (or `mountPoint`) | **virtiofs** |

So `vm.mounts` is purely additive to `y.Mounts` and cannot interact with the
image-disk machinery. `limatemplate.BuildMounts()` builds the whole list —
registry cache first, then user mounts — and is exported so `klimax up` can
compute the desired set without regenerating the rest of the instance YAML.

### Why `vm.mounts` reconciles in place instead of needing `destroy && up`

`vm.imageDisk` and `network.disablePortMirroring` are baked in at instance
creation because they have on-disk or guest-side consequences. Mounts have
neither: Lima reads the list when the VM starts, so a stopped VM plus an edited
`lima.yaml` *is* the whole change. Making people pay a `klimax destroy` — which
throws away every kind cluster — to share a folder would be absurd.

`cli.reconcileMounts` therefore:

1. reads the live `mounts:` out of the instance `lima.yaml` (not
   `limatype.Instance.Config`, which has Lima's defaults filled in);
2. compares against `limatemplate.BuildMounts(cfg)` via `vm.MountsEqual`, which
   normalizes Lima's defaulting (`mountPoint` defaults to `location`, `writable`
   to `false`);
3. writes the new list only when the VM is **stopped** — running, it prints the
   diff and offers a restart (declining, or a non-interactive run, changes
   nothing). `EnsureRunning` then restarts it.

`vm.WriteInstanceMounts` edits the parsed **yaml.Node tree**, not the struct: the
instance config carries provisioning scripts the guest has already run, and
regenerating the file from `Build()` would mean a klimax upgrade silently changed
how a live guest is provisioned. `mounts_test.go` pins that the embedded literal
block scalars survive the re-encode byte for byte.

Locations are `~`-expanded in `BuildMounts` rather than left to Lima, so the
value written to the instance config is exactly what drift detection reads back.

> `klimax up` refuses a `vm.mounts` location that does not exist
> (`cli.checkMountLocations`). Lima only warns, then hands Virtualization.framework
> a share for a missing path, which surfaces much later as an opaque VM start
> failure.

---

## Registry cache persistence

Mirror registry containers (`registry-dockerio`, `registry-quayio`, `registry-gcrio`, `registry-us-docker-pkgdev`, `registry-us-central1-docker-pkgdev`) are started with `-v <cacheDir>:/var/lib/registry`. The cache dir location depends on `registries.cacheStorage`:

- **`host`** (default): `~/.klimax/registry-cache/<name>/` on the macOS host, virtiofs-mounted into the VM at the same absolute path. Survives `klimax destroy`. Lima mount is added to the instance at creation time in `limatemplate.Build()`.
- **`guest`**: `/var/lib/klimax/registry-cache/<name>/` inside the VM. Persists across `klimax down`/`up`, wiped on `klimax destroy`.

> Changing `cacheStorage` after instance creation requires `klimax destroy && klimax up`.

### Growing the image disk — Lima does the guest half

`klimax disk resize-image` only grows the **host-side** backing file. Do not add
`growpart`/`resize2fs` to klimax's mount unit: Lima's own
`05-lima-disks.sh` already runs both for additional disks, and it runs *before*
klimax's `klimax-image-disk.service`. Verified end to end — a 10GiB → 30GiB
resize surfaced as a 30GiB ext4 in the guest with no klimax-side partition work.
The `resize2fs` in the mount script is belt-and-braces only.

---

## Safety and idempotency

- `klimax up` is safe to run repeatedly — every step checks before acting.
- **Host over-commit check** (`warnOverCommittedResources` in `up.go` → `vm.CheckResources`): warns when `vm.cpus` exceeds the Mac's cores, when `vm.memory` exceeds its RAM (or passes 75% of it — a softer, differently-worded warning), or when `vm.disk + vm.imageDisk` exceeds free space. Advisory only: it never blocks, because the disks are sparse and macOS swaps rather than refusing. Host facts that cannot be read are skipped rather than guessed.
- iptables rules are inserted only if not already present (`-C` check before `-I`).
- Registry containers are started only if not already running.
- `kind create cluster` only runs for clusters that don't exist.
- The macOS route is added/refreshed only when missing or pointing at a stale gateway; when it already targets the current lima0 IP, `klimax up` skips it entirely and does **not** invoke sudo (so re-running `up` on a live VM never prompts). See `routing.RouteGateway`.
- Route presence is judged by `routing.RouteGateway`, which requires `route -n get` to return the CIDR's own base as the destination. `RouteExists` (used by `status` and `doctor`) delegates to it — a plain `route -n get` succeeds for *any* address via the default route, which previously made both report a missing route as present.
- `klimax up` needs root **only** for that route. `klimax sudoers` emits NOPASSWD rules for exactly the two `/sbin/route` invocations, which is what makes `klimax autostart` viable (launchd cannot answer a password prompt).
- `cluster create` warns (does not block) when `kind.nodeVersion` differs from `config.DefaultKindNodeVersion` — the image the bundled kind CLI is validated against.
- **On first VM creation only**, `klimax up` reviews an existing config (`reviewConfigBeforeCreate` in `up.go`): it lists options this klimax version adds that the config doesn't set (`config.MissingKeys` — schema diff of the user's file vs the defaulted struct), and if `kind.nodeVersion` drifts from `config.DefaultKindNodeVersion` it **interactively offers to rewrite it** in the config file (`rewriteNodeVersion`, preserves comments/indent). Non-interactive (no TTY): it keeps the pinned value and only warns — never blocks. Skipped entirely when the VM already exists.
- All kubeconfigs are written atomically with `0600` permissions.

---

## Known caveats

- Docker rewrites iptables on restart → handled by `ExecStartPost` drop-in.
- MetalLB (and metrics-server) readiness waits are `180s` — their images pull from quay.io through the mirror on first use (controller must pull+run before speaker's `memberlist` secret exists, then speaker pulls); the mirror caches them so subsequent clusters are fast. (The multi-minute timeouts seen historically were actually the hostname-shadowing mirror-bypass bug — direct, uncached pulls — not a genuinely slow mirror.) `kind create cluster --wait 5m` covers the core node images (preloaded).
- macOS VPN software can conflict with the host route → `klimax doctor` warns.
- `podSubnet: 10.1<num>.0.0/16` overlaps with `serviceSubnet: 10.<num+10>.0.0/16` for num≥10. In practice keep num 1–9 per VM.
- The vzNAT subnet is macOS-assigned and not configurable; do not overlap `kindBridgeCIDR` with it (the macOS-assigned range is typically `192.168.64.x` but may vary).
- `DOCKER_HOST` env var overrides the active Docker context — use one mechanism or the other, not both.
- Registry containers run inside the VM; `guest.WriteFile` uses `sudo tee` and `sudo rm -rf` to handle root-owned stale paths from previous failed runs.
- **Mirror names must not be hostnames.** A mirror container joins the shared `kind` Docker network; a name like `quay.io` makes Docker's embedded DNS resolve `quay.io` to the container itself, so the pull-through proxy (whose `remoteurl` is `https://quay.io`) resolves upstream to itself → connection refused → `404 manifest unknown` → containerd silently falls back to slow, unauthenticated **direct** pulls (no cache, docker.io throttling). `config.Validate` now rejects mirror names containing `.` or `:`. Renaming a mirror also requires recreating its container (`docker rm -f` the old one, then `klimax up`) since the old-named container keeps its port.
- **The host filesystem is not shared by default.** Only `~/.klimax/registry-cache` is, plus whatever `vm.mounts` lists. Because dockerd runs in the guest and resolves bind sources there, `docker run -v <unshared host path>:/x` does not fail — it creates the path in the guest and the container gets an empty directory. `klimax up` refuses a `vm.mounts` location that doesn't exist, but it cannot catch a bind for a path nobody listed.
- `klimax down` does **not** remove the macOS host route by default (stale route is harmless; `klimax up` refreshes it). Use `--remove-route` to remove it explicitly.
- `network.disablePortMirroring` defaults to **true** — kubeconfigs use the VM's `lima0` IP, which is assigned dynamically by macOS and may change on VM restart; re-run `klimax kubeconfig merge <name>` after a restart to refresh kubeconfigs. Host-based security software (e.g. CrowdStrike) may block TCP connections to vzNAT IPs — set `disablePortMirroring: false` (loopback/127.0.0.1 mode) in that case.

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
- **Ship the website changelog *before* pushing the tag.** `../klimax-website` needs a `docs/changelog.html` entry (plus any page whose documented defaults or commands changed) per its own `CLAUDE.md`. Merging that first means the docs are live when the binaries appear, instead of briefly describing a version nobody can install.
- A tag pushed by mistake can be recalled if you are quick: `gh run cancel <id>`, then `git push --delete origin vX.Y.Z && git tag -d vX.Y.Z`. Check `gh release view` and the Cask version first — once goreleaser has published, retagging is no longer clean and the fix is a new patch release.

### Supply chain / SBOM

- **SBOM per release**: `.goreleaser.yaml` `sboms:` runs **syft** (installed by `release.yml` via `anchore/sbom-action`) to attach an SPDX SBOM for each release archive to the GitHub release. An SBOM is pinned to its build, so it's generated once per tag — not regenerated on a schedule.
- **Vuln scanning**: `govulncheck` runs in `ci.yml` on every PR/push, and weekly via `security.yml` (cron) to catch newly-disclosed CVEs against unchanged deps. `dependabot.yml` opens weekly update PRs for gomod + github-actions.
- Scope note: the SBOM covers the klimax **binary's** Go deps — not the tools klimax provisions inside the VM (Docker, kind, kubectl, MetalLB, node images), which are version-pinned in code instead.
