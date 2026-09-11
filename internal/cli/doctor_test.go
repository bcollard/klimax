package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReportOK(t *testing.T) {
	tests := []struct {
		name   string
		checks []doctorCheck
		want   bool
	}{
		{"no checks", nil, true},
		{"all ok", []doctorCheck{{Status: checkOK}, {Status: checkOK}}, true},
		{"warnings do not fail the report", []doctorCheck{{Status: checkOK}, {Status: checkWarn}}, true},
		{"one failure fails the report", []doctorCheck{{Status: checkOK}, {Status: checkFail}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reportOK(&doctorReport{Checks: tt.checks}); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAnyFixable(t *testing.T) {
	tests := []struct {
		name   string
		checks []doctorCheck
		want   bool
	}{
		{"nothing failing", []doctorCheck{{Status: checkOK, Fixable: true}}, false},
		{"failing but not fixable", []doctorCheck{{Status: checkFail, Fixable: false}}, false},
		{"failing and fixable", []doctorCheck{{Status: checkFail, Fixable: true}}, true},
		{"a warning is never fixable", []doctorCheck{{Status: checkWarn, Fixable: true}}, false},
		{"mixed", []doctorCheck{{Status: checkFail}, {Status: checkFail, Fixable: true}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := anyFixable(&doctorReport{Checks: tt.checks}); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// applyDoctorFixes must never touch a check that is passing or that klimax
// cannot repair itself — a nil env would panic if it tried.
func TestApplyDoctorFixesSkipsIneligibleChecks(t *testing.T) {
	rep := &doctorReport{Checks: []doctorCheck{
		{ID: checkIDRoute, Status: checkOK, Fixable: true},
		{ID: checkIDVM, Status: checkFail, Fixable: false},
		{ID: checkIDHostagent, Status: checkWarn, Fixable: false},
	}}
	applyDoctorFixes(t.Context(), rep, &doctorEnv{})

	for _, c := range rep.Checks {
		if c.Fixed != nil {
			t.Errorf("check %q should not have been attempted, got Fixed=%v", c.ID, *c.Fixed)
		}
	}
}

// A fixable check with no running VM must record the failure on the check
// rather than silently reporting success.
func TestApplyDoctorFixesRecordsFailureWithoutGuest(t *testing.T) {
	rep := &doctorReport{Checks: []doctorCheck{
		{ID: checkIDIPTables, Status: checkFail, Fixable: true},
	}}
	applyDoctorFixes(t.Context(), rep, &doctorEnv{})

	c := rep.Checks[0]
	if c.Fixed == nil || *c.Fixed {
		t.Fatalf("expected Fixed=false, got %v", c.Fixed)
	}
	if c.FixError == "" {
		t.Error("expected a FixError explaining why the fix could not run")
	}
	if c.Status != checkFail {
		t.Errorf("a failed fix must leave the check failing, got %q", c.Status)
	}
}

func TestCheckRosettaHostNotRequestedNeverFails(t *testing.T) {
	// Whatever the host is, not asking for Rosetta must never produce a failure.
	if got := checkRosettaHost(false); got.Status == checkFail {
		t.Errorf("vm.rosetta=false should never fail, got %+v", got)
	}
}

// processAlive must actually check existence. os.FindProcess — what this
// replaced — succeeds unconditionally on Unix, so a test that passes with
// FindProcess would prove nothing; the dead-pid case is the one that matters.
func TestProcessAlive(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Error("the test process itself should be reported alive")
	}
	if !processAlive(1) {
		t.Error("pid 1 is alive but owned by root; EPERM must count as alive")
	}
	// Above the default pid_max on macOS and Linux, so it cannot be in use.
	if processAlive(0x7FFFFFFE) {
		t.Error("an impossible pid must not be reported alive")
	}
}

func TestProcessExePathResolvesSelf(t *testing.T) {
	got, err := processExePath(os.Getpid())
	if err != nil {
		t.Fatalf("could not resolve own exe path: %v", err)
	}
	if got == "" {
		t.Fatal("got an empty exe path")
	}
	// The test binary's own path should match what the OS reports for us.
	self, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable unavailable: %v", err)
	}
	if !sameFile(got, self) {
		t.Errorf("processExePath(%d) = %q, os.Executable = %q", os.Getpid(), got, self)
	}
}

func TestProcessExePathDeadPid(t *testing.T) {
	if _, err := processExePath(0x7FFFFFFE); err == nil {
		t.Error("expected an error for a pid that does not exist")
	}
}

// Homebrew installs klimax as a symlink into the Caskroom, and ps and
// os.Executable disagree about which side of it they report — so a plain string
// compare produces a false "different binary" warning.
func TestSameFileFollowsSymlinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "klimax-real")
	if err := os.WriteFile(real, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "klimax-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	if !sameFile(real, link) {
		t.Error("a symlink and its target must compare equal")
	}
	if !sameFile(real, real) {
		t.Error("identical paths must compare equal")
	}

	other := filepath.Join(dir, "klimax-other")
	if err := os.WriteFile(other, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if sameFile(real, other) {
		t.Error("distinct files must not compare equal")
	}
	// Unresolvable paths must not silently compare equal.
	if sameFile(filepath.Join(dir, "gone-a"), filepath.Join(dir, "gone-b")) {
		t.Error("two nonexistent paths must not compare equal")
	}
}

func TestProcessStartTimeIsSane(t *testing.T) {
	start, err := processStartTime(os.Getpid())
	if err != nil {
		t.Skipf("process start time unavailable on this platform: %v", err)
	}
	if start.IsZero() || start.After(time.Now()) {
		t.Errorf("implausible start time %v", start)
	}
	// The test binary cannot have been running for a year.
	if time.Since(start) > 365*24*time.Hour {
		t.Errorf("start time %v is implausibly old", start)
	}
}

// The case plain path comparison cannot see: same path, but the file behind it
// changed after the process launched.
func TestBinaryReplacedSinceLaunch(t *testing.T) {
	if _, err := processStartTime(os.Getpid()); err != nil {
		t.Skipf("process start time unavailable on this platform: %v", err)
	}
	dir := t.TempDir()

	// Written now, so newer than this process — the "replaced" case.
	fresh := filepath.Join(dir, "fresh")
	if err := os.WriteFile(fresh, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if replaced, _, _ := binaryReplacedSinceLaunch(fresh, os.Getpid()); !replaced {
		t.Error("a file written after the process started must count as replaced")
	}

	// Backdated well before the process started — the normal case.
	old := filepath.Join(dir, "old")
	if err := os.WriteFile(old, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	if replaced, _, _ := binaryReplacedSinceLaunch(old, os.Getpid()); replaced {
		t.Error("a file older than the process must not count as replaced")
	}

	// Unreadable path must not produce a false warning.
	if replaced, _, _ := binaryReplacedSinceLaunch(filepath.Join(dir, "nope"), os.Getpid()); replaced {
		t.Error("a missing file must not count as replaced")
	}
	// An impossible pid means we cannot tell — must stay quiet.
	if replaced, _, _ := binaryReplacedSinceLaunch(fresh, 0x7FFFFFFE); replaced {
		t.Error("an unknown process must not produce a warning")
	}
}
