package limatemplate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bcollard/klimax/internal/config"
)

func TestBuildMountsIncludesRegistryCache(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	cfg := &config.Config{}
	cfg.Registries.CacheStorage = "host"

	got := BuildMounts(cfg)
	if len(got) != 1 {
		t.Fatalf("got %d mounts, want 1", len(got))
	}
	want := filepath.Join(home, ".klimax", "registry-cache")
	if got[0].Location != want {
		t.Errorf("Location = %q, want %q", got[0].Location, want)
	}
	// Registry containers bind this in at /var/lib/registry and write blobs to it.
	if got[0].Writable == nil || !*got[0].Writable {
		t.Error("the registry cache must be writable")
	}
}

func TestBuildMountsGuestCacheStorageSharesNothing(t *testing.T) {
	cfg := &config.Config{}
	cfg.Registries.CacheStorage = "guest"
	if got := BuildMounts(cfg); len(got) != 0 {
		t.Errorf("got %v, want no mounts", got)
	}
}

func TestBuildMountsAppendsUserMounts(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	cfg := &config.Config{}
	cfg.Registries.CacheStorage = "host"
	cfg.VM.Mounts = []config.Mount{
		{Location: "~/projects", Writable: true},
		{Location: "/Users/tester/conf"},
		{Location: "/Users/tester/data", MountPoint: "/srv/data", Writable: true},
	}

	got := BuildMounts(cfg)
	if len(got) != 4 {
		t.Fatalf("got %d mounts, want 4 (cache + 3)", len(got))
	}

	// The cache comes first; user mounts keep their config order after it.
	if got[0].Location != filepath.Join(home, ".klimax", "registry-cache") {
		t.Errorf("mounts[0] = %q, want the registry cache first", got[0].Location)
	}

	// "~" is expanded here rather than left to Lima, so the value written to the
	// instance config matches what drift detection reads back out of it.
	if want := filepath.Join(home, "projects"); got[1].Location != want {
		t.Errorf("mounts[1].Location = %q, want %q", got[1].Location, want)
	}
	if got[1].Writable == nil || !*got[1].Writable {
		t.Error("mounts[1] should be writable")
	}

	// writable is always written explicitly, so `false` is visible in the
	// instance config rather than implied by absence.
	if got[2].Writable == nil || *got[2].Writable {
		t.Error("mounts[2] should be explicitly non-writable")
	}
	// No mountPoint for the common case: Lima then defaults it to the location,
	// which is what makes a host-path `docker -v` bind resolve in the guest.
	if got[2].MountPoint != nil {
		t.Errorf("mounts[2].MountPoint = %q, want unset", *got[2].MountPoint)
	}

	if got[3].MountPoint == nil || *got[3].MountPoint != "/srv/data" {
		t.Errorf("mounts[3].MountPoint = %v, want /srv/data", got[3].MountPoint)
	}
}

func TestBuildSetsVirtiofsForMounts(t *testing.T) {
	cfg := &config.Config{}
	cfg.Registries.CacheStorage = "host"
	cfg.VM.Mounts = []config.Mount{{Location: "/Users/tester/projects", Writable: true}}

	y := Build(cfg)
	// mountType governs host-directory shares only. The VM's root disk and the
	// vm.imageDisk data disk are virtio-blk block devices and are unaffected.
	if y.MountType == nil || *y.MountType != "virtiofs" {
		t.Errorf("MountType = %v, want virtiofs", y.MountType)
	}
	if len(y.Mounts) != 2 {
		t.Errorf("got %d mounts in the built YAML, want 2", len(y.Mounts))
	}
}
