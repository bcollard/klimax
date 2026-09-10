package hostres

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

func TestReadForReturnsSomething(t *testing.T) {
	res := ReadFor(t.TempDir())
	if res.CPUs < 1 {
		t.Errorf("expected at least one CPU, got %d", res.CPUs)
	}
	if res.FreeDisk == 0 {
		t.Error("expected non-zero free disk for a temp dir")
	}
}

const gib = uint64(1) << 30

func TestDefaultCPUs(t *testing.T) {
	tests := []struct{ cores, want int }{
		{0, 0},   // unknown — caller falls back
		{1, 1},   // floor is 2, but never more than the host has
		{2, 2},
		{4, 3},
		{8, 6},
		{10, 8}, // M1 Pro 10-core
		{12, 9},
		{16, 12},
	}
	for _, tt := range tests {
		if got := DefaultCPUs(tt.cores); got != tt.want {
			t.Errorf("DefaultCPUs(%d) = %d, want %d", tt.cores, got, tt.want)
		}
	}
}

func TestDefaultMemoryBytes(t *testing.T) {
	tests := []struct {
		ramGiB  uint64
		wantGiB uint64
		why     string
	}{
		{0, 0, "unknown — caller falls back"},
		{8, 4, "small Mac: the half floor protects macOS"},
		{16, 8, "half"},
		{24, 12, "half — reserve gives the same here"},
		{32, 20, "above the reserve, so more than half"},
		{36, 24, "all but the 12GiB reserve"},
		{64, 48, "capped at 75% rather than 52"},
		{128, 96, "capped at 75%"},
	}
	for _, tt := range tests {
		got := DefaultMemoryBytes(tt.ramGiB * gib)
		if got != tt.wantGiB*gib {
			t.Errorf("DefaultMemoryBytes(%dGiB) = %v, want %dGiB (%s)",
				tt.ramGiB, RoundedGiB(got), tt.wantGiB, tt.why)
		}
	}
}

// The VM must never be handed more than the machine has, at any size.
func TestDefaultsNeverExceedTheHost(t *testing.T) {
	for cores := 1; cores <= 64; cores++ {
		if got := DefaultCPUs(cores); got > cores {
			t.Fatalf("DefaultCPUs(%d) = %d, more than the host has", cores, got)
		}
	}
	for ram := uint64(1); ram <= 256; ram++ {
		got := DefaultMemoryBytes(ram * gib)
		if got > ram*gib {
			t.Fatalf("DefaultMemoryBytes(%dGiB) = %v, more than the host has", ram, RoundedGiB(got))
		}
		if float64(got) > float64(ram*gib)*0.75+1 {
			t.Fatalf("DefaultMemoryBytes(%dGiB) = %v, above the 75%% cap", ram, RoundedGiB(got))
		}
	}
}

// The computed default must not trip the over-commit warning it ships beside.
func TestComputedDefaultsDoNotWarn(t *testing.T) {
	for _, ram := range []uint64{8, 16, 24, 32, 64} {
		for _, cores := range []int{4, 8, 10, 16} {
			res := HostResources{CPUs: cores, MemoryBytes: ram * gib, FreeDisk: 500 * gib}
			mem := RoundedGiB(DefaultMemoryBytes(ram * gib))
			if w := CheckResources(res, DefaultCPUs(cores), mem, "20GiB", "30GiB"); len(w) != 0 {
				t.Errorf("%d cores / %dGiB: computed defaults warn:\n%s", cores, ram, msgs(w))
			}
		}
	}
}

func TestRoundedGiB(t *testing.T) {
	for _, tt := range []struct {
		in   uint64
		want string
	}{
		{20 * gib, "20GiB"},
		{20*gib + 512*1024*1024, "20GiB"}, // truncates rather than showing 20.5
		{0, "1GiB"},                       // never zero
	} {
		if got := RoundedGiB(tt.in); got != tt.want {
			t.Errorf("RoundedGiB(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
