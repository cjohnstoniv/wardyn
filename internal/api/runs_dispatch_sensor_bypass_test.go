// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The host sensor cannot see inside a gVisor (CC2) or Kata (CC3) guest. On a
// deployment that runs the Kubernetes sensor, wardynd records that gap itself,
// once per run, as the daemon. CC3 is recorded everywhere, as before; CC2 only
// where the Kubernetes sensor runs.
func TestDispatch_SensorBypassRecordedByClass(t *testing.T) {
	cases := []struct {
		name      string
		class     types.ConfinementClass
		k8sSensor bool
		want      string // "" = no bypass row
	}{
		{"cc2 with k8s sensor", types.CC2, true, "cc2-gvisor-host-ebpf-blind"},
		{"cc3 with k8s sensor", types.CC3, true, "cc3-kata-host-ebpf-blind"},
		{"cc1 with k8s sensor", types.CC1, true, ""},
		{"cc2 without k8s sensor", types.CC2, false, ""},
		{"cc3 without k8s sensor", types.CC3, false, "cc3-kata-host-ebpf-blind"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rn := &killRaceRunner{fakeRunner: &fakeRunner{}}
			srv, st, audit, run := dispatchTeardownFixture(t, rn, types.RunPending)
			srv.cfg.GroundtruthK8sSensor = tc.k8sSensor
			run.ConfinementClass = tc.class
			st.mu.Lock()
			st.run.ConfinementClass = tc.class
			st.mu.Unlock()
			// End the dispatch at create: the bypass row is written before it.
			rn.onCreate = func() {
				st.mu.Lock()
				st.state = types.RunKilled
				st.mu.Unlock()
			}

			srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
				RunToken: "run-token", Image: "wardyn/claude-code:latest",
				Policy: types.RunPolicySpec{MinConfinementClass: tc.class},
			})

			var rows []types.AuditEvent
			for _, ev := range audit.snapshot() {
				if ev.Action == "kernel.sensor.bypass" && ev.RunID != nil && *ev.RunID == run.ID {
					rows = append(rows, ev)
				}
			}
			if tc.want == "" {
				if len(rows) != 0 {
					t.Fatalf("want no bypass row, got %d", len(rows))
				}
				return
			}
			if len(rows) != 1 {
				t.Fatalf("want exactly one bypass row, got %d", len(rows))
			}
			ev := rows[0]
			if ev.ActorType != types.ActorSystem || ev.Actor != "wardynd" {
				t.Errorf("actor = %s/%s, want system/wardynd", ev.ActorType, ev.Actor)
			}
			var data struct {
				Reason string `json:"reason"`
				RunID  string `json:"run_id"`
			}
			if err := json.Unmarshal(ev.Data, &data); err != nil {
				t.Fatal(err)
			}
			if data.Reason != tc.want || data.RunID != run.ID.String() {
				t.Errorf("data = %+v, want reason %q for run %s", data, tc.want, run.ID)
			}
		})
	}
}
