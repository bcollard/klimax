package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRewriteVMDisk(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	content := "vm:\n" +
		"  name: \"klimax\"\n" +
		"  memory: \"20GiB\"\n" +
		"  disk: \"40GiB\"  # grow with 'klimax disk resize'\n" +
		"registries:\n" +
		"  cacheStorage: \"host\"\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := rewriteVMDisk(p, "80GiB"); err != nil {
		t.Fatal(err)
	}

	out, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `  disk: "80GiB"`) {
		t.Errorf("disk not updated / indentation lost:\n%s", s)
	}
	if !strings.Contains(s, "# grow with 'klimax disk resize'") {
		t.Errorf("inline comment was dropped:\n%s", s)
	}
	if !strings.Contains(s, `memory: "20GiB"`) {
		t.Errorf("unrelated line was changed:\n%s", s)
	}
	if strings.Contains(s, "40GiB") {
		t.Errorf("old size still present:\n%s", s)
	}
}

// A stray `disk:` key elsewhere in the file must not be rewritten too.
func TestRewriteVMDiskOnlyFirstMatch(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	content := "vm:\n  disk: \"40GiB\"\nsomethingElse:\n  disk: \"5GiB\"\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rewriteVMDisk(p, "80GiB"); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(p)
	if s := string(out); !strings.Contains(s, `  disk: "5GiB"`) {
		t.Errorf("a later disk: key was rewritten:\n%s", s)
	}
}

func TestRewriteVMDiskMissingLine(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte("vm:\n  cpus: 8\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rewriteVMDisk(p, "80GiB"); err == nil {
		t.Error("expected an error when no vm.disk line is present")
	}
}

func TestRewriteLimaDisk(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "lima.yaml")
	// Lima writes the instance config unquoted, with other top-level keys around it.
	content := "vmType: vz\ncpus: 8\nmemory: 20GiB\ndisk: 40GiB\nmountType: virtiofs\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := rewriteLimaDisk(p, "80GiB"); err != nil {
		t.Fatal(err)
	}

	out, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "\ndisk: 80GiB\n") {
		t.Errorf("disk not updated (or quoting introduced):\n%s", s)
	}
	if !strings.Contains(s, "memory: 20GiB") || !strings.Contains(s, "mountType: virtiofs") {
		t.Errorf("unrelated lines were changed:\n%s", s)
	}

	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o644 {
		t.Errorf("file permissions changed: got %O, want 0644", perm)
	}
}

func TestRewriteLimaDiskIgnoresNestedDiskKeys(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "lima.yaml")
	// Only the top-level (column 0) disk: key is the instance disk size.
	content := "additionalDisks:\n  - disk: extra\ndisk: 40GiB\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rewriteLimaDisk(p, "80GiB"); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(p)
	s := string(out)
	if !strings.Contains(s, "  - disk: extra") {
		t.Errorf("nested disk key was rewritten:\n%s", s)
	}
	if !strings.Contains(s, "\ndisk: 80GiB") {
		t.Errorf("top-level disk not updated:\n%s", s)
	}
}
