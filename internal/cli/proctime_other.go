//go:build !darwin

package cli

import (
	"errors"
	"time"
)

// processStartTime is implemented for darwin only. klimax targets macOS, and the
// staleness check that uses it exists for a macOS failure mode; elsewhere the
// caller treats the error as "cannot tell" and skips the check.
func processStartTime(int) (time.Time, error) {
	return time.Time{}, errors.New("process start time is only available on darwin")
}
