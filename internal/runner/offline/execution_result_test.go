// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"fmt"
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
	rows, err := allState(ctx, m)
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
	rows, err = allState(ctx, m)
	if err != nil || len(rows) != 1 || rows[0].Execution == nil || *rows[0].Execution != result {
		t.Fatalf("bound STATE differs from query: %+v %v", rows, err)
	}
	if err := m.BeforeRefCall(ctx, runnerwire.MethodExec, id.String(), gen); err == nil {
		t.Fatal("query made generation executable again")
	}
}

func TestStateConcurrentExecutionIntents(t *testing.T) {
	m, _, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	executionIntent(t, m, id, org, runnerwire.MethodExec)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		for ctx.Err() == nil {
			rows, err := allState(ctx, m)
			if ctx.Err() != nil {
				break
			}
			if err != nil {
				done <- err
				return
			}
			if len(rows) != 1 || rows[0].Execution == nil || !rows[0].Execution.Attempted || rows[0].Execution.Generation == uuid.Nil || rows[0].Execution.Operation != runnerwire.MethodExec {
				done <- fmt.Errorf("inconsistent execution snapshot: %+v", rows)
				return
			}
		}
		done <- nil
	}()
	<-started
	for range 256 {
		m.mu.Lock()
		lease := m.state.Runs[id].Lease
		m.mu.Unlock()
		lease.ExecutionGeneration = uuid.New()
		if err := m.Lease(context.Background(), lease); err != nil {
			t.Fatal(err)
		}
		if err := m.BeforeRefCall(context.Background(), runnerwire.MethodExec, id.String(), lease.ExecutionGeneration); err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
