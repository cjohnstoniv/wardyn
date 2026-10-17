// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerio

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
)

// The org commits what it accepted (Options.OutputAccepted) before the runner
// is told: while the commit fails the runner gets no ACK and keeps the bytes;
// once it succeeds the ACK follows, never ahead of the committed end.
func TestOutputAccepted_CommitBeforeAck(t *testing.T) {
	server := outputServer(t)
	var fail atomic.Bool
	fail.Store(true)
	failed := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var committed int64
	var ahead []string
	sub, _ := orgProcessWith(t, server, remote.Options{OutputAccepted: func(_ context.Context, run uuid.UUID, accepted int64) error {
		mu.Lock()
		defer mu.Unlock()
		if ack := bufferState(server, run).Ack; ack > committed {
			ahead = append(ahead, "the runner was told before the org committed")
		}
		if fail.Load() {
			once.Do(func() { close(failed) })
			return errors.New("store unavailable")
		}
		committed = max(committed, accepted)
		return nil
	}})
	id := uuid.New()
	capture := &outputCapture{done: make(chan struct{})}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if _, err := sub.CreateSandbox(ctx, runner.SandboxSpec{RunID: id, ExecOutput: capture}); err != nil {
		t.Fatal(err)
	}
	done := drain(t, server, id, []byte("hello"))
	select {
	case <-failed:
	case <-ctx.Done():
		t.Fatal("the org never tried to commit the output it accepted")
	}
	if ack := bufferState(server, id).Ack; ack != 0 {
		t.Fatalf("runner ACK = %d while the org's commit fails, want 0", ack)
	}
	fail.Store(false)
	if err := waitFor(t, "drain", done); err != nil {
		t.Fatal(err)
	}
	select {
	case <-capture.done:
	case <-ctx.Done():
		t.Fatal("org output never completed")
	}
	st := bufferState(server, id)
	mu.Lock()
	defer mu.Unlock()
	if st.Ack != 5 || committed != 5 || len(ahead) != 0 {
		t.Fatalf("runner ACK %d, committed %d, %v; want 5, 5 and the commit first", st.Ack, committed, ahead)
	}
}
