# klimax vs. colima — Lima integration analysis

Comparative analysis of how klimax and colima leverage Lima, covering package reuse,
Lima capabilities, and what each project builds on top.

---

## The fundamental difference

**Colima does not use Lima Go packages at all.** It shells out to `limactl` for
everything — `limactl start`, `limactl stop`, `limactl delete`, `limactl list --json`,
`limactl shell <profile> <cmd>`. Lima is a runtime binary dependency, not a library.

**Klimax embeds Lima as a Go module** and calls its internal APIs directly —
`instance.Start()`, `instance.StopGracefully()`, `instance.Delete()`,
`store.Inspect()`. No `limactl` binary needed at runtime.

---

## Package-level comparison

| Layer | Colima | Klimax |
|---|---|---|
| Lima dependency type | External binary (`limactl`) | Go module (`lima/v2/pkg/instance`, `pkg/store`, `pkg/limatype`, `pkg/ptr`) |
| VM create | `limactl start <profile>.yaml` | `instance.Create(ctx, name, yamlBytes, false)` |
| VM start | `limactl start <profile>` | `instance.Start(ctx, inst, false, true)` |
| VM stop | `limactl stop <profile>` | `instance.StopGracefully(ctx, inst, false)` |
| VM delete | `limactl delete --force <profile>` | `instance.Delete(ctx, inst, true)` |
| VM status | `limactl list <profile> --json` → parse | `store.Inspect(ctx, name)` → `limatype.Instance` |
| SSH/guest exec | `limactl shell <profile> <cmd>` | Custom SSH client using Lima's key from `$LIMA_HOME/_config/user` |
| Lima YAML | Built as a struct, serialised to temp file | Built as `*limatype.LimaYAML`, marshalled to bytes |

**Consequence:** Klimax ships as a single self-contained binary. Colima requires Lima
(and optionally `socket_vmnet`) installed on the host as separate tools.

---

## Network modes

| Mode | Colima | Klimax |
|---|---|---|
| vzNAT | Yes | Yes (only mode) |
| socket_vmnet shared | Yes | No |
| socket_vmnet bridged | Yes | No |
| user-v2 (always on in Colima) | Yes | No |
| Host-reachable VM IP | Via Colima daemon + vmnet | Via vzNAT (macOS-assigned), detected at runtime |
| Host→pod routing | SNAT (standard) | No SNAT — custom iptables nat exemption + macOS route |
| Port range forwarding | 1–65535 (TCP/UDP) | Docker socket + cluster API ports (700N) |

Colima runs a **host-side daemon** (`go-daemon` fork) to manage `socket_vmnet` for
bridged/shared networking. Klimax has no daemon — it's a pure CLI that applies
iptables rules in the guest and a `route add` on the host.

---

## Provisioning and container runtimes

| Capability | Colima | Klimax |
|---|---|---|
| Docker | Yes | Yes |
| containerd | Yes | No |
| Incus | Yes | No |
| Kubernetes | k3s (built-in, single-node) | kind (multi-cluster, each full cluster) |
| kind clusters | No | Yes — create/delete/list, auto-assigned num |
| Provision mechanism | Lima YAML `provision` blocks | Lima YAML `provision` blocks (same pattern) |
| Guest exec for setup | `limactl shell` | SSH client (own implementation) |

---

## What klimax adds on top of Lima that colima doesn't do

| Feature | Detail |
|---|---|
| Multi-cluster kind lifecycle | `cluster create/delete/list/use/merge` — up to 99 clusters per VM |
| Auto cluster numbering | Detects live port bindings to find lowest free slot |
| Per-cluster subnet allocation | `serviceSubnet 10.N.0.0/16`, `podSubnet 10.1N.0.0/16` |
| MetalLB with IP pool | Installed in every cluster; pool derived from `kindBridgeCIDR` + num |
| Pull-through registry mirrors | docker.io, quay.io, gcr.io running as containers in the VM |
| Local push registry | `kind-registry:5000` connected to the `kind` Docker network |
| Containerd mirror patches | Generated per-cluster into the kind config YAML |
| Pure L3 routing | iptables nat exemption + systemd oneshot + docker.service.d drop-in |
| macOS route management | `sudo route add/delete` for `kindBridgeCIDR` → VM IP |
| CoreDNS custom domains | Configurable zones forwarded to 8.8.8.8 at cluster creation |
| Kubeconfig export | Written to `~/.kube/klimax/<name>.kubeconfig`; server points to `127.0.0.1:700N` via Lima port forwarding |
| Kubeconfig auto-merge | `autoMergeKubeconfig` / `autoRemoveKubeconfig` config flags; `cluster merge` for manual merge |
| Topology labels | `topology.kubernetes.io/region/zone` on kind nodes |
| Persistent registry cache | Mirror blobs bind-mounted from `~/.klimax/registry-cache/` (host, survives destroy) or inside VM (`guest` mode) |
| Interactive cluster picker | `klimax cluster delete` — raw-terminal multi-select TUI with arrow keys, space toggle, `a`=all |
| E2E smoke test | `klimax cluster e2e-test-nginx [--cleanup]` — deploys nginx, exposes service, curls it using host kubectl |
| Docker context | `klimax docker-context` creates/updates a named Docker context; `docker-env` for per-shell env var |
| SSH shell | `klimax shell` — interactive SSH session into the VM |
| Config editor | `klimax config edit` — opens config in `$VISUAL` / `$EDITOR` |
| Shell completion | `klimax completion bash|zsh|fish|powershell` |
| Registry cache cleanup | `klimax registry clean-cache` — stops mirror containers and removes cache dirs |

---

## What colima does that klimax doesn't

| Feature | Detail |
|---|---|
| Multiple network modes | vzNAT, socket_vmnet shared/bridged |
| Multiple VM types | vz, qemu (x86 on ARM via Rosetta), krunkit |
| Multiple container runtimes | Docker, containerd, Incus |
| k3s Kubernetes | Single-node, simpler than kind |
| Full port forwarding | All ports 1–65535 forwarded from guest |
| Disk resize | Online disk expansion |
| Multiple profiles | `colima start myprofile` — multiple isolated VMs |
| Mount management | Configurable host→guest mounts with inotify sync |
| Rosetta 2 | ARM64 VMs can run x86 binaries |
| Nested virtualisation | For running VMs inside the Lima VM |

---

## Design philosophy divergence

Colima is a **general-purpose local container runtime** — it replaces Docker Desktop
for any workload. It abstracts Lima completely and exposes its own opinionated config
surface.

Klimax is a **multi-cluster Kubernetes lab** — it takes full ownership of the
networking layer (pure L3, no SNAT) and kind cluster lifecycle, accepting that the
trade-off is macOS+vzNAT only, Docker only, kind only. The deeper Lima Go API
integration is a deliberate choice: it avoids the `limactl` binary dependency and
gives direct access to `limatype.LimaYAML` for programmatic config.
