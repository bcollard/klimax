package cli

import "testing"

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
