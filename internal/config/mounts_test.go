package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateAcceptsGoodMounts(t *testing.T) {
	cfg := baseValidConfig()
	cfg.VM.Mounts = []Mount{
		{Location: "~/projects", Writable: true},
		{Location: "/Users/tester/conf"},
		{Location: "/Users/tester/data", MountPoint: "/srv/data", Writable: true},
	}
	if err := Validate(cfg); err != nil {
		t.Errorf("Validate rejected valid mounts: %v", err)
	}
}

func TestValidateRejectsBadMounts(t *testing.T) {
	tests := []struct {
		name   string
		mounts []Mount
		want   string
	}{
		{
			name:   "empty location",
			mounts: []Mount{{Location: "  "}},
			want:   "must not be empty",
		},
		{
			// filepath.Abs would resolve this against the working directory, so
			// the same config would mean different things per invocation.
			name:   "relative location",
			mounts: []Mount{{Location: "projects"}},
			want:   "must be an absolute path",
		},
		{
			name:   "dot-relative location",
			mounts: []Mount{{Location: "./projects"}},
			want:   "must be an absolute path",
		},
		{
			// Lima supports "~" and "~/x" only.
			name:   "unsupported tilde form",
			mounts: []Mount{{Location: "~other/projects"}},
			want:   "must be an absolute path",
		},
		{
			// There is no tilde expansion for guest paths — this would create a
			// directory literally named "~" in the VM.
			name:   "tilde mountPoint",
			mounts: []Mount{{Location: "/Users/tester/x", MountPoint: "~/x"}},
			want:   "absolute guest path",
		},
		{
			name:   "relative mountPoint",
			mounts: []Mount{{Location: "/Users/tester/x", MountPoint: "srv/x"}},
			want:   "absolute guest path",
		},
		{
			name:   "guest system path",
			mounts: []Mount{{Location: "/Users/tester/etc", MountPoint: "/etc"}},
			want:   "guest system path",
		},
		{
			// Shadowing the image store would hide the very images the
			// vm.imageDisk data disk exists to preserve.
			name:   "image store mountPoint",
			mounts: []Mount{{Location: "/Users/tester/images", MountPoint: "/var/lib/containerd"}},
			want:   "guest system path",
		},
		{
			// Lima silently merges these, last writable wins.
			name: "duplicate guest path",
			mounts: []Mount{
				{Location: "/Users/tester/a", MountPoint: "/srv/x"},
				{Location: "/Users/tester/b", MountPoint: "/srv/x"},
			},
			want: "both mount at guest path",
		},
		{
			name: "duplicate location",
			mounts: []Mount{
				{Location: "/Users/tester/a"},
				{Location: "/Users/tester/a/"},
			},
			want: "both mount at guest path",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseValidConfig()
			cfg.VM.Mounts = tt.mounts
			err := Validate(cfg)
			if err == nil {
				t.Fatalf("Validate accepted %+v, want error", tt.mounts)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error should mention %q, got: %v", tt.want, err)
			}
		})
	}
}

func TestMountExpand(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	got, err := Mount{Location: "~/projects"}.Expand()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "projects"); got.Location != want {
		t.Errorf("Location = %q, want %q", got.Location, want)
	}
}

func TestMountGuestPath(t *testing.T) {
	// With no mountPoint the guest path is the host path — the convention that
	// makes a host-path `docker -v` bind resolve inside the VM.
	if got := (Mount{Location: "/Users/tester/x"}).GuestPath(); got != "/Users/tester/x" {
		t.Errorf("GuestPath = %q, want the host path", got)
	}
	if got := (Mount{Location: "/Users/tester/x", MountPoint: "/srv/x"}).GuestPath(); got != "/srv/x" {
		t.Errorf("GuestPath = %q, want /srv/x", got)
	}
}

// TestDefaultConfigDeclaresMounts keeps vm.mounts present (and empty) in a
// freshly written config: MissingKeys compares against the marshaled defaults,
// so a key that marshals away would never be reported to someone whose config
// predates it.
func TestDefaultConfigDeclaresMounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := WriteDefaultConfig(path); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "mounts: []") {
		t.Errorf("default config should declare an empty vm.mounts:\n%s", body)
	}

	// And a config without the key is reported as missing it.
	missing, err := MissingKeys([]byte("vm:\n  name: klimax\n"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, k := range missing {
		if k == "vm.mounts" {
			found = true
		}
	}
	if !found {
		t.Errorf("MissingKeys = %v, want it to include vm.mounts", missing)
	}
}
