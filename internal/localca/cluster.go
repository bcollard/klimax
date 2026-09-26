package localca

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/bcollard/klimax/internal/guest"
)

// In-cluster names. The wildcard Secret lives in default because a Secret can
// only be mounted in its own namespace; `klimax ca secret` copies it elsewhere.
const (
	WildcardSecret = "klimax-wildcard-tls"
	RootConfigMap  = "klimax-root-ca"
	IssuerName     = "klimax-ca"
	// FleetWildcardSecret and FleetIssuerName are the fleet zone's
	// counterparts, installed into every member beside the cluster's own.
	FleetWildcardSecret = "klimax-fleet-wildcard-tls"
	FleetIssuerName     = "klimax-fleet-ca"
	secretNamespace     = "default"
	// NodeCAFile is the name the root gets in each node's trust store.
	NodeCAFile = "klimax-local-ca.crt"
)

// SecretName is the wildcard Secret this zone's material is installed as.
func (c *Cluster) SecretName() string {
	if c.Kind == FleetZone {
		return FleetWildcardSecret
	}
	return WildcardSecret
}

// IssuerName is the cert-manager ClusterIssuer backed by this zone's intermediate.
func (c *Cluster) IssuerName() string {
	if c.Kind == FleetZone {
		return FleetIssuerName
	}
	return IssuerName
}

// InstallResult says what InstallInCluster set up.
type InstallResult struct {
	// IssuerNamespace is where cert-manager runs, when it does: the
	// klimax-ca ClusterIssuer was created and its Secret put there. Empty when
	// the cluster has no cert-manager.
	IssuerNamespace string
}

// InstallInCluster puts a cluster's CA material into the cluster:
//   - default/klimax-wildcard-tls — the wildcard, as a kubernetes.io/tls Secret
//     (tls.crt carries the intermediate after the leaf; ca.crt is the root);
//   - default/klimax-root-ca — the root, as a ConfigMap to mount into clients;
//   - when cert-manager is already installed, a klimax-ca ClusterIssuer backed
//     by the cluster's intermediate. klimax never installs cert-manager itself:
//     a Helm-managed cert-manager added later would collide with one klimax
//     applied.
//
// Key material reaches the guest through WriteSecretFile, never through a
// command line or script body, so it is not in any log; the files are removed
// when the script exits.
//
// target is the cluster to install into: c itself for a cluster zone, a member
// for a fleet zone.
func InstallInCluster(ctx context.Context, g *guest.Client, target string, c *Cluster) (InstallResult, error) {
	var res InstallResult
	dir := fmt.Sprintf("/tmp/klimax-ca-%s-%s-%s", target, c.Kind, c.Name)
	files := map[string]string{
		"wildcard.crt": c.WildcardPEM,
		"wildcard.key": c.WildcardKeyPEM,
		"root.crt":     c.RootPEM,
		"chain.crt":    c.ChainPEM,
		"ca.key":       c.KeyPEM,
	}
	for name, content := range files {
		if err := g.WriteSecretFile(ctx, dir+"/"+name, content); err != nil {
			_, _ = g.Run(ctx, "sudo rm -rf "+dir)
			return res, err
		}
	}

	kube := kubeconfigPrelude(target)
	cmNS, _ := g.Run(ctx, kube+` kubectl --kubeconfig ${KIND_KUBECONFIG} get crd clusterissuers.cert-manager.io >/dev/null 2>&1 && \
kubectl --kubeconfig ${KIND_KUBECONFIG} get deploy -A -l app.kubernetes.io/name=cert-manager,app.kubernetes.io/component=controller \
  -o jsonpath='{.items[0].metadata.namespace}' 2>/dev/null || true`)
	res.IssuerNamespace = strings.TrimSpace(cmNS)

	issuer := ""
	if res.IssuerNamespace != "" {
		issuer = fmt.Sprintf(`
kubectl --kubeconfig ${KIND_KUBECONFIG} -n %[1]s create secret tls %[2]s \
  --cert=${D}/chain.crt --key=${D}/ca.key --dry-run=client -o yaml \
  | kubectl --kubeconfig ${KIND_KUBECONFIG} label --local -f - app.kubernetes.io/managed-by=klimax -o yaml \
  | kubectl --kubeconfig ${KIND_KUBECONFIG} apply -f - >/dev/null
# cert-manager's webhook may not be serving yet on a cluster that just got it.
DEADLINE=$((SECONDS + 60))
until cat <<'ISSUER_EOF' | kubectl --kubeconfig ${KIND_KUBECONFIG} apply -f - >/dev/null
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: %[2]s
  labels:
    app.kubernetes.io/managed-by: klimax
spec:
  ca:
    secretName: %[2]s
ISSUER_EOF
do
  if [ ${SECONDS} -ge ${DEADLINE} ]; then echo "ClusterIssuer %[2]s not accepted within 60s" >&2; exit 1; fi
  sleep 2
done
`, res.IssuerNamespace, c.IssuerName())
	}

	script := fmt.Sprintf(`#!/bin/bash
set -euo pipefail
D=%[1]s
trap 'rm -rf ${D}' EXIT
%[2]s
kubectl --kubeconfig ${KIND_KUBECONFIG} -n %[3]s create secret generic %[4]s --type=kubernetes.io/tls \
  --from-file=tls.crt=${D}/wildcard.crt --from-file=tls.key=${D}/wildcard.key --from-file=ca.crt=${D}/root.crt \
  --dry-run=client -o yaml \
  | kubectl --kubeconfig ${KIND_KUBECONFIG} label --local -f - app.kubernetes.io/managed-by=klimax -o yaml \
  | kubectl --kubeconfig ${KIND_KUBECONFIG} apply -f - >/dev/null
kubectl --kubeconfig ${KIND_KUBECONFIG} -n %[3]s create configmap %[5]s --from-file=ca.crt=${D}/root.crt \
  --dry-run=client -o yaml \
  | kubectl --kubeconfig ${KIND_KUBECONFIG} label --local -f - app.kubernetes.io/managed-by=klimax -o yaml \
  | kubectl --kubeconfig ${KIND_KUBECONFIG} apply -f - >/dev/null
%[6]s`, dir, kube, secretNamespace, c.SecretName(), RootConfigMap, issuer)

	slog.Info("Installing local CA material", "cluster", target, "wildcard", c.WildcardNames[0], "certManager", res.IssuerNamespace != "")
	if err := g.RunScript(ctx, fmt.Sprintf("install %s CA on cluster %q", c.WildcardNames[0], target), script); err != nil {
		return res, err
	}
	return res, nil
}

// CopySecret copies the wildcard Secret into another namespace — a Secret can
// only be mounted from its own namespace, and an Ingress controller reads
// its TLS Secret from the Ingress's namespace.
func CopySecret(ctx context.Context, g *guest.Client, cluster, secret, namespace string) error {
	script := fmt.Sprintf(`#!/bin/bash
set -euo pipefail
%s
kubectl --kubeconfig ${KIND_KUBECONFIG} get namespace %[2]s >/dev/null
kubectl --kubeconfig ${KIND_KUBECONFIG} -n %[3]s get secret %[4]s -o json \
  | jq 'del(.metadata.namespace, .metadata.uid, .metadata.resourceVersion, .metadata.creationTimestamp, .metadata.managedFields, .metadata.annotations)' \
  | kubectl --kubeconfig ${KIND_KUBECONFIG} -n %[2]s apply -f - >/dev/null
`, kubeconfigPrelude(cluster), namespace, secretNamespace, secret)
	return g.RunScript(ctx, fmt.Sprintf("copy %s to %s/%s", secret, cluster, namespace), script)
}

func kubeconfigPrelude(cluster string) string {
	return fmt.Sprintf("KIND_KUBECONFIG=/tmp/klimax-kube-%[1]s.yaml; kind get kubeconfig --name %[1]s | sed 's|https://0.0.0.0:|https://127.0.0.1:|g' > ${KIND_KUBECONFIG};", cluster)
}
