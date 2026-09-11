package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestImageDiskNameFitsExt4Label guards the invariant behind a silent, data
// destroying failure mode.
//
// Lima labels a data disk "lima-<name>" and, on every boot, re-runs first-time
// setup unless /dev/disk/by-label/lima-<name> exists. ext4 truncates labels to
// 16 bytes, so a name longer than 11 chars makes that path unreachable and Lima
// reformats the disk on EVERY boot — silently wiping the container image store.
//
// Observed for real: vm name "klimax" with the old "-images" suffix produced
// "lima-klimax-images", which mkfs stored as "lima-klimax-imag".
func TestImageDiskNameFitsExt4Label(t *testing.T) {
	const ext4LabelMax = 16

	names := []string{
		"klimax",
		"k",
		"dev",
		"klimax-prod",
		"a-very-long-klimax-vm-name-indeed",
		"0123456789012345678901234567890123456789",
	}

	for _, vmName := range names {
		disk := ImageDiskName(vmName)
		if label := "lima-" + disk; len(label) > ext4LabelMax {
			t.Errorf("ImageDiskName(%q) = %q -> label %q is %d bytes, ext4 truncates past %d",
				vmName, disk, label, len(label), ext4LabelMax)
		}
		if disk == "" {
			t.Errorf("ImageDiskName(%q) returned an empty disk name", vmName)
		}
	}
}

// TestImageDiskNameDistinct ensures VM names that collide after truncation still
// get separate disks — otherwise two VMs would silently share one image store.
func TestImageDiskNameDistinct(t *testing.T) {
	a := ImageDiskName("a-very-long-klimax-vm-name-one")
	b := ImageDiskName("a-very-long-klimax-vm-name-two")
	if a == b {
		t.Errorf("distinct VM names produced the same disk name %q", a)
	}
}

func TestImageDiskNameStable(t *testing.T) {
	if got, want := ImageDiskName("klimax"), "klimax-img"; got != want {
		t.Errorf("ImageDiskName(\"klimax\") = %q, want %q", got, want)
	}
}

// The image disk is always on. An empty value is treated as unset and
// re-defaulted, so the `!= ""` guards elsewhere are unreachable for any config
// that came through LoadConfig.
//
// This was documented the other way round for several releases — "Empty =
// disabled (the image store lives on the VM's root disk)" — which was never
// achievable. Pinned here so reintroducing a disable path is a deliberate
// change that updates the docs with it.
func TestImageDiskIsAlwaysOn(t *testing.T) {
	for _, tc := range []struct{ name, yaml string }{
		{"key absent", "vm:\n  name: k\n"},
		{"explicitly empty", "vm:\n  name: k\n  imageDisk: \"\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(p, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(p)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.VM.ImageDisk != DefaultImageDisk {
				t.Errorf("ImageDisk = %q, want %q", cfg.VM.ImageDisk, DefaultImageDisk)
			}
		})
	}
}

// An explicit size must survive defaulting, or a configured disk would silently
// become 30GiB.
func TestImageDiskExplicitValueWins(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("vm:\n  name: k\n  imageDisk: 10GiB\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VM.ImageDisk != "10GiB" {
		t.Errorf("ImageDisk = %q, want 10GiB", cfg.VM.ImageDisk)
	}
}
