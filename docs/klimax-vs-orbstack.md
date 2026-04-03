# klimax vs. OrbStack

[OrbStack](https://orbstack.dev) is a fast, polished macOS application that bundles a Docker
engine, a built-in Kubernetes cluster, and Linux VMs into a single GUI tool. It is one of the
most popular Docker Desktop replacements and is worth understanding in detail when choosing a
local Kubernetes setup.

---

## Quick reference

| | klimax | OrbStack |
|---|---|---|
| **Purpose** | Multi-cluster kind lab | Developer Docker / K8s / Linux VM desktop |
| **Kubernetes** | kind (multi-cluster, N nodes) | k3s (single-node, single cluster) |
| **Multi-cluster** | Yes — up to 99 kind clusters | No — one cluster, always |
| **K8s version** | Any `kindest/node` image tag | Fixed — whatever OrbStack ships |
| **VM technology** | Apple Virtualization.framework (vzNAT) | Apple Virtualization.framework |
| **Container runtime** | Docker | Docker (or containerd) |
| **Host→pod routing** | Pure L3 — static macOS route, no SNAT | Custom VZ network stack + event-based port forwarding |
| **LoadBalancer** | MetalLB — routable IPs via L3 | ServiceLB (klipper-lb) + `.orb.local` DNS |
| **Registry mirrors** | Pre-provisioned (docker.io, quay.io, gcr.io) | Manual `daemon.json` config only |
| **Port mirroring** | Optional — disable with `disablePortMirroring` | Always on (event-based, not Lima) |
| **Self-contained binary** | Yes — no extra installs | macOS app bundle |
| **GUI** | CLI only | Native macOS GUI |
| **Licensing** | Open source (MIT) | Commercial — free for personal use, $8/user/month for commercial |
| **Source code** | Public | Closed source |

---

## VM technology

Both klimax and OrbStack use **Apple Virtualization.framework** (`vmType: vz`). Each runs a
separate Linux VM. macOS assigns each VZ VM its own distinct `bridge1xx` interface with a
dynamically allocated IP — there is no IP conflict between klimax and OrbStack VMs.

The key architectural difference: klimax embeds Lima's Go packages directly and drives the VM
lifecycle programmatically. OrbStack implements its own custom VMM on top of VZ, with
Swift/Go/Rust services, a custom VirtioFS caching layer, and deeper integration with the macOS
system (Rosetta, system keychain, etc.).

---

## Kubernetes

### OrbStack

OrbStack ships one built-in Kubernetes cluster. Based on its filesystem layout and behaviour,
it is **k3s** (though OrbStack does not officially document this). Key characteristics:

- **Single-node only** — no worker nodes
- **Single cluster** — [multiple clusters are not planned](https://github.com/orbstack/orbstack/issues/587)
- **Fixed K8s version** — you cannot pin or downgrade
- **CNI: Flannel** (fixed; Cilium, Calico, etc. are unsupported)
- **LoadBalancer: ServiceLB** (k3s's klipper-lb) — creates `svclb-*` DaemonSet pods
- **No MetalLB** — OrbStack's custom networking + `.orb.local` DNS handle service reachability instead

> Note: you can run kind or k3d clusters on top of OrbStack's Docker engine. Those clusters are
> unmanaged by OrbStack and do not get `.orb.local` DNS or OrbStack's ServiceLB integration.
> You would still need MetalLB for LoadBalancer services in those clusters.

### klimax

klimax creates and manages **multiple kind clusters** inside a single Lima VM. Each cluster:

- Uses any `kindest/node` image (pinned via `kind.nodeVersion`)
- Gets a dedicated API-server port (`700N`), service subnet (`10.N.0.0/16`), pod subnet (`10.1N.0.0/16`)
- Gets MetalLB with a dedicated IP address pool (`172.30.N.1–7` and `172.30.N.16–254`)
- Gets full topology labels (`topology.kubernetes.io/region` + `zone`) for scheduler testing
- Gets containerd mirror patches pointing to the shared registry mirror fleet

---

## Host-to-pod networking

### OrbStack's approach

OrbStack uses a **custom virtual network stack** built on Virtualization.framework. Container
and pod IPs are routable directly from the Mac without any extra configuration — no `route add`
command required. DNS entries (`*.orb.local`, `*.k8s.orb.local`) resolve these IPs.

OrbStack also uses event-based port forwarding (similar in spirit to Lima's port mirroring) to
expose listening ports at `localhost` on the Mac. This is transparent to the user.

Because OrbStack owns the full network stack (virtual NIC, bridge, and routing rules), it can
install proper routes without iptables surgery. It is a tightly integrated, proprietary solution.

### klimax's approach

klimax uses **pure L3 routing** via a static macOS route:

```
sudo route -n add -net 172.30.0.0/16 <lima0_IP>
```

No SNAT: an iptables `ACCEPT` rule inserted before Docker's `MASQUERADE` preserves source IPs
in host→cluster replies. This means pod IPs (e.g. `172.30.1.5`) are directly reachable from
the Mac without OrbStack's proprietary network layer.

The iptables rules are idempotent and survived across Docker restarts via a systemd drop-in
(`docker.service.d/99-no-nat-kind.conf`).

The networking goal is identical; the implementation is transparent and fully auditable.

---

## LoadBalancer services

### OrbStack

k3s's **ServiceLB** (klipper-lb) creates a DaemonSet pod on the node for each `LoadBalancer`
service that mirrors traffic from the node's port. OrbStack's network stack then makes this
accessible via `<service>.<namespace>.svc.cluster.local` and `*.k8s.orb.local` DNS from the Mac.

This works well for development. You do not need to know the external IP — the DNS name handles it.
However, ServiceLB assigns the node's IP (not a dedicated IP range), so multiple services on the
same NodePort can conflict.

### klimax

**MetalLB** (L2 mode) assigns dedicated IPs from a CIDR pool (`172.30.N.1–7` and
`172.30.N.16–254`). These IPs are:

- Directly routable from the Mac via the static L3 route
- Stable and non-overlapping across clusters (each cluster gets a sub-range from its num N)
- Not dependent on OrbStack's proprietary DNS layer

This is the correct production-mirroring model: your service gets a real IP, not a node port
redirect, and it is reachable without a DNS abstraction layer.

---

## Registry mirrors

### OrbStack

No built-in pull-through cache. Users add registry mirrors via the Docker daemon config:

```json
// ~/.orbstack/config/docker.json (or via orb config docker)
{
  "registry-mirrors": ["https://your-mirror.example.com"]
}
```

This applies only to Docker pulls. For Kubernetes pod image pulls (which go through containerd),
additional containerd configuration is required and is not exposed through the OrbStack UI.

### klimax

klimax provisions three pull-through mirror containers inside the VM on every `klimax up`:

| Mirror | Port | Remote |
|---|---|---|
| `registry-dockerio` | 5030 | `https://registry-1.docker.io` |
| `registry-quayio` | 5010 | `https://quay.io` |
| `registry-gcrio` | 5020 | `https://gcr.io` |

Mirrors are connected to the `kind` Docker network, so every kind cluster node can resolve them
by hostname. Each cluster's containerd is patched at creation time to use these mirrors. The
mirror cache is persisted on the macOS host (`~/.klimax/registry-cache/`) via virtiofs and
survives `klimax destroy` (configurable via `registries.cacheStorage`).

---

## kubeconfig management

OrbStack merges its context into `~/.kube/config` with the hardcoded context name `orbstack`.
The context name cannot be changed.

klimax writes each cluster's kubeconfig to `~/.kube/klimax/<name>.kubeconfig` and optionally
merges it into `~/.kube/config` with the bare cluster name as context (controlled by
`kind.autoMergeKubeconfig`). Multiple clusters, multiple contexts, no naming conflicts.

---

## Coexisting with OrbStack

klimax and OrbStack coexist without special configuration:

- Each tool manages its own VM on a separate `bridge1xx` interface — no IP conflicts
- klimax's Docker socket is at `~/.<vmName>.docker.sock` (e.g. `~/.klimax.docker.sock`); OrbStack's is at `~/.orbstack/run/docker.sock`; `/var/run/docker.sock` points to whichever was last activated — use `docker context` to switch
- klimax's `172.30.0.0/16` route does not overlap with OrbStack's VM subnet

If you also run Lima-based tools (Colima, Rancher Desktop, kind-on-lima), set
`network.disablePortMirroring: true` in klimax's config to prevent API-server port conflicts
on `127.0.0.1`. This does not affect OrbStack, which does not use Lima port mirroring.

---

## Licensing and pricing

| | klimax | OrbStack |
|---|---|---|
| **License** | MIT (open source) | Proprietary (closed source) |
| **Personal use** | Free | Free |
| **Commercial use** | Free | $8/user/month (or ~$6.40/month billed annually) |
| **Enterprise** | Free | Custom pricing |
| **Source code** | [github.com/bcollard/klimax](https://github.com/bcollard/klimax) | Not available |

---

## When to use OrbStack

- You want a **single, polished GUI** that handles Docker + K8s + Linux VMs in one app
- You only need **one Kubernetes cluster** at a time
- You don't need to test against **specific Kubernetes versions**
- You don't need **MetalLB** or custom IP address pools
- The `.orb.local` DNS model fits your workflow
- You want the **fastest possible** Docker engine on Apple Silicon for non-Kubernetes workloads

## When to use klimax

- You need **multiple concurrent kind clusters** (multi-team, multi-env, chaos testing)
- You need **specific Kubernetes versions** pinned per cluster
- You need **MetalLB** with real, routable LoadBalancer IPs
- You need **topology labels** (`region`/`zone`) for scheduler and affinity testing
- You need **pre-provisioned pull-through registry mirrors** with persistent cache
- You want **pure L3 routing** with no proprietary network layer between host and pods
- You want an **open-source, auditable** tool with no per-seat cost
- You are already running OrbStack (or any other tool) and want to add kind clusters without conflicts
