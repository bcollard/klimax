package config

import "testing"

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
