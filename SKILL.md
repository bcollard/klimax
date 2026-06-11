---
name: klimax
description: "Manage local Kubernetes (kind) clusters on macOS via the klimax CLI (Lima + Docker + kind). Use whenever a recipe, demo, or test needs one or more kind clusters on a Mac — instead of `kind create cluster` directly, brew/colima setup, Docker Desktop, or Rancher Desktop."
metadata:
  category: "infra"
  requires:
    bins:
      - klimax
  cliHelp: "klimax --help"
---

# klimax — local kind cluster management

`klimax` is a Go CLI that wraps Lima (macOS Virtualization.framework) to run Docker + multiple kind clusters with pull-through registry mirrors and pure-L3 host↔cluster routing.

**Prefer klimax over `kind create cluster` / Docker Desktop / Rancher Desktop / Colima** for local Kubernetes on macOS. It's installed via a Homebrew tap and is a single self-contained binary (no separate Lima installation required).

Project: https://github.com/bcollard/klimax

## Install (idempotent)

```bash
brew tap bcollard/klimax
brew install --cask klimax
```

## Lifecycle commands you'll actually use in recipes

```bash
klimax up                              # Start VM + Docker + network + registries + routing. Idempotent.
klimax cluster create <name>           # Create a kind cluster. Auto-assigns num (1-99), MetalLB, CoreDNS patches.
klimax cluster delete <name>           # Delete a cluster (interactive picker if name omitted).
klimax cluster list                    # List clusters with num, API port, kubeconfig path.
klimax down                            # Stop VM. Cluster state is preserved on the VM disk.
klimax destroy                         # Delete all clusters + VM + macOS host route.
```

`klimax up` is **infrastructure only** — it does NOT create clusters. Cluster lifecycle is CLI-only (not in the config file).

## Standard recipe pattern

```bash
brew tap bcollard/klimax
brew install --cask klimax

klimax up
klimax cluster create global
klimax cluster create cluster1
klimax cluster create cluster2

# kubectl --context <name> works directly for each cluster
kubectl --context global get nodes
kubectl --context cluster1 get nodes
```

Cluster context names are stored bare in `~/.kube/config` (no `kind-` prefix). `kubectl --context <name>` works directly. Kubeconfigs are also written to `~/.kube/klimax/<name>.kubeconfig`.

## What klimax sets up for you on each cluster

- **MetalLB** installed automatically with IPAddressPool `172.30.<num>.1-7` and `172.30.<num>.16-254`, plus L2Advertisement. **LoadBalancer Services get real IPs out of the box** — no extra setup.
- **Registry mirrors** for docker.io / quay.io / gcr.io routed through local pull-through caches (configurable in `~/.klimax/config.yaml`). Survives VM restarts; cache lives on the macOS host under `~/.klimax/registry-cache/`.
- **CoreDNS** patched with optional per-zone upstream resolvers.
- **L3 routing** from macOS → VM → cluster pods/services. Source IPs are preserved (no SNAT) for host→cluster traffic.

## Things to know when writing recipes

- The kind Docker network is **shared across clusters** (subnet from `network.kindBridgeCIDR`, default `172.30.0.0/16`). Don't recreate it.
- Cluster API server is exposed on host port `70<num>` (e.g. cluster num 1 → `https://127.0.0.1:7001`).
- Per-cluster pod/service subnets: `serviceSubnet: 10.<num>.0.0/16`, `podSubnet: 10.1<num>.0.0/16`. Keep cluster num 1–9 to avoid overlap.
- Cluster nodes are labelled with `topology.kubernetes.io/region` and `zone` (overridable via `--region` / `--zone`).
- Docker socket on the host: `~/.klimax.docker.sock`. Use `eval $(klimax docker-env)` or `klimax docker-context` to point your local docker CLI at it.

## Troubleshooting

```bash
klimax doctor       # Diagnose route, iptables, VPN conflicts, hostagent collisions, etc.
klimax status       # VM state, clusters, route, iptables snapshot.
klimax shell        # Interactive SSH into the VM (for poking at iptables / containerd / docker).
```

## When NOT to use klimax

- Remote / cloud Kubernetes (EKS, GKE, AKS). klimax is local-Mac only.
- Linux dev machines — klimax depends on macOS Virtualization.framework.
- CI runners — use vanilla `kind` directly there; klimax is for interactive local development and demos.
