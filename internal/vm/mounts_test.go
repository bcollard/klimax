package vm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lima-vm/lima/v2/pkg/limatype"
	"github.com/lima-vm/lima/v2/pkg/ptr"
)

// limaYAMLFixture mirrors the shape of a real klimax-generated instance config:
// nested mappings, a sequence of mappings, and — the part that makes node-level
// surgery worth testing — a literal block scalar holding a shell script whose
// own '#' comments and blank lines must survive a re-encode untouched.
const limaYAMLFixture = `vmType: vz
vmOpts:
    vz:
        rosetta:
            enabled: true
            binfmt: true
cpus: 8
memory: 20GiB
disk: 20GiB
additionalDisks:
    - name: klimax-img
      format: true
      fsType: ext4
mounts:
    - location: /Users/tester/.klimax/registry-cache
      writable: true
mountType: virtiofs
provision:
    - mode: data
      content: |
        #!/bin/bash
        set -euo pipefail

        # Find the disk by label, not by device order.
        DEV="/dev/disk/by-label/lima-klimax-img"
        mount "${DEV}" /var/lib/containerd
      path: /usr/local/sbin/klimax-image-disk.sh
containerd:
    system: false
    user: false
user:
    name: lima
`

func writeFixture(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lima.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestReadInstanceMounts(t *testing.T) {
	got, err := ReadInstanceMounts(writeFixture(t, limaYAMLFixture))
	if err != nil {
		t.Fatal(err)
	}
	want := []NormalizedMount{{
		HostPath:  "/Users/tester/.klimax/registry-cache",
		GuestPath: "/Users/tester/.klimax/registry-cache",
		Writable:  true,
	}}
	if norm := NormalizeMounts(got); !equalNormalized(norm, want) {
		t.Errorf("mounts = %v, want %v", norm, want)
	}
}

func TestReadInstanceMountsAbsent(t *testing.T) {
	body := strings.Replace(limaYAMLFixture,
		"mounts:\n    - location: /Users/tester/.klimax/registry-cache\n      writable: true\n", "", 1)
	got, err := ReadInstanceMounts(writeFixture(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("mounts = %v, want none", got)
	}
}

// TestWriteInstanceMountsPreservesRest is the point of doing this with yaml
// nodes instead of regenerating the file: everything outside `mounts:` — and in
// particular the embedded shell script — must come back byte for byte.
func TestWriteInstanceMountsPreservesRest(t *testing.T) {
	path := writeFixture(t, limaYAMLFixture)
	mounts := []limatype.Mount{
		{Location: "/Users/tester/.klimax/registry-cache", Writable: ptr.Of(true)},
		{Location: "/Users/tester/projects", Writable: ptr.Of(true)},
		{Location: "/Users/tester/conf", Writable: ptr.Of(false)},
	}
	if err := WriteInstanceMounts(path, mounts); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)

	for _, fragment := range []string{
		"#!/bin/bash\n        set -euo pipefail\n\n        # Find the disk by label, not by device order.\n",
		"      content: |\n",
		"vmType: vz\n",
		"            binfmt: true\n",
		"    - name: klimax-img\n",
		"user:\n    name: lima\n",
	} {
		if !strings.Contains(got, fragment) {
			t.Errorf("re-encoded file lost fragment %q\n---\n%s", fragment, got)
		}
	}

	back, err := ReadInstanceMounts(path)
	if err != nil {
		t.Fatal(err)
	}
	if !MountsEqual(back, mounts) {
		t.Errorf("round-tripped mounts = %v, want %v", NormalizeMounts(back), NormalizeMounts(mounts))
	}
}

func TestWriteInstanceMountsInsertsWhenAbsent(t *testing.T) {
	body := strings.Replace(limaYAMLFixture,
		"mounts:\n    - location: /Users/tester/.klimax/registry-cache\n      writable: true\n", "", 1)
	path := writeFixture(t, body)

	mounts := []limatype.Mount{{Location: "/Users/tester/projects", Writable: ptr.Of(true)}}
	if err := WriteInstanceMounts(path, mounts); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	if !strings.Contains(got, "mounts:\n    - location: /Users/tester/projects") {
		t.Errorf("mounts block not inserted:\n%s", got)
	}
	// Inserted next to mountType, not appended after the provision scripts.
	if strings.Index(got, "mounts:") > strings.Index(got, "mountType:") {
		t.Errorf("mounts should be inserted before mountType:\n%s", got)
	}
}

func TestWriteInstanceMountsRemovesWhenEmpty(t *testing.T) {
	path := writeFixture(t, limaYAMLFixture)
	if err := WriteInstanceMounts(path, nil); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	if strings.Contains(got, "mounts:") && !strings.Contains(got, "mountType:") {
		t.Fatalf("unexpected content:\n%s", got)
	}
	// `mountType:` must survive; only the `mounts:` key is dropped.
	if strings.Contains(got, "\nmounts:") {
		t.Errorf("mounts key should have been removed:\n%s", got)
	}
	if !strings.Contains(got, "mountType: virtiofs") {
		t.Errorf("mountType should survive:\n%s", got)
	}
}

// TestWriteInstanceMountsPreservesMode guards the file staying readable by Lima
// with whatever permissions it was created with.
func TestWriteInstanceMountsPreservesMode(t *testing.T) {
	path := writeFixture(t, limaYAMLFixture)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteInstanceMounts(path, []limatype.Mount{{Location: "/Users/tester/projects"}}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("mode = %o, want 644", got)
	}
}

func TestMountsEqual(t *testing.T) {
	tests := []struct {
		name string
		a, b []limatype.Mount
		want bool
	}{
		{
			name: "identical",
			a:    []limatype.Mount{{Location: "/a", Writable: ptr.Of(true)}},
			b:    []limatype.Mount{{Location: "/a", Writable: ptr.Of(true)}},
			want: true,
		},
		{
			// Lima defaults writable to false, so nil and explicit false match.
			name: "nil writable equals explicit false",
			a:    []limatype.Mount{{Location: "/a"}},
			b:    []limatype.Mount{{Location: "/a", Writable: ptr.Of(false)}},
			want: true,
		},
		{
			// ...and mountPoint defaults to location.
			name: "nil mountPoint equals explicit same path",
			a:    []limatype.Mount{{Location: "/a"}},
			b:    []limatype.Mount{{Location: "/a", MountPoint: ptr.Of("/a")}},
			want: true,
		},
		{
			name: "writable differs",
			a:    []limatype.Mount{{Location: "/a", Writable: ptr.Of(true)}},
			b:    []limatype.Mount{{Location: "/a"}},
			want: false,
		},
		{
			name: "mountPoint differs",
			a:    []limatype.Mount{{Location: "/a", MountPoint: ptr.Of("/b")}},
			b:    []limatype.Mount{{Location: "/a"}},
			want: false,
		},
		{
			name: "extra mount",
			a:    []limatype.Mount{{Location: "/a"}},
			b:    []limatype.Mount{{Location: "/a"}, {Location: "/b"}},
			want: false,
		},
		{
			name: "both empty",
			a:    nil,
			b:    []limatype.Mount{},
			want: true,
		},
		{
			name: "trailing slash normalized away",
			a:    []limatype.Mount{{Location: "/a/"}},
			b:    []limatype.Mount{{Location: "/a"}},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MountsEqual(tt.a, tt.b); got != tt.want {
				t.Errorf("MountsEqual = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDescribeMounts(t *testing.T) {
	if got := DescribeMounts(nil); got != "    (none)" {
		t.Errorf("empty = %q", got)
	}
	got := DescribeMounts([]limatype.Mount{
		{Location: "/a", Writable: ptr.Of(true)},
		{Location: "/b"},
		{Location: "/c", MountPoint: ptr.Of("/mnt/c")},
	})
	want := "    /a (rw)\n    /b (ro)\n    /c → /mnt/c (ro)"
	if got != want {
		t.Errorf("DescribeMounts =\n%s\nwant\n%s", got, want)
	}
}

func equalNormalized(a, b []NormalizedMount) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
