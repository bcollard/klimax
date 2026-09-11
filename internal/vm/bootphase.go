package vm

import (
	"encoding/json"
	"io"
	"os"
	"regexp"
	"strings"
)

// bootPhaseTailBytes is how much of the hostagent log to read when looking for
// the current phase. The log grows for the life of the VM — minutes of port
// forwarding chatter dwarf the boot itself — so only the tail is worth reading.
const bootPhaseTailBytes = 64 * 1024

// requirementRE matches Lima's phase announcements, e.g.
//
//	Waiting for the essential requirement 1 of 3: `ssh`
//	Waiting for the final requirement 1 of 1: `boot scripts must have finished`
//
// These are the only progress Lima reports during a start, and they go to
// logrus, which klimax quiets to error by default (resolveLimaLogLevel). Reading
// them back out of the log is what lets the heartbeat say something more useful
// than "still starting".
var requirementRE = regexp.MustCompile("^Waiting for the (?:essential|optional|final) requirement [0-9]+ of [0-9]+: `(.+)`$")

// latestBootPhase returns the most recent phase Lima announced in the hostagent
// log, or "" when the log has none yet, cannot be read, or the phase is already
// satisfied.
//
// Errors are deliberately swallowed: this only decorates a progress message, and
// a boot must never fail because a log file was mid-write or missing.
func latestBootPhase(logPath string) string {
	data, truncated, err := tailFile(logPath, bootPhaseTailBytes)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	// Only when the tail offset actually cut into the file is the first line a
	// partial record. Dropping it unconditionally loses the phase in the common
	// case of a log smaller than the window.
	if truncated && len(lines) > 1 {
		lines = lines[1:]
	}
	phase := ""
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var rec struct {
			Msg string `json:"msg"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if m := requirementRE.FindStringSubmatch(rec.Msg); m != nil {
			phase = m[1]
			continue
		}
		// A satisfied requirement clears the phase, so the heartbeat does not
		// keep naming a step that finished while the next one is unnamed.
		if strings.Contains(rec.Msg, "requirement") && strings.Contains(rec.Msg, "is satisfied") {
			phase = ""
		}
	}
	return phase
}

// tailFile reads up to the last n bytes of a file, reporting whether the file
// was longer than that — in which case the first line read is a partial record.
func tailFile(path string, n int64) (data []byte, truncated bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if off := st.Size() - n; off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return nil, false, err
		}
		truncated = true
	}
	data, err = io.ReadAll(f)
	return data, truncated, err
}
