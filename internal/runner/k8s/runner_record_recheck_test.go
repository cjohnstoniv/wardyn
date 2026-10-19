// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

// An accepted node record releases a run only for the tuple it was approved for: a holder of the node
// name latched after the comparison, or after acceptance, lapses it and the run stays contained.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// reuseOnLoad is the evidence store with one observer write landing at a chosen moment: once armed,
// the load after `skip` more loads first latches reuse into the stored evidence, as the periodic
// observer would between two of the driver's own reads.
type reuseOnLoad struct {
	*memEvidence
	t     *testing.T
	armed bool
	skip  int
	reuse runner.NodeSample
}

func (s *reuseOnLoad) LoadEvidence(id uuid.UUID) ([]byte, error) {
	if s.armed {
		if s.skip > 0 {
			s.skip--
		} else {
			s.armed = false
			ev := s.memEvidence.get(s.t, id)
			ev.Reuse = append(ev.Reuse, s.reuse)
			b, err := json.Marshal(ev)
			if err != nil {
				s.t.Fatal(err)
			}
			if err := s.memEvidence.SaveEvidence(id, b); err != nil {
				s.t.Fatal(err)
			}
		}
	}
	return s.memEvidence.LoadEvidence(id)
}

// TestNodeRecord_ReuseLatchedDuringAcceptanceLapsesIt: the tuple is compared again inside the write
// that stores the record, so a reuse latched after the first comparison refuses it.
func TestNodeRecord_ReuseLatchedDuringAcceptanceLapsesIt(t *testing.T) {
	h := newRunnerHarness(t)
	h.boundProxy(t)
	h.dropNode(t, "n1")
	rec := nodeRecordFixture(h, t, runner.AttestNodePoweredOff, "alice")
	store := &reuseOnLoad{memEvidence: h.ev, t: t, reuse: runner.NodeSample{Name: "n1", UID: "uid-n1-new", MachineID: "m7", SystemUUID: "s7"}}
	h.d.cfg.Evidence = store
	// The acceptance list is the last list before the comparison; the load after the list's own
	// write is the one that stores the record.
	h.cs.PrependReactor("list", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
		store.armed, store.skip = true, 1
		return false, nil, nil
	})
	err := h.d.AcceptNodeRecord(context.Background(), agentPodName(h.runID), rec)
	if !errors.Is(err, runner.ErrNodeRecordRefused) {
		t.Fatalf("AcceptNodeRecord after a reuse latched mid-acceptance = %v, want a refusal", err)
	}
	if ev := h.ev.get(t, h.runID); ev.Record != nil || len(ev.Reuse) != 1 {
		t.Fatalf("record %+v reuse %+v, want no record and the latched reuse", ev.Record, ev.Reuse)
	}
}

// TestNodeRecord_ReuseSeenAfterAcceptanceLapsesIt: a record accepted while the name was empty no longer
// releases the run once a later list shows another Node holding the name; the run stays contained.
func TestNodeRecord_ReuseSeenAfterAcceptanceLapsesIt(t *testing.T) {
	ctx := context.Background()
	h := newRunnerHarness(t)
	h.boundProxy(t)
	h.dropNode(t, "n1")
	rec := nodeRecordFixture(h, t, runner.AttestNodePoweredOff, "alice")
	if err := h.d.AcceptNodeRecord(ctx, agentPodName(h.runID), rec); err != nil {
		t.Fatal(err)
	}
	h.node(t, "n1", "uid-n1-new", "m7", "s7", "10.0.0.5")
	_ = h.d.StopProxy(ctx, agentPodName(h.runID)) // the run is held; how it is held is not this test's
	ev := h.ev.get(t, h.runID)
	if w := ev.Pods[componentProxy].Witness; w != nil && w.Arm == armRecord {
		t.Fatalf("witness %+v took the record arm after the reuse", w)
	}
	if len(ev.Reuse) != 1 {
		t.Fatalf("reuse %+v, want the later holder latched", ev.Reuse)
	}
}
