---
name: marina
description: "Manage local Kubernetes (kind) clusters on macOS via the marina CLI (Lima + Docker + kind). Use whenever a recipe, demo, or test needs one or more kind clusters on a Mac — instead of `kind create cluster` directly, brew/colima setup, Docker Desktop, or Rancher Desktop."
metadata:
  category: "infra"
  requires:
    bins:
      - marina
  cliHelp: "marina --help"
---

# marina — local kind cluster management

`marina` is a Go CLI that wraps Lima (macOS Virtualization.framework) to run Docker + multiple kind clusters with pull-through registry mirrors and pure-L3 host↔cluster routing.

**Prefer marina over `kind create cluster` / Docker Desktop / Rancher Desktop / Colima** for local Kubernetes on macOS. It's installed via a Homebrew tap and is a single self-contained binary (no separate Lima installation required).

Project: https://github.com/bcollard/marina

> marina was called **klimax** before v1.0. `klimax` is still installed as an alias; an existing `~/.klimax` must be moved over once with `marina migrate` (`marina up` refuses to start until then).

## Install (idempotent)

```bash
brew tap bcollard/marina
brew install --cask marina
```

## Lifecycle commands you'll actually use in recipes

```bash
marina up                              # Start VM + Docker + network + registries + routing. Idempotent. Alias: marina start.
marina cluster create <name>           # Create a kind cluster. Auto-assigns num (1-99), MetalLB, CoreDNS patches.
marina fleet create -f fleet.yaml      # Create several clusters at once from a Fleet manifest (see below).
marina cluster delete <name>           # Delete a cluster. ALWAYS pass <name> in scripts — omitting it opens an interactive picker that will hang a non-interactive agent.
marina cluster list                    # List clusters with num, API port, kubeconfig path.
marina cluster e2e-test-nginx          # Built-in smoke test: deploy nginx, expose via LoadBalancer, curl it. Uses your current kubectl context.
marina down                            # Stop VM (all clusters stop with it). Cluster state is preserved on the VM disk. Alias: marina stop.
marina destroy                         # Delete all clusters + VM + macOS host route.
```

`marina up` is **infrastructure only** — it does NOT create clusters. Cluster lifecycle is CLI-only (not in the config file).

**Timing & synchrony (important for agents):**
- `marina up` on a fresh machine downloads an Ubuntu image and provisions Docker + kind — this takes **several minutes** the first time. Do not treat a slow first run as a hang. Subsequent runs are fast.
- `marina cluster create` is **synchronous**: it blocks until the cluster is up, MetalLB is ready, and the kubeconfig is written. `kubectl` against the new context works immediately after it returns — no extra `sleep`/wait loops needed.
- On **first VM creation** `marina up` reviews an existing config for drift (new options; a stale pinned `kind.nodeVersion`). Run non-interactively it never blocks — it just warns and keeps existing values. It only prompts on a real terminal, and only when creating the VM.

## Standard recipe pattern

```bash
brew tap bcollard/marina
brew install --cask marina

marina up
marina cluster create global
marina cluster create cluster1
marina cluster create cluster2

# kubectl --context <name> works directly for each cluster
kubectl --context global get nodes
kubectl --context cluster1 get nodes
```

Cluster context names are stored bare in `~/.kube/config` (no `kind-` prefix). `kubectl --context <name>` works directly. Kubeconfigs are also written to `~/.kube/marina/<name>.kubeconfig`. To switch the active context use `marina kubeconfig use <name>` (merge + `kubectl config use-context`); other helpers: `marina kubeconfig path|env|merge|remove <name>`.

## Creating several clusters at once (Fleet)

To bring up a whole set of clusters declaratively, write a **Fleet** manifest and `marina cluster apply -f`. The minimal manifest lists only cluster names:

```yaml
# fleet.yaml
apiVersion: marina.run/v1alpha1
kind: Fleet
spec:
  clusters:
    - global
    - cluster1
    - cluster2
```

```bash
marina fleet create -f fleet.yaml             # create the missing clusters (existing ones are skipped)
marina fleet create -f fleet.yaml --dry-run   # preview the plan (nums, order, options); creates nothing
marina fleet list                             # fleets and their member clusters
marina fleet describe dev-fleet               # members with num, ports, node version/readiness, labels
marina fleet export dev staging > fleet.yaml  # capture live clusters as a Fleet manifest
marina fleet export                           # ...or pick them interactively
marina fleet label dev-fleet -l tier=gold     # label every cluster in the fleet (key- to remove)
marina fleet delete dev-fleet --yes           # delete every cluster in the fleet; ALWAYS pass --yes in scripts (else it prompts and hangs a non-interactive agent)
marina fleet delete -f fleet.yaml --yes       # ...or delete exactly what the manifest lists
marina fleet adopt dev-fleet legacy1          # pull an existing standalone cluster into the fleet
```

If a manifest lists a cluster that already exists but isn't in the fleet, `fleet create` warns and skips it (no silent relabel); re-run with `--adopt` to pull them in.

`create` is additive and synchronous — each cluster is fully ready when it returns. Fleet membership is tracked by the `marina.run/fleet=<name>` node label, so `fleet list/label/delete <name>` work on live clusters. Optional per-cluster fields: `dependsOn` (ordering), `num`, `region`/`zone`, `nodeVersion`, `registries` (cherry-pick mirrors), `addons.metricsServer`, and `labels`. `spec.maxParallel` builds independent clusters concurrently (dependsOn is always honoured); `spec.defaults` supplies values inherited by every cluster. See the annotated `examples/fleet.yaml` in the repo for the full reference.

You can also filter/select clusters by label: `marina cluster list -l marina.run/fleet=dev-fleet` and `marina cluster delete -l env=test --yes`. (`marina cluster apply -f` / `cluster delete -f` remain as lower-level equivalents of `fleet create` / `fleet delete -f`.)

## Ephemeral cluster for testing (agent recipe)

The intended pattern for running a script, scenario, or e2e test against a throwaway cluster: **create → test → always tear down.** Give the cluster a unique name and delete it in a `trap` so a failed test never leaks a cluster.

```bash
set -euo pipefail

marina up                               # idempotent; no-op if the VM is already running

CLUSTER="test-$$"                        # unique per run
marina cluster create "$CLUSTER"         # synchronous — ready when it returns
trap 'marina cluster delete "$CLUSTER"' EXIT   # tear down on success OR failure

# ...run your test against the cluster...
kubectl --context "$CLUSTER" apply -f ./manifests/
kubectl --context "$CLUSTER" wait --for=condition=Available deploy/myapp --timeout=120s
kubectl --context "$CLUSTER" get svc          # LoadBalancer Services get a real 172.30.<num>.x IP (MetalLB)

# Optional quick sanity check of networking + LoadBalancer end to end:
KUBECONFIG=~/.kube/marina/"$CLUSTER".kubeconfig marina cluster e2e-test-nginx
```

Prefer one fresh cluster per test run for isolation; clusters are cheap and creation is fast after the first `marina up`. Only reuse a long-lived cluster when a test explicitly needs persisted state.

## Getting a test image into the cluster

Point your host Docker at the marina VM to build the image inside the VM's Docker daemon, then load it into the cluster's nodes with `kind load`. The kind CLI lives in the VM, so run it with `marina shell <cmd>` — non-interactive, safe for agents:

```bash
eval "$(marina docker-env)"                       # DOCKER_HOST → the marina VM's Docker
docker build -t myapp:test .                      # image now lives in the VM's Docker

marina shell kind load docker-image myapp:test --name <cluster>

# In manifests, reference the image and pin the pull policy so the node
# uses the loaded image instead of pulling from a registry:
#   image: myapp:test
#   imagePullPolicy: IfNotPresent
```

Public images pull transparently through the built-in pull-through mirrors (docker.io, quay.io, gcr.io, us-docker.pkg.dev, us-central1-docker.pkg.dev) — no extra steps.

## What marina sets up for you on each cluster

- **MetalLB** installed automatically with IPAddressPool `172.30.<num>.1-7` and `172.30.<num>.16-254`, plus L2Advertisement. **LoadBalancer Services get real IPs out of the box** — no extra setup.
- **Registry mirrors** for docker.io / quay.io / gcr.io / us-docker.pkg.dev / us-central1-docker.pkg.dev routed through local pull-through caches (configurable in `~/.marina/config.yaml`). Survives VM restarts; cache lives on the macOS host under `~/.marina/registry-cache/`.
- **CoreDNS** patched with optional per-zone upstream resolvers.
- **Local DNS names** (on by default, `network.dns`): every LoadBalancer Service resolves as `<service>.<namespace>.<cluster>.marina.internal` from the Mac, the VM and every pod. Prefer the name over the VIP in recipes. `marina dns list` shows what is published; custom names use the `external-dns.kubernetes.io/hostname` annotation (the old `external-dns.alpha.kubernetes.io/` prefix is ignored) and must sit under `<cluster>.marina.internal`. A new name appears within ~15 s (ExternalDNS sync); wait for `marina dns list` to show it before the first lookup — macOS caches a "no such name" answer for ~75 s. HTTPS: `network.dns.tls` (on by default) puts a `*.<cluster>.marina.internal` wildcard in Secret `default/marina-wildcard-tls` (browser-trusted, covers annotated names and Ingress hosts, not the two-label automatic names — use cert-manager's `marina-ca` ClusterIssuer for those after `marina ca attach`); pods trust it by mounting ConfigMap `default/marina-root-ca`. Fleet members also publish fleet-wide names under `<fleet>.marina.internal` (annotate: `external-dns.kubernetes.io/hostname: gateway.<fleet>.marina.internal`; first member to publish owns it) with a `*.<fleet>.marina.internal` wildcard in Secret `default/marina-fleet-wildcard-tls`. Prefer a fleet-wide name for anything clients should reach regardless of which cluster serves it. Verify from the Mac with `curl` or `dscacheutil -q host -a name <name>` — plain `dig <name>` skips `/etc/resolver` and always fails.
- **L3 routing** from macOS → VM → cluster pods/services. Source IPs are preserved (no SNAT) for host→cluster traffic.

## Things to know when writing recipes

- The kind Docker network is **shared across clusters** (subnet from `network.kindBridgeCIDR`, default `172.30.0.0/16`). Don't recreate it.
- Cluster API server is exposed on port `70<num>`. By default (`network.disablePortMirroring: true`) the kubeconfig points at the VM's `lima0` IP (e.g. `https://192.168.64.3:7001`); set `disablePortMirroring: false` and it points at `https://127.0.0.1:7001` instead. Either way, just use the exported kubeconfig — don't hardcode the address.
- Per-cluster pod/service subnets: `serviceSubnet: 10.<num>.0.0/16`, `podSubnet: 10.1<num>.0.0/16`. Keep cluster num 1–9 to avoid overlap.
- Cluster nodes are labelled with `managed-by=marina`, `topology.kubernetes.io/region` + `zone` (overridable via `--region` / `--zone`), and `marina.run/fleet=<name>` for clusters created from a Fleet. Add custom node labels with `marina cluster create -l key=value` (repeatable), the Fleet `labels:` / `defaults.labels` fields, or relabel an existing cluster with `marina cluster label <name> -l key=value` (`-l key-` removes).
- Docker socket on the host: `~/.marina.docker.sock`. Use `eval $(marina docker-env)` or `marina docker-context` to point your local docker CLI at it.
- To run anything inside the VM (kind, ctr, iptables, docker), use `marina shell <cmd> [args...]` — it is non-interactive, passes stdin/stdout through, and exits with the remote command's exit code. Bare `marina shell` opens an interactive session and **will hang a non-interactive agent**. Put `--` before the command if its flags could be mistaken for marina's: `marina shell -- bash -c '...'`.
- Move files with `marina copy ./file vm:/tmp/file` (and back with `marina copy vm:/tmp/file ./file`); `-r` for directories.
- **The host filesystem is not shared by default.** Docker runs inside the VM, so a bind mount resolves there: `docker run -v ~/conf:/etc/app` against an unshared path does **not** fail — dockerd creates the missing directory in the guest and the container gets an **empty** one. Either `marina copy` the files in and bind the in-VM path, or add the directory to `vm.mounts` in the config (`location:` + `writable: true`), where it appears in the guest at the same absolute path so host-path binds work. `marina up` applies a `vm.mounts` change by offering to restart the VM (no `destroy` needed); `marina status` lists the VM's current shares. This applies to `docker compose` from macOS too — the VM's own `docker compose` plugin works, and so does the host `docker-compose` binary against the marina context.
- Disk filling up (kind node images are ~2.7GB each): `marina shell df -h /` to check, `marina prune --dry-run` to find reclaimable caches, `marina disk resize 40GiB` + `marina down && marina up` to grow it.
- CPU and memory defaults scale to the Mac: 3/4 of the cores and half the RAM (a 10-core/32GiB M1 Pro gets 8 CPUs and 16GiB). Disks default to 20GiB root + 30GiB image store. `marina up` warns if a config asks for more than the machine has — it never blocks, so treat it as guidance to lower the value.
- Registry mirrors: cluster pulls use all of them (containerd `certs.d` per node), and so does `docker pull` on the VM (dockerd's own `/etc/docker/certs.d`).
- marina can be used as a plain Docker host, not just for clusters: `marina docker-context` then `docker ps`. Set `network.disablePortMirroring: false` if you want `-p 8080:80` to land on the Mac's `localhost` the way Docker Desktop does — it is a VM-level setting, so decide before building a lab on it.
- Behind a TLS-intercepting proxy, add its root CA with `vm.caCerts.files` — marina installs it in the VM, the registry mirrors and every kind node. Without it, pulls fail with `x509: certificate signed by unknown authority` even when the proxy itself is correct.
- Behind a corporate proxy: marina uses the Mac's system proxy automatically; set `network.proxy` only to override it. `marina doctor` reports which proxy dockerd is actually using. If shell commands work but image pulls fail, dockerd never got the proxy — `marina up` writes the systemd drop-in that fixes it.
- Image store full (`/var/lib/containerd`, the `vm.imageDisk` data disk — this is where OCI layers actually live, not the root disk): `marina shell df -h /var/lib/containerd`, then `marina down && marina disk resize-image 30GiB && marina up`. Editing `vm.imageDisk` in the config alone does nothing to a disk that already exists.
- If `marina up` prompts for a sudo password (it needs root only for the macOS host route), install the rules once with `marina sudoers | sudo tee /etc/sudoers.d/marina >/dev/null && sudo chmod 0440 /etc/sudoers.d/marina`. Required for `marina autostart`, since launchd cannot answer a prompt.

## Troubleshooting

```bash
marina doctor              # Diagnose route, iptables, VPN conflicts, hostagent collisions, etc.
marina doctor --fix        # Apply what marina can repair (route, iptables, IP forwarding).
marina doctor -o json      # Machine-readable checks: stable id, status, fixable.
marina status              # VM state, clusters, route, iptables snapshot.
marina status -o json      # Same, machine-readable — prefer this when scripting.
marina shell <cmd>         # Run a diagnostic command in the VM, e.g. marina shell iptables -t nat -L POSTROUTING -n
marina sudoers --check     # Are the passwordless host-route rules in effect?
```

Bare `marina shell` (interactive) is for humans only — always pass a command when scripting.

## When NOT to use marina

- Remote / cloud Kubernetes (EKS, GKE, AKS). marina is local-Mac only.
- Linux dev machines — marina depends on macOS Virtualization.framework.
- CI runners — use vanilla `kind` directly there; marina is for interactive local development and demos.
