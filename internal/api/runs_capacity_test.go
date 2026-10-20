// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// Only a reason in runner.CapacityBlockerReasons, on any component's detail, is a capacity
// blocker; a pending proxy and the empty detail are not.
func TestCapacityBlockerReason(t *testing.T) {
	for detail, want := range map[string]string{
		"pod: Unschedulable: 0/3 nodes are available: Insufficient cpu.": "Unschedulable",
		"proxy: Pending: waiting":  "",
		"agent: ContainerCreating": "",
		"":                         "",
	} {
		if got := capacityBlockerReason(detail); got != want {
			t.Errorf("capacityBlockerReason(%q) = %q, want %q", detail, got, want)
		}
	}
}

// capacityRecordingStore records the opts the handler passes to RunCapacity.
type capacityRecordingStore struct {
	*authzStore
	got store.RunCapacityOpts
}

func (s *capacityRecordingStore) RunCapacity(_ context.Context, o store.RunCapacityOpts) (store.RunCapacity, error) {
	s.got = o
	return store.RunCapacity{}, nil
}

// The admin read answers 200 labelled as configured reservations with a generation time, and
// hands the store the deployment's runner kind and the capacity classifier.
func TestRunCapacityHandlerSuccess(t *testing.T) {
	st := &capacityRecordingStore{authzStore: newAuthzStore()}
	cfg := baseTestConfig(newHarness(t), st)
	cfg.RunnerTarget = "k8s"
	srv := New(cfg)

	w := do(t, srv, http.MethodGet, "/api/v1/admin/runs/capacity", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Basis       string    `json:"basis"`
		GeneratedAt time.Time `json:"generated_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	if body.Basis != "configured_reservations" {
		t.Errorf("basis = %q, want configured_reservations", body.Basis)
	}
	if body.GeneratedAt.IsZero() {
		t.Error("generated_at is zero")
	}
	if st.got.CurrentKind != cfg.RunnerTarget {
		t.Errorf("opts.CurrentKind = %q, want %q", st.got.CurrentKind, cfg.RunnerTarget)
	}
	if st.got.UnschedulableReason == nil {
		t.Error("opts.UnschedulableReason is nil")
	}
}
