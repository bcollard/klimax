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
	if got := zoneKey("dev.demo.internal"); got != "/skydns/internal/demo/dev/" {
		t.Errorf("zoneKey = %q", got)
	}
	// The trailing slash is what keeps a purge of "dev" off "dev2".
	if strings.HasPrefix(zoneKey("dev2.demo.internal"), zoneKey("dev.demo.internal")) {
		t.Error("purging one cluster would match another cluster's records")
	}
}

func TestParseRecords(t *testing.T) {
	// Real `etcdctl get --prefix` output shape from ExternalDNS v0.22 (keys and
	// values on alternating lines; TXT ownership records carry "text", not "host").
	out := `/skydns/internal/demo/dev/default/a-web/299d5f18
{"text":"\"heritage=external-dns,external-dns/owner=dev\"","targetstrip":1}
/skydns/internal/demo/dev/default/web/0f01a28f
{"host":"172.30.1.1","ttl":0}
/skydns/internal/demo/dev/custom/10036216
{"host":"172.30.1.2","ttl":0}`
	got := parseRecords(out)
	want := []Record{
		{Name: "custom.dev.demo.internal", IP: "172.30.1.2"},
		{Name: "web.default.dev.demo.internal", IP: "172.30.1.1"},
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
		"demo.internal:53 {",
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
	m := ExternalDNSManifest(testConfig(t), "dev", "")
	for _, want := range []string{
		"--provider=coredns",
		"--txt-owner-id=dev",
		"--domain-filter=dev.demo.internal",
		"--policy=sync",
		// Without it, headless Services publish pod IPs and host-network pods node IPs.
		"--service-type-filter=LoadBalancer",
		// Must reach the pod unrendered: the Helm chart's tpl turned this into
		// "..dev.demo.internal" and every record landed under an empty name.
		"--fqdn-template={{.Name}}.{{.Namespace}}.dev.demo.internal",
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
	if got := ResolverPath(cfg); got != "/etc/resolver/demo.internal" {
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

func TestOwnedKeys(t *testing.T) {
	// Two members publish into fleet zone lab; east owns gateway, west owns
	// portal and a deeper name under gateway that must survive east's purge.
	out := `/skydns/internal/demo/lab/a-gateway/1111
{"text":"\"heritage=external-dns,external-dns/owner=lab-east,external-dns/resource=service/gateway/gw\""}
/skydns/internal/demo/lab/gateway/2222
{"host":"172.30.4.3","ttl":0}
/skydns/internal/demo/lab/gateway/a-admin/3333
{"text":"\"heritage=external-dns,external-dns/owner=lab-west,external-dns/resource=service/gateway/admin\""}
/skydns/internal/demo/lab/gateway/admin/4444
{"host":"172.30.5.9","ttl":0}
/skydns/internal/demo/lab/a-portal/5555
{"text":"\"heritage=external-dns,external-dns/owner=lab-west\""}
/skydns/internal/demo/lab/portal/6666
{"host":"172.30.5.4","ttl":0}`
	got := ownedKeys(out, "lab-east")
	want := []string{"/skydns/internal/demo/lab/a-gateway/1111", "/skydns/internal/demo/lab/gateway/2222"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ownedKeys(east) = %v, want %v", got, want)
	}
	if n := len(ownedKeys(out, "lab-west")); n != 4 {
		t.Errorf("ownedKeys(west) returned %d keys, want 4 (two TXT + two A)", n)
	}
	if len(ownedKeys(out, "lab")) != 0 {
		t.Error("an owner name that is a prefix of another must not match")
	}
}

func TestExternalDNSManifestIngress(t *testing.T) {
	cfg := testConfig(t)
	// network.dns.ingress defaults to off: the template would name every
	// Ingress object too.
	if m := ExternalDNSManifest(cfg, "dev", ""); strings.Contains(m, "--source=ingress") {
		t.Error("the ingress source must be off by default")
	}
	on := true
	cfg.Network.DNS.Ingress = &on
	m := ExternalDNSManifest(cfg, "dev", "")
	if !strings.Contains(m, "--source=service\n            - --source=ingress\n") {
		t.Error("network.dns.ingress: true must add the ingress source after the service one")
	}
}

func TestExternalDNSManifestFleet(t *testing.T) {
	cfg := testConfig(t)
	m := ExternalDNSManifest(cfg, "lab-east", "lab")
	for _, want := range []string{"--domain-filter=lab-east.demo.internal", "--domain-filter=lab.demo.internal", "--txt-owner-id=lab-east"} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest missing %q", want)
		}
	}
	if strings.Contains(ExternalDNSManifest(cfg, "dev", ""), "--domain-filter=.demo.internal") {
		t.Error("no fleet must not add an empty-fleet filter")
	}
}
