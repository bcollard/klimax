package localdns

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/bcollard/klimax/internal/config"
	"github.com/bcollard/klimax/internal/guest"
)

// ExternalDNSImage is the ExternalDNS release the manifest below was written
// against. v0.22.0 matters specifically: it changed the annotation prefix to
// external-dns.kubernetes.io/ with no fallback, and the docs klimax points at
// assume that.
const ExternalDNSImage = "registry.k8s.io/external-dns/external-dns:v0.22.0"

// ExternalDNSNamespace is where the per-cluster deployment lives.
const ExternalDNSNamespace = "external-dns"

// ExternalDNSManifest renders ExternalDNS for one cluster.
//
// Applied with plain kubectl rather than the Helm chart: the VM ships kubectl
// but not helm, and the chart runs its extraArgs through `tpl`, which renders
// --fqdn-template's {{.Name}} itself and silently publishes every record under
// an empty name. RBAC and security context match chart 1.22.0.
//
// The flags that make it safe for several clusters to share one etcd:
//   - --domain-filter pins each cluster to its own subzone;
//   - --txt-owner-id marks which cluster wrote a record;
//   - together they make --policy=sync safe, so records are deleted when their
//     Service is, instead of piling up under upsert-only.
//
// --service-type-filter=LoadBalancer is what keeps the zone to Services the Mac
// can actually reach. Without it the service source publishes every type, and
// --fqdn-template names them all: headless Services resolve to pod IPs (10.x,
// unroutable from the host) and host-network pods to node IPs — found on a real
// cluster running kube-prometheus-stack.
//
// --fqdn-template gives every LoadBalancer Service a name with no annotation
// (OrbStack-style), from network.dns.nameTemplate; --combine-fqdn-annotation keeps that name when an
// external-dns.kubernetes.io/hostname annotation adds a custom one.
//
// fleet, when set, adds the fleet's zone (<fleet>.<domain>) to the filter, so a
// member cluster can publish fleet-wide names such as gateway.<fleet>.<domain>
// through the hostname annotation. Automatic names always stay in the
// cluster's own zone.
func ExternalDNSManifest(cfg *config.Config, cluster, fleet string) string {
	zone := cfg.ClusterDNSZone(cluster)
	fleetFilter := ""
	if fleet != "" {
		fleetFilter = "\n            - --domain-filter=" + cfg.FleetDNSZone(fleet)
	}
	return fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %[1]s
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: external-dns
  namespace: %[1]s
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: klimax-external-dns
rules:
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["list", "watch"]
  - apiGroups: [""]
    resources: ["pods", "services"]
    verbs: ["get", "watch", "list"]
  - apiGroups: ["discovery.k8s.io"]
    resources: ["endpointslices"]
    verbs: ["get", "watch", "list"]
  - apiGroups: ["extensions", "networking.k8s.io"]
    resources: ["ingresses"]
    verbs: ["get", "watch", "list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: klimax-external-dns
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: klimax-external-dns
subjects:
  - kind: ServiceAccount
    name: external-dns
    namespace: %[1]s
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: external-dns
  namespace: %[1]s
  labels:
    app.kubernetes.io/name: external-dns
    app.kubernetes.io/managed-by: klimax
spec:
  replicas: 1
  strategy:
    type: Recreate
  selector:
    matchLabels:
      app.kubernetes.io/name: external-dns
  template:
    metadata:
      labels:
        app.kubernetes.io/name: external-dns
    spec:
      serviceAccountName: external-dns
      securityContext:
        fsGroup: 65534
        runAsNonRoot: true
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: external-dns
          image: %[2]s
          args:
            - --source=service
            - --source=ingress
            - --service-type-filter=LoadBalancer
            - --provider=coredns
            - --registry=txt
            - --txt-owner-id=%[3]s
            - --domain-filter=%[4]s%[7]s
            - --policy=sync
            - --interval=15s
            - --fqdn-template=%[6]s.%[4]s
            - --combine-fqdn-annotation
          env:
            - name: ETCD_URLS
              value: http://%[5]s:2379
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop: ["ALL"]
            privileged: false
            readOnlyRootFilesystem: true
            runAsGroup: 65532
            runAsNonRoot: true
            runAsUser: 65532
`, ExternalDNSNamespace, ExternalDNSImage, cluster, zone, cfg.DNSEtcdIP(), cfg.DNSNameTemplate(), fleetFilter)
}

// InstallExternalDNS applies the manifest to a cluster and waits for it.
// Re-applying is safe: kubectl apply converges to the same objects.
func InstallExternalDNS(ctx context.Context, g *guest.Client, cfg *config.Config, cluster, fleet string) error {
	slog.Info("Installing ExternalDNS for local DNS", "cluster", cluster, "zone", cfg.ClusterDNSZone(cluster), "fleet", fleet)
	script := fmt.Sprintf(`#!/bin/bash
set -euo pipefail
KIND_KUBECONFIG=/tmp/klimax-kube-%[1]s.yaml
kind get kubeconfig --name %[1]s | sed 's|https://0.0.0.0:|https://127.0.0.1:|g' > ${KIND_KUBECONFIG}
cat <<'MANIFEST_EOF' | kubectl --kubeconfig ${KIND_KUBECONFIG} apply -f -
%[2]sMANIFEST_EOF
kubectl --kubeconfig ${KIND_KUBECONFIG} -n %[3]s rollout status deploy/external-dns --timeout=180s
`, cluster, ExternalDNSManifest(cfg, cluster, fleet), ExternalDNSNamespace)
	return g.RunScript(ctx, fmt.Sprintf("install ExternalDNS on cluster %q", cluster), script)
}

// ClusterForward is the CoreDNS stanza that sends the zone from a cluster's
// pods to the klimax DNS server. Expressed as a CustomDNSResolver so it goes
// through the same Corefile patch as the user's own zones.
func ClusterForward(cfg *config.Config) config.CustomDNSResolver {
	return config.CustomDNSResolver{Domain: cfg.DNSDomain(), Resolvers: []string{cfg.DNSServerIP()}}
}
