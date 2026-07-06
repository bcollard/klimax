package kind

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/bcollard/klimax/internal/guest"
)

// InstallMetricsServer installs the Kubernetes metrics-server addon into the
// named cluster. version is a release tag (e.g. "v0.7.2"); empty means the
// latest published release. On kind, the kubelet serving certificate is not
// signed by the cluster CA, so --kubelet-insecure-tls is normally required
// (kubeletInsecureTLS=true) for metrics-server to scrape kubelets.
func InstallMetricsServer(ctx context.Context, g *guest.Client, clusterName, version string, kubeletInsecureTLS bool) error {
	slog.Info("Installing metrics-server addon", "cluster", clusterName, "version", orLatest(version), "kubeletInsecureTLS", kubeletInsecureTLS)

	url := "https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml"
	if version != "" {
		url = fmt.Sprintf("https://github.com/kubernetes-sigs/metrics-server/releases/download/%s/components.yaml", version)
	}

	insecurePatch := ""
	if kubeletInsecureTLS {
		insecurePatch = `kubectl --kubeconfig ${KIND_KUBECONFIG} -n kube-system patch deployment metrics-server --type=json \
  -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'
`
	}

	script := fmt.Sprintf(`#!/bin/bash
set -euo pipefail
KIND_KUBECONFIG=/tmp/klimax-kube-%s.yaml
kind get kubeconfig --name %s | sed 's|https://0.0.0.0:|https://127.0.0.1:|g' > ${KIND_KUBECONFIG}

kubectl --kubeconfig ${KIND_KUBECONFIG} apply -f %s

%skubectl --kubeconfig ${KIND_KUBECONFIG} -n kube-system rollout status deploy/metrics-server --timeout=180s

echo "metrics-server installed on cluster %s"
`, clusterName, clusterName, url, insecurePatch, clusterName)

	return g.RunScript(ctx, fmt.Sprintf("install metrics-server on cluster %q", clusterName), script)
}

func orLatest(v string) string {
	if v == "" {
		return "latest"
	}
	return v
}
