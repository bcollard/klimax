//go:build !darwin

package vm

import "errors"

// hostMemoryBytes is darwin-only; elsewhere the caller skips the memory check.
func hostMemoryBytes() (uint64, error) {
	return 0, errors.New("physical memory size is only available on darwin")
}
