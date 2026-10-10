// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/google/uuid"
)

func TestLocalSpoolReservationsBoundConcurrentForwarding(t *testing.T) {
	ctx := context.Background()
	id := uuid.New()
	dir := filepath.Join(t.TempDir(), "spool")
	s, err := newLocalDecisionSink(dir, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close(ctx)
	p := &Proxy{sink: s}
	var mu sync.Mutex
	var accepted []*httptest.ResponseRecorder
	var tickets []uuid.UUID
	var releases []func()
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "http://allowed.test/x", nil)
			admitted, release := p.admitSpoolHTTP(w, r)
			mu.Lock()
			defer mu.Unlock()
			if admitted != nil {
				tickets = append(tickets, spoolTicket(admitted.Context()))
				releases = append(releases, release)
				accepted = append(accepted, w)
			} else if w.Code != 503 {
				t.Errorf("refusal status%d", w.Code)
			}
		}()
	}
	wg.Wait()
	if len(tickets) == 0 || len(tickets) > runnerspool.MaxBytes/runnerspool.MaxRecordBytes {
		t.Fatalf("capacity over-admitted %d", len(tickets))
	}
	for i, ticket := range tickets {
		s.emit(egress.DecisionLog{Request: egress.Request{RunID: id, Host: "allowed.test", Method: "GET", Time: time.Now(), AuditReservation: ticket}, Decision: egress.Allow, RuleSource: "policy:allowed"})
		releases[i]()
	}
	depth, _, drops, err := s.local.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if depth != len(tickets) || drops != uint64(64-len(tickets)) {
		t.Fatalf("lost admitted tail or refusal count: depth%d admitted%d drops%d", depth, len(tickets), drops)
	}
	first := true
	rows := 0
	if err := s.local.Replay(ctx, func(_ context.Context, row runnerspool.Record) error {
		if first {
			if row.Decision.RuleSource != "builtin:spool-full" || row.Count != drops {
				t.Fatalf("overflow not first: %+v", row)
			}
			first = false
		} else {
			rows++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if rows != len(tickets) {
		t.Fatalf("history rows%d admitted%d", rows, len(tickets))
	}
	w := httptest.NewRecorder()
	r, release := p.admitSpoolHTTP(w, httptest.NewRequest("GET", "http://allowed.test/again", nil))
	if r == nil {
		t.Fatal("drain did not reopen admission")
	}
	release()
}
func TestLocalSpoolSingleWriterAndSecretMask(t *testing.T) {
	ctx := context.Background()
	id := uuid.New()
	dir := filepath.Join(t.TempDir(), "spool")
	s, err := newLocalDecisionSink(dir, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close(ctx)
	if other, err := newLocalDecisionSink(dir, id, nil); err == nil {
		other.close(ctx)
		t.Fatal("two proxy writers can overcommit reservations")
	}
	secret := "spool-secret-" + uuid.NewString()
	procMask([]byte(secret))
	s.emit(egress.DecisionLog{Request: egress.Request{RunID: id, Host: "example.test", Path: "/" + secret, Time: time.Now()}, Decision: egress.Deny, RuleSource: "policy:denied"})
	rows, err := s.local.After(ctx, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Decision.Request.Path == "/"+secret {
		t.Fatalf("secret retained: %+v", rows)
	}
}
