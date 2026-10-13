// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runneractivity"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

type spoolTicketKey struct{}

func spoolTicket(ctx context.Context) uuid.UUID {
	id, _ := ctx.Value(spoolTicketKey{}).(uuid.UUID)
	return id
}

func newLocalDecisionSink(dir string, id uuid.UUID, out io.Writer) (*decisionSink, error) {
	spool, err := runnerspool.Open(dir, id)
	if err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "writer.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		spool.Close()
		return nil, err
	}
	st, err := lock.Stat()
	link, linkErr := os.Lstat(filepath.Join(dir, "writer.lock"))
	if err != nil || linkErr != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || link.Mode()&os.ModeSymlink != 0 || !os.SameFile(st, link) {
		lock.Close()
		spool.Close()
		return nil, errors.New("unsafe proxy spool writer lock")
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		spool.Close()
		return nil, errors.New("another proxy owns this run spool")
	}
	if err := spool.CanRecord(context.Background()); err != nil && !errors.Is(err, runnerspool.ErrFull) {
		lock.Close()
		spool.Close()
		return nil, err
	}
	return &decisionSink{local: spool, writerLock: lock, out: out, activityDir: dir, reservations: map[uuid.UUID]spoolReservation{}}, nil
}

// spoolReservation is one admitted request's hold on the spool: the activity
// epoch it was admitted under and the bytes its final decision may take.
type spoolReservation struct {
	epoch uuid.UUID
	bytes int64
}

// spoolRecordEnvelope is what a decision record holds beyond its request's own
// strings: identifiers, times, the rule source, a cause sentence and a scan
// summary without findings (a skip or bypass).
const spoolRecordEnvelope = 64 << 10

// spoolReservationBytes is the most a request's final decision record may take.
// A body-bearing request while content inspection is on may be scanned, and a
// scanned record carries its findings (up to the scanner's cap, each with a
// field path), which no request-derived bound covers: it reserves the largest
// wire record. Any other request reserves its target escaped twice over (JSON
// can expand one byte into six, and a cause sentence can name the target again)
// plus the record's envelope, never more than the largest wire record.
// Reserving every request at the wire maximum held a 32 MiB spool to 32
// requests in flight.
func (p *Proxy) spoolReservationBytes(r *http.Request) int64 {
	if p.scanner != nil && p.scanner.Mode() != contentscan.ModeOff && bodyBearingMethod(r.Method) {
		return runnerspool.MaxRecordBytes
	}
	return min(int64(2*6*(len(r.URL.String())+len(r.Host)+len(r.Method))+spoolRecordEnvelope), runnerspool.MaxRecordBytes)
}

// Reserve the request's own record bound before forwarding. The mutex covers both
// reservation and append accounting; a burst cannot all observe the same free
// slot and then forward requests whose final decision has nowhere to land.
func (s *decisionSink) reserve(ctx context.Context, need int64) (uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.localErr != nil {
		return uuid.Nil, errors.New("runner audit spool unavailable")
	}
	if err := s.local.CanRecord(ctx); err != nil {
		if errors.Is(err, runnerspool.ErrFull) {
			return uuid.Nil, errors.Join(err, s.local.Refuse(ctx))
		}
		return uuid.Nil, err
	}
	depth, bytes, _, err := s.local.Stats(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	if depth+len(s.reservations) >= runnerspool.MaxRecords || bytes+s.heldBytes+need > runnerspool.MaxBytes {
		return uuid.Nil, errors.Join(runnerspool.ErrFull, s.local.Refuse(ctx))
	}
	id := uuid.New()
	s.reservations[id] = spoolReservation{epoch: runneractivity.Capture(s.activityDir), bytes: need}
	s.heldBytes += need
	return id, nil
}

// drop ends a reservation; the caller holds s.mu.
func (s *decisionSink) drop(id uuid.UUID) {
	s.heldBytes -= s.reservations[id].bytes
	delete(s.reservations, id)
}

func (s *decisionSink) release(id uuid.UUID) { s.mu.Lock(); s.drop(id); s.mu.Unlock() }

func (p *Proxy) admitSpoolHTTP(w http.ResponseWriter, r *http.Request) (*http.Request, func()) {
	if p.sink == nil || p.sink.local == nil {
		return r, func() {}
	}
	// JSON can expand one control byte into six. Refuse an oversized target
	// before forwarding rather than discover that the final row cannot fit.
	if r.URL == nil || 6*(len(r.URL.String())+len(r.Host)+len(r.Method)) > runnerspool.MaxRecordBytes-4096 {
		http.Error(w, "egress audit record too large", http.StatusRequestURITooLong)
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	id, err := p.sink.reserve(ctx, p.spoolReservationBytes(r))
	if err != nil {
		http.Error(w, "egress audit spool unavailable", http.StatusServiceUnavailable)
		return nil, nil
	}
	return r.WithContext(context.WithValue(r.Context(), spoolTicketKey{}, id)), func() { p.sink.release(id) }
}

func (s *decisionSink) emitLocal(log egress.DecisionLog) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	id := log.Request.AuditReservation
	held, admitted := s.reservations[id]
	epoch := held.epoch
	admitted = admitted && log.Decision != egress.Pending
	b, err := json.Marshal(log)
	if err == nil {
		b = maskDecisionBytes(b)
		err = json.Unmarshal(b, &log)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err == nil && admitted {
		s.drop(id)
		err = s.local.AppendAdmitted(ctx, log)
	} else if err == nil {
		depth, bytes, _, statErr := s.local.Stats(ctx)
		if statErr != nil {
			err = statErr
		} else if depth+len(s.reservations) >= runnerspool.MaxRecords || bytes+s.heldBytes+int64(len(b))+1024 > runnerspool.MaxBytes {
			err = errors.Join(runnerspool.ErrFull, s.local.Refuse(ctx))
		} else {
			err = s.local.Append(ctx, log)
		}
	}
	if err == nil && admitted && log.RuleSource != ruleSourceApprovals && log.RuleSource != "credential:reauth-timeout" {
		_ = runneractivity.Record(s.activityDir, epoch)
	}
	if err != nil && !errors.Is(err, runnerspool.ErrFull) {
		s.localErr = err
	}
}

func configuredDecisionSink(cfg *Config, token *tokenSource, client *http.Client, out io.Writer) (*decisionSink, error) {
	if cfg.DecisionSpoolDir != "" {
		return newLocalDecisionSink(cfg.DecisionSpoolDir, cfg.RunID, out)
	}
	return newDecisionSink(cfg.ControlPlaneURL, token, cfg.DecisionBufferSize, client, out), nil
}
