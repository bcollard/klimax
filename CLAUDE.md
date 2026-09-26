# Project: marina

Go CLI that wraps **Lima** to manage a macOS Virtualization.framework VM running Docker, with support for multiple kind clusters, pull-through registry mirrors, and pure L3 host↔cluster routing.

---

## Renamed from klimax (v1.0)

marina was **klimax** until v1.0 ("klimax" reads as "climax" in English). The
rename is a clean break, not a compatibility layer — see `internal/cli/migrate.go`:

- Every identifier moved: module `github.com/bcollard/marina`, `~/.marina`
  (LIMA_HOME), VM `marina` / disk `marina-img`, `marina.internal`, labels
  `marina.run/fleet` + `managed-by=marina`, `apiVersion: marina.run/v1alpha1`,
  `marina-*` Secrets/ClusterIssuers/containers, launchd `run.marina.autostart`.
- `marina migrate` deletes the klimax VM (by pointing LIMA_HOME at `~/.klimax` —
  the klimax binary is gone after `brew upgrade`), rewrites the config (default
  VM name, DNS zone), moves the registry cache, removes the launchd agent.
  **It does not move the image disk** (Lima would reformat a renamed disk: it
  checks the ext4 label `lima-<disk name>`) nor the CA (constrained to
  `.klimax.internal`).
- `marina up` refuses to run while an unmigrated `~/.klimax` exists (two VMs on
  one kind CIDR would fight over the host route).
- `/etc/resolver` cleanup also removes files carrying the old `# Managed by klimax` marker.
- The cask installs `klimax` as an alias of `marina` (goreleaser `custom_block`)
  so existing scripts keep working — drop it in a later release.
- Historical notes below that cite a version (e.g. "until v0.1.62") refer to
  klimax releases; the text was renamed wholesale.

Release-time steps outside this repo (not done by code): rename the GitHub repos
(`klimax` → `marina`, `homebrew-klimax` → `homebrew-marina`, `klimax-website`,
`klimax-ui`); add `cask_renames.json` `{"klimax": "marina"}` to the tap so
`brew upgrade` moves users over; add the new website repo to the GCS WIF
binding **before** renaming it; buy `marina.run`; re-export the draw.io diagrams (still labelled klimax).

---

**Logo:** `docs/logo/marina-mark.svg` (mark) and `docs/logo/marina-logo.svg`
(mark + wordmark + tagline) are the sources; every `docs/marina-logo-*.png` is
rendered from them with `rsvg-convert -w <W> -h <H>` at the sizes already in the
repo. The wordmark uses Avenir Next Heavy, which rsvg finds on macOS.

## Workspace rules (CRITICAL)

1. **Referenced projects** — `./marina.code-workspace` lists `../kind-on-lima`, `../lima`, `../colima` as reference folders. **Do NOT read these directories into your main context.** Spawn a sub-agent to search them when needed.
2. **Sub-agents** — Use the `Explore` agent to look up Lima/Colima internals. Extract only what you need and report back.

---

## Design principles

- **Lima Go packages** (`github.com/lima-vm/lima/v2`) — never shell out to `limactl`.
- **`vmType: vz` only** — macOS Virtualization.framework; no QEMU.
- **`up` is infrastructure-only** — it creates/starts the VM, provisions Docker, the kind network, registries, and routing rules. It does **not** create or manage kind clusters.
- **Cluster lifecycle is CLI-only** — `marina cluster create/delete/list`. There is no cluster list in the config file. `marina cluster apply -f <Fleet>` creates a *fleet* declaratively, but the manifest is a **separate, ephemeral input** (like `kubectl apply -f`), never `config.yaml` — the config stays infra-only. `apply` is additive (create-if-absent, skip-if-present).
- **All provisioning is idempotent** — safe to re-run `marina up` at any time.
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

marina does this correctly: `routing.Lima0IP()` SSHs into the VM and reads `ip -o -4 addr show lima0`. No IP is hardcoded anywhere in marina.

**Multiple Lima VMs (limactl, colima, etc.) are safe:** each vzNAT VM gets a distinct IP from macOS on its own `bridge1xx` interface. marina's macOS route always targets the specific IP of the marina VM, so other VMs don't interfere.

Note: the `192.168.105.x` subnet is Lima's **socket_vmnet** range (shared/bridged mode) — entirely different from vzNAT.

### kind network
All kind clusters share a single Docker bridge network named `kind` with a user-specified subnet (default `172.30.0.0/16`). The bridge interface is `br-<id>` inside the VM.

### Pure L3 routing (Strategy 2 — no SNAT)
Docker installs: `POSTROUTING -s <bridge-cidr> ! -o <bridge-if> -j MASQUERADE`

To preserve source IPs in host→cluster replies, marina inserts a nat exemption rule **before** Docker's MASQUERADE:
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
cmd/marina/main.go                   entry point
skill.go                             root package `marina`; go:embed SKILL.md into var SkillMD (shipped by `marina skill install`)
SKILL.md                             canonical Agent Skill (single source of truth; embedded into the binary)

internal/config/config.go            Config struct, LoadConfig, Validate, defaults
internal/limatemplate/template.go    builds limatype.LimaYAML (Ubuntu 26.04 LTS, portForwards, provision script)
internal/vm/vm.go                    Manager: EnsureRunning, Stop, Delete, Inspect
internal/guest/guest.go              SSH Client: Run, RunScript, RunScriptStream, WriteFile, SSHArgs
internal/docker/network.go           EnsureKindNetwork (idempotent, CIDR comparison)
internal/registry/registry.go        EnsureRegistries, RegistryHosts (pull-through mirrors → containerd certs.d hosts.toml + cache volumes)
internal/registry/daemon.go          HubMirrorEndpoint, MergeDaemonConfig — dockerd's Hub-only registry-mirrors (classic image store)
internal/registry/dockerhosts.go     DockerdRegistryHosts, DockerHostsTOML — per-registry mirrors for dockerd via /etc/docker/certs.d
internal/kind/kind.go                CreateCluster, DeleteCluster, ListClusters, DetectUsedNums, NextFreeNum, LabelNodes
internal/kind/query.go               ClustersMatchingSelector (kubectl -l), ClustersByFleet (marina.run/fleet via jq), ClusterInfoFor (nodes/version/ready/labels)
internal/kind/addons.go              InstallMetricsServer (addon installers)
internal/localdns/localdns.go        Ensure/Remove (etcd + CoreDNS containers at x.y.255.52/.53), Corefile, PurgeCluster, ListRecords, ProbeFromHost
internal/localdns/externaldns.go     ExternalDNSManifest / InstallExternalDNS (plain kubectl, not Helm), ClusterForward (CoreDNS stanza)
internal/localdns/resolver.go        EnsureHostResolver / RemoveHostResolvers — /etc/resolver/<domain> on the Mac (marker-guarded, sudo only on change)
internal/config/dns.go               DNSConfig (+ NameTemplate, TLSConfig), DNSEnabled/TLSEnabled/DNSDomain/DNSNameTemplate/DNSServerIP/DNSEtcdIP/ClusterDNSZone, validateDNS
internal/localca/localca.go          Store: EnsureRoot / EnsureCluster (intermediate + wildcard, renew <30d) / RemoveCluster — in-process via github.com/bcollard/homepki/pkg/pki
internal/localca/cluster.go          InstallInCluster (wildcard Secret, root ConfigMap, ClusterIssuer if cert-manager present), CopySecret
internal/localca/trust.go            Trusted / Trust / Untrust — macOS System keychain via `security` under sudo
internal/hostsudo/hostsudo.go        Run — sudo (or `sudo -n` when non-interactive) for the resolver file and keychain trust
internal/fleet/fleet.go              Fleet manifest: types, Parse, Validate (names-only minimal form, dependsOn DAG, cycle detection)
internal/fleet/plan.go               Resolve → Plan (num pre-assignment, defaults merge, existence marking), DeletionOrder
internal/routing/macos.go            EnsureRoute, DeleteRoute, RouteExists, Lima0IP
internal/routing/iptables.go         InstallNoNat (+ Rule 4: local-DNS raw exemption and VM link DNS), CheckNoNatRule, CheckDNSRule

internal/vm/guestagent.go            EnsureGuestAgent — downloads & caches lima-guestagent from GitHub releases
internal/vm/disk.go                  EnsureImageDisk / ResizeImageDisk — the persistent Lima data disk for the container image store
internal/vm/mounts.go                Read/WriteInstanceMounts (yaml-node surgery on the instance config), NormalizeMounts, MountsEqual — vm.mounts reconciliation
internal/config/proxy.go             ProxyConfig, NoProxy/NoProxyString/ProxyEnv — the computed no_proxy list
internal/hostres/hostres.go          Read/ReadFor (host CPU, RAM, free disk), DefaultCPUs + DefaultMemoryBytes (host-scaled
                                     defaults), CheckResources (over-commit warnings). Its own package because
                                     internal/vm imports internal/config, so config cannot import vm.
internal/hostres/hostmem_darwin.go   hostMemoryBytes via sysctl hw.memsize (build-tagged; the package still builds for linux)

internal/cli/root.go                 cobra root command, persistent flags (--config, --debug)
internal/cli/up.go                   `marina up` (alias `start`) — infra only (VM + network + registries + routing)
internal/cli/down.go                 `marina down` (aliases `stop`, `d`) [--remove-route]
internal/cli/destroy.go              `marina destroy`
internal/cli/status.go               `marina status` — collectStatus() → statusReport, rendered as text/json/yaml (host mounts read from the instance config, so they show on a stopped VM)
internal/cli/doctor.go               `marina doctor` — diagnose() → []doctorCheck, `--fix` applies the Fixable ones
internal/cli/fleet_export.go         `marina fleet export` — live clusters → Fleet manifest (args, -l selector, or picker)
internal/cli/version.go              `marina version`
internal/cli/shell.go                `marina shell` — interactive SSH session, or non-interactive command runner (args → remote command, exit code propagated)
internal/cli/copy.go                 `marina copy` — scp between host and VM (`vm:`/`<vmName>:` marks the guest side)
internal/cli/sudoers.go              `marina sudoers` — emits/checks the NOPASSWD rules for the two /sbin/route commands
internal/cli/disk.go                 `marina disk resize` — grows vm.disk + the Lima instance config (applied on next start)
                                     `marina disk resize-image` — grows the vm.imageDisk data disk in place (VM must be stopped)
internal/cli/prune.go                `marina prune` — removes superseded guest agents, orphaned registry caches, (opt-in) Lima download cache
internal/cli/autostart.go            `marina autostart` — launchd agent (run.marina.autostart) running `marina up` at login
internal/cli/config_cmd.go           `marina config edit` — opens config in $VISUAL / $EDITOR
internal/cli/cluster.go              `marina cluster` subcommands (create/delete/list/label/e2e-test-nginx; use+merge deprecated → kubeconfig)
internal/cli/kubeconfig.go           `marina kubeconfig` (path/env/merge/remove/use) — kubeconfig helpers; `use` merges + kubectl use-context
internal/cli/cluster_apply.go        `marina cluster apply -f`/`delete -f` — Fleet manifest: dependsOn DAG scheduler, maxParallel, skip-existing, serialized kubeconfig merge, per-cluster overrides
internal/cli/fleet.go                `marina fleet` subcommands (list/describe/create/delete/label) — fleet membership tracked by the marina.run/fleet node label, not the manifest; describe curates infra labels in text, full set in json/yaml
internal/cli/registry.go             `marina registry clean-cache`
internal/cli/fleetzone.go            fleet zones: checkZoneNames/checkClusterNameFree (fleet ≠ cluster name), installFleetCA, joinFleetZone (adopt), leaveFleetZone (delete: owner-scoped purge; last member removes zone + CA)
internal/cli/ca.go                   `marina ca status|cert|attach|secret|trust|untrust`; reconcileLocalCA (up), localCAForCluster/installLocalCA (create), removeLocalCA (delete/destroy)
internal/cli/dns.go                  `marina dns list|attach`; helpers wired into up/create/delete (withLocalDNSForward, installLocalDNS, deleteCluster)
internal/cli/skill.go                `marina skill install|path` — install the embedded Agent Skill for AI coding tools
internal/cli/completion.go           `marina completion bash|zsh|fish|powershell`
internal/cli/docker_env.go           `marina docker-env` — prints DOCKER_HOST export (current shell only)
internal/cli/docker_context.go       `marina docker-context` — creates/switches Docker context (persistent)
internal/cli/hostagent.go            `marina hostagent` — hidden; Lima spawns this as a detached daemon
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
  name: "marina"         # Lima instance name; socket at ~/.<name>.docker.sock
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
                         # `marina disk resize-image`.
  mounts: []             # host dirs shared into the guest over virtiofs; empty by default.
                         # Each appears at the SAME absolute path in the VM, which is
                         # what makes a host-path `docker -v` bind resolve.
                         #   - location: "~/projects"   ("~" expanded; must exist)
                         #     writable: true           (default false, matching Lima)
                         #     mountPoint: "/srv/x"     (optional; differing guest path
                         #                               breaks `-v <host path>`)
                         # NOT VM-level: `marina up` offers a restart to apply.

network:
  kindBridgeCIDR: "172.30.0.0/16"   # Docker "kind" network subnet
  disablePortMirroring: true         # default true: disable Lima TCP port mirroring; kubeconfigs use VM's lima0 IP directly
                                     # Coexists with other Lima VMs (kind-on-lima, Rancher Desktop) that manage
                                     # kind clusters — prevents API-server port conflicts on 127.0.0.1.
                                     # Set false to force loopback (127.0.0.1) — e.g. host security software
                                     # (CrowdStrike) blocking vzNAT IPs.
                                     # ⚠ VM-level: only takes effect on new VMs (marina destroy && up).
  dns:
    enabled: true                    # default true: local DNS for LoadBalancer Services (see "Local DNS")
    domain: "marina.internal"        # names: <svc>.<ns>.<cluster>.<domain>; must not be a bare TLD or .local
    nameTemplate: "{{.Name}}.{{.Namespace}}"  # ExternalDNS --fqdn-template relative to <cluster>.<domain>; validated by rendering a sample
    tls:
      enabled: true                  # default true: local CA (see "Local CA"); ignored when dns.enabled is false
                                     # NOT VM-level: reconciled on every `marina up`; false removes everything

kind:
  nodeVersion: "v1.36.1"             # kindest/node image tag (default)
  metalLBVersion: "v0.16.1"          # MetalLB manifest version (default)
  customDnsResolvers:                # per-zone upstream resolvers; resolvers default to 8.8.8.8/8.8.4.4 if omitted
    # - domain: "runlocal.dev"       # example; empty by default in code
  autoMergeKubeconfig: true          # merge context into ~/.kube/config after cluster create (default: true)
  autoRemoveKubeconfig: true         # remove context from ~/.kube/config after cluster delete (default: true)

registries:
  cacheStorage: "host"               # "host" (default): ~/.marina/registry-cache/ via virtiofs, survives destroy
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
- `eval $(marina docker-env)` — sets `DOCKER_HOST` in the current shell
- `marina docker-context` — creates/updates a named Docker context (persistent across shells); conflicts with `DOCKER_HOST` if both are set

### Self-contained guest agent

Lima requires a small Linux binary (`lima-guestagent`) uploaded into the VM at startup for port forwarding. `internal/vm/guestagent.go` (`EnsureGuestAgent`) downloads it from Lima's GitHub release on first `marina up` and caches it at `~/.marina/share/lima/lima-guestagent.Linux-<arch>-<limaVer>.gz`. The cache filename is **version-stamped**, so bumping the `lima/v2` module re-downloads the matching guest agent (guest and host agents must be the same version). No separate Lima installation required.

The downloaded version is matched to the Lima Go module version at runtime via `runtime/debug.ReadBuildInfo()`. The release asset uses `uname -m` naming: `Darwin-arm64` / `Darwin-x86_64` (not `Darwin-amd64`).

### lima-version file

Lima writes a `lima-version` file (mode `0o444`) into the instance dir during `instance.Create()` with version `"<unknown>"` when no ldflags are set. marina fixes this in `vm.create()` by calling `os.Remove()` then `os.WriteFile()` with the actual Lima module version read via `runtime/debug.ReadBuildInfo()`. `os.WriteFile` alone silently fails on a read-only file — the Remove step is required.

### hostagent subprocess

Lima's `instance.StartWithPaths()` spawns `os.Executable() hostagent INSTANCE --socket ... --guestagent ...` as a detached daemon. `internal/cli/hostagent.go` implements this hidden subcommand (ports Lima's `cmd/limactl/hostagent.go`). It must configure **logrus JSON formatter** — Lima's event watcher parses JSON log lines to detect VM readiness. `runtime.LockOSThread()` is required when `--run-gui` is set (VZ on macOS).

### Building locally: the binary must be codesigned

`go build` alone produces a binary that **cannot start a VM**. Virtualization.framework
refuses it with a misleading error that says nothing about signing:

```
Error Domain=VZErrorDomain Code=2 "Invalid virtual machine configuration.
The process doesn't have the com.apple.security.virtualization entitlement."
```

`make build` handles this; a bare `go build -o /tmp/marina ./cmd/marina` does not:

```sh
codesign --sign - --entitlements entitlements.plist --force <binary>
```

Read-only commands (`status`, `doctor`, `cluster list`, `fleet export`,
`disk resize-image`) work fine unsigned, so an unsigned build can look healthy
right up until it tries to boot the VM.

### Binary replacement safety

Replacing `/usr/local/bin/marina` while the hostagent is running causes macOS `amfid` to kill subsequent marina execs. `make dev-install` aborts if a hostagent process is detected. `marina doctor` also warns with the kill+cleanup fix command.

---

## Cluster creation flow (`marina cluster create <name>`)

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
9. **Export kubeconfig** → `~/.kube/marina/<name>.kubeconfig`; server set to `https://<lima0IP>:700N` by default (`disablePortMirroring: true`) or `https://127.0.0.1:700N` (when `disablePortMirroring: false`)

### kubeconfig naming

`exportKubeconfig` strips the `kind-` prefix from context/cluster/user names — contexts are stored as bare `<name>` in `~/.kube/config`, not `kind-<name>`. `removeFromKubeconfig` must use the bare name accordingly.

---

## Fleet manifest (`marina cluster apply -f`)

A declarative fleet applied via `marina cluster apply -f <file>`. See `examples/fleet.yaml`.

- **Minimal manifest lists only names** — everything else defaults:
  ```yaml
  apiVersion: marina.run/v1alpha1
  kind: Fleet
  spec:
    clusters: [dev, staging]
  ```
- **`ClusterEntry` unmarshals from a bare string OR an object** (`fleet.ClusterEntry.UnmarshalYAML`) — that's what makes the names-only form work.
- **Per-cluster options**: `dependsOn`, `num`, `nodeVersion`, `region`, `zone`, `registries` (cherry-pick: `mirrors` — `nil`=all, `["*"]`=all, `[]`=none, `[names]`=subset, by config mirror name), `addons.metricsServer` (`enabled`/`version`/`kubeletInsecureTLS`), `labels` (map). `spec.defaults` supplies inherited values (labels are merged, entry wins).
- **Scheduler** (`internal/cli/cluster_apply.go`): builds a dependsOn DAG, creates clusters up to `spec.maxParallel` at a time (default 1 = sequential), gating each on its dependencies via per-cluster `done` channels. `strategy: FailFast` (default) stops scheduling new clusters after the first failure; `ContinueOnError` presses on.
- **Race-safety** (ties into [[project_concurrent_cluster_create]]): all nums are **pre-assigned** in `fleet.Resolve` before any create (honouring explicit nums, filling gaps around live clusters); kubeconfig merges are **serialized** behind a mutex even when creates run in parallel.
- **Additive**: existing clusters are skipped (never recreated/mutated). Mirror-name selections are validated against the config catalog up front.
- **Teardown**: `marina cluster delete -f <file>` deletes the manifest's clusters that exist, in reverse-dependency order (`fleet.DeletionOrder`), prompting unless `--yes`.
- **Node labels** (`kind.applyNodeLabels`, applied post-create via `kubectl label nodes --all --overwrite`, admin creds so no NodeRestriction): every marina cluster always gets `managed-by=marina`; fleets add `marina.run/fleet=<metadata.name>`; `region`/`zone` are surfaced as `topology.kubernetes.io/*` (kubeadm node-labels patch); custom labels come from `-l key=value` (CLI) or `labels:`/`defaults.labels` (Fleet). Validated by `config.ValidateLabels` before any create. Existing clusters can be relabeled with `marina cluster label <name> -l key=value` / `-l key-` (reuses `kind.LabelNodes`).
- **`fleet` command & selectors**: fleet membership is derived from the live `marina.run/fleet` node label (not the manifest), so `marina fleet list/describe/delete/label <name>` operate on whatever clusters currently carry the label.
- **Adoption**: `apply`/`fleet create` only *skip* pre-existing clusters by name — it does not relabel them, so a listed cluster that isn't already a member is **not** silently pulled into the fleet. Instead it warns and lists them; re-run with `--adopt` to relabel them into the fleet (fleet label + the manifest entry's labels, via `adoptIntoFleet` → `kind.LabelNodes`). `marina fleet adopt <fleet> <cluster>…` does the same for arbitrary existing clusters (just sets `marina.run/fleet`). `fleet delete <name>` and `fleet label <name>` resolve members via `kind.ClustersMatchingSelector(g, "marina.run/fleet=<name>")`; `fleet list` groups via `kind.ClustersByFleet`. `cluster list -l` / `cluster delete -l` take an arbitrary kubectl label selector (matched in-guest by kubectl, one call per cluster). `fleet create -f`/`delete -f` delegate to the same code as `cluster apply -f`/`delete -f`. Selectors are charset-validated (`selectorRE`) before shell interpolation.

---

## CLI reference

```
marina up                              Start VM + infra (idempotent; alias: start)
marina down                            Stop VM (no sudo required; aliases: stop, d)
marina down --remove-route             Stop VM and remove macOS host route (requires sudo)
marina destroy                         Delete all clusters, delete VM, remove route
marina status                          Show VM state, host mounts, local DNS, clusters, route, iptables
  -o text|json|yaml                    Output format (json/yaml for tooling; `clusters.names` is always a list)
marina doctor                          Diagnose common issues (VM, route, iptables, IP forwarding, Rosetta host+VM state, local DNS path)
  -o text|json|yaml                    Output format; each check has a stable `id`, `status`, `fixable`
  --fix                                Apply the repairs marina can perform: route, iptables, IP forwarding.
                                       VM creation/start, Rosetta install and hostagent cleanup stay advisory.
marina version                         Print version
marina shell                           Open interactive SSH session in the VM
marina shell <cmd> [args...]           Run a command in the VM (stdin/stdout passed through, exit code propagated)
  -t, --tty                            Force pseudo-terminal allocation
marina copy <src>... <dst>             Copy files host↔VM; prefix the VM side with `vm:` (or the VM's name)
  -r, --recursive                      Copy directories
marina config edit                     Open config in $VISUAL / $EDITOR / nano / vi

marina disk resize <size>              Grow the VM disk (e.g. 80GiB); rewrites vm.disk + instance lima.yaml, applied on next start
marina disk resize-image <size>        Grow the persistent image-cache disk (vm.imageDisk), preserving the cached images.
                                       VM must be stopped: the backing file cannot be resized while attached.
                                       Lima's own boot script grows the partition + ext4 on the next start.
                                       ⚠ vm.imageDisk in the config applies only at disk *creation* — EnsureImageDisk
                                       never resizes an existing disk, so `up` warns when the two drift.
marina prune                           Remove reclaimable caches (superseded guest agents, orphaned registry-cache dirs)
  --dry-run                            Report without removing
  --downloads                          Also clear Lima's shared image download cache (~/Library/Caches/lima/download)
  -y, --yes                            Skip the confirmation prompt (required when non-interactive)
marina sudoers                         Print sudoers rules so `up` never prompts for the host route
  --check                              Inspect `sudo -l` output for both NOPASSWD route rules
marina autostart install               Install + load the launchd agent (--print writes the plist to stdout)
marina autostart uninstall             Unload + remove it
marina autostart status                Report plist presence and launchd state

marina docker-env                      Print: export DOCKER_HOST=unix://~/.<name>.docker.sock
marina docker-env --unset              Print: unset DOCKER_HOST

marina docker-context                  Create/update "marina" Docker context + docker context use <name>
marina docker-context --unset          docker context use default

marina cluster create <name>           Create a kind cluster (num auto-assigned)
  --region europe-west1                Override topology region label
  --zone   europe-west1-b              Override topology zone label
  -l, --label key=value               Extra node label (repeatable)
marina cluster apply -f <file>         Create a fleet from a Fleet manifest (- for stdin)
  --dry-run                            Print the resolved plan (nums, DAG, options) and exit
  --max-parallel N                     Override spec.maxParallel (concurrent creations)
marina cluster delete [name]           Delete a cluster; interactive multi-select picker if no name given
  -f <file>                            Delete the clusters listed in a Fleet manifest (reverse-dependency order)
  -l, --selector <sel>                 Delete clusters whose nodes match a label selector
  -y, --yes                            Skip the confirmation prompt
marina cluster list                    List clusters with num, API port, kubeconfig path
  -o text|json|yaml                    Output format
  -l, --selector <sel>                 Filter by node label selector (e.g. marina.run/fleet=f1)
marina cluster use <name>              DEPRECATED → 'marina kubeconfig env <name>'
marina cluster merge <name>            DEPRECATED → 'marina kubeconfig merge <name>'

marina kubeconfig path <name>          Print the cluster's kubeconfig file path
marina kubeconfig env <name>           Print: export KUBECONFIG=~/.kube/marina/<name>.kubeconfig
marina kubeconfig merge <name>         Merge the cluster's context into ~/.kube/config
marina kubeconfig remove <name>        Remove the cluster's context from ~/.kube/config
marina kubeconfig use <name>           Merge + `kubectl config use-context <name>` (switch active context)
marina cluster label <name>            Label an existing cluster's nodes
  -l, --label key=value               Set/overwrite a node label (repeatable)
  -l, --label key-                    Remove a node label
marina cluster e2e-test-nginx          Deploy nginx, expose, curl — uses current kubectl context on host
  --cleanup                            Only remove nginx pod/svc (does NOT run the test)

marina fleet create -f <file>          Create clusters from a Fleet manifest (alias of 'cluster apply -f'; --dry-run, --max-parallel)
  --adopt                              Adopt pre-existing clusters listed in the manifest into this fleet (relabel them)
marina fleet adopt <fleet> <cluster>…  Adopt existing clusters into a fleet (sets their marina.run/fleet label)
marina fleet list [-o text|json|yaml]  List fleets (grouped by marina.run/fleet) and their member clusters
marina fleet describe <name>           Show a fleet's members with num, API port, kubeconfig, node count/version/readiness, labels ([-o text|json|yaml])
marina fleet export [cluster...]       Write a Fleet manifest for live clusters (reverse of `fleet create -f`)
  -l, --selector <sel>                 Select by node label selector instead of names
  --name <fleet>                       metadata.name (default: shared marina.run/fleet label, else "exported")
  --nums                               Record each cluster's num, pinning API ports on re-apply (default true)
                                       No names and no selector → interactive picker.
                                       Not captured (not recoverable from live state): dependsOn, registries, addons.
marina fleet delete <name>             Delete all clusters in the named fleet (-y to skip prompt)
marina fleet delete -f <file>          Delete the clusters listed in a Fleet manifest
marina fleet label <name> -l key=value Apply node labels to every cluster in the fleet (key- to remove)

marina registry clean-cache            Stop mirror containers + delete cache dirs; run 'marina up' to restart

marina dns list [-o text|json|yaml]    List names published in the local DNS zone (A records only; TXT ownership records hidden)
marina dns attach <cluster>...         Install ExternalDNS + the CoreDNS forward on existing clusters (restarts their CoreDNS)

marina ca status [-o text|json|yaml]   Root CA path/expiry/keychain trust, per-cluster wildcards
marina ca cert                         Print the root CA (PEM)
marina ca attach <cluster>...          Issue/renew the cluster's wildcard + install it; ClusterIssuer if cert-manager exists
marina ca secret <cluster> -n <ns>     Copy default/marina-wildcard-tls into another namespace (--fleet: marina-fleet-wildcard-tls)
marina ca trust | untrust              Add/remove the root's System-keychain trust (sudo)

marina skill install                   Install the embedded Agent Skill into ~/.claude/skills/marina/SKILL.md
  --claude                             Target Claude Code's user skills dir (default true)
  --print                              Write the skill to stdout instead of installing
  --force, -f                          Overwrite an existing installed skill
marina skill path                      Print the Claude Code install path for the skill

marina completion bash|zsh|fish|powershell   Print shell completion script
```

Global flags (all commands): `-c config.yaml`, `--debug`, `--lima-log-level <level>`

**Logging:** marina's own logs use `log/slog`; Lima's library logs use `logrus`. By default marina raises logrus to `error` so only marina logs (and genuine Lima errors) show — the noisy `INFO[…]`/`WARN[…]` Lima lines are hidden. `--debug` surfaces Lima at `info` (and marina at debug); `--lima-log-level trace|debug|info|warn|error|off` overrides explicitly (`off`→panic-only). Set in `root.go` `PersistentPreRunE` (`resolveLimaLogLevel`). The `hostagent` subcommand re-sets logrus to debug/JSON in `initHostagentLogrus`, so quieting the parent never affects VM readiness detection.

---

## Local DNS (`network.dns`)

Every LoadBalancer Service (and Ingress host) resolves as
`<svc>.<ns>.<cluster>.<domain>` from the Mac, the VM and every pod, with no
domain to own. On by default. The real-domain alternative (ExternalDNS + a
cloud provider) is documented on the website and is still the only way to get
publicly trusted certificates.

| Piece | Where | Written by |
|---|---|---|
| etcd `marina-dns-etcd` | kind network, `x.y.255.52` | `localdns.Ensure` in `up` |
| CoreDNS `marina-dns` (etcd plugin) | kind network, `x.y.255.53` | `localdns.Ensure` in `up` |
| ExternalDNS, provider `coredns` | each cluster, ns `external-dns` | `installLocalDNS` after `CreateCluster` |
| CoreDNS forward of the zone | each cluster | `withLocalDNSForward` → `ApplyCoreDNSPatch` |
| raw-table exemption + VM link DNS | no-NAT script (Rule 4) | `routing.InstallNoNat` |
| `/etc/resolver/<domain>` | Mac | `localdns.EnsureHostResolver` in `up` |

Facts that shaped it, all verified on the live VM:

- **Docker drops host traffic to container IPs.** Docker 29 installs a
  per-container `raw PREROUTING -d <ip> ! -i br-… -j DROP`. The `raw` table runs
  before `filter`, so marina's `DOCKER-USER` ACCEPTs never see the packet.
  MetalLB VIPs are unaffected because they are not container IPs. Rule 4 inserts
  `-i lima0 -s <host gw> -d <dns ip>/32 -m comment --comment marina-dns -j ACCEPT`
  at the top; Docker **appends** its own rules, so the insert stays ahead of
  them. Only the DNS server is exempted — etcd takes unauthenticated writes and
  stays unreachable from the Mac.
- **The resolver file never goes stale.** Its nameserver is on the kind network,
  which the host route already follows across lima0 IP changes. So it is
  written once, and `up` skips sudo when it matches. No NOPASSWD rule on purpose:
  a passwordless `tee` on `/etc/resolver` would let anything on the Mac redirect
  any domain. Non-interactive runs use `sudo -n` and warn.
- **The VM resolves the zone via a route-only link domain** (`resolvectl dns/
  domain ~<domain>/default-route false` on the kind bridge), re-applied by the
  no-NAT script because Docker recreates the bridge link on restart.
- **ExternalDNS is applied as plain manifests, not the Helm chart.** The chart
  runs `extraArgs` through `tpl`, which renders `--fqdn-template`'s
  `{{.Name}}` itself — every record lands under an empty name.
- **ExternalDNS v0.22 changed the annotation prefix** to
  `external-dns.kubernetes.io/`, with no fallback; `external-dns.alpha.…` is ignored silently.
  `--combine-fqdn-annotation` keeps the automatic name alongside a custom one.
- **`--service-type-filter=LoadBalancer` is required.** The service source
  publishes every Service type by default, and `--fqdn-template` names them all:
  headless Services resolve to pod IPs (10.x, unroutable from the Mac) and
  host-network pods to node IPs. Shipped without it in v0.2.0; fixed in v0.2.1
  (existing clusters: `marina dns attach`, and `policy: sync` removes the extras).
- **Each cluster owns a disjoint subzone** (`--domain-filter=<cluster>.<domain>`,
  `--txt-owner-id=<cluster>`), which makes `--policy=sync` safe.
- **The etcd plugin serves TTL 300 for ExternalDNS's TTL-0 records, and a fixed
  30s SOA minimum for negative answers.** The Corefile's `cache { success 9984 30;
  denial 9984 5 }` caps both in the replies (authority section included — which
  `rewrite ttl` does not touch). So a Service moved to a new VIP is not cached
  for five minutes, and a name looked up before ExternalDNS's first sync shows
  up ~6s after it is published instead of 30s. No wildcard records.
- **`cluster delete` purges `/skydns/<reversed zone>/`** — ExternalDNS dies with
  its cluster and never cleans up.
- **Clients that skip `/etc/resolver`:** `dig`, Go's pure-Go resolver
  (`PreferGo`/`netgo`), Chrome with a custom Secure DNS provider. Go's default
  resolver on macOS (cgo or not) and Chrome's default setting work.
- `validateDNS` rejects `.local` (mDNS: resolves in some tools and not others)
  and a bare TLD, and requires a /16-ish `kindBridgeCIDR` so `x.y.255.53` is inside it.

The no-NAT script's bridge lookup was fixed alongside: it used to yield the
network id without the `br-` prefix, so Rule 3 (`-o <name>`) silently matched
nothing. The script now removes that dead rule.

## Local CA (`network.dns.tls`)

In-process, via `github.com/bcollard/homepki/pkg/pki` (stdlib-only; no homepki or
openssl binary). ECDSA P-256 everywhere — homepki defaults to RSA-2048.

| Tier | Where | Constraint | Lifetime |
|---|---|---|---|
| Root | `~/.marina/pki/<domain>/root.crt`, key in `private/` (0600/0700) — never leaves the Mac | `.<domain>` (critical) | ~6 years (`pki.CAValidityDays`) |
| Intermediate, per cluster | `clusters/<cluster>/ca.crt` + `chain.crt` | `.<cluster>.<domain>` and `<cluster>.<domain>` | ~6 years |
| Wildcard leaf, per cluster | `clusters/<cluster>/wildcard.crt` (leaf + intermediate) | — | 365 days; `EnsureCluster` re-issues within 30 days |

- **Apple's verifier needs ONE DNS subtree per intermediate.** It requires a name
  to match every permitted subtree, not any (RFC 5280 says any). v0.2.2–0.2.3
  constrained intermediates to both `.zone` and `zone`; the wildcard's bare `zone`
  SAN then failed `.zone` and macOS rejected the whole certificate as a
  name-constraint violation — Safari, Go on darwin, and curl (which defers to
  `SecTrust` without `--cacert`) all failed, while `curl --cacert` (LibreSSL's own
  verifier) passed, which is how it slipped through. Now a single `zone` subtree;
  `macos_test.go` runs `security verify-cert` so it cannot regress. Measured with
  the fix and the root in the keychain: `/usr/bin/curl`, Homebrew curl (`Native:
  Apple SecTrust`), Apple's python3 and Go all verify with no `--cacert`.
- **Constraints are what make keychain trust safe.** Verified: a `github.com` leaf
  signed with a cluster intermediate is rejected by Go (`CANotAuthorizedForThisName`),
  curl (`permitted subtree violation`) and macOS. cert-manager does **not** check
  constraints when signing — enforcement is client-side, which is where it counts.
- **marina never installs cert-manager.** A kubectl-applied cert-manager would
  collide with the Helm install most recipes do. When one is present,
  `InstallInCluster` adds Secret `<cm-ns>/marina-ca` (intermediate chain + key) and
  ClusterIssuer `marina-ca`, retrying while the webhook comes up.
- **In the cluster:** Secret `default/marina-wildcard-tls` (`tls.crt` = leaf +
  intermediate, `ca.crt` = root), ConfigMap `default/marina-root-ca`, and the root in
  each node's trust store (`kind.ConfigureCACerts`, file `marina-local-ca.crt`) so
  containerd trusts registries on zone names.
- **Key material never reaches a log.** `guest.Run` logs commands and `RunScript`
  logs script bodies at debug level, so keys go through `guest.WriteSecretFile`
  (content on stdin, path allowlisted) into `/tmp/marina-ca-<cluster>/`, removed by
  the install script's `trap`.
- **A wildcard covers one label.** `*.<cluster>.<domain>` covers annotated names and
  Ingress hosts, not the default two-label automatic names; `nameTemplate:
  "{{.Name}}-{{.Namespace}}"` flattens them (considered as a default and rejected:
  ambiguous joins, shared 63-char label, and a rename for v0.2.x users).
- **Fleet zones (`<fleet>.<domain>`).** Members' ExternalDNS get a second
  `--domain-filter` for the fleet zone (set at create, `fleet adopt`/`--adopt`,
  and `dns attach`, from the live `marina.run/fleet` label). Automatic names stay in
  the cluster zone; fleet names come only from the hostname annotation. **First
  member to publish a name owns it** (TXT registry; `--txt-owner-id` stays per
  cluster, so `sync` is safe). Verified: deleting the owner hands the name to the
  next claimant within one sync (~30s). Fleet CA: `fleets/<fleet>/` intermediate
  constrained to `.<fleet>.<domain>`, wildcard in every member as
  `default/marina-fleet-wildcard-tls` (+ `marina-fleet-ca` ClusterIssuer). A fleet
  and a cluster may not share a name — both would own `<name>.<domain>`
  (`checkZoneNames`, `checkClusterNameFree`).
- **Cluster delete purges fleet-zone records by owner, not by prefix**
  (`localdns.PurgeOwned`: the TXT registry records naming the cluster as owner,
  plus the records beside them — exact keys, so a deeper name another member owns
  survives). The last member's delete purges the whole zone and the fleet CA.
- **Issuance is serialised** (`localca.storeMu`): a fleet applied with
  `maxParallel > 1` has every member ask for the same fleet intermediate at once.
- `up` creates + trusts the root (`sudo -n` when non-interactive → warning).
  `destroy` keeps the root and removes cluster intermediates. Turning `tls` off
  neither deletes nor untrusts — `marina ca untrust` is explicit.
- **macOS negative cache is ~75s** regardless of the zone's SOA (measured with TTL 5,
  minimum 30). The `denial 5` cap helps the VM and pods only.

## Registry mirrors reach dockerd and the clusters differently

Cluster pulls and `docker pull` take different paths. Both can use every
mirror, but through separate, non-interchangeable config trees.

| Consumer | Mechanism | Written by |
|---|---|---|
| kind nodes (cluster pulls) | `/etc/containerd/certs.d/<host>/hosts.toml` on each node | `kind.configureRegistryMirrors` |
| dockerd, containerd image store | `/etc/docker/certs.d/<host>/hosts.toml` in the VM | `cli.reconcileDockerRegistryHosts` |
| dockerd, classic graphdriver store | `registry-mirrors` in `/etc/docker/daemon.json` | `cli.reconcileDockerDaemonConfig` |

**`certs.d` is a per-registry config directory, not just TLS material.** The
name is historical — Docker used it for `ca.crt`/`client.cert` from 2015 — but
containerd 1.5 added `hosts.toml` to the same layout, and that file is what
declares mirrors. Both daemons now read both kinds of file from their own tree.

**The two trees are separate and neither daemon reads the other's.**
`/etc/containerd/certs.d` is the CRI plugin's (kubelet, so the kind nodes);
`/etc/docker/certs.d` is dockerd's, hardcoded as `registry.CertsDir()` in
moby's `daemon/pkg/registry/config.go`. They share a file format only because
dockerd calls containerd's loader — `hostconfig.ConfigureHosts` in moby's
`daemon/hosts.go`.

> **This was documented backwards until v0.1.62.** An earlier note claimed
> dockerd could not do per-registry mirrors at all and that `certs.d` was "not
> a way around this". That conclusion came from testing
> `/etc/containerd/certs.d` — the wrong tree — and seeing `docker pull` ignore
> it. `/etc/docker/certs.d/quay.io/hosts.toml` works: verified on Docker 29.8.0
> by pulling an uncached gcr.io image and watching the mirror cache grow from
> 0 to 1 MB.

### Both dockerd mechanisms are written, deliberately

`registry-mirrors` is Hub-only and genuinely cannot be otherwise: moby's
`lookupV2Endpoints` (`daemon/pkg/registry/service_v2.go`) consults mirrors only
when the hostname is `docker.io`, returning a single endpoint for anything else.

Which mechanism applies depends on the **image store**:

- **containerd image store** (default since Docker 28) — `daemon.RegistryHosts`
  reads `/etc/docker/certs.d`. Wired in only under `usesSnapshotter`
  (`daemon/daemon.go`). Per-registry mirrors work.
- **classic graphdriver store** — `lookupV2Endpoints`. Hub only.

Since a user can switch stores without marina running again, `marina up` writes
both. When `hosts.toml` supplies more than one host, moby skips the legacy
merge itself, so they never conflict.

`hosts.toml` needs **no Docker restart** — `RegistryHosts` is resolved per pull.
`daemon.json` does, which is why only the latter restarts dockerd.

The `server = "<upstream>"` line matters: containerd tries the `[host.…]`
entries first and falls back to `server`, so a mirror that is down degrades to
a direct pull instead of failing it.

Files carry a `# Managed by marina` first line. Pruning a removed mirror checks
for that marker and deletes only `hosts.toml`, never the directory's other
contents — the same path is where a user drops a registry CA.

### The endpoint must be 127.0.0.1, not the container name

`registry.HubMirrorEndpoint` and `registry.DockerdRegistryHosts` both return
`http://127.0.0.1:<port>`, deliberately not the container names the kind nodes
use. A kind node is a container on
the `kind` network, where Docker's embedded DNS resolves that name; **dockerd
runs in the VM's host namespace, where it does not resolve at all**. Measured:
`curl registry-dockerio:5030` from the VM returns nothing, `127.0.0.1:5030`
returns 200.

Getting this wrong fails silently in the worst way — `docker info` lists the
mirror, and every pull ignores it.

### daemon.json is merged, never overwritten

`registry.MergeDaemonConfig` sets only the `registry-mirrors` key.
`daemon.json` is a file users edit (`insecure-registries`, `log-driver`,
`default-address-pools`), and marina has no business discarding that to set one
field. A file that does not parse is reported and left alone rather than
replaced.

---

## HTTP proxy support

`network.proxy` configures dockerd, the registry mirrors, and (indirectly) the
kind nodes. Most of the chain already exists upstream; marina fills two gaps.

**What Lima and kind already do:**

- Lima reads the Mac's system proxy (`propagateProxyEnv`, on by default) and
  writes it into the guest's `/etc/environment` `#LIMA-START` block.
- marina's SSH commands inherit it: `/etc/pam.d/sshd` has `session required
  pam_env.so` and sshd runs with `UsePAM yes`.
- So `kind create cluster`, run over SSH, sees the proxy — and kind injects
  `ENV HTTP_PROXY/HTTPS_PROXY/NO_PROXY` into every node it creates. The node
  side is free.

**What marina must add:**

1. **A dockerd systemd drop-in** (`30-marina-proxy.conf`). `/etc/environment` is
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

> `10.0.0.0/8` is deliberate. marina allocates `10.<num>.0.0/16` and
> `10.1<num>.0.0/16` per cluster, so enumerating them is impractical. On a
> corporate network that also uses 10/8 those hosts bypass the proxy too, which
> is normally correct — and the alternative breaks every cluster.

`inheritFromHost` (default true) is resolved by reading the values back out of
the guest's `/etc/environment` rather than re-running `scutil --proxy` on the
host: Lima also rewrites loopback proxy addresses to a gateway the guest can
reach, and re-deriving them would lose that.

Reconciled on every `marina up` (`cli.reconcileDockerProxy`), like `vm.mounts`
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
behind an intercepting proxy before any marina provisioning runs.

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

### Why marina's own env block is stripped before inheriting

`network.proxy` is written into a `#MARINA-START`/`#MARINA-END` block in the
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

| marina config | Lima field | Guest | Mechanism |
|---|---|---|---|
| `vm.disk` | `disk` | `/dev/vda1` → `/` | virtio-blk block device, ext4 |
| `vm.imageDisk` | `additionalDisks` | `/dev/vdb1` → `/var/lib/containerd` | virtio-blk block device, ext4 |
| `registries.cacheStorage: host` | `mounts[]` | same absolute path | **virtiofs** |
| `vm.mounts` | `mounts[]` | same absolute path (or `mountPoint`) | **virtiofs** |

So `vm.mounts` is purely additive to `y.Mounts` and cannot interact with the
image-disk machinery. `limatemplate.BuildMounts()` builds the whole list —
registry cache first, then user mounts — and is exported so `marina up` can
compute the desired set without regenerating the rest of the instance YAML.

### Why `vm.mounts` reconciles in place instead of needing `destroy && up`

`vm.imageDisk` and `network.disablePortMirroring` are baked in at instance
creation because they have on-disk or guest-side consequences. Mounts have
neither: Lima reads the list when the VM starts, so a stopped VM plus an edited
`lima.yaml` *is* the whole change. Making people pay a `marina destroy` — which
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
regenerating the file from `Build()` would mean a marina upgrade silently changed
how a live guest is provisioned. `mounts_test.go` pins that the embedded literal
block scalars survive the re-encode byte for byte.

Locations are `~`-expanded in `BuildMounts` rather than left to Lima, so the
value written to the instance config is exactly what drift detection reads back.

> `marina up` refuses a `vm.mounts` location that does not exist
> (`cli.checkMountLocations`). Lima only warns, then hands Virtualization.framework
> a share for a missing path, which surfaces much later as an opaque VM start
> failure.

---

## Registry cache persistence

Mirror registry containers (`registry-dockerio`, `registry-quayio`, `registry-gcrio`, `registry-us-docker-pkgdev`, `registry-us-central1-docker-pkgdev`) are started with `-v <cacheDir>:/var/lib/registry`. The cache dir location depends on `registries.cacheStorage`:

- **`host`** (default): `~/.marina/registry-cache/<name>/` on the macOS host, virtiofs-mounted into the VM at the same absolute path. Survives `marina destroy`. Lima mount is added to the instance at creation time in `limatemplate.Build()`.
- **`guest`**: `/var/lib/marina/registry-cache/<name>/` inside the VM. Persists across `marina down`/`up`, wiped on `marina destroy`.

> Changing `cacheStorage` after instance creation requires `marina destroy && marina up`.

### Growing the image disk — Lima does the guest half

`marina disk resize-image` only grows the **host-side** backing file. Do not add
`growpart`/`resize2fs` to marina's mount unit: Lima's own
`05-lima-disks.sh` already runs both for additional disks, and it runs *before*
marina's `marina-image-disk.service`. Verified end to end — a 10GiB → 30GiB
resize surfaced as a 30GiB ext4 in the guest with no marina-side partition work.
The `resize2fs` in the mount script is belt-and-braces only.

---

## Safety and idempotency

- `marina up` is safe to run repeatedly — every step checks before acting.
- **Host over-commit check** (`warnOverCommittedResources` in `up.go` → `vm.CheckResources`): warns when `vm.cpus` exceeds the Mac's cores, when `vm.memory` exceeds its RAM (or passes 75% of it — a softer, differently-worded warning), or when `vm.disk + vm.imageDisk` exceeds free space. Advisory only: it never blocks, because the disks are sparse and macOS swaps rather than refusing. Host facts that cannot be read are skipped rather than guessed.
- iptables rules are inserted only if not already present (`-C` check before `-I`).
- Registry containers are started only if not already running.
- `kind create cluster` only runs for clusters that don't exist.
- The macOS route is added/refreshed only when missing or pointing at a stale gateway; when it already targets the current lima0 IP, `marina up` skips it entirely and does **not** invoke sudo (so re-running `up` on a live VM never prompts). See `routing.RouteGateway`.
- Route presence is judged by `routing.RouteGateway`, which requires `route -n get` to return the CIDR's own base as the destination. `RouteExists` (used by `status` and `doctor`) delegates to it — a plain `route -n get` succeeds for *any* address via the default route, which previously made both report a missing route as present.
- `marina up` needs root **only** for that route. `marina sudoers` emits NOPASSWD rules for exactly the two `/sbin/route` invocations, which is what makes `marina autostart` viable (launchd cannot answer a password prompt).
- `cluster create` warns (does not block) when `kind.nodeVersion` differs from `config.DefaultKindNodeVersion` — the image the bundled kind CLI is validated against.
- **On first VM creation only**, `marina up` reviews an existing config (`reviewConfigBeforeCreate` in `up.go`): it lists options this marina version adds that the config doesn't set (`config.MissingKeys` — schema diff of the user's file vs the defaulted struct), and if `kind.nodeVersion` drifts from `config.DefaultKindNodeVersion` it **interactively offers to rewrite it** in the config file (`rewriteNodeVersion`, preserves comments/indent). Non-interactive (no TTY): it keeps the pinned value and only warns — never blocks. Skipped entirely when the VM already exists.
- All kubeconfigs are written atomically with `0600` permissions.

---

## Known caveats

- Docker rewrites iptables on restart → handled by `ExecStartPost` drop-in.
- MetalLB (and metrics-server) readiness waits are `180s` — their images pull from quay.io through the mirror on first use (controller must pull+run before speaker's `memberlist` secret exists, then speaker pulls); the mirror caches them so subsequent clusters are fast. (The multi-minute timeouts seen historically were actually the hostname-shadowing mirror-bypass bug — direct, uncached pulls — not a genuinely slow mirror.) `kind create cluster --wait 5m` covers the core node images (preloaded).
- macOS VPN software can conflict with the host route → `marina doctor` warns.
- `podSubnet: 10.1<num>.0.0/16` overlaps with `serviceSubnet: 10.<num+10>.0.0/16` for num≥10. In practice keep num 1–9 per VM.
- The vzNAT subnet is macOS-assigned and not configurable; do not overlap `kindBridgeCIDR` with it (the macOS-assigned range is typically `192.168.64.x` but may vary).
- `DOCKER_HOST` env var overrides the active Docker context — use one mechanism or the other, not both.
- Registry containers run inside the VM; `guest.WriteFile` uses `sudo tee` and `sudo rm -rf` to handle root-owned stale paths from previous failed runs.
- **Mirror names must not be hostnames.** A mirror container joins the shared `kind` Docker network; a name like `quay.io` makes Docker's embedded DNS resolve `quay.io` to the container itself, so the pull-through proxy (whose `remoteurl` is `https://quay.io`) resolves upstream to itself → connection refused → `404 manifest unknown` → containerd silently falls back to slow, unauthenticated **direct** pulls (no cache, docker.io throttling). `config.Validate` now rejects mirror names containing `.` or `:`. Renaming a mirror also requires recreating its container (`docker rm -f` the old one, then `marina up`) since the old-named container keeps its port.
- **The host filesystem is not shared by default.** Only `~/.marina/registry-cache` is, plus whatever `vm.mounts` lists. Because dockerd runs in the guest and resolves bind sources there, `docker run -v <unshared host path>:/x` does not fail — it creates the path in the guest and the container gets an empty directory. `marina up` refuses a `vm.mounts` location that doesn't exist, but it cannot catch a bind for a path nobody listed.
- `marina down` does **not** remove the macOS host route by default (stale route is harmless; `marina up` refreshes it). Use `--remove-route` to remove it explicitly.
- `network.disablePortMirroring` defaults to **true** — kubeconfigs use the VM's `lima0` IP, which is assigned dynamically by macOS and may change on VM restart; re-run `marina kubeconfig merge <name>` after a restart to refresh kubeconfigs. Host-based security software (e.g. CrowdStrike) may block TCP connections to vzNAT IPs — set `disablePortMirroring: false` (loopback/127.0.0.1 mode) in that case.

---

## Keeping docs in sync

When adding or changing a user-facing command, flag, or manifest field, update **all** the doc surfaces — they drift independently:

- `README.md` (user reference) and this `CLAUDE.md`
- `config.example.yaml` and/or `examples/fleet.yaml` where a config/manifest field changed
- `SKILL.md` — the Agent Skill is **`go:embed`-ded into the binary** (shipped by `marina skill install`). It does not update itself; after a build, verify with `marina skill install --print`. It shipped stale once because it was easy to forget.

Verify behavior end-to-end against the live VM before cutting a release (see the live-VM testing caution: throwaway clusters, clean up).

## Release

- `goreleaser` for cross-compilation and GitHub releases
- Triggered by pushing a `vX.Y.Z` tag (`.github/workflows/release.yml`); default bump is a patch on the latest tag.
- **Tags must be annotated** — `git tag -a vX.Y.Z -m "..."`. A lightweight `git tag vX.Y.Z` fails with `fatal: no tag message?` (repo is configured to require annotated tags).
- Homebrew distribution is a **Cask**, not a Formula: `bcollard/homebrew-marina` → `Casks/marina.rb`. goreleaser bumps it automatically on release. Users install/upgrade with `brew upgrade --cask marina` (or `brew reinstall --cask marina`).
- **Ship the website changelog *before* pushing the tag.** `../marina-website` needs a `docs/changelog.html` entry (plus any page whose documented defaults or commands changed) per its own `CLAUDE.md`. Merging that first means the docs are live when the binaries appear, instead of briefly describing a version nobody can install.
- A tag pushed by mistake can be recalled if you are quick: `gh run cancel <id>`, then `git push --delete origin vX.Y.Z && git tag -d vX.Y.Z`. Check `gh release view` and the Cask version first — once goreleaser has published, retagging is no longer clean and the fix is a new patch release.

### Supply chain / SBOM

- **SBOM per release**: `.goreleaser.yaml` `sboms:` runs **syft** (installed by `release.yml` via `anchore/sbom-action`) to attach an SPDX SBOM for each release archive to the GitHub release. An SBOM is pinned to its build, so it's generated once per tag — not regenerated on a schedule.
- **Vuln scanning**: `govulncheck` runs in `ci.yml` on every PR/push, and weekly via `security.yml` (cron) to catch newly-disclosed CVEs against unchanged deps. `dependabot.yml` opens weekly update PRs for gomod + github-actions.
- Scope note: the SBOM covers the marina **binary's** Go deps — not the tools marina provisions inside the VM (Docker, kind, kubectl, MetalLB, node images), which are version-pinned in code instead.
