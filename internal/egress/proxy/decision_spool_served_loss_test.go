// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/google/uuid"
)

// A served record the spool cannot take is lost, not a refused admission: the spool does not count it as
// builtin:spool-full, the sink refuses later egress, and close's summary counts it, so a replay of the
// run fails on it instead of passing on rows that read as requests never sent.
func TestLocalSinkCountsASentRecordTheSpoolCannotTakeIntoItsSummary(t *testing.T) {
	tooLarge := "/" + strings.Repeat("x", runnerspool.MaxRecordBytes)
	for _, tc := range []struct {
		name string
		rows int // records the spool keeps before the summary
		emit func(t *testing.T, s *decisionSink, dir string, req func(path string, r uuid.UUID) egress.Request)
	}{
		{"too large, no reservation", 0, func(t *testing.T, s *decisionSink, _ string, req func(string, uuid.UUID) egress.Request) {
			s.emit(decisionLog(req(tooLarge, uuid.Nil), egress.Allow, "policy:allowed"))
		}},
		{"too large, its reserved first record", 0, func(t *testing.T, s *decisionSink, _ string, req func(string, uuid.UUID) egress.Request) {
			r, err := s.reserve(context.Background(), runnerspool.MaxRecordBytes)
			if err != nil {
				t.Fatal(err)
			}
			s.emit(decisionLog(req(tooLarge, r), egress.Allow, "policy:allowed"))
		}},
		{"a follow-up past the summary's room", 1, func(t *testing.T, s *decisionSink, dir string, req func(string, uuid.UUID) egress.Request) {
			r, err := s.reserve(context.Background(), spoolRecordEnvelope)
			if err != nil {
				t.Fatal(err)
			}
			s.emit(decisionLog(req("/", r), egress.Allow, "policy:allowed"))
			setSpoolFill(t, dir, 1, runnerspool.MaxBytes-runnerspool.LossReserveBytes-100)
			s.emit(decisionLog(req("/"+strings.Repeat("x", spoolFollowUpBytes), r), egress.Allow, "policy:allowed"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, id := filepath.Join(t.TempDir(), "spool"), uuid.New()
			s, err := newLocalDecisionSink(dir, id, nil)
			if err != nil {
				t.Fatal(err)
			}
			tc.emit(t, s, dir, func(path string, r uuid.UUID) egress.Request {
				return egress.Request{RunID: id, Host: "served.test", Method: "GET", Path: path, Time: time.Now(), AuditReservation: r}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			_, err = s.reserve(ctx, spoolRecordEnvelope)
			cancel()
			if !errors.Is(err, errAuditLost) {
				t.Fatalf("reserve after the loss = %v; want later egress refused", err)
			}
			if err := s.close(context.Background()); err != nil {
				t.Fatalf("close: %v", err)
			}
			runnerd, err := runnerspool.Open(dir, id)
			if err != nil {
				t.Fatal(err)
			}
			defer runnerd.Close()
			if _, _, drops, err := runnerd.Stats(context.Background()); err != nil || drops != 0 {
				t.Fatalf("spool counts %d refusals, %v; want none for a request that was sent", drops, err)
			}
			rows, err := runnerd.After(context.Background(), 0, 10)
			if err != nil || len(rows) != tc.rows+1 {
				t.Fatalf("spooled %d rows, %v; want %d and the summary", len(rows), err, tc.rows)
			}
			if sum := rows[tc.rows].Decision; sum.RuleSource != "egress:dropped-decisions-1" || sum.Request.RunID != id {
				t.Fatalf("last row = %+v; want the summary counting the one lost record", sum)
			}
		})
	}
}

// sealSpool marks the spool's manifest sealed, or not, and sets its bytes, so every write to it fails with
// runnerspool.ErrSealed, an error that is not a full spool.
func sealSpool(t *testing.T, dir string, sealed bool, bytes int64) {
	t.Helper()
	state := filepath.Join(dir, "state.json")
	b, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	m["sealed"], m["bytes"] = sealed, bytes
	if b, err = json.Marshal(m); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A deny or a hold of a request that was not sent, which the spool neither takes nor counts as a refusal (an
// error that is not a full spool, or a refusal count that fails), is counted for close's summary, so a replay of
// the run fails on it. It does not refuse later egress: nothing was sent.
func TestLocalSinkCountsAnUnsentRecordTheSpoolCouldNotStoreIntoItsSummary(t *testing.T) {
	for _, tc := range []struct {
		name  string
		bytes int64 // the sealed spool's bytes: room for the record, or none
		emit  func(t *testing.T, s *decisionSink, dir string, req func(r uuid.UUID) egress.Request)
	}{
		{"a deny the spool refuses", 0, func(t *testing.T, s *decisionSink, _ string, req func(uuid.UUID) egress.Request) {
			s.emit(decisionLog(req(uuid.Nil), egress.Deny, "policy:denied"))
		}},
		{"a hold the spool refuses", 0, func(t *testing.T, s *decisionSink, dir string, req func(uuid.UUID) egress.Request) {
			sealSpool(t, dir, false, 0) // admission checks the spool can record
			r, err := s.reserve(context.Background(), spoolRecordEnvelope)
			if err != nil {
				t.Fatal(err)
			}
			sealSpool(t, dir, true, 0)
			s.emit(decisionLog(req(r), egress.Pending, "approval:pending"))
			s.release(r)
		}},
		{"a refusal the spool cannot count", runnerspool.MaxBytes - runnerspool.LossReserveBytes - 10, func(t *testing.T, s *decisionSink, _ string, req func(uuid.UUID) egress.Request) {
			s.emit(decisionLog(req(uuid.Nil), egress.Deny, "policy:denied"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, id := filepath.Join(t.TempDir(), "spool"), uuid.New()
			s, err := newLocalDecisionSink(dir, id, nil)
			if err != nil {
				t.Fatal(err)
			}
			sealSpool(t, dir, true, tc.bytes)
			tc.emit(t, s, dir, func(r uuid.UUID) egress.Request {
				return egress.Request{RunID: id, Host: "unsent.test", Method: "GET", Path: "/", Time: time.Now(), AuditReservation: r}
			})
			s.mu.Lock()
			latched := s.localErr
			s.mu.Unlock()
			if latched != nil {
				t.Fatalf("sink latched on a record of a request that was not sent: %v", latched)
			}
			sealSpool(t, dir, false, 0)
			if err := s.close(context.Background()); err != nil {
				t.Fatalf("close: %v", err)
			}
			runnerd, err := runnerspool.Open(dir, id)
			if err != nil {
				t.Fatal(err)
			}
			defer runnerd.Close()
			if _, _, drops, err := runnerd.Stats(context.Background()); err != nil || drops != 0 {
				t.Fatalf("spool counts %d refusals, %v; want none", drops, err)
			}
			rows, err := runnerd.After(context.Background(), 0, 10)
			if err != nil || len(rows) != 1 || rows[0].Decision.RuleSource != "egress:dropped-decisions-1" {
				t.Fatalf("spooled %d rows, %v; want only the summary counting the one record the spool could not store", len(rows), err)
			}
		})
	}
}

// A deny the spool refused as full is counted by the spool alone. One whose refusal the spool could not commit,
// its manifest write having failed so that its full answer comes joined with that error, is counted for close's
// summary instead, so a replay of the run fails on it. A non-empty directory at runnerspool's temporary write
// name (files.go, writeFile) fails that write, as root too.
func TestLocalSinkCountsAFullRefusalTheSpoolCouldNotCommitIntoItsSummary(t *testing.T) {
	tooLarge := "/" + strings.Repeat("x", runnerspool.MaxRecordBytes)
	for _, tc := range []struct {
		name        string
		blocked     bool
		drops, rows int
	}{
		{"a refusal the spool counted", false, 1, 0},
		{"a refusal the spool could not commit", true, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, id := filepath.Join(t.TempDir(), "spool"), uuid.New()
			s, err := newLocalDecisionSink(dir, id, nil)
			if err != nil {
				t.Fatal(err)
			}
			block := filepath.Join(dir, "write.tmp")
			if tc.blocked {
				if err := os.MkdirAll(filepath.Join(block, "held"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			s.emit(decisionLog(egress.Request{RunID: id, Host: "unsent.test", Method: "GET", Path: tooLarge, Time: time.Now()}, egress.Deny, "policy:denied"))
			s.mu.Lock()
			latched := s.localErr
			s.mu.Unlock()
			if latched != nil {
				t.Fatalf("sink latched on a record of a request that was not sent: %v", latched)
			}
			if err := os.RemoveAll(block); err != nil {
				t.Fatal(err)
			}
			if err := s.close(context.Background()); err != nil {
				t.Fatalf("close: %v", err)
			}
			runnerd, err := runnerspool.Open(dir, id)
			if err != nil {
				t.Fatal(err)
			}
			defer runnerd.Close()
			if _, _, drops, err := runnerd.Stats(context.Background()); err != nil || drops != uint64(tc.drops) {
				t.Fatalf("spool counts %d refusals, %v; want %d", drops, err, tc.drops)
			}
			rows, err := runnerd.After(context.Background(), 0, 10)
			if err != nil || len(rows) != tc.rows || (tc.rows == 1 && rows[0].Decision.RuleSource != "egress:dropped-decisions-1") {
				t.Fatalf("spooled %d rows, %v; want %d, a summary counting the refusal the spool could not commit", len(rows), err, tc.rows)
			}
		})
	}
}
