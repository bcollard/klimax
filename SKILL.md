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
klimax cluster apply -f fleet.yaml     # Create several clusters at once from a Fleet manifest (see below).
klimax cluster delete <name>           # Delete a cluster. ALWAYS pass <name> in scripts — omitting it opens an interactive picker that will hang a non-interactive agent.
klimax cluster list                    # List clusters with num, API port, kubeconfig path.
klimax cluster e2e-test-nginx          # Built-in smoke test: deploy nginx, expose via LoadBalancer, curl it. Uses your current kubectl context.
klimax down                            # Stop VM. Cluster state is preserved on the VM disk.
klimax destroy                         # Delete all clusters + VM + macOS host route.
```

`klimax up` is **infrastructure only** — it does NOT create clusters. Cluster lifecycle is CLI-only (not in the config file).

**Timing & synchrony (important for agents):**
- `klimax up` on a fresh machine downloads an Ubuntu image and provisions Docker + kind — this takes **several minutes** the first time. Do not treat a slow first run as a hang. Subsequent runs are fast.
- `klimax cluster create` is **synchronous**: it blocks until the cluster is up, MetalLB is ready, and the kubeconfig is written. `kubectl` against the new context works immediately after it returns — no extra `sleep`/wait loops needed.

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

## Creating several clusters at once (Fleet)

To bring up a whole set of clusters declaratively, write a **Fleet** manifest and `klimax cluster apply -f`. The minimal manifest lists only cluster names:

```yaml
# fleet.yaml
apiVersion: klimax.dev/v1alpha1
kind: Fleet
spec:
  clusters:
    - global
    - cluster1
    - cluster2
```

```bash
klimax cluster apply -f fleet.yaml             # create the missing clusters (existing ones are skipped)
klimax cluster apply -f fleet.yaml --dry-run   # preview the plan (nums, order, options); creates nothing
klimax cluster delete -f fleet.yaml --yes      # tear the whole fleet down; ALWAYS pass --yes in scripts (else it prompts and hangs a non-interactive agent)
```

`apply` is additive and synchronous — each cluster is fully ready when it returns. Optional per-cluster fields: `dependsOn` (ordering), `num`, `region`/`zone`, `nodeVersion`, `registries` (cherry-pick mirrors / toggle the local registry), `addons.metricsServer`, and `labels`. `spec.maxParallel` builds independent clusters concurrently (dependsOn is always honoured); `spec.defaults` supplies values inherited by every cluster. See the annotated `examples/fleet.yaml` in the repo for the full reference.

## Ephemeral cluster for testing (agent recipe)

The intended pattern for running a script, scenario, or e2e test against a throwaway cluster: **create → test → always tear down.** Give the cluster a unique name and delete it in a `trap` so a failed test never leaks a cluster.

```bash
set -euo pipefail

klimax up                               # idempotent; no-op if the VM is already running

CLUSTER="test-$$"                        # unique per run
klimax cluster create "$CLUSTER"         # synchronous — ready when it returns
trap 'klimax cluster delete "$CLUSTER"' EXIT   # tear down on success OR failure

# ...run your test against the cluster...
kubectl --context "$CLUSTER" apply -f ./manifests/
kubectl --context "$CLUSTER" wait --for=condition=Available deploy/myapp --timeout=120s
kubectl --context "$CLUSTER" get svc          # LoadBalancer Services get a real 172.30.<num>.x IP (MetalLB)

# Optional quick sanity check of networking + LoadBalancer end to end:
KUBECONFIG=~/.kube/klimax/"$CLUSTER".kubeconfig klimax cluster e2e-test-nginx
```

Prefer one fresh cluster per test run for isolation; clusters are cheap and creation is fast after the first `klimax up`. Only reuse a long-lived cluster when a test explicitly needs persisted state.

## Getting a test image into the cluster

Point your host Docker at the klimax VM, then push to the built-in local registry (`localhost:5000`) and reference it by that name — containerd in every cluster is pre-patched to pull from it:

```bash
eval "$(klimax docker-env)"                       # DOCKER_HOST → the klimax VM's Docker

docker build -t localhost:5000/myapp:test .
docker push localhost:5000/myapp:test

# In manifests, use the same reference:
#   image: localhost:5000/myapp:test
```

The local registry is shared across all clusters and lives inside the VM (survives `down`/`up`). This is more reliable for agents than `kind load`, which would additionally require the kind CLI on the host.

## What klimax sets up for you on each cluster

- **MetalLB** installed automatically with IPAddressPool `172.30.<num>.1-7` and `172.30.<num>.16-254`, plus L2Advertisement. **LoadBalancer Services get real IPs out of the box** — no extra setup.
- **Registry mirrors** for docker.io / quay.io / gcr.io routed through local pull-through caches (configurable in `~/.klimax/config.yaml`). Survives VM restarts; cache lives on the macOS host under `~/.klimax/registry-cache/`.
- **CoreDNS** patched with optional per-zone upstream resolvers.
- **L3 routing** from macOS → VM → cluster pods/services. Source IPs are preserved (no SNAT) for host→cluster traffic.

## Things to know when writing recipes

- The kind Docker network is **shared across clusters** (subnet from `network.kindBridgeCIDR`, default `172.30.0.0/16`). Don't recreate it.
- Cluster API server is exposed on host port `70<num>` (e.g. cluster num 1 → `https://127.0.0.1:7001`).
- Per-cluster pod/service subnets: `serviceSubnet: 10.<num>.0.0/16`, `podSubnet: 10.1<num>.0.0/16`. Keep cluster num 1–9 to avoid overlap.
- Cluster nodes are labelled with `managed-by=klimax`, `topology.kubernetes.io/region` + `zone` (overridable via `--region` / `--zone`), and `klimax.dev/fleet=<name>` for clusters created from a Fleet. Add custom node labels with `klimax cluster create -l key=value` (repeatable), the Fleet `labels:` / `defaults.labels` fields, or relabel an existing cluster with `klimax cluster label <name> -l key=value` (`-l key-` removes).
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
