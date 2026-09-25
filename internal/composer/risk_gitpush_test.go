// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestGrade_GitPushAnyBranch pins the following. git_push_any_branch turns OFF
// branch-namespace confinement for a run's brokered pushes: with it on, the
// broker forwards a push only under refs/heads/wardyn/<run-id>/, which is what
// stops an agent rewriting main. Clamp already treats it as a privilege and
// forces it false unless the operator's ceiling sets it — but Grade, the rail
// a human reads before approving the run, scored it nowhere. An unambiguous
// widening was invisible on the one surface built to show widenings.
func TestGrade_GitPushAnyBranch(t *testing.T) {
	run := RunInput{Interactive: true}
	base := types.RunPolicySpec{MinConfinementClass: types.CC2, AutoStopAfterSec: 3600}

	off := Grade(run, base)
	if it := itemFor(off, "git_push_any_branch"); it != nil {
		t.Errorf("graded %+v with the field absent: the default posture is not a finding", it)
	}

	on := base
	on.GitPushAnyBranch = true
	it := itemFor(Grade(run, on), "git_push_any_branch")
	if it == nil {
		t.Fatal("git_push_any_branch=true is not graded at all")
	}
	if it.Level != RiskHigh {
		t.Errorf("level = %q, want %q", it.Level, RiskHigh)
	}
	if it.Value != "true" {
		t.Errorf("value = %q, want \"true\"", it.Value)
	}
	if !strings.Contains(strings.ToLower(it.Rationale), "branch") {
		t.Errorf("rationale %q does not say what is turned off", it.Rationale)
	}
	if OverallLevel(Grade(run, on)) != RiskHigh {
		t.Error("a run that may push to any branch must not grade below high overall")
	}
}

func itemFor(items []RiskItem, field string) *RiskItem {
	for i := range items {
		if items[i].Field == field {
			return &items[i]
		}
	}
	return nil
}
