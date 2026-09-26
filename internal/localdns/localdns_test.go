package localdns

import (
	"strings"
	"testing"

	"github.com/bcollard/klimax/internal/config"
	"gopkg.in/yaml.v3"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	// Unset network.dns fields fall back to their defaults in the accessors.
	return &config.Config{Network: config.NetworkConfig{KindBridgeCIDR: config.DefaultKindCIDR}}
}

func TestZoneKey(t *testing.T) {
	if got := zoneKey("dev.klimax.internal"); got != "/skydns/internal/klimax/dev/" {
		t.Errorf("zoneKey = %q", got)
	}
	// The trailing slash is what keeps a purge of "dev" off "dev2".
	if strings.HasPrefix(zoneKey("dev2.klimax.internal"), zoneKey("dev.klimax.internal")) {
		t.Error("purging one cluster would match another cluster's records")
	}
}

func TestParseRecords(t *testing.T) {
	// Real `etcdctl get --prefix` output shape from ExternalDNS v0.22 (keys and
	// values on alternating lines; TXT ownership records carry "text", not "host").
	out := `/skydns/internal/klimax/dev/default/a-web/299d5f18
{"text":"\"heritage=external-dns,external-dns/owner=dev\"","targetstrip":1}
/skydns/internal/klimax/dev/default/web/0f01a28f
{"host":"172.30.1.1","ttl":0}
/skydns/internal/klimax/dev/custom/10036216
{"host":"172.30.1.2","ttl":0}`
	got := parseRecords(out)
	want := []Record{
		{Name: "custom.dev.klimax.internal", IP: "172.30.1.2"},
		{Name: "web.default.dev.klimax.internal", IP: "172.30.1.1"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("record %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if parseRecords("") != nil {
		t.Error("empty output should parse to no records")
	}
}

func TestCorefile(t *testing.T) {
	cf := Corefile(testConfig(t))
	for _, want := range []string{
		"klimax.internal:53 {",
		"endpoint http://172.30.255.52:2379",
		"path /skydns",
		"success 9984 30",
		"denial 9984 5",
		"reload",
	} {
		if !strings.Contains(cf, want) {
			t.Errorf("Corefile missing %q:\n%s", want, cf)
		}
	}
}

func TestExternalDNSManifest(t *testing.T) {
	m := ExternalDNSManifest(testConfig(t), "dev")
	for _, want := range []string{
		"--provider=coredns",
		"--txt-owner-id=dev",
		"--domain-filter=dev.klimax.internal",
		"--policy=sync",
		// Without it, headless Services publish pod IPs and host-network pods node IPs.
		"--service-type-filter=LoadBalancer",
		// Must reach the pod unrendered: the Helm chart's tpl turned this into
		// "..dev.klimax.internal" and every record landed under an empty name.
		"--fqdn-template={{.Name}}.{{.Namespace}}.dev.klimax.internal",
		"--combine-fqdn-annotation",
		"value: http://172.30.255.52:2379",
		ExternalDNSImage,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest missing %q", want)
		}
	}
	// Every document must parse.
	dec := yaml.NewDecoder(strings.NewReader(m))
	kinds := 0
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("manifest is not valid YAML: %v", err)
		}
		if doc["kind"] == nil {
			t.Errorf("document without kind: %v", doc)
		}
		kinds++
	}
	if kinds != 5 {
		t.Errorf("got %d documents, want 5 (Namespace, SA, ClusterRole, Binding, Deployment)", kinds)
	}
}

func TestResolverContent(t *testing.T) {
	cfg := testConfig(t)
	if got := ResolverPath(cfg); got != "/etc/resolver/klimax.internal" {
		t.Errorf("ResolverPath = %q", got)
	}
	c := ResolverContent(cfg)
	if !strings.HasPrefix(c, resolverMarker) {
		t.Error("resolver file must start with the marker, or removal would skip it")
	}
	if !strings.Contains(c, "\nnameserver 172.30.255.53\n") {
		t.Errorf("unexpected content: %q", c)
	}
}
