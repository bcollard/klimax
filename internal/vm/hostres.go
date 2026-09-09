package vm

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

// ReadHostResources gathers the host's CPU count, physical memory and free disk.
// Memory or disk may be zero when unavailable; callers skip the corresponding
// check rather than guessing.
func ReadHostResources(klimaxHome string) HostResources {
	res := HostResources{CPUs: runtime.NumCPU()}
	if mem, err := hostMemoryBytes(); err == nil {
		res.MemoryBytes = mem
	}
	var st unix.Statfs_t
	if err := unix.Statfs(klimaxHome, &st); err == nil {
		res.FreeDisk = uint64(st.Bavail) * uint64(st.Bsize)
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
