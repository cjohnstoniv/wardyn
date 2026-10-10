// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerio

import (
	"io"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/google/uuid"
)

func TestOutputSettlementRequiresProducerCompletionAndAck(t *testing.T) {
	s, err := New(runnertest.NewFake(), uuid.NewString(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	out, err := s.Output(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BindOutput(id, "sandbox"); err != nil {
		t.Fatal(err)
	}
	if _, err := out.Write([]byte("tail")); err != nil {
		t.Fatal(err)
	}
	if _, settled, err := s.OutputSettled(t.Context(), id, "sandbox"); err != nil || settled {
		t.Fatal("live producer settled")
	}
	b := s.outputs[id]
	b.EndDrain(nil)
	if _, settled, err := s.OutputSettled(t.Context(), id, "sandbox"); err != nil || settled {
		t.Fatal("unacknowledged output settled")
	}
	if err := b.ack(4); err != nil {
		t.Fatal(err)
	}
	if end, settled, err := s.OutputSettled(t.Context(), id, "sandbox"); err != nil || !settled || end != 4 {
		t.Fatalf("settlement: %d %v %v", end, settled, err)
	}
	if _, err := out.Write([]byte("late")); err != io.ErrClosedPipe {
		t.Fatalf("completed producer wrote late bytes: %v", err)
	}
	if _, _, err := s.OutputSettled(t.Context(), id, "foreign"); err == nil {
		t.Fatal("foreign output ref accepted")
	}
	if len(s.outputs) != 1 {
		t.Fatal("settlement forgot output")
	}
}
