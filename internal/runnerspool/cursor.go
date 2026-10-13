// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerspool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/ackcursor"
	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// Get implements ackcursor.Store.
func (s *Spool) Get(ctx context.Context) (pos ackcursor.Pos, err error) {
	err = s.locked(ctx, func() error {
		m, e := s.readManifest()
		if e != nil {
			return e
		}
		pos = m.Ack
		return nil
	})
	return
}

// After implements ackcursor.Source. The acknowledged row's identity lives in
// the manifest, so reclaimed payloads do not make ackcursor infer a reset.
func (s *Spool) After(ctx context.Context, seq int64, limit int) (rows []Record, err error) {
	if limit < 1 || limit > MaxRecords {
		return nil, errors.New("invalid spool read limit")
	}
	err = s.locked(ctx, func() error {
		m, e := s.readManifest()
		if e != nil {
			return e
		}
		if seq < m.Ack.Seq {
			rows = append(rows, Record{SpoolID: m.SpoolID, Seq: m.Ack.Seq, Hash: m.Ack.Hash})
		}
		for n := max(seq+1, m.Ack.Seq+1); n <= m.Last && len(rows) < limit; n++ {
			var row Record
			if e := s.readJSON(recordName(n), MaxRecordBytes, &row); e != nil {
				return e
			}
			if row.Seq != n || row.SpoolID != m.SpoolID || row.Decision.Request.RunID != s.runID || !row.ValidHash() {
				return errors.New("spool row identity or hash mismatch")
			}
			rows = append(rows, row)
		}
		return nil
	})
	return
}

// Set commits an acknowledged cursor before reclaiming any payload. A crash
// during reclamation leaves harmless acknowledged files, never missing history.
func (s *Spool) Set(ctx context.Context, pos ackcursor.Pos) error {
	return s.locked(ctx, func() error {
		m, err := s.readManifest()
		if err != nil {
			return err
		}
		if pos == m.Ack {
			return s.reclaim(m.Ack.Seq)
		}
		if pos.Seq <= m.Ack.Seq || pos.Seq > m.Last {
			return errors.New("spool cursor cannot reset or advance past its rows")
		}
		var row Record
		if err := s.readJSON(recordName(pos.Seq), MaxRecordBytes, &row); err != nil {
			return err
		}
		if row.Position() != pos || !row.ValidHash() {
			return errors.New("acknowledgement does not match spool row")
		}
		for n := m.Ack.Seq + 1; n <= pos.Seq; n++ {
			st, err := s.root.Stat(recordName(n))
			if err != nil {
				return err
			}
			m.Bytes -= st.Size()
		}
		if m.Bytes < 0 {
			return errors.New("spool byte count is inconsistent")
		}
		m.Ack = pos
		if err := s.writeManifest(m); err != nil {
			return err
		}
		return s.reclaim(pos.Seq)
	})
}

func (s *Spool) reclaim(through int64) error {
	// Enumerate only when ACK advances (not on append). Crashed reclamation is
	// retried here, so old files cannot accumulate over successful drains.
	dir, err := s.root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if len(name) != 25 || name[20:] != ".json" || name > recordName(through) {
			continue
		}
		if err := s.root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return dir.Sync()
}

// Overflow freezes one cumulative refused-attempt interval before transmission.
// A crash or new refusals cannot change the identity/count of an in-flight row.
func (s *Spool) Overflow(ctx context.Context) (row *Record, err error) {
	err = s.locked(ctx, func() error {
		m, e := s.readManifest()
		if e != nil {
			return e
		}
		if m.Overflow == nil && m.Drops > m.Reported {
			r := Record{SpoolID: m.SpoolID, Count: m.Drops - m.Reported, DropEnd: m.Drops, Decision: egress.DecisionLog{
				Request: egress.Request{RunID: s.runID, Time: time.Now().UTC()}, Decision: egress.Deny, RuleSource: "builtin:spool-full",
			}}
			r.Hash = r.digest()
			m.Overflow = &r
			if e := s.writeManifest(m); e != nil {
				return e
			}
		}
		if m.Overflow != nil {
			copy := *m.Overflow
			row = &copy
		}
		return nil
	})
	return
}

// AckOverflow records only the frozen interval, retaining later refusals.
func (s *Spool) AckOverflow(ctx context.Context, row Record) error {
	return s.locked(ctx, func() error {
		m, err := s.readManifest()
		if err != nil {
			return err
		}
		if m.Overflow == nil || m.Overflow.Hash != row.Hash || !row.ValidHash() {
			return errors.New("overflow acknowledgement does not match")
		}
		m.Reported += m.Overflow.Count
		m.Overflow = nil
		return s.writeManifest(m)
	})
}

type delivery struct {
	send func(context.Context, []Record) error
}

// Deliver sends the whole batch before waiting, so a batch costs one round trip.
func (d delivery) Deliver(ctx context.Context, rows []Record) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	if err := d.send(ctx, rows); err != nil {
		return 0, err
	}
	return rows[len(rows)-1].Seq, nil
}

// Replay uses the shared acknowledgement cursor. send gets each batch in seq
// order and must return nil only after durable checked org ingestion of every
// row in it (H2 Peer.EventsAck), not after a write.
//
// Replay ends once it has delivered the head (rows and refused attempts) seen
// when it started. Rows a live proxy appends meanwhile wait for the next call,
// so a producer faster than the drain cannot keep Replay running.
func (s *Spool) Replay(ctx context.Context, send func(context.Context, []Record) error) error {
	if send == nil {
		return errors.New("spool replay needs a durable acknowledgement")
	}
	c := ackcursor.Cursor[Record]{Source: s, Sink: delivery{send: send}, Store: s, Key: Record.Position, Batch: ReplayBatch}
	if err := c.Load(ctx); err != nil {
		return err
	}
	var head manifest
	if err := s.locked(ctx, func() (e error) { head, e = s.readManifest(); return }); err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		overflow, err := s.Overflow(ctx)
		if err != nil {
			return err
		}
		if overflow != nil {
			if err := send(ctx, []Record{*overflow}); err != nil {
				return err
			}
			if err := s.AckOverflow(ctx, *overflow); err != nil {
				return err
			}
		}
		if _, err := c.Step(ctx); err != nil {
			return err
		}
		done := false
		err = s.locked(ctx, func() error {
			m, e := s.readManifest()
			if e != nil {
				return e
			}
			done = m.Ack.Seq >= head.Last && m.Reported >= head.Drops
			return nil
		})
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

// Marshal is the bounded EVENT.spool payload; no run token authenticates replay.
func (r Record) Marshal() ([]byte, error) {
	b, err := json.Marshal(r)
	if err == nil && len(b) > MaxRecordBytes {
		return nil, errors.New("spool record exceeds wire bound")
	}
	return b, err
}
