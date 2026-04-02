# klimax vs. other Lima-based tools

How klimax compares to Rancher Desktop, Colima, and kind-on-lima — the three most common
Lima-based solutions for running containers and Kubernetes on macOS.

---

## Quick reference

| | klimax | Rancher Desktop | Colima | kind-on-lima |
|---|---|---|---|---|
| **Purpose** | Multi-cluster kind lab | Docker / k3s desktop replacement | Docker / containerd runtime | Single-node kind over Lima (shell scripts) |
| **Lima integration** | Go module (no `limactl` binary) | `limactl` binary | `limactl` binary | `limactl` binary |
| **VM type** | vzNAT only | vzNAT or socket_vmnet | vzNAT, socket_vmnet, QEMU | vzNAT or socket_vmnet |
| **Container runtime** | Docker | dockerd or containerd (nerdctl) | Docker or containerd | Docker |
| **Kubernetes** | kind (multi-cluster) | k3s (single-node) or no k8s | k3s (optional) | kind (single-cluster) |
| **Host→pod routing** | Pure L3 — no SNAT | Port-mapped (no direct IP) | Port-mapped (no direct IP) | Static route (no SNAT, same as klimax) |
| **MetalLB** | Yes — every cluster | No | No | Optional (manual) |
| **Registry mirrors** | docker.io, quay.io, gcr.io (pull-through) | Mirrors configurable | No built-in mirrors | No |
| **Port mirroring to host** | Optional — can disable via `disablePortMirroring` | Always on | Always on | Always on |
| **Coexists with other Lima VMs** | Yes (with `disablePortMirroring: true`) | Yes (separate VM) | Yes (separate VM) | Yes (separate VM) |
| **Self-contained binary** | Yes — no Lima install required | Ships Lima internally | Requires `limactl` installed | Requires `limactl` + scripts |

---

## Rancher Desktop

[Rancher Desktop](https://rancherdesktop.io/) is a full Docker Desktop replacement for macOS.
It bundles Lima, `socket_vmnet`, and either `dockerd` or `containerd` (via `nerdctl`) into a
GUI application.

### How it uses Lima

Rancher Desktop manages a single Lima VM named `rancher-desktop`. It uses Lima's standard
port mirroring: every TCP port that a process inside the VM listens on is automatically
forwarded to `127.0.0.1` on the host by the Lima hostagent.

This is fine in isolation, but becomes a problem when running alongside any other Lima VM
that manages kind clusters — both hostagents race to mirror the same API-server ports
(e.g. `7001`) to `127.0.0.1`, causing connection failures.

### Running klimax alongside Rancher Desktop

Set `network.disablePortMirroring: true` in klimax's config. This disables all Lima TCP port
mirroring for the klimax VM (including the kind API-server ports), so Rancher Desktop's
hostagent has no ports to conflict with.

Kubeconfigs for klimax clusters will use the VM's direct `lima0` IP instead of `127.0.0.1`.
The `lima0` IP is L2-reachable from the host via vzNAT without any extra routing or VPN.

```
# ~/.klimax/config.yaml
network:
  disablePortMirroring: true  # prevent port conflicts with Rancher Desktop
```

Then recreate the VM:
```
klimax destroy
klimax up
```

### What klimax offers that Rancher Desktop doesn't

- **Multiple kind clusters** — Rancher Desktop ships k3s (one single-node cluster). klimax
  gives you up to 99 independent kind clusters, each with a dedicated subnet, API port, and
  MetalLB pool.
- **Real LoadBalancer IPs** — MetalLB assigns routable IPs on your host Mac. Rancher Desktop
  has no concept of LoadBalancer services reachable from the host.
- **Pull-through registry cache** — docker.io, quay.io, gcr.io pulls are served from local
  containers; the cache survives `klimax destroy`.
- **Pure L3 routing** — direct IP access to every pod and service IP, no port-mapping, no NAT.

---

## Colima

[Colima](https://github.com/abiosoft/colima) is a general-purpose Docker Desktop replacement
that wraps Lima. It is the most popular Lima-based Docker runtime on macOS.

### How it uses Lima

Colima manages Lima purely by shelling out to the `limactl` binary — `limactl start`,
`limactl stop`, `limactl shell`, etc. It does not use Lima's Go packages directly. As a
result, Colima requires Lima installed separately on the host.

Like Rancher Desktop, Colima uses Lima's default TCP port mirroring: all guest TCP ports are
forwarded to `127.0.0.1` on the host.

### Running klimax alongside Colima

Same fix as with Rancher Desktop: set `network.disablePortMirroring: true` in klimax's config.
Each tool gets its own Lima VM (Colima's is named `colima` by default, klimax's is `klimax`),
each on its own vzNAT bridge and IP — no IP conflicts. With port mirroring disabled in the
klimax VM, there are no port conflicts on `127.0.0.1` either.

### Key architectural difference

Colima is a **general-purpose container runtime** designed to replace Docker Desktop for any
workload — it supports Docker, containerd, multiple network modes (vzNAT, socket_vmnet,
QEMU), and optional k3s.

Klimax is a **multi-cluster Kubernetes lab** — it accepts the constraint of macOS+vzNAT+Docker
in exchange for multi-cluster kind lifecycle, MetalLB, registry mirrors, and pure L3 routing
that preserves real source IPs end-to-end.

See [klimax-vs-colima.md](klimax-vs-colima.md) for a deeper technical comparison.

---

## kind-on-lima

[kind-on-lima](https://github.com/bcollard/kind-on-lima) is the shell-script predecessor to
klimax. It implements the same core idea — Lima VM + Docker + kind + L3 routing + MetalLB —
but as a collection of bash scripts rather than a compiled Go binary.

### How it uses Lima

kind-on-lima drives Lima through `limactl`. It implements the same vzNAT networking strategy
as klimax: a static macOS route for the kind bridge CIDR, and iptables no-SNAT rules in the
guest. The registry mirror containers and MetalLB setup are also direct inspirations for
klimax's implementation.

### Running klimax alongside kind-on-lima

This is the primary coexistence scenario `disablePortMirroring` was designed for. Both tools
manage kind clusters with the same API-server port scheme (`7001`, `7002`, …). Without
`disablePortMirroring: true`, Lima would forward both VMs' ports to `127.0.0.1` and the
kubeconfigs would point at the wrong cluster.

With `disablePortMirroring: true`:
- klimax clusters use `https://<lima0IP>:700N` in kubeconfigs (direct VM IP)
- kind-on-lima clusters use `https://127.0.0.1:700N` (Lima port-mirrored)
- No overlap, no conflicts

### What klimax improves over kind-on-lima

| Aspect | kind-on-lima | klimax |
|---|---|---|
| Implementation | Bash scripts | Single Go binary |
| Lima integration | `limactl` binary | Go module (no `limactl` required) |
| Multi-cluster | Manual, one-at-a-time | `cluster create/delete/list` with auto-assigned nums |
| Cluster numbering | Manual | Auto-detected from live port bindings |
| Registry mirrors | Manual container setup | Automated via `EnsureRegistries`, idempotent |
| Kubeconfig | Manual export | Auto-export + auto-merge into `~/.kube/config` |
| Config | Ad-hoc env vars | Structured YAML with defaults and validation |
| Idempotency | Partial | Full — `klimax up` is safe to re-run at any time |
| Port conflict avoidance | None | `disablePortMirroring: true` |

---

## What about Docker Desktop and Podman Desktop?

**Docker Desktop** does not use Lima. It runs a proprietary Linux VM using Apple
Virtualization.framework directly, and uses its own port-forwarding mechanism. It cannot
coexist conflicts with Lima VMs in terms of the vzNAT bridge network, but its own port
mappings (for Kubernetes) can still overlap with Lima-forwarded ports on `127.0.0.1`.

**Podman Desktop** uses `podman machine`, which on macOS Apple Silicon runs an `applehv`
VM (also using Apple Virtualization.framework) — not Lima. Podman's kind support requires
additional setup; it does not provide the MetalLB + L3 routing stack klimax has.

Neither integrates with Lima, so `disablePortMirroring` is irrelevant for them. However,
any tool that maps ports to `127.0.0.1` on the host could still conflict with Lima's default
port mirroring — `disablePortMirroring: true` eliminates this risk on the klimax side
regardless of what else is running on the host.

---

## Summary: why klimax coexists where others don't

Lima's default TCP port mirroring is a feature, not a bug — it makes guest services
accessible from the host without any extra configuration. But it becomes a liability when
multiple VMs independently manage services on the same port numbers.

Klimax solves this with a single config flag. When `disablePortMirroring: true`:

1. Lima's hostagent stops mirroring any TCP port from the klimax VM to `127.0.0.1`.
2. Kubeconfigs use the VM's `lima0` IP directly — stable, routable via vzNAT, no port-mapping.
3. API server certs include the `lima0` IP as a SAN — TLS verification works out of the box.
4. Every other Lima VM (Rancher Desktop, Colima, kind-on-lima) continues to mirror its own
   ports to `127.0.0.1` unaffected.

The result: klimax and any other Lima-based tool can run simultaneously on the same Mac,
each managing its own clusters, with no port conflicts and no manual intervention.
