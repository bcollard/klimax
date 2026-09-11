package registry

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bcollard/klimax/internal/config"
)

// DaemonConfigPath is dockerd's configuration file in the guest.
const DaemonConfigPath = "/etc/docker/daemon.json"

// HubMirrorEndpoint returns the address dockerd should use to reach the
// pull-through cache for Docker Hub, or "" when no Hub mirror is configured.
//
// The address is deliberately 127.0.0.1:<port> and not the container name that
// the kind nodes use. A kind node is a container on the `kind` network, where
// Docker's embedded DNS resolves `registry-dockerio`; dockerd runs in the VM's
// host namespace, where that name does not resolve at all. Pointing dockerd at
// the container name fails silently — `docker info` lists the mirror and every
// pull ignores it.
func HubMirrorEndpoint(cfg config.RegistryConfig) string {
	for _, m := range cfg.Mirrors {
		if strings.Contains(m.RemoteURL, "docker.io") {
			return fmt.Sprintf("http://127.0.0.1:%d", m.Port)
		}
	}
	return ""
}

// DaemonConfig renders the contents of daemon.json for a config, or "" when
// there is nothing for klimax to set.
//
// Only registry-mirrors is managed. dockerd's mirror support is Docker
// Hub-only — there is no per-registry equivalent — so quay.io, gcr.io and
// Artifact Registry cannot be routed through their caches for `docker pull`.
// They still are for cluster pulls, which go through containerd in the kind
// nodes and read /etc/containerd/certs.d.
//
// containerd's certs.d is not an option here either: dockerd resolves
// registries with its own client and never reads it, even with the containerd
// snapshotter enabled. Verified on Docker 29 — a certs.d entry for quay.io on
// the VM left `docker pull quay.io/...` going direct.
func DaemonConfig(cfg config.RegistryConfig) string {
	endpoint := HubMirrorEndpoint(cfg)
	if endpoint == "" {
		return ""
	}
	doc := map[string]any{"registry-mirrors": []string{endpoint}}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return ""
	}
	return string(b) + "\n"
}

// MergeDaemonConfig sets (or removes) the registry-mirrors key in an existing
// daemon.json, preserving every other setting. It returns the new contents,
// whether anything changed, and an error if the existing file is not valid JSON.
//
// Merging rather than overwriting matters: daemon.json is a file users edit —
// insecure-registries, log-driver, default-address-pools — and klimax has no
// business discarding that to set one field. An unparseable file is reported
// rather than replaced, because it is more likely to be a user's typo than
// something klimax should clean up.
//
// An empty want removes the key, and returns "" when that leaves the document
// empty so the caller can delete the file instead of leaving `{}` behind.
func MergeDaemonConfig(current, want string) (string, bool, error) {
	doc := map[string]any{}
	if strings.TrimSpace(current) != "" {
		if err := json.Unmarshal([]byte(current), &doc); err != nil {
			return "", false, err
		}
	}

	existing, _ := doc["registry-mirrors"].([]any)
	had := make([]string, 0, len(existing))
	for _, v := range existing {
		if sv, ok := v.(string); ok {
			had = append(had, sv)
		}
	}

	switch {
	case want == "":
		if _, present := doc["registry-mirrors"]; !present {
			return current, false, nil
		}
		delete(doc, "registry-mirrors")
	case len(had) == 1 && had[0] == want:
		return current, false, nil
	default:
		doc["registry-mirrors"] = []string{want}
	}

	if len(doc) == 0 {
		return "", true, nil
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", false, err
	}
	return string(b) + "\n", true, nil
}
