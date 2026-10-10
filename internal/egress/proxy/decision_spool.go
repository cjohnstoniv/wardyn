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
	return &decisionSink{local: spool, writerLock: lock, out: out, activityDir: dir, reservations: map[uuid.UUID]uuid.UUID{}}, nil
}

// Reserve the largest wire record before forwarding. The mutex covers both
// reservation and append accounting; a burst cannot all observe the same free
// slot and then forward requests whose final decision has nowhere to land.
func (s *decisionSink) reserve(ctx context.Context) (uuid.UUID, error) {
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
	held := len(s.reservations)
	if depth+held >= runnerspool.MaxRecords || bytes+int64(held+1)*runnerspool.MaxRecordBytes > runnerspool.MaxBytes {
		return uuid.Nil, errors.Join(runnerspool.ErrFull, s.local.Refuse(ctx))
	}
	id := uuid.New()
	s.reservations[id] = runneractivity.Capture(s.activityDir)
	return id, nil
}
func (s *decisionSink) release(id uuid.UUID) { s.mu.Lock(); delete(s.reservations, id); s.mu.Unlock() }

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
	id, err := p.sink.reserve(ctx)
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
	epoch, admitted := s.reservations[id]
	admitted = admitted && log.Decision != egress.Pending
	b, err := json.Marshal(log)
	if err == nil {
		b = maskDecisionBytes(b)
		err = json.Unmarshal(b, &log)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err == nil && admitted {
		delete(s.reservations, id)
		err = s.local.AppendAdmitted(ctx, log)
	} else if err == nil {
		depth, bytes, _, statErr := s.local.Stats(ctx)
		if statErr != nil {
			err = statErr
		} else if depth+len(s.reservations) >= runnerspool.MaxRecords || bytes+int64(len(s.reservations))*runnerspool.MaxRecordBytes+int64(len(b))+1024 > runnerspool.MaxBytes {
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
