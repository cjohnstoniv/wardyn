// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/google/uuid"
)

func TestExecutionResultRequiresExactDurableIntent(t *testing.T) {
	m, _, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	ctx := context.Background()
	gen := executionIntent(t, m, id, org, runnerwire.MethodExec)
	result, err := m.ExecutionResult(ctx, id.String(), gen)
	if err != nil || !result.Attempted || result.ExecID != "" || result.Generation != gen || result.Operation != runnerwire.MethodExec {
		t.Fatalf("ambiguous result %+v %v", result, err)
	}
	rows, err := m.State(ctx)
	if err != nil || len(rows) != 1 || rows[0].Execution == nil || *rows[0].Execution != result {
		t.Fatalf("ambiguous STATE differs from query: %+v %v", rows, err)
	}
	for _, wrong := range []uuid.UUID{uuid.Nil, uuid.New()} {
		if _, err := m.ExecutionResult(ctx, id.String(), wrong); err == nil {
			t.Fatal("unissued generation returned a result")
		}
	}
	if _, err := m.ExecutionResult(ctx, "foreign", gen); err == nil {
		t.Fatal("foreign reference returned a result")
	}
	if err := m.AfterRefCall(ctx, runnerwire.MethodExec, id.String(), "exec-1", gen); err != nil {
		t.Fatal(err)
	}
	result, err = m.ExecutionResult(ctx, id.String(), gen)
	if err != nil || result.ExecID != "exec-1" {
		t.Fatalf("bound result %+v %v", result, err)
	}
	rows, err = m.State(ctx)
	if err != nil || len(rows) != 1 || rows[0].Execution == nil || *rows[0].Execution != result {
		t.Fatalf("bound STATE differs from query: %+v %v", rows, err)
	}
	if err := m.BeforeRefCall(ctx, runnerwire.MethodExec, id.String(), gen); err == nil {
		t.Fatal("query made generation executable again")
	}
}
