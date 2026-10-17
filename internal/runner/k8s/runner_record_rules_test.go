// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

// The node record acceptance rules the organisation relies on runnerd to decide: "node powered off" only
// under (i), an acceptance list showing no holder; "no prior holder" only under (ii), on the no-association path and never on an empty acceptance list; and after a
// recorded name reuse only "node powered off", always by two people.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// recordRulesHarness is a bound proxy whose pod has a terminal status, so only the rule under test can
// refuse a record. noCause puts the run on the no-association path; reused records one name reuse.
func recordRulesHarness(t *testing.T, noCause, reused bool) *runnerHarness {
	t.Helper()
	h := newRunnerHarness(t)
	pod := h.boundProxy(t)
	terminate(pod, "containerd://p1")
	if err := h.cs.Tracker().Update(podsGVR, pod, testNamespace); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.mutate(h.runID, func(ev *runEvidence) (bool, error) {
		if noCause {
			ev.Pods[componentProxy].Assoc = assocU1
		}
		if reused {
			ev.Reuse = append(ev.Reuse, runner.NodeSample{Name: "n1", UID: "uid-n1-new", MachineID: "m7", SystemUUID: "s7", InternalIP: "10.0.0.9"})
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	return h
}

func acceptRecord(t *testing.T, h *runnerHarness, rec runner.NodeRecord) error {
	t.Helper()
	return h.d.AcceptNodeRecord(context.Background(), agentPodName(h.runID), rec)
}

func wantRefused(t *testing.T, err error, says string) {
	t.Helper()
	if !errors.Is(err, runner.ErrNodeRecordRefused) || !strings.Contains(err.Error(), says) {
		t.Fatalf("AcceptNodeRecord = %v, want a refusal saying %q", err, says)
	}
}

// TestRecordRule_PoweredOffOnlyUnderAnEmptyAcceptanceList: (i) is "node powered off" with no Node holding
// the name on the acceptance list sent at acceptance.
func TestRecordRule_PoweredOffOnlyUnderAnEmptyAcceptanceList(t *testing.T) {
	h := recordRulesHarness(t, false, false)
	wantRefused(t, acceptRecord(t, h, nodeRecordFixture(h, t, runner.AttestNodePoweredOff, "alice")), "still holds")
	h.dropNode(t, "n1")
	if err := acceptRecord(t, h, nodeRecordFixture(h, t, runner.AttestNodePoweredOff, "alice")); err != nil {
		t.Fatalf("node powered off under an empty acceptance list: %v", err)
	}
}

// TestRecordRule_NoPriorHolderOnlyOnTheNoAssociationPath: (ii) is never a causal run's release, even with a
// holder-bound terminal status.
func TestRecordRule_NoPriorHolderOnlyOnTheNoAssociationPath(t *testing.T) {
	h := recordRulesHarness(t, false, false)
	wantRefused(t, acceptRecord(t, h, nodeRecordFixture(h, t, runner.AttestNoPriorHolder, "alice", "bob")), "no-association path only")
}

// TestRecordRule_NoPriorHolderNeverOnAnEmptyAcceptanceList: with no Node holding the name there is no
// holder to bind the status to, so "no prior holder" is refused.
func TestRecordRule_NoPriorHolderNeverOnAnEmptyAcceptanceList(t *testing.T) {
	h := recordRulesHarness(t, true, false)
	h.dropNode(t, "n1")
	wantRefused(t, acceptRecord(t, h, nodeRecordFixture(h, t, runner.AttestNoPriorHolder, "alice", "bob")), "empty acceptance list")
}

// TestRecordRule_ReuseRefusesNoPriorHolder: after a recorded reuse only "node powered off" releases.
func TestRecordRule_ReuseRefusesNoPriorHolder(t *testing.T) {
	h := recordRulesHarness(t, true, true)
	h.dropNode(t, "n1")
	wantRefused(t, acceptRecord(t, h, nodeRecordFixture(h, t, runner.AttestNoPriorHolder, "alice", "bob")), "node name reused")
}

// TestRecordRule_ReuseNeedsTwoPeople: on a causal run with four-eyes off, a recorded reuse still makes
// "node powered off" a two-person record.
func TestRecordRule_ReuseNeedsTwoPeople(t *testing.T) {
	h := recordRulesHarness(t, false, true)
	h.dropNode(t, "n1")
	wantRefused(t, acceptRecord(t, h, nodeRecordFixture(h, t, runner.AttestNodePoweredOff, "alice")), "needs 2 distinct human approvers")
	if err := acceptRecord(t, h, nodeRecordFixture(h, t, runner.AttestNodePoweredOff, "alice", "bob")); err != nil {
		t.Fatalf("a two-person record after a reuse: %v", err)
	}
}

// TestRelayIPFencedCrossesTheWireAsItsCode: the fenced-IP refusal a create returns is sent as the
// relay_ip_fenced code, not as an internal error the org would read as a daemon fault.
func TestRelayIPFencedCrossesTheWireAsItsCode(t *testing.T) {
	wire := runnerwire.ErrorFor(fmt.Errorf("k8s: proxy pod %s: %w", testProxyIP, errRelayIPFenced))
	if wire.Code != runnerwire.CodeRelayIPFenced || !errors.Is(wire.Err(), runner.ErrRelayIPFenced) {
		t.Fatalf("fenced refusal crossed as %+v", wire)
	}
}
