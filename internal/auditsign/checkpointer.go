// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package auditsign

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auditreceipt"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Row is one chained row a checkpoint covers: its seq and its row_hash, already re-hashed and link-checked.
type Row struct {
	Seq     int64
	RowHash string
}

// Checkpoint is one stored signed checkpoint.
type Checkpoint struct {
	SeqLo, SeqHi int64
	Kid          string
	Statement    string
	Signature    []byte
}

// ChainBreak is Store.Range's refusal: the chain is broken at Seq.
type ChainBreak struct {
	Seq    int64
	Reason string
}

func (e *ChainBreak) Error() string {
	return fmt.Sprintf("audit chain broken at seq %d: %s", e.Seq, e.Reason)
}

// ErrExists is Store.Insert's answer when a stored checkpoint overlaps the new one's range; Insert then also
// returns that stored checkpoint.
var ErrExists = errors.New("an audit checkpoint already covers that range")

// Store is the audit log as the signer reads and writes it (cmd/wardynd adapts the Postgres store to it).
type Store interface {
	// ChainHead is the newest chained row's seq and row_hash ("" while no row is hashed).
	ChainHead(ctx context.Context) (seq int64, rowHash string, err error)
	// Range returns the rows above afterSeq, oldest first, at most max, re-hashed and walked from afterHash;
	// a broken chain is a *ChainBreak. highWater is the newest seq.
	Range(ctx context.Context, afterSeq int64, afterHash string, max int) (rows []Row, highWater int64, err error)
	Insert(ctx context.Context, cp Checkpoint) (existing Checkpoint, err error)
	// Latest is the stored checkpoint with the highest seq_hi.
	Latest(ctx context.Context) (cp Checkpoint, found bool, err error)
	// EventSeq is the seq of the stored audit row with this id above afterSeq; found is false while it is not
	// in the chain (spooled, or never written).
	EventSeq(ctx context.Context, id uuid.UUID, afterSeq int64) (seq int64, found bool, err error)
}

// Failure reasons, the label of wardyn_audit_checkpoint_failures_total.
const (
	ReasonKeyUnavailable = "key_unavailable"
	ReasonChainBreak     = "chain_break"
	ReasonForeignRow     = "foreign_row"
	ReasonWrite          = "write"
)

// Reasons is every failure reason, in the order the metrics write them.
var Reasons = []string{ReasonKeyUnavailable, ReasonChainBreak, ReasonForeignRow, ReasonWrite}

// Status is what the signer reports for the metrics. Safe for concurrent use; the zero value is ready.
type Status struct {
	lag      atomic.Int64
	lastUnix atomic.Int64 // unix nanoseconds of the last checkpoint this process wrote, 0 for none
	mu       sync.Mutex
	failures map[string]int64
}

// StatusSnapshot is one read of a Status.
type StatusSnapshot struct {
	// LagRows is the newest seq minus the last checkpoint's seq_hi, as of this process's last tick.
	LagRows int64
	// LastSigned is when this process last wrote a checkpoint; zero for never.
	LastSigned time.Time
	// Failures holds every reason in Reasons, zero included.
	Failures map[string]int64
}

func (s *Status) fail(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failures == nil {
		s.failures = map[string]int64{}
	}
	s.failures[reason]++
}

// KeyUnavailable counts a signing key that could not be loaded or made.
func (s *Status) KeyUnavailable() { s.fail(ReasonKeyUnavailable) }

// Snapshot reads the status.
func (s *Status) Snapshot() StatusSnapshot {
	out := StatusSnapshot{LagRows: s.lag.Load(), Failures: map[string]int64{}}
	if n := s.lastUnix.Load(); n != 0 {
		out.LastSigned = time.Unix(0, n).UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range Reasons {
		out.Failures[r] = s.failures[r]
	}
	return out
}

// Signer writes signed checkpoints over the rows appended since the last one. One Signer per process; Tick is
// not safe to call concurrently on one Signer (Run calls it from one goroutine).
type Signer struct {
	Store  Store
	Key    *Key
	Locker db.Locker
	// Record writes the audit.checkpoint.create row through the normal recorder, so every sink receives the
	// signed checkpoint: that off-box copy is what makes a later rewrite detectable.
	Record audit.Recorder
	// MaxRows caps the rows of one checkpoint (the store's own cap).
	MaxRows int
	Now     func() time.Time
	Status  *Status

	// start is the chain head the first checkpoint follows, read once while no checkpoint is stored.
	start *Row
	// own is the audit.checkpoint.create row this signer last wrote and the checkpoint it describes. A range
	// that holds exactly that row, found in the chain by its id, right after that checkpoint, is not signed, so
	// an idle install writes nothing. Any other lone row is signed.
	own     *ownRow
	latched bool
}

type ownRow struct {
	event     uuid.UUID
	statement string // the checkpoint the row describes, which must still be the newest stored one
}

// Run ticks every interval until ctx ends, the first tick at once.
func (s *Signer) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		s.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick writes every checkpoint that is due, at most MaxRows rows each, while it holds the checkpoint lock; a
// replica that loses the lock skips the tick. A broken chain or a checkpoint row this install did not sign
// stops the signer for the life of the process (one ERROR line, a failure count, and no audit row: the sweep's
// rule, since a row about a broken chain would itself be chained onto it).
func (s *Signer) Tick(ctx context.Context) {
	if s.latched {
		return
	}
	lctx, unlock, ok, err := s.Locker.TryLock(ctx, db.NewLockKey(db.AuditCheckpointLockClass))
	if err != nil {
		slog.WarnContext(ctx, "wardynd: audit checkpoint: could not take the signer lock; trying next tick", slog.Any("err", err))
		return
	}
	if !ok {
		return
	}
	defer unlock()
	for s.signOne(lctx) {
	}
}

// signOne writes at most one checkpoint and reports whether another may be due at once.
func (s *Signer) signOne(ctx context.Context) bool {
	latest, found, err := s.Store.Latest(ctx)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: audit checkpoint: could not read the latest checkpoint; trying next tick", slog.Any("err", err))
		return false
	}
	var afterSeq int64
	var afterHash, prev string
	if found {
		st, ok := s.ours(latest)
		if !ok {
			s.latch(ctx, ReasonForeignRow, latest.SeqLo, "the latest stored checkpoint is not signed by this install's key")
			return false
		}
		afterSeq, afterHash, prev = st.SeqHi, st.HeadHash, auditreceipt.StatementHash(latest.Statement)
	} else {
		if s.start == nil {
			seq, hash, err := s.Store.ChainHead(ctx)
			if err != nil {
				slog.WarnContext(ctx, "wardynd: audit checkpoint: could not read the chain head; trying next tick", slog.Any("err", err))
				return false
			}
			s.start = &Row{Seq: seq, RowHash: hash}
		}
		afterSeq, afterHash = s.start.Seq, s.start.RowHash
	}

	rows, hw, err := s.Store.Range(ctx, afterSeq, afterHash, s.MaxRows)
	var br *ChainBreak
	switch {
	case errors.As(err, &br):
		s.latch(ctx, ReasonChainBreak, br.Seq, br.Reason)
		return false
	case err != nil:
		slog.WarnContext(ctx, "wardynd: audit checkpoint: could not read new audit rows; trying next tick", slog.Any("err", err))
		return false
	}
	s.Status.lag.Store(max(hw-afterSeq, 0))
	if len(rows) == 0 {
		return false
	}
	if len(rows) == 1 && found && s.isOwnRow(ctx, latest, rows[0]) {
		return false
	}

	seqs := make([]int64, len(rows))
	hashes := make([]string, len(rows))
	for i, r := range rows {
		seqs[i], hashes[i] = r.Seq, r.RowHash
	}
	now := s.Now().UTC()
	st := auditreceipt.Statement{
		Format: auditreceipt.CheckpointFormat, Kid: s.Key.KeyID(),
		SeqLo: seqs[0], SeqHi: seqs[len(seqs)-1], Rows: int64(len(rows)),
		HeadHash: hashes[len(hashes)-1], Digest: auditreceipt.Fold(prev, hashes), SeqDigest: auditreceipt.SeqDigest(seqs, hashes),
		Prev: prev, SignedAt: now.Format(time.RFC3339Nano),
	}
	text := string(st.Bytes())
	cp := Checkpoint{SeqLo: st.SeqLo, SeqHi: st.SeqHi, Kid: st.Kid, Statement: text, Signature: s.Key.Sign([]byte(text))}
	existing, err := s.Store.Insert(ctx, cp)
	switch {
	case errors.Is(err, ErrExists):
		if _, ok := s.ours(existing); !ok {
			s.latch(ctx, ReasonForeignRow, existing.SeqLo, "a stored checkpoint over the range is not signed by this install's key")
		}
		// Otherwise another replica wrote it between the two reads; the next tick chains to it.
		return false
	case err != nil:
		s.Status.fail(ReasonWrite)
		slog.ErrorContext(ctx, "wardynd: audit checkpoint: could not store a checkpoint; trying next tick", slog.Any("err", err))
		return false
	}
	s.Status.lastUnix.Store(now.UnixNano())
	s.Status.lag.Store(max(hw-st.SeqHi, 0))
	s.own = nil
	if id, ok := s.recordCheckpoint(ctx, st, cp); ok {
		s.own = &ownRow{event: id, statement: text}
	}
	return len(rows) == s.MaxRows
}

// isOwnRow reports whether row, alone after latest, is the audit row this signer wrote for latest: latest is
// still the checkpoint it wrote (another signer's newer one clears the claim) and the stored row with that
// event id is this row. A lookup error signs the row: an extra checkpoint is cheaper than a missing receipt.
func (s *Signer) isOwnRow(ctx context.Context, latest Checkpoint, row Row) bool {
	if s.own == nil {
		return false
	}
	if s.own.statement != latest.Statement {
		s.own = nil
		return false
	}
	seq, found, err := s.Store.EventSeq(ctx, s.own.event, latest.SeqHi)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: audit checkpoint: could not look up the last checkpoint's own row; signing the range", slog.Any("err", err))
		return false
	}
	return found && seq == row.Seq
}

// ours parses a stored checkpoint and checks its signature against this install's keys. A row whose columns
// differ from its own statement is not ours either.
func (s *Signer) ours(c Checkpoint) (auditreceipt.Statement, bool) {
	st, err := auditreceipt.ParseStatement(c.Statement)
	if err != nil || st.Kid != c.Kid || st.SeqLo != c.SeqLo || st.SeqHi != c.SeqHi {
		return st, false
	}
	return st, s.Key.Verify(c.Kid, []byte(c.Statement), c.Signature)
}

func (s *Signer) latch(ctx context.Context, reason string, seq int64, detail string) {
	s.latched = true
	s.Status.fail(reason)
	slog.ErrorContext(ctx, "wardynd: audit checkpoint signing stopped until wardynd restarts; run wardyn audit chain verify",
		slog.String("reason", reason), slog.Int64("broken_seq", seq), slog.String("detail", detail))
}

// recordCheckpoint writes the audit.checkpoint.create row: a complete, self-verifying signed checkpoint for every
// sink. It returns the row's id and whether the store accepted it now; a spooled row is not in the chain yet,
// so it is never treated as the chain's tail.
func (s *Signer) recordCheckpoint(ctx context.Context, st auditreceipt.Statement, cp Checkpoint) (uuid.UUID, bool) {
	data, _ := json.Marshal(map[string]any{
		"seq_lo": st.SeqLo, "seq_hi": st.SeqHi, "rows": st.Rows,
		"head_hash": st.HeadHash, "digest": st.Digest, "seq_digest": st.SeqDigest,
		"kid": st.Kid, "public_key": base64.StdEncoding.EncodeToString(s.Key.PublicKey()),
		"statement": cp.Statement, "signature": base64.StdEncoding.EncodeToString(cp.Signature),
	})
	ev := types.AuditEvent{
		ID:        uuid.New(),
		Time:      s.Now().UTC(),
		ActorType: types.ActorSystem,
		Actor:     "wardynd",
		Action:    "audit.checkpoint.create",
		Target:    "audit_checkpoints",
		Outcome:   "success",
		Data:      json.RawMessage(data),
	}
	err := s.Record.Record(ctx, ev)
	if err != nil && !errors.Is(err, audit.ErrSpooled) {
		audit.LogWriteFailure(ctx, ev, err)
	}
	return ev.ID, err == nil
}
