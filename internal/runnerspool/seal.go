// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerspool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/filelock"
	"github.com/cjohnstoniv/wardyn/internal/spoolowner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ErrSealed refuses new evidence production after exact settled retirement.
var ErrSealed = errors.New("runner spool is durably sealed")

func receipt(m manifest) types.RunnerSpoolReceipt {
	return types.RunnerSpoolReceipt{SpoolID: m.SpoolID, Seq: m.Ack.Seq, Hash: m.Ack.Hash, ReportedDrops: m.Reported}
}

// Receipt reads durable acceptance, not the unacknowledged current head.
func (s *Spool) Receipt(ctx context.Context) (out types.RunnerSpoolReceipt, err error) {
	err = s.locked(ctx, func() error {
		m, err := s.readManifest()
		if err == nil {
			out = receipt(m)
		}
		return err
	})
	return
}

// Seal requires the proxy's lifetime writer lock and an exact fully accepted
// current head. A still-running producer or unsettled evidence defers sealing.
func (s *Spool) Seal(ctx context.Context, expected types.RunnerSpoolReceipt) (sealed bool, err error) {
	writer, err := s.openWriter()
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, writer.Close()) }()
	if err = filelock.TryLock(writer); err != nil {
		if errors.Is(err, filelock.ErrLocked) {
			return false, nil
		}
		return false, err
	}
	defer filelock.Unlock(writer) //nolint:errcheck // close also releases the lock
	err = s.locked(ctx, func() error {
		m, err := s.readManifest()
		if err != nil {
			return err
		}
		if receipt(m) != expected || m.Last != m.Ack.Seq || m.Drops != m.Reported || m.Overflow != nil || !m.remoteSettled() {
			return nil
		}
		if !m.Sealed {
			m.Sealed = true
			if err := s.writeManifest(m); err != nil {
				return err
			}
		}
		sealed = true
		return nil
	})
	return
}

func (s *Spool) openWriter() (*os.File, error) {
	writer, err := s.root.OpenFile("writer.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	st, err := writer.Stat()
	link, linkErr := s.root.Lstat("writer.lock")
	if err != nil || linkErr != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || link.Mode()&os.ModeSymlink != 0 || !os.SameFile(st, link) {
		writer.Close()
		return nil, errors.New("unsafe spool writer lock")
	}
	// Only after the checks: a name that is not the spool's own regular 0600 file is never handed over.
	if err := spoolowner.Keep(s.root, writer); err != nil {
		writer.Close()
		return nil, err
	}
	return writer, nil
}

// ErrWriterHeld means another handle owns the spool's writer lock.
var ErrWriterHeld = errors.New("spool writer lock is held")

// HoldWriter takes the writer lock for a remote producer's lifetime, as a live
// local proxy holds it: Seal defers until the returned file is closed.
func (s *Spool) HoldWriter() (*os.File, error) {
	writer, err := s.openWriter()
	if err != nil {
		return nil, err
	}
	if err := filelock.TryLock(writer); err != nil {
		writer.Close()
		if errors.Is(err, filelock.ErrLocked) {
			return nil, ErrWriterHeld
		}
		return nil, err
	}
	return writer, nil
}

// Liability is the remote producer's durable state, and whether the spool is
// sealed.
type Liability struct {
	Gen         uint64
	Producer    *Producer
	Grant       *Grant
	PendingLoss *PendingLoss
	Sealed      bool
}

// Settled reports a producer whose liability has been written off.
func (l Liability) Settled() bool { return l.Producer != nil && l.Grant == nil }

// Liability reads the remote producer's durable state.
func (s *Spool) Liability(ctx context.Context) (out Liability, err error) {
	err = s.locked(ctx, func() error {
		m, err := s.readManifest()
		if err == nil {
			out = Liability{Gen: m.Gen, Producer: m.Producer, Grant: m.Grant, PendingLoss: m.PendingLoss, Sealed: m.Sealed}
		}
		return err
	})
	return
}

// Drained is a proxy's drained reply: its last sequence and the credit units it
// consumed (sequences plus reservations still held).
type Drained struct {
	Gen      uint64
	FinalSeq int64
	Consumed uint64
}

// Loss is one settlement's count and basis.
type Loss struct {
	Count uint64
	Basis LossBasis
}

// settledInstance marks a producer that never connected once it is proven gone.
const settledInstance = "00000000000000000000000000000000"

// SettleLoss writes off the remote producer's liability once it is proven gone
// (completion evidence; never a NotFound alone): exact when drained answered at
// the current generation with every sequence committed, else the upper bound
// Grant.Through - Producer.Seq. One write adds it to pending_loss and clears
// the grant; a settled producer is never accepted again.
func (s *Spool) SettleLoss(ctx context.Context, drained *Drained) (loss Loss, err error) {
	err = s.locked(ctx, func() error {
		m, err := s.readManifest()
		if err != nil {
			return err
		}
		if m.Sealed || (m.Producer != nil && m.Grant == nil) {
			return nil
		}
		if m.Producer == nil {
			m.Producer = &Producer{Instance: settledInstance}
			return s.writeManifest(m)
		}
		loss = Loss{Count: uint64(m.Grant.Through - m.Producer.Seq), Basis: LossUpperBound}
		if d := drained; d != nil && d.Gen == m.Gen && d.FinalSeq == m.Producer.Seq && d.Consumed >= uint64(d.FinalSeq) {
			loss = Loss{Count: d.Consumed - uint64(d.FinalSeq), Basis: LossExact}
		}
		if loss.Count > 0 {
			m.PendingLoss = addLoss(m.PendingLoss, loss.Count, loss.Basis)
		}
		m.Grant = nil
		return s.writeManifest(m)
	})
	return
}

// Succeed opens the spool to a new producer once the last one is settled: a
// Docker runner's revive starts a new proxy process, which is a new instance.
// The settled producer's pending loss stays until EmitLoss. A producer still
// holding a grant is refused, so the new one never overlaps it. The caller
// hands the run to the new proxy under a new key, so the settled instance
// cannot come back.
func (s *Spool) Succeed(ctx context.Context) error {
	return s.locked(ctx, func() error {
		m, err := s.readManifest()
		if err != nil {
			return err
		}
		if m.Sealed {
			return ErrSealed
		}
		if m.Producer == nil {
			return nil
		}
		if m.Grant != nil {
			return ErrProducer
		}
		m.Producer = nil
		return s.writeManifest(m)
	})
}

// EmitLoss turns pending_loss into one ordinary builtin:spool-lost row in the
// loss reserve, clearing pending_loss in the same write. Unreported Drops never
// refuse it; while an earlier unacknowledged row still fills the reserve it
// waits (emitted=false) for the next call.
func (s *Spool) EmitLoss(ctx context.Context) (emitted bool, err error) {
	err = s.locked(ctx, func() error {
		m, err := s.readManifest()
		if err != nil {
			return err
		}
		if m.Sealed || m.PendingLoss == nil {
			return nil
		}
		l := m.PendingLoss
		r := Record{SpoolID: m.SpoolID, Seq: m.Last + 1, Decision: egress.DecisionLog{
			Request:  egress.Request{RunID: s.runID, Time: time.Now().UTC()},
			Decision: egress.Deny, RuleSource: "builtin:spool-lost",
			Cause: fmt.Sprintf("%d egress decisions were not recorded (%s)", l.Count, l.Basis),
		}}
		r.Hash = r.digest()
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if m.Last-m.Ack.Seq >= MaxRecords || m.Bytes+int64(len(b)) > MaxBytes {
			return nil
		}
		if err := s.writeBytes(recordName(r.Seq), b); err != nil {
			return err
		}
		m.Last, m.Bytes, m.PendingLoss, emitted = r.Seq, m.Bytes+int64(len(b)), nil, true
		return s.writeManifest(m)
	})
	return
}
