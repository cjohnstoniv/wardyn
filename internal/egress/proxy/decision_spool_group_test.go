// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/google/uuid"
)

// An admitted request's record is appended without the sink's mutex: while the spool is busy with
// another commit, the sink still answers other requests, and concurrent records share one commit.
func TestLocalSpoolAppendsAnAdmittedRecordWithoutTheSinkLock(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "spool")
	runID := uuid.New()
	s, err := newLocalDecisionSink(dir, runID, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close(ctx)
	p := &Proxy{sink: s}
	admitted, release := p.admitSpoolHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://allowed.test/x", nil), "")
	if admitted == nil {
		t.Fatal("request refused")
	}
	defer release()
	id := admitted.Context().Value(spoolTicketKey{}).(uuid.UUID)

	// Hold the spool's process-shared lock, as a commit in progress does.
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.emit(decisionLog(egress.Request{RunID: runID, Host: "allowed.test", Method: "GET", Path: "/x", Time: time.Now(), AuditReservation: id}, egress.Allow, "policy:allowed"))
	}()
	// The record has taken its reservation and waits on the spool, and the sink's mutex is free.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		if s.mu.TryLock() {
			charged := s.reservations[id].used
			s.mu.Unlock()
			if charged {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the sink's mutex was held for the whole spool append")
		}
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	<-done
	if depth, _, _, err := s.local.Stats(ctx); err != nil || depth != 1 {
		t.Fatalf("spool depth %d %v, want the record", depth, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.localErr != nil {
		t.Fatalf("sink broke: %v", s.localErr)
	}
}
