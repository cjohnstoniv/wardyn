// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// AuditSpool durably records audit events whose PRIMARY store write failed, so a
// security event is never silently lost (C1). The control-plane audit log is the
// system of record; silently dropping a credential.mint / run.kill / egress.deny
// event when the database blips would let a run proceed while reporting success —
// exactly the failure a governance tool must not have.
//
// It is a mutex-guarded append-only JSONL file: intentionally simple, fit for the
// local-first single-host deployment. A background drain loop (StartDrain, wired
// from the API server) replays spooled events back into the durable store once it
// recovers and empties the file, so the queryable audit trail becomes complete
// again automatically — the write-only sink is no longer operationally inert. A
// nil *AuditSpool disables spooling (a failed primary write is then only logged).
type AuditSpool struct {
	mu   sync.Mutex
	f    *os.File
	path string
}

// NewAuditSpool opens (creating if needed) an append-only JSONL spool at path.
func NewAuditSpool(path string) (*AuditSpool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &AuditSpool{f: f, path: path}, nil
}

// Append writes one event as a single JSON line and fsyncs it, so a crash right
// after a primary-write failure still preserves the event. It returns an error
// only when even the fallback write fails (the last-resort signal the event is lost).
func (a *AuditSpool) Append(ev types.AuditEvent) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.f.Write(append(b, '\n')); err != nil {
		return err
	}
	return a.f.Sync()
}

// Drain replays up to batch spooled events into rec (the DURABLE store recorder)
// and removes exactly those it confirmed, leaving the rest for the next call. It
// returns the number of events replayed. On the first replay error it stops and
// keeps every not-yet-confirmed line on disk (including the one that failed), so a
// still-down store just leaves the spool untouched to retry later.
//
// rec MUST be a raw durable recorder (e.g. store.Recorder) — NOT the spooling
// chain: Drain holds the spool lock across the whole operation, so a recorder that
// re-entered Append on failure would deadlock. Holding the lock also makes it safe
// against concurrent Append (a failed write during a drain blocks briefly instead
// of racing the truncate); the bounded batch keeps that hold short.
//
// ponytail: at-least-once. A crash between rec.Record succeeding and the on-disk
// trim can re-replay a duplicate on the next Drain; give InsertAuditEvent an
// `ON CONFLICT (id) DO NOTHING` and that becomes exactly-once.
func (a *AuditSpool) Drain(ctx context.Context, rec audit.Recorder, batch int) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	buf, err := os.ReadFile(a.path)
	if err != nil {
		return 0, err
	}
	if len(bytes.TrimSpace(buf)) == 0 {
		return 0, nil
	}
	lines := bytes.Split(bytes.TrimRight(buf, "\n"), []byte{'\n'})

	consumed := 0 // lines to remove from the file (recorded, empty, or corrupt)
	replayed := 0 // real events landed in the store (bounds the batch, logged)
	var replayErr error
	for _, line := range lines {
		if replayed >= batch {
			break
		}
		if len(bytes.TrimSpace(line)) == 0 {
			consumed++
			continue
		}
		var ev types.AuditEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			// A corrupt/partial line (e.g. a torn last write) can never be
			// replayed; drop it with a loud log rather than wedging the drain.
			slog.WarnContext(ctx, "wardynd: dropping unparseable audit spool line", slog.Any("err", err))
			consumed++
			continue
		}
		if err := rec.Record(ctx, ev); err != nil {
			replayErr = err
			break
		}
		consumed++
		replayed++
	}

	if consumed == 0 {
		return 0, replayErr
	}
	// Rewrite the file to only the undrained remainder — CRASH-SAFELY. A naive
	// Truncate(0)+Write leaves a window where a crash between the two loses every
	// not-yet-replayed line (data LOSS, worse than the at-least-once ceiling and
	// contrary to C1 'never silently lost'). Instead write the remainder to a temp
	// file, fsync it, and atomically rename it over the spool: a crash at any point
	// leaves EITHER the old full file OR the complete remainder on disk, never a
	// half-written one. Duplicate re-replay of an already-recorded event is already
	// permitted (at-least-once), so recovering the old file is safe.
	remaining := lines[consumed:]
	var out []byte
	if len(remaining) > 0 {
		out = append(bytes.Join(remaining, []byte{'\n'}), '\n')
	}
	tmp := a.path + ".tmp"
	tf, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return replayed, err
	}
	if _, err := tf.Write(out); err != nil {
		tf.Close()
		return replayed, err
	}
	if err := tf.Sync(); err != nil {
		tf.Close()
		return replayed, err
	}
	if err := tf.Close(); err != nil {
		return replayed, err
	}
	if err := os.Rename(tmp, a.path); err != nil {
		return replayed, err
	}
	// The old fd still points at the renamed-away inode; reopen O_APPEND on the new
	// file so subsequent Appends land in the spool operators actually read.
	nf, err := os.OpenFile(a.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return replayed, err
	}
	_ = a.f.Close()
	a.f = nf
	return replayed, replayErr
}

// StartDrain runs Drain on a ticker until ctx is cancelled, replaying spooled
// events into rec once the store recovers. Each tick drains repeatedly (yielding
// the lock between batches so Append is not starved) until the backlog clears or a
// replay error defers the rest to the next tick. It blocks; run it in a goroutine.
func (a *AuditSpool) StartDrain(ctx context.Context, rec audit.Recorder, interval time.Duration, batch int) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			total := 0
			for {
				n, err := a.Drain(ctx, rec, batch)
				total += n
				if err != nil {
					slog.WarnContext(ctx, "wardynd: audit spool drain deferred (store still failing)",
						slog.Int("drained", total), slog.Any("err", err))
					break
				}
				if n < batch { // backlog cleared
					break
				}
			}
			if total > 0 {
				slog.InfoContext(ctx, "wardynd: drained audit spool into durable store", slog.Int("events", total))
			}
		}
	}
}
