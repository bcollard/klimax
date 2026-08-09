package cli

import (
	"strings"
	"testing"
)

func TestShellQuote(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"simple", []string{"docker", "ps"}, `'docker' 'ps'`},
		{"flags kept verbatim", []string{"docker", "ps", "-a"}, `'docker' 'ps' '-a'`},
		{"embedded spaces stay one word", []string{"bash", "-c", "kind get clusters"}, `'bash' '-c' 'kind get clusters'`},
		{"single quotes escaped", []string{"sh", "-c", "echo 'hi'"}, `'sh' '-c' 'echo '\''hi'\'''`},
		{"globs not expanded locally", []string{"ls", "/tmp/*.yaml"}, `'ls' '/tmp/*.yaml'`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shellQuote(tt.args); got != tt.want {
				t.Errorf("shellQuote(%q) = %s, want %s", tt.args, got, tt.want)
			}
		})
	}
}

func TestSplitGuestPath(t *testing.T) {
	tests := []struct {
		in        string
		vmName    string
		wantPath  string
		wantGuest bool
	}{
		{"vm:/tmp/file", "klimax", "/tmp/file", true},
		{"klimax:/tmp/file", "klimax", "/tmp/file", true},
		{"./local/file", "klimax", "./local/file", false},
		{"/abs/local/file", "klimax", "/abs/local/file", false},
		// A relative path that merely contains a colon is not a guest path.
		{"weird:name.txt", "klimax", "weird:name.txt", false},
	}
	for _, tt := range tests {
		gotPath, gotGuest := splitGuestPath(tt.in, tt.vmName)
		if gotPath != tt.wantPath || gotGuest != tt.wantGuest {
			t.Errorf("splitGuestPath(%q, %q) = (%q, %v), want (%q, %v)",
				tt.in, tt.vmName, gotPath, gotGuest, tt.wantPath, tt.wantGuest)
		}
	}
}

func TestSudoersSnippet(t *testing.T) {
	got := sudoersSnippet("alice", "172.30.0.0/16")
	// The two rules must match exactly what routing.EnsureRoute/DeleteRoute run.
	wantAdd := "alice ALL=(root) NOPASSWD: /sbin/route -n add -net 172.30.0.0/16 *"
	wantDel := "alice ALL=(root) NOPASSWD: /sbin/route -n delete -net 172.30.0.0/16"
	for _, want := range []string{wantAdd, wantDel} {
		if !strings.Contains(got, want) {
			t.Errorf("snippet missing rule %q:\n%s", want, got)
		}
	}
}

func TestFindRouteRules(t *testing.T) {
	const cidr = "172.30.0.0/16"
	// Shaped like real `sudo -l` output, which reformats the rules it lists.
	installed := `User alice may run the following commands on host:
    (ALL) ALL
    (root : wheel) NOSETENV: NOPASSWD: /opt/socket_vmnet/bin/socket_vmnet --pidfile\=/x
    (root) NOPASSWD: /sbin/route -n add -net 172.30.0.0/16 *
    (root) NOPASSWD: /sbin/route -n delete -net 172.30.0.0/16
`
	if add, del := findRouteRules(installed, cidr); !add || !del {
		t.Errorf("expected both rules found, got add=%v del=%v", add, del)
	}

	// Only the add rule present.
	partial := "    (root) NOPASSWD: /sbin/route -n add -net 172.30.0.0/16 *\n"
	if add, del := findRouteRules(partial, cidr); !add || del {
		t.Errorf("expected add-only, got add=%v del=%v", add, del)
	}

	// Blanket sudo access is not the same as the NOPASSWD route rules.
	if add, del := findRouteRules("    (ALL) ALL\n", cidr); add || del {
		t.Errorf("(ALL) ALL must not count as the route rules, got add=%v del=%v", add, del)
	}

	// Rules for a different CIDR must not match.
	other := "    (root) NOPASSWD: /sbin/route -n add -net 10.0.0.0/8 *\n" +
		"    (root) NOPASSWD: /sbin/route -n delete -net 10.0.0.0/8\n"
	if add, del := findRouteRules(other, cidr); add || del {
		t.Errorf("rules for another CIDR matched, got add=%v del=%v", add, del)
	}
}
