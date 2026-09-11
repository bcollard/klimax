package registry

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bcollard/klimax/internal/config"
)

func mirrorsCfg() config.RegistryConfig {
	return config.RegistryConfig{Mirrors: []config.RegistryMirror{
		{Name: "registry-quayio", Port: 5010, RemoteURL: "https://quay.io"},
		{Name: "registry-dockerio", Port: 5030, RemoteURL: "https://registry-1.docker.io"},
	}}
}

// dockerd runs in the VM's host namespace, where the container name the kind
// nodes use does not resolve. Pointing it there fails silently.
func TestHubMirrorEndpointUsesLoopbackNotContainerName(t *testing.T) {
	got := HubMirrorEndpoint(mirrorsCfg())
	if got != "http://127.0.0.1:5030" {
		t.Errorf("got %q, want the published port on loopback", got)
	}
	if strings.Contains(got, "registry-dockerio") {
		t.Error("used the container name; dockerd cannot resolve it")
	}
}

func TestHubMirrorEndpointEmptyWithoutHubMirror(t *testing.T) {
	cfg := config.RegistryConfig{Mirrors: []config.RegistryMirror{
		{Name: "registry-quayio", Port: 5010, RemoteURL: "https://quay.io"},
	}}
	if got := HubMirrorEndpoint(cfg); got != "" {
		t.Errorf("no docker.io mirror should yield \"\", got %q", got)
	}
}

func TestDaemonConfigIsValidJSON(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal([]byte(DaemonConfig(mirrorsCfg())), &doc); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	m, _ := doc["registry-mirrors"].([]any)
	if len(m) != 1 || m[0] != "http://127.0.0.1:5030" {
		t.Errorf("unexpected registry-mirrors: %v", doc["registry-mirrors"])
	}
}

// daemon.json is a file users edit. Setting one key must not discard the rest.
func TestMergeDaemonConfigPreservesOtherSettings(t *testing.T) {
	const current = `{
  "insecure-registries": ["internal.corp:5000"],
  "log-driver": "json-file"
}`
	out, changed, err := MergeDaemonConfig(current, "http://127.0.0.1:5030")
	if err != nil || !changed {
		t.Fatalf("err=%v changed=%v", err, changed)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["log-driver"] != "json-file" {
		t.Error("log-driver was dropped")
	}
	if ir, _ := doc["insecure-registries"].([]any); len(ir) != 1 {
		t.Errorf("insecure-registries was dropped: %v", doc["insecure-registries"])
	}
	if m, _ := doc["registry-mirrors"].([]any); len(m) != 1 || m[0] != "http://127.0.0.1:5030" {
		t.Errorf("mirror not set: %v", doc["registry-mirrors"])
	}
}

// Re-running `up` must not restart Docker every time.
func TestMergeDaemonConfigIdempotent(t *testing.T) {
	out, _, err := MergeDaemonConfig("", "http://127.0.0.1:5030")
	if err != nil {
		t.Fatal(err)
	}
	if _, changed, _ := MergeDaemonConfig(out, "http://127.0.0.1:5030"); changed {
		t.Error("second pass reported a change")
	}
}

func TestMergeDaemonConfigRemovesKeyAndFile(t *testing.T) {
	// Only klimax's key present -> the whole file should go.
	out, changed, err := MergeDaemonConfig(`{"registry-mirrors":["http://127.0.0.1:5030"]}`, "")
	if err != nil || !changed {
		t.Fatalf("err=%v changed=%v", err, changed)
	}
	if out != "" {
		t.Errorf("expected the file to be removed, got %q", out)
	}

	// Other settings present -> keep the file, drop only the key.
	out, changed, err = MergeDaemonConfig(`{"registry-mirrors":["http://127.0.0.1:5030"],"log-driver":"json-file"}`, "")
	if err != nil || !changed {
		t.Fatalf("err=%v changed=%v", err, changed)
	}
	if strings.Contains(out, "registry-mirrors") {
		t.Errorf("key not removed: %s", out)
	}
	if !strings.Contains(out, "log-driver") {
		t.Errorf("other settings lost: %s", out)
	}

	// Nothing to do.
	if _, changed, _ := MergeDaemonConfig(`{"log-driver":"json-file"}`, ""); changed {
		t.Error("reported a change when there was nothing to remove")
	}
}

// A file klimax cannot parse is the user's problem to fix, not ours to replace.
func TestMergeDaemonConfigRefusesInvalidJSON(t *testing.T) {
	if _, _, err := MergeDaemonConfig(`{ this is not json`, "http://127.0.0.1:5030"); err == nil {
		t.Error("expected an error rather than silently overwriting")
	}
}
