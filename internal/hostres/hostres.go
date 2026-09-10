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

	// memShare is the fraction of the Mac's RAM given to the VM. Half, flat:
	// macOS keeps the rest, and the VM reserves this whether or not it uses it.
	memShare = 0.5
	// minMemoryBytes keeps the VM usable on a very small machine.
	minMemoryBytes = 2 << 30 // 2 GiB
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

// DefaultMemoryBytes returns the RAM to give the VM: half the host's, leaving
// the other half to macOS. 16 GiB -> 8 GiB, 32 GiB -> 16 GiB, 64 GiB -> 32 GiB.
//
// Returns 0 when total is unknown, so the caller can fall back to a constant.
func DefaultMemoryBytes(total uint64) uint64 {
	if total == 0 {
		return 0
	}
	want := uint64(float64(total) * memShare)
	if want < minMemoryBytes {
		// Only reachable on a machine too small to run klimax well anyway; the
		// over-commit check still warns if this exceeds what is there.
		want = minMemoryBytes
	}
	if want > total {
		want = total
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
