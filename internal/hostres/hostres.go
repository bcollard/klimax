package hostres

import (
	"fmt"
	"runtime"

	"github.com/docker/go-units"
	"golang.org/x/sys/unix"
)

// HostResources is what the Mac actually has, for sanity-checking a config that
// asks for more than exists.
type HostResources struct {
	CPUs        int
	MemoryBytes uint64
	FreeDisk    uint64 // free bytes on the filesystem holding the klimax home
}

// Read gathers the host's CPU count, physical memory and free disk.
// Memory or disk may be zero when unavailable; callers skip the corresponding
// check rather than guessing.
func Read() HostResources {
	return ReadFor("")
}

// ReadFor is Read, additionally reporting free space on the filesystem holding
// klimaxHome. Pass "" to skip the disk lookup.
func ReadFor(klimaxHome string) HostResources {
	res := HostResources{CPUs: runtime.NumCPU()}
	if mem, err := hostMemoryBytes(); err == nil {
		res.MemoryBytes = mem
	}
	if klimaxHome != "" {
		var st unix.Statfs_t
		if err := unix.Statfs(klimaxHome, &st); err == nil {
			res.FreeDisk = uint64(st.Bavail) * uint64(st.Bsize)
		}
	}
	return res
}

// ResourceWarning is one way a config over-commits the host.
type ResourceWarning struct {
	Message string
	Hint    string
}

// memoryHeadroom is the share of physical RAM above which a VM leaves macOS
// itself uncomfortably little. Below the hard "more than exists" check, but
// worth saying: the guest reserves this whether or not it uses it.
const memoryHeadroom = 0.75

// CheckResources reports where a config asks for more than the host has.
//
// The disk figures are compared against free space even though Lima's images
// are sparse — a 20GiB disk costs what it holds, not 20GiB. So that one is
// about eventual growth, not an immediate problem, and says so.
func CheckResources(res HostResources, cpus int, memory, disk, imageDisk string) []ResourceWarning {
	var out []ResourceWarning

	if res.CPUs > 0 && cpus > res.CPUs {
		out = append(out, ResourceWarning{
			Message: fmt.Sprintf("vm.cpus is %d but this Mac has %d", cpus, res.CPUs),
			Hint:    fmt.Sprintf("set vm.cpus to %d or fewer", res.CPUs),
		})
	}

	if res.MemoryBytes > 0 {
		if want, err := units.RAMInBytes(memory); err == nil && want > 0 {
			switch {
			case uint64(want) > res.MemoryBytes:
				out = append(out, ResourceWarning{
					Message: fmt.Sprintf("vm.memory is %s but this Mac has %s of RAM",
						memory, units.BytesSize(float64(res.MemoryBytes))),
					Hint: "the VM will not start, or will swap heavily — lower vm.memory",
				})
			case float64(want) > float64(res.MemoryBytes)*memoryHeadroom:
				out = append(out, ResourceWarning{
					Message: fmt.Sprintf("vm.memory is %s of this Mac's %s — little left for macOS",
						memory, units.BytesSize(float64(res.MemoryBytes))),
					Hint: "consider lowering vm.memory if the machine feels slow while the VM runs",
				})
			}
		}
	}

	if res.FreeDisk > 0 {
		total := int64(0)
		for _, v := range []string{disk, imageDisk} {
			if v == "" {
				continue
			}
			if b, err := units.RAMInBytes(v); err == nil {
				total += b
			}
		}
		if total > 0 && uint64(total) > res.FreeDisk {
			out = append(out, ResourceWarning{
				Message: fmt.Sprintf("vm.disk + vm.imageDisk total %s but only %s is free",
					units.BytesSize(float64(total)), units.BytesSize(float64(res.FreeDisk))),
				Hint: "both grow on demand rather than up front, so this is a ceiling, not an immediate problem",
			})
		}
	}

	return out
}

// Sizing rules for the defaults klimax picks when a config leaves a value unset.
const (
	// cpuShare is the fraction of the Mac's cores given to the VM.
	cpuShare = 0.75
	// minCPUs keeps a usable VM on very small machines.
	minCPUs = 2

	// memShare is the floor: never less than this fraction of RAM.
	memShare = 0.5
	// hostReserve is what macOS wants for itself. It is roughly fixed rather
	// than proportional — a 64 GiB Mac does not need 32 GiB for the desktop —
	// so on larger machines this lets the VM take more than half.
	hostReserve = 12 << 30 // 12 GiB
	// memCap stops the VM starving the host no matter how much RAM there is.
	memCap = 0.75
)

// DefaultCPUs returns the core count to give the VM: three quarters of the
// host's, rounded to nearest, never below minCPUs.
//
// Returns 0 when cores is unknown, so the caller can fall back to a constant.
func DefaultCPUs(cores int) int {
	if cores < 1 {
		return 0
	}
	n := int(float64(cores)*cpuShare + 0.5)
	if n < minCPUs {
		n = minCPUs
	}
	if n > cores {
		n = cores
	}
	return n
}

// DefaultMemoryBytes returns the RAM to give the VM: half the host's, or
// everything above a fixed reserve for macOS, whichever is larger — capped so
// the host always keeps a quarter.
//
// Half alone under-provisions a big Mac (32 GiB would yield 16); the reserve
// alone starves a small one (16 GiB would yield 4). Taking the larger gives 8
// of 16 and 20 of 32, which is the shape wanted.
//
// Returns 0 when total is unknown, so the caller can fall back to a constant.
func DefaultMemoryBytes(total uint64) uint64 {
	if total == 0 {
		return 0
	}
	half := uint64(float64(total) * memShare)
	var aboveReserve uint64
	if total > hostReserve {
		aboveReserve = total - hostReserve
	}
	want := half
	if aboveReserve > want {
		want = aboveReserve
	}
	if cap := uint64(float64(total) * memCap); want > cap {
		want = cap
	}
	return want
}

// RoundedGiB renders bytes as a whole-GiB size string ("20GiB"), which is what
// a config file should contain — nobody wants `memory: "19.6GiB"`.
func RoundedGiB(b uint64) string {
	const gib = 1 << 30
	n := b / gib
	if n < 1 {
		n = 1
	}
	return fmt.Sprintf("%dGiB", n)
}
