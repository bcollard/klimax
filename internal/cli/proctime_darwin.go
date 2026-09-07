//go:build darwin

package cli

import (
	"time"

	"golang.org/x/sys/unix"
)

// processStartTime returns when pid was launched, read from the kernel's proc
// table. There is no /proc on macOS, and `ps` only reports a locale-formatted
// date, so go straight to the sysctl.
func processStartTime(pid int) (time.Time, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return time.Time{}, err
	}
	tv := kp.Proc.P_starttime
	return time.Unix(tv.Sec, int64(tv.Usec)*1000), nil
}
