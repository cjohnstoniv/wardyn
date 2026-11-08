// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestScheduledRunsLimitWire: the door rides a restriction into its limits overlay, on the wire
// name the console mirrors, and an unset limit stays off the wire.
func TestScheduledRunsLimitWire(t *testing.T) {
	on := true
	var l RestrictionLimits
	if err := json.Unmarshal([]byte(`{"deny_scheduled_runs":true}`), &l); err != nil {
		t.Fatal(err)
	}
	if l.DenyScheduledRuns == nil || !*l.DenyScheduledRuns {
		t.Fatalf("restriction limits dropped deny_scheduled_runs: %+v", l)
	}
	if o := l.Overlay(); o.DenyScheduledRuns == nil || !*o.DenyScheduledRuns {
		t.Errorf("Overlay() dropped DenyScheduledRuns: %+v", o)
	}
	if o := (RestrictionLimits{}).Overlay(); o.DenyScheduledRuns != nil {
		t.Errorf("an unset restriction produced an opinion: %+v", o)
	}
	b, _ := json.Marshal(LimitsOverlay{DenyScheduledRuns: &on})
	if string(b) != `{"deny_scheduled_runs":true}` {
		t.Errorf("overlay wire = %s", b)
	}
	b, _ = json.Marshal(GovernanceLimits{DenyScheduledRuns: true})
	if string(b) != `{"deny_scheduled_runs":true}` {
		t.Errorf("limits wire = %s", b)
	}
	b, _ = json.Marshal(GovernanceLimits{})
	if strings.Contains(string(b), "deny_scheduled_runs") {
		t.Errorf("an open limit reached the wire: %s", b)
	}
}

// TestSelfDefinedComponentRubricWire: the rubric field is validated like the nine others and
// travels on its wire name; the posture says whether the run carries a self-defined component.
func TestSelfDefinedComponentRubricWire(t *testing.T) {
	b, _ := json.Marshal(AutonomyRubric{SelfDefinedComponent: AutonomyL1})
	if string(b) != `{"self_defined_component":"L1"}` {
		t.Errorf("rubric wire = %s", b)
	}
	if err := (AutonomyRubric{SelfDefinedComponent: AutonomyL3}).Validate(); err != nil {
		t.Errorf("a defined level was refused: %v", err)
	}
	err := (AutonomyRubric{SelfDefinedComponent: "L9"}).Validate()
	if err == nil || !strings.HasPrefix(err.Error(), "self_defined_component:") {
		t.Errorf("an undefined level = %v, want an error naming self_defined_component", err)
	}
	b, _ = json.Marshal(AutonomyPosture{Egress: AutonomyEgressSealed, Secrets: AutonomySecretsNone, Confinement: CC2})
	if strings.Contains(string(b), "custom_component") {
		t.Errorf("an unset custom_component reached the wire: %s", b)
	}
	b, _ = json.Marshal(AutonomyPosture{CustomComponent: true})
	if !strings.Contains(string(b), `"custom_component":true`) {
		t.Errorf("posture wire = %s, want custom_component", b)
	}
}

// TestScheduledRunsDeniedEndReason: the booked-run end token is defined, has its own sentence and
// does not name the deployment switch.
func TestScheduledRunsDeniedEndReason(t *testing.T) {
	if !RunEndScheduledRunsDenied.Valid() || string(RunEndScheduledRunsDenied) != "scheduled_runs_denied" {
		t.Fatalf("end reason = %q valid %v", RunEndScheduledRunsDenied, RunEndScheduledRunsDenied.Valid())
	}
	if s := RunEndScheduledRunsDenied.Sentence(); strings.Contains(s, "WARDYN_SCHEDULED_RUNS") || !strings.Contains(s, "turned off scheduled runs for your team") {
		t.Errorf("sentence = %q", s)
	}
}
