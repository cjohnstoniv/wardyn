// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package orchestrator

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// samplingSubstrate is a fakeSubstrate that reports a CPU reading per ref.
type samplingSubstrate struct {
	*fakeSubstrate
	batch bool
	err   error
	asked [][]string
}

func (s *samplingSubstrate) BatchSample() bool { return s.batch }

func (s *samplingSubstrate) SampleCPU(_ context.Context, refs []string) (map[string]float64, error) {
	s.asked = append(s.asked, refs)
	out := map[string]float64{}
	for _, r := range refs {
		out[r] = 7
	}
	return out, s.err
}

// TestOrchestrator_SampleCPU_AsksEachSubstrateForItsOwnRefs: refs reach the
// substrate that owns them, one call each; a substrate with no sampler gives no
// reading; its error does not cost the others' readings.
func TestOrchestrator_SampleCPU_AsksEachSubstrateForItsOwnRefs(t *testing.T) {
	k8s := &samplingSubstrate{fakeSubstrate: &fakeSubstrate{name: "k8s", refPrefix: "k-"}, batch: true, err: runner.ErrActivityUnavailable}
	dkr := &samplingSubstrate{fakeSubstrate: &fakeSubstrate{name: "docker", refPrefix: "d-"}}
	plain := &fakeSubstrate{name: "vmm", refPrefix: "v-"}
	o := New(k8s, dkr, plain)
	for _, s := range []struct {
		sub  *fakeSubstrate
		refs []string
	}{{k8s.fakeSubstrate, []string{"k-1", "k-2"}}, {dkr.fakeSubstrate, []string{"d-1"}}, {plain, []string{"v-1"}}} {
		for _, r := range s.refs {
			o.mu.Lock()
			o.byRef[r] = s.sub
			o.mu.Unlock()
		}
	}

	got, err := o.SampleCPU(context.Background(), []string{"k-1", "k-2", "d-1", "v-1", "unrouted"})
	if !errors.Is(err, runner.ErrActivityUnavailable) {
		t.Errorf("err = %v, want the k8s substrate's unavailable carried up", err)
	}
	if len(got) != 3 || got["d-1"] != 7 || got["k-1"] != 7 || got["k-2"] != 7 {
		t.Errorf("readings = %v, want d-1, k-1 and k-2 only", got)
	}
	if len(k8s.asked) != 1 || !slices.Equal(k8s.asked[0], []string{"k-1", "k-2"}) || len(dkr.asked) != 1 {
		t.Errorf("k8s asked %v, docker asked %v; want one call each over its own refs", k8s.asked, dkr.asked)
	}
	if o.BatchSample() {
		t.Error("BatchSample = true with a per-ref substrate wired; it must set the budget for all")
	}
}

// TestOrchestrator_SampleCPU_NilRefsProbesAndNothingWiredIsUnavailable.
func TestOrchestrator_SampleCPU_NilRefsProbesAndNothingWiredIsUnavailable(t *testing.T) {
	k8s := &samplingSubstrate{fakeSubstrate: &fakeSubstrate{name: "k8s"}, batch: true}
	o := New(k8s)
	if _, err := o.SampleCPU(context.Background(), nil); err != nil || len(k8s.asked) != 1 || k8s.asked[0] != nil {
		t.Errorf("probe err = %v, asked %v; want one nil-refs call and no error", err, k8s.asked)
	}
	if !o.BatchSample() {
		t.Error("BatchSample = false for a sole batching substrate")
	}
	none := New(&fakeSubstrate{name: "vmm"})
	if _, err := none.SampleCPU(context.Background(), nil); !errors.Is(err, runner.ErrActivityUnavailable) {
		t.Errorf("err = %v, want unavailable when no substrate can sample", err)
	}
	if none.BatchSample() {
		t.Error("BatchSample = true with no sampler wired")
	}
}
