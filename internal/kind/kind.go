package kind

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bcollard/klimax/internal/config"
	"github.com/bcollard/klimax/internal/guest"
	"github.com/bcollard/klimax/internal/registry"
)

// CreateCluster creates a kind cluster with the given config.
// It:
//  1. Writes a kind cluster config with containerd patches for all registries.
//  2. Creates the cluster using the configured node image version.
//  3. Installs MetalLB and configures an IP address pool.
//  4. Creates the local-registry-hosting ConfigMap in kube-public.
//  5. Exports the kubeconfig to ~/.kube/kind/<name>.kubeconfig on the host,
//     patching the API server address from 127.0.0.1 to the VM's lima0 IP.
func CreateCluster(ctx context.Context, g *guest.Client, cl config.ClusterConfig, kindCfg config.KindConfig, regCfg config.RegistryConfig, kindCIDR string) error {
	// Apply locality defaults based on num.
	if cl.Region == "" {
		cl.Region = fmt.Sprintf("europe-west%d", cl.Num)
	}
	if cl.Zone == "" {
		cl.Zone = fmt.Sprintf("europe-west%d-b", cl.Num)
	}

	slog.Info("Creating kind cluster", "cluster", cl.Name, "num", cl.Num, "nodeVersion", kindCfg.NodeVersion, "region", cl.Region, "zone", cl.Zone)

	containerdPatches := registry.ContainerdPatches(regCfg)
	subnetPrefix := kindSubnetPrefix(kindCIDR)
	apiPort := 7000 + cl.Num

	clusterConfig := fmt.Sprintf(`kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: %s
nodes:
- role: control-plane
  extraPortMappings:
  - containerPort: 6443
    hostPort: %d
    listenAddress: "0.0.0.0"
networking:
  serviceSubnet: "10.%d.0.0/16"
  podSubnet: "10.1%d.0.0/16"
kubeadmConfigPatches:
- |
  kind: InitConfiguration
  nodeRegistration:
    kubeletExtraArgs:
      node-labels: "ingress-ready=true,topology.kubernetes.io/region=%s,topology.kubernetes.io/zone=%s"
containerdConfigPatches:
- |-
%s
`, cl.Name, apiPort, cl.Num, cl.Num, cl.Region, cl.Zone, containerdPatches)

	configPath := fmt.Sprintf("/tmp/klimax-kind-%s.yaml", cl.Name)

	createScript := fmt.Sprintf(`#!/bin/bash
set -euo pipefail

cat > %s <<'KINDCFG_EOF'
%s
KINDCFG_EOF

kind create cluster \
  --config %s \
  --image kindest/node:%s \
  --retain \
  --name %s \
  --wait 5m

echo "kind cluster %s created"
`, configPath, clusterConfig, configPath, kindCfg.NodeVersion, cl.Name, cl.Name)

	if err := g.RunScript(ctx, fmt.Sprintf("create kind cluster %q", cl.Name), createScript); err != nil {
		return fmt.Errorf("creating kind cluster %q: %w", cl.Name, err)
	}

	if err := installMetalLB(ctx, g, cl, kindCfg.MetalLBVersion, subnetPrefix); err != nil {
		return fmt.Errorf("installing MetalLB for cluster %q: %w", cl.Name, err)
	}

	if regCfg.LocalRegistry.Enabled {
		if err := applyLocalRegistryConfigMap(ctx, g, cl.Name, regCfg.LocalRegistry.Port); err != nil {
			return fmt.Errorf("applying local registry ConfigMap for cluster %q: %w", cl.Name, err)
		}
	}

	if len(kindCfg.CoreDNSDomains) > 0 {
		if err := applyCoreDNSPatch(ctx, g, cl.Name, kindCfg.CoreDNSDomains); err != nil {
			return fmt.Errorf("applying CoreDNS patch for cluster %q: %w", cl.Name, err)
		}
	}

	if err := exportKubeconfig(ctx, g, cl.Name, apiPort); err != nil {
		return fmt.Errorf("exporting kubeconfig for cluster %q: %w", cl.Name, err)
	}

	slog.Info("kind cluster ready", "cluster", cl.Name, "apiPort", apiPort)
	return nil
}

// DeleteCluster deletes the named kind cluster and removes its kubeconfig file.
func DeleteCluster(ctx context.Context, g *guest.Client, name string) error {
	slog.Info("Deleting kind cluster", "cluster", name)
	if _, err := g.Run(ctx, fmt.Sprintf("kind delete cluster --name %s", name)); err != nil {
		return fmt.Errorf("deleting kind cluster %q: %w", name, err)
	}
	kubeconfigPath := kindKubeconfigPath(name)
	if err := os.Remove(kubeconfigPath); err != nil && !os.IsNotExist(err) {
		slog.Warn("could not remove kubeconfig", "path", kubeconfigPath, "err", err)
	}
	return nil
}

// ListClusters returns the names of all running kind clusters in the guest.
func ListClusters(ctx context.Context, g *guest.Client) ([]string, error) {
	out, err := g.Run(ctx, "kind get clusters 2>/dev/null || true")
	if err != nil {
		return nil, fmt.Errorf("listing kind clusters: %w", err)
	}
	out = strings.TrimSpace(out)
	if out == "" || out == "No kind clusters found." {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// installMetalLB applies the MetalLB manifests and configures an IPAddressPool
// and L2Advertisement derived from the kindCIDR and cluster num.
func installMetalLB(ctx context.Context, g *guest.Client, cl config.ClusterConfig, version, subnetPrefix string) error {
	slog.Info("Installing MetalLB", "cluster", cl.Name, "version", version)

	poolStart1 := fmt.Sprintf("%s.%d.1", subnetPrefix, cl.Num)
	poolEnd1 := fmt.Sprintf("%s.%d.7", subnetPrefix, cl.Num)
	poolStart2 := fmt.Sprintf("%s.%d.16", subnetPrefix, cl.Num)
	poolEnd2 := fmt.Sprintf("%s.%d.254", subnetPrefix, cl.Num)

	metallbScript := fmt.Sprintf(`#!/bin/bash
set -euo pipefail
KIND_KUBECONFIG=/tmp/klimax-kube-%s.yaml
kind get kubeconfig --name %s | sed 's|https://0.0.0.0:|https://127.0.0.1:|g' > ${KIND_KUBECONFIG}

kubectl --kubeconfig ${KIND_KUBECONFIG} apply \
  -f https://raw.githubusercontent.com/metallb/metallb/%s/config/manifests/metallb-native.yaml

kubectl --kubeconfig ${KIND_KUBECONFIG} \
  -n metallb-system wait pod --all --timeout=120s --for=condition=Ready

kubectl --kubeconfig ${KIND_KUBECONFIG} \
  -n metallb-system wait deploy controller --timeout=120s --for=condition=Available

sleep 5

cat <<EOF | kubectl --kubeconfig ${KIND_KUBECONFIG} apply -f -
apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: kind-pool
  namespace: metallb-system
spec:
  addresses:
  - %s-%s
  - %s-%s
---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata:
  name: kind-l2
  namespace: metallb-system
EOF

echo "MetalLB configured for cluster %s"
`,
		cl.Name, cl.Name,
		version,
		poolStart1, poolEnd1,
		poolStart2, poolEnd2,
		cl.Name,
	)

	return g.RunScript(ctx, fmt.Sprintf("install MetalLB on cluster %q", cl.Name), metallbScript)
}

// applyLocalRegistryConfigMap creates the standard kube-public ConfigMap that
// advertises the local push registry to tooling (e.g. Tilt, Skaffold).
func applyLocalRegistryConfigMap(ctx context.Context, g *guest.Client, clusterName string, port int) error {
	slog.Info("Applying local-registry-hosting ConfigMap", "cluster", clusterName)

	script := fmt.Sprintf(`#!/bin/bash
set -euo pipefail
KIND_KUBECONFIG=/tmp/klimax-kube-%s.yaml
kind get kubeconfig --name %s | sed 's|https://0.0.0.0:|https://127.0.0.1:|g' > ${KIND_KUBECONFIG}
cat <<EOF | kubectl --kubeconfig ${KIND_KUBECONFIG} apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: local-registry-hosting
  namespace: kube-public
data:
  localRegistryHosting.v1: |
    host: "kind-registry:%d"
    help: "https://kind.sigs.k8s.io/docs/user/local-registry/"
EOF
`, clusterName, clusterName, port)

	return g.RunScript(ctx, fmt.Sprintf("local registry ConfigMap for cluster %q", clusterName), script)
}

// exportKubeconfig fetches the kubeconfig from the VM, patches the server address
// from 127.0.0.1/0.0.0.0 to the VM's lima0 IP, and writes it to
// ~/.kube/kind/<name>.kubeconfig on the macOS host.
func exportKubeconfig(ctx context.Context, g *guest.Client, clusterName string, apiPort int) error {
	slog.Info("Exporting kubeconfig to host", "cluster", clusterName, "apiPort", apiPort)

	raw, err := g.Run(ctx, fmt.Sprintf("kind get kubeconfig --name %s", clusterName))
	if err != nil {
		return fmt.Errorf("getting kubeconfig: %w", err)
	}

	// Lima's hostagent automatically forwards guest TCP ports to 127.0.0.1 on the host
	// via the guest agent event stream — no static portForwards config needed.
	// Using 127.0.0.1 works with any security software that blocks direct vzNAT IP access.
	patched := strings.ReplaceAll(raw, "https://0.0.0.0:", "https://127.0.0.1:")

	outPath := kindKubeconfigPath(clusterName)
	if err := os.MkdirAll(filepath.Dir(outPath), 0o750); err != nil {
		return fmt.Errorf("creating kubeconfig dir: %w", err)
	}
	if err := os.WriteFile(outPath, []byte(patched), 0o600); err != nil {
		return fmt.Errorf("writing kubeconfig: %w", err)
	}
	slog.Info("Kubeconfig written", "path", outPath)
	fmt.Printf("kubeconfig: %s\n  export KUBECONFIG=%s\n", outPath, outPath)
	return nil
}

// KindKubeconfigPath returns the host path for a cluster's kubeconfig.
func KindKubeconfigPath(clusterName string) string {
	return kindKubeconfigPath(clusterName)
}

func kindKubeconfigPath(clusterName string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kube", "kind", clusterName+".kubeconfig")
}

// applyCoreDNSPatch patches the CoreDNS ConfigMap to forward the given custom domains
// to public resolvers (8.8.8.8 / 8.8.4.4) while preserving the default .:53 block.
func applyCoreDNSPatch(ctx context.Context, g *guest.Client, clusterName string, domains []string) error {
	slog.Info("Applying CoreDNS patch", "cluster", clusterName, "domains", domains)

	// Build a stanza per extra domain.
	var domainBlocks strings.Builder
	for _, d := range domains {
		domainBlocks.WriteString(fmt.Sprintf(`%s:53 {
    errors
    cache 30
    forward . 8.8.8.8 8.8.4.4
}
`, d))
	}

	// Corefile content (unindented); will be indented for YAML embedding below.
	corefile := fmt.Sprintf(`%s.:53 {
    errors
    health {
       lameduck 5s
    }
    ready
    kubernetes cluster.local in-addr.arpa ip6.arpa {
       pods insecure
       fallthrough in-addr.arpa ip6.arpa
       ttl 30
    }
    prometheus :9153
    forward . /etc/resolv.conf {
       max_concurrent 1000
    }
    cache 30
    loop
    reload
    loadbalance
}
`, domainBlocks.String())

	// Indent every line by 4 spaces so the block scalar is valid YAML under "Corefile: |".
	indentedCorefile := "    " + strings.ReplaceAll(strings.TrimRight(corefile, "\n"), "\n", "\n    ")

	script := fmt.Sprintf(`#!/bin/bash
set -euo pipefail
KIND_KUBECONFIG=/tmp/klimax-kube-%s.yaml
kind get kubeconfig --name %s | sed 's|https://0.0.0.0:|https://127.0.0.1:|g' > ${KIND_KUBECONFIG}
cat <<'EOF' | kubectl --kubeconfig ${KIND_KUBECONFIG} apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: coredns
  namespace: kube-system
data:
  Corefile: |
%s
EOF
# Restart CoreDNS to pick up the new config
kubectl --kubeconfig ${KIND_KUBECONFIG} \
  -n kube-system rollout restart deployment/coredns
kubectl --kubeconfig ${KIND_KUBECONFIG} \
  -n kube-system rollout status deployment/coredns --timeout=60s
echo "CoreDNS patched for cluster %s"
`, clusterName, clusterName, indentedCorefile, clusterName)

	return g.RunScript(ctx, fmt.Sprintf("CoreDNS patch for cluster %q", clusterName), script)
}

// DetectUsedNums inspects all running kind control-plane containers and returns
// the set of cluster nums currently in use (derived from host port 70N on :6443).
func DetectUsedNums(ctx context.Context, g *guest.Client) (map[int]string, error) {
	names, err := ListClusters(ctx, g)
	if err != nil {
		return nil, err
	}
	used := make(map[int]string, len(names)) // num → cluster name
	for _, name := range names {
		container := name + "-control-plane"
		out, err := g.Run(ctx, fmt.Sprintf(
			`docker inspect -f '{{json .HostConfig.PortBindings}}' %s 2>/dev/null || true`,
			container,
		))
		if err != nil || strings.TrimSpace(out) == "" {
			continue
		}
		// PortBindings shape: {"6443/tcp":[{"HostIp":"0.0.0.0","HostPort":"7001"}]}
		var bindings map[string][]struct {
			HostPort string `json:"HostPort"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &bindings); err != nil {
			continue
		}
		for _, entries := range bindings {
			for _, e := range entries {
				port, err := strconv.Atoi(e.HostPort)
				if err != nil {
					continue
				}
				if port > 7000 && port <= 7099 {
					used[port-7000] = name
				}
			}
		}
	}
	return used, nil
}

// NextFreeNum returns the lowest num in [1,99] not present in usedNums.
func NextFreeNum(usedNums map[int]string) (int, error) {
	for n := 1; n <= 99; n++ {
		if _, taken := usedNums[n]; !taken {
			return n, nil
		}
	}
	return 0, fmt.Errorf("no free cluster num available (all 1-99 are in use)")
}

// kindSubnetPrefix returns the first two octets of a CIDR (e.g. "172.30" from "172.30.0.0/16").
func kindSubnetPrefix(cidr string) string {
	ip, _, err := net.ParseCIDR(cidr)
	if err != nil {
		return "172.30"
	}
	parts := strings.Split(ip.String(), ".")
	if len(parts) < 2 {
		return "172.30"
	}
	return parts[0] + "." + parts[1]
}
