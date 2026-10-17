// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerio

import (
	"errors"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"io"
	"os"
	"path/filepath"
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

func TestOutputSettlementRefusesMissingProducerHistory(t *testing.T) {
	s, err := New(runnertest.NewFake(), uuid.NewString(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	if end, settled, err := s.OutputSettled(t.Context(), id, "sandbox"); end != 0 || settled || !errors.Is(err, runner.ErrOutputUnrecoverable) {
		t.Fatalf("missing output falsely settled: %d %v %v", end, settled, err)
	}
	if len(s.outputs) != 0 {
		t.Fatal("settlement invented output history")
	}
}

// Erasing an org-erased run's output deletes its directory, stops its
// producer and survives a restart; it is idempotent.
func TestEraseOutputDeletesRetainedOutput(t *testing.T) {
	dir := t.TempDir()
	s, err := New(runnertest.NewFake(), uuid.NewString(), dir)
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
	if _, err := out.Write([]byte("transcript")); err != nil {
		t.Fatal(err)
	}
	if err := s.EraseOutput(id); err != nil {
		t.Fatal(err)
	}
	if _, err := out.Write([]byte("late")); err == nil {
		t.Fatal("erased producer still writes")
	}
	if _, _, err := s.OutputSettled(t.Context(), id, "sandbox"); !errors.Is(err, runner.ErrOutputUnrecoverable) {
		t.Fatalf("erased output settlement = %v", err)
	}
	if err := s.EraseOutput(id); err != nil {
		t.Fatalf("second erase = %v", err)
	}
	restarted, err := New(runnertest.NewFake(), uuid.NewString(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.outputs[id] != nil {
		t.Fatal("erased output came back after restart")
	}
}

// An erase interrupted after its rename leaves only the hidden name, which the next start removes.
func TestNewSweepsAnInterruptedErase(t *testing.T) {
	dir := t.TempDir()
	left := filepath.Join(dir, "output", erasingPrefix+uuid.NewString())
	if err := os.MkdirAll(filepath.Join(left, "partial"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := New(runnertest.NewFake(), uuid.NewString(), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(left); !os.IsNotExist(err) {
		t.Fatalf("interrupted erase survived a restart: %v", err)
	}
}
