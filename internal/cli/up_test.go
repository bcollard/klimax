package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRewriteNodeVersion(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	content := "kind:\n" +
		"  # kindest/node image tag\n" +
		"  nodeVersion: \"v1.32.0\"  # inline note\n" +
		"  metalLBVersion: \"v0.14.9\"\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := rewriteNodeVersion(p, "v1.35.0"); err != nil {
		t.Fatal(err)
	}

	out, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `  nodeVersion: "v1.35.0"`) {
		t.Errorf("nodeVersion not updated / indentation lost:\n%s", s)
	}
	if !strings.Contains(s, "# inline note") {
		t.Errorf("inline comment was dropped:\n%s", s)
	}
	if !strings.Contains(s, "# kindest/node image tag") {
		t.Errorf("preceding comment was dropped:\n%s", s)
	}
	if !strings.Contains(s, `metalLBVersion: "v0.14.9"`) {
		t.Errorf("unrelated line was changed:\n%s", s)
	}
	if strings.Contains(s, "v1.32.0") {
		t.Errorf("old version still present:\n%s", s)
	}
}

func TestRewriteNodeVersionMissingLine(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte("kind:\n  metalLBVersion: \"v0.15.2\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rewriteNodeVersion(p, "v1.35.0"); err == nil {
		t.Error("expected an error when no nodeVersion line is present")
	}
}
