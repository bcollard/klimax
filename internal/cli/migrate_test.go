package cli

import (
	"strings"
	"testing"
)

func TestRewriteLegacyConfig(t *testing.T) {
	in := `# my config
vm:
  name: "klimax"      # the VM
  cpus: 8
network:
  dns:
    domain: "klimax.internal"
kind:
  customDnsResolvers:
    - domain: "corp.internal"
`
	out := rewriteLegacyConfig(in)
	for _, want := range []string{`  name: "marina"      # the VM`, `domain: "marina.internal"`, `# my config`, `domain: "corp.internal"`, `cpus: 8`} {
		if !strings.Contains(out, want) {
			t.Errorf("rewritten config missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "klimax") {
		t.Errorf("klimax left in rewritten config:\n%s", out)
	}
	// A custom VM name is the user's choice and is kept.
	if got := rewriteLegacyConfig("vm:\n  name: lab\n"); got != "vm:\n  name: lab\n" {
		t.Errorf("custom VM name changed: %q", got)
	}
	if rewrittenVMName("klimax") != "marina" || rewrittenVMName("lab") != "lab" {
		t.Error("rewrittenVMName")
	}
}
