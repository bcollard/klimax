package vm

import (
	"strings"
	"testing"
)

const (
	gib16 = 16 * 1024 * 1024 * 1024
	gib32 = 32 * 1024 * 1024 * 1024
)

func msgs(ws []ResourceWarning) string {
	var b strings.Builder
	for _, w := range ws {
		b.WriteString(w.Message + " | " + w.Hint + "\n")
	}
	return b.String()
}

func TestCheckResourcesQuietWhenItFits(t *testing.T) {
	// The shipped defaults on a 10-core / 32 GiB Mac with plenty of disk.
	res := HostResources{CPUs: 10, MemoryBytes: gib32, FreeDisk: 200 * 1024 * 1024 * 1024}
	if got := CheckResources(res, 8, "20GiB", "20GiB", "30GiB"); len(got) != 0 {
		t.Errorf("defaults should not warn on a comfortable host:\n%s", msgs(got))
	}
}

func TestCheckResourcesFlagsTooManyCPUs(t *testing.T) {
	res := HostResources{CPUs: 4, MemoryBytes: gib32, FreeDisk: 1 << 40}
	got := CheckResources(res, 8, "8GiB", "20GiB", "30GiB")
	if len(got) != 1 || !strings.Contains(got[0].Message, "vm.cpus is 8") {
		t.Fatalf("expected a cpu warning, got:\n%s", msgs(got))
	}
	if !strings.Contains(got[0].Hint, "4 or fewer") {
		t.Errorf("hint should name the host's count: %q", got[0].Hint)
	}
}

// Equal CPU counts are allowed: the guest is scheduled by macOS, not pinned.
func TestCheckResourcesAllowsEqualCPUs(t *testing.T) {
	res := HostResources{CPUs: 8, MemoryBytes: gib32, FreeDisk: 1 << 40}
	if got := CheckResources(res, 8, "8GiB", "20GiB", "30GiB"); len(got) != 0 {
		t.Errorf("cpus == host cpus should not warn:\n%s", msgs(got))
	}
}

// The default 20GiB on a 16 GiB Mac — the case this check exists for.
func TestCheckResourcesFlagsMemoryLargerThanHost(t *testing.T) {
	res := HostResources{CPUs: 8, MemoryBytes: gib16, FreeDisk: 1 << 40}
	got := CheckResources(res, 8, "20GiB", "20GiB", "30GiB")
	if len(got) != 1 {
		t.Fatalf("expected exactly one memory warning, got:\n%s", msgs(got))
	}
	if !strings.Contains(got[0].Message, "vm.memory is 20GiB") || !strings.Contains(got[0].Message, "16GiB") {
		t.Errorf("warning should name both sizes: %q", got[0].Message)
	}
	if !strings.Contains(got[0].Hint, "swap") {
		t.Errorf("hint should explain the consequence: %q", got[0].Hint)
	}
}

// Above the headroom share but still under physical RAM: worth saying, but a
// different and softer message than "more than exists".
func TestCheckResourcesFlagsMemoryHeadroom(t *testing.T) {
	res := HostResources{CPUs: 8, MemoryBytes: gib16, FreeDisk: 1 << 40}
	got := CheckResources(res, 8, "14GiB", "20GiB", "30GiB")
	if len(got) != 1 {
		t.Fatalf("expected one headroom warning, got:\n%s", msgs(got))
	}
	if strings.Contains(got[0].Hint, "swap") {
		t.Errorf("headroom warning should not reuse the harder message: %q", got[0].Hint)
	}
	if !strings.Contains(got[0].Message, "little left for macOS") {
		t.Errorf("unexpected message: %q", got[0].Message)
	}
}

func TestCheckResourcesFlagsDiskCeiling(t *testing.T) {
	res := HostResources{CPUs: 8, MemoryBytes: gib32, FreeDisk: 20 * 1024 * 1024 * 1024}
	got := CheckResources(res, 8, "20GiB", "20GiB", "30GiB")
	if len(got) != 1 {
		t.Fatalf("expected one disk warning, got:\n%s", msgs(got))
	}
	// Sparse images mean this is a ceiling, not an immediate failure — the hint
	// must say so, or it reads as far more alarming than it is.
	if !strings.Contains(got[0].Hint, "grow on demand") {
		t.Errorf("hint should explain sparseness: %q", got[0].Hint)
	}
}

// Unavailable host facts must produce silence, not a bogus warning.
func TestCheckResourcesSkipsUnknownHostFacts(t *testing.T) {
	if got := CheckResources(HostResources{}, 64, "999GiB", "999GiB", "999GiB"); len(got) != 0 {
		t.Errorf("zeroed host facts should be skipped, got:\n%s", msgs(got))
	}
}

func TestCheckResourcesIgnoresUnsetImageDisk(t *testing.T) {
	res := HostResources{CPUs: 8, MemoryBytes: gib32, FreeDisk: 25 * 1024 * 1024 * 1024}
	if got := CheckResources(res, 8, "20GiB", "20GiB", ""); len(got) != 0 {
		t.Errorf("an unset imageDisk must not count toward the total:\n%s", msgs(got))
	}
}

func TestReadHostResourcesReturnsSomething(t *testing.T) {
	res := ReadHostResources(t.TempDir())
	if res.CPUs < 1 {
		t.Errorf("expected at least one CPU, got %d", res.CPUs)
	}
	if res.FreeDisk == 0 {
		t.Error("expected non-zero free disk for a temp dir")
	}
}
