//go:build darwin

package hostres

import "golang.org/x/sys/unix"

// hostMemoryBytes returns the Mac's physical memory.
func hostMemoryBytes() (uint64, error) {
	return unix.SysctlUint64("hw.memsize")
}
