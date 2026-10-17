// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestWithMaskScope(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"no data":          {"", `{"mask_scope":"globals_only"}`},
		"null":             {"null", `{"mask_scope":"globals_only"}`},
		"empty object":     {"{}", `{"mask_scope":"globals_only"}`},
		"object":           {`{"a":1}`, `{"mask_scope":"globals_only","a":1}`},
		"padded object":    {" {\"a\":1} ", `{"mask_scope":"globals_only","a":1}`},
		"already labelled": {`{"mask_scope":"globals_only","a":1}`, `{"mask_scope":"globals_only","a":1}`},
		"an array":         {`[1]`, `[1]`},
		"not json":         {`{oops`, `{oops`},
	} {
		got := withMaskScope(json.RawMessage(tc.in))
		if string(got) != tc.want {
			t.Errorf("%s: withMaskScope(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
		if tc.in != "" && tc.want != tc.in && !json.Valid(got) {
			t.Errorf("%s: the label made the row's data invalid JSON: %q", name, got)
		}
	}
}

type sinkRecorder struct{ got []types.AuditEvent }

func (s *sinkRecorder) Record(_ context.Context, ev types.AuditEvent) error {
	s.got = append(s.got, ev)
	return nil
}

var _ audit.Recorder = (*sinkRecorder)(nil)

// A row of a run this process does not hold the complete manifest of carries
// mask_scope globals_only; a covered run's row, a run-less row and a process
// that has not armed the scope carry nothing.
func TestMaskingRecorder_LabelsRowsOfUncoveredRuns(t *testing.T) {
	covered, uncovered := uuid.New(), uuid.New()
	scope := &maskScope{}
	inner := &sinkRecorder{}
	rec := maskingRecorder{inner: inner, reg: secretmask.NewRegistry(), scope: scope}
	ctx := context.Background()
	row := func(run *uuid.UUID, data string) types.AuditEvent {
		return types.AuditEvent{RunID: run, Action: "run.dispatch", Data: json.RawMessage(data)}
	}

	// Not armed: nothing is labelled yet.
	_ = rec.Record(ctx, row(&uncovered, `{"a":1}`))
	if got := string(inner.got[0].Data); got != `{"a":1}` {
		t.Errorf("before the scope is armed the row carries %s", got)
	}

	scope.arm(func(id uuid.UUID) bool { return id == covered })
	_ = rec.Record(ctx, row(&uncovered, `{"a":1}`))
	_ = rec.Record(ctx, row(&covered, `{"a":1}`))
	_ = rec.Record(ctx, row(nil, `{"a":1}`))
	if got := string(inner.got[1].Data); got != `{"mask_scope":"globals_only","a":1}` {
		t.Errorf("an uncovered run's row carries %s", got)
	}
	if got := string(inner.got[2].Data); got != `{"a":1}` {
		t.Errorf("a covered run's row carries %s", got)
	}
	if got := string(inner.got[3].Data); got != `{"a":1}` {
		t.Errorf("a run-less row carries %s", got)
	}
	if (*maskScope)(nil).uncovered(uncovered) {
		t.Error("a nil scope labelled a run")
	}
}
