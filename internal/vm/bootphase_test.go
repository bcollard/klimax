package vm

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rec renders a hostagent log line in the shape logrus's JSON formatter emits.
func rec(msg string) string {
	return fmt.Sprintf(`{"level":"info","msg":%q,"time":"2026-09-11T11:50:29+02:00"}`, msg)
}

func writeLog(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ha.stderr.log")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLatestBootPhaseReportsCurrentRequirement(t *testing.T) {
	p := writeLog(t,
		rec("Waiting for the essential requirement 1 of 3: `ssh`"),
		rec("Waiting for the essential requirement 2 of 3: `user session is ready for ssh`"),
		rec("Waiting for the final requirement 1 of 1: `boot scripts must have finished`"),
	)
	if got, want := latestBootPhase(p), provisionPhase; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Once Lima says the requirement is met, naming it would report a step that
// already finished as if it were still running.
func TestLatestBootPhaseClearsOnSatisfied(t *testing.T) {
	p := writeLog(t,
		rec("Waiting for the final requirement 1 of 1: `boot scripts must have finished`"),
		rec("The final requirement 1 of 1 is satisfied"),
	)
	if got := latestBootPhase(p); got != "" {
		t.Errorf("expected no phase after satisfied, got %q", got)
	}
}

// The heartbeat must never be the reason a start fails or stalls.
func TestLatestBootPhaseToleratesBadInput(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		if got := latestBootPhase(filepath.Join(t.TempDir(), "nope.log")); got != "" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("non-json noise", func(t *testing.T) {
		p := writeLog(t, "not json at all", "", "{broken", rec("unrelated message"))
		if got := latestBootPhase(p); got != "" {
			t.Errorf("got %q", got)
		}
	})
}

// The log runs for the life of the VM; only the tail is read. A phase far
// beyond the window is correctly not reported, but the tail must still parse
// rather than being derailed by the record the offset cut in half.
func TestLatestBootPhaseReadsTailOfLargeLog(t *testing.T) {
	lines := []string{rec("Waiting for the essential requirement 1 of 3: `ssh`")}
	for i := 0; i < 4000; i++ {
		lines = append(lines, rec(fmt.Sprintf("Forwarding TCP from 0.0.0.0:%d to 127.0.0.1:%d", 9000+i, 9000+i)))
	}
	lines = append(lines, rec("Waiting for the final requirement 1 of 1: `boot scripts must have finished`"))

	p := writeLog(t, lines...)
	if st, _ := os.Stat(p); st.Size() <= bootPhaseTailBytes {
		t.Fatalf("fixture must exceed the tail window, got %d bytes", st.Size())
	}
	if got, want := latestBootPhase(p), provisionPhase; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A partial final line is normal: the hostagent is writing while we read.
func TestLatestBootPhaseIgnoresTruncatedTrailingLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ha.stderr.log")
	body := rec("Waiting for the final requirement 1 of 1: `boot scripts must have finished`") +
		"\n" + `{"level":"info","msg":"Forwarding TCP fr`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := latestBootPhase(p), provisionPhase; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
