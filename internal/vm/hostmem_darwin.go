//go:build darwin

package vm

import "golang.org/x/sys/unix"

// hostMemoryBytes returns the Mac's physical memory.
func hostMemoryBytes() (uint64, error) {
	return unix.SysctlUint64("hw.memsize")
}
