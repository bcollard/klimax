package routing

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderNoNatScript(t *testing.T) {
	on := renderNoNatScript("172.30.0.0/16", LocalDNS{ServerIP: "172.30.255.53", Domain: "marina.internal"})
	for _, want := range []string{`KIND_CIDR="172.30.0.0/16"`, `DNS_IP="172.30.255.53"`, `DNS_DOMAIN="marina.internal"`, "--comment marina-dns"} {
		if !strings.Contains(on, want) {
			t.Errorf("script missing %q", want)
		}
	}
	if strings.Contains(on, "{{ .") {
		t.Error("unrendered placeholder left in script")
	}

	// Off: the variables are empty, which makes the script remove the rule.
	off := renderNoNatScript("172.30.0.0/16", LocalDNS{})
	if !strings.Contains(off, `DNS_IP=""`) {
		t.Error(`disabled DNS should render DNS_IP=""`)
	}

	// Both variants must at least parse as bash.
	for name, s := range map[string]string{"on": on, "off": off} {
		p := filepath.Join(t.TempDir(), name+".sh")
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("bash", "-n", p).CombinedOutput(); err != nil {
			t.Errorf("%s script has a bash syntax error: %v\n%s", name, err, out)
		}
	}
}
