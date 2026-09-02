// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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
	// tornDrops counts spool lines Drain could not unmarshal — a torn tail from
	// an ENOSPC/partial write, dropped rather than replayed (D30). Surfaced on
	// /metrics so a corruption episode is visible, not just logged.
	tornDrops atomic.Int64
	// quarantined counts lines moved aside because the store rejected them
	// spoolPoisonAttempts times running (see Drain). Surfaced on /metrics: the
	// spool draining back to 0 must not be the same signal as the spool being
	// COMPLETE, and after a quarantine those two differ.
	quarantined atomic.Int64
	// poisonKey/poisonHits track consecutive rejections of ONE line, keyed by
	// its exact bytes: a store that is DOWN fails every line and must keep
	// retrying forever, while a line the store will NEVER accept fails
	// identically every time. Counting per-line is what tells those apart
	// without asking the caller to classify a driver error. Reset implicitly —
	// a different head line replaces the key. Both are guarded by mu (only
	// Drain touches them), not atomics: they are read and written as a pair.
	poisonKey  [sha256.Size]byte
	poisonHits int
}

// spoolPoisonAttempts is how many consecutive Drain attempts one line gets
// before Drain stops taking its word for it and PROBES the lines behind it (see
// Drain). It is not on its own a licence to quarantine: the probe has to find
// the store accepting a later line first.
//
// Not 1: a single rejection is more likely a store that is down or a momentary
// deadlock than a line that can never land, and reordering the replay on that
// evidence would be gratuitous. Not 100: every attempt after the first
// re-proves the same rejection while the events BEHIND it wait, and
// docs/OPERATIONS.md promises the trail "becomes complete again automatically".
const spoolPoisonAttempts = 3

// errSpoolLineQuarantined is what Drain returns after moving a line aside. It
// is an ERROR and not a silent success on purpose — a quarantine means the
// queryable trail is now permanently missing an event that the spool holds, and
// that must reach the operator through the same channel a failing drain does —
// but StartDrain tells it apart from a still-failing store, because the right
// response to it is to keep draining rather than to back off.
var errSpoolLineQuarantined = errors.New("audit spool line quarantined")

// NewAuditSpool opens (creating if needed) an append-only JSONL spool at path.
// The parent directory is created too (MkdirAll, so an already-existing one is
// a no-op): the flag's own default is the RELATIVE "./data/audit-spool.jsonl"
// (cmd/wardynd/boot_flags.go), and a fresh working directory or mount with no
// "data" subdirectory yet would otherwise fail this open outright, silently
// disabling the C1 fallback spool exactly when a deploy's own defaults haven't
// pre-created the path for it.
func NewAuditSpool(path string) (*AuditSpool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
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
	// D30: if the spool ends in a torn tail — a newline-less fragment left by an
	// ENOSPC episode or a partial Write that landed bytes then errored — separate
	// it from this event with a leading newline. Without it, Drain splits on '\n'
	// and reads `fragment{this event}` as ONE line, fails to unmarshal it, and
	// drops BOTH: this good event (whose primary-store write had ALREADY failed)
	// is silently destroyed, exactly the "never silently lost" invariant C1 is
	// meant to hold. With the separator the fragment is its own (dropped, counted)
	// line and this event survives on its own line.
	payload := append(b, '\n')
	if a.endsUnterminated() {
		payload = append([]byte{'\n'}, payload...)
	}
	if _, err := a.f.Write(payload); err != nil {
		return err
	}
	return a.f.Sync()
}

// endsUnterminated reports whether the spool's last on-disk byte is not '\n' —
// a torn tail. It reads via ReadAt (pread), which O_APPEND leaves unaffected, so
// the append fd doubles as the probe. Any stat/read error returns false: the
// safe fallback is to append exactly as before (a healthy or unreadable file is
// never given a spurious separator).
func (a *AuditSpool) endsUnterminated() bool {
	fi, err := a.f.Stat()
	if err != nil || fi.Size() == 0 {
		return false
	}
	var last [1]byte
	if _, err := a.f.ReadAt(last[:], fi.Size()-1); err != nil {
		return false
	}
	return last[0] != '\n'
}

// Drain replays up to batch spooled events into rec (the DURABLE store recorder)
// and removes exactly those it confirmed, leaving the rest for the next call. It
// returns the number of events replayed. On a replay error it stops and keeps
// every not-yet-confirmed line on disk (including the one that failed), so a
// still-down store just leaves the spool untouched to retry later.
//
// EXCEPT for a line the store will never accept. Stopping at the first error is
// right for an outage and wrong for a rejection that cannot resolve — a CHECK
// violation, a payload a column type refuses, a hand-edited line, an event shape
// from another binary version. That line sat at the head of the file and every
// event behind it was replayed never, while the only symptom was a
// wardyn_audit_spool_lines gauge that stopped falling; docs/OPERATIONS.md
// meanwhile promises the trail "becomes complete again automatically". After
// spoolPoisonAttempts consecutive rejections of the SAME line, Drain therefore
// moves it to the quarantine sidecar (fsynced there before it leaves the spool)
// and carries on with the lines behind it, returning errSpoolLineQuarantined so
// the move is never silent.
//
// rec MUST be a raw durable recorder (e.g. store.Recorder) — NOT the spooling
// chain: Drain holds the spool lock across the whole operation, so a recorder that
// re-entered Append on failure would deadlock. Holding the lock also makes it safe
// against concurrent Append (a failed write during a drain blocks briefly instead
// of racing the truncate); the bounded batch keeps that hold short.
//
// at-least-once, and it stays that way. A crash between rec.Record succeeding
// and the on-disk trim can re-replay a duplicate on the next Drain.
// `ON CONFLICT (id) DO NOTHING` does NOT fix that: audit_events has no unique
// constraint on `id` (the PK is the surrogate `seq`), so Postgres rejects that
// clause at PLAN time with 42P10 — it would break every audit insert, not just
// the replayed ones. Retrofitting the index is also not a boot-time migration:
// pre-existing duplicate ids would fail it inside applyMigration's transaction
// and abort startup, CREATE UNIQUE INDEX CONCURRENTLY cannot run in that
// transaction (25001), and de-duplicating first is blocked by the append-only
// DELETE trigger (P0001) — i.e. it would require disabling the very guarantee
// db.AuditDDLProtected exists to verify.
//
// The duplicate is also the benign direction: `seq` still identifies the row
// uniquely, a replayed event is byte-identical and self-identifying by its
// repeated `id`, and an append-only log that records an event twice is a far
// smaller integrity problem than one that drops it. If exactly-once is ever
// wanted, the non-destructive path is an operator-run, out-of-band
// CREATE UNIQUE INDEX CONCURRENTLY after a duplicate check, behind a flag.
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

	// drop marks the lines this pass removes from the file: recorded, empty,
	// corrupt, or quarantined. It is a per-line mark rather than the prefix
	// count it used to be, because the poison probe below can record a line
	// while an EARLIER one stays behind (that is the whole point of it).
	drop := make([]bool, len(lines))
	replayed := 0 // real events landed in the store (bounds the batch, logged)
	var replayErr error
	// suspect is a line the store has now rejected spoolPoisonAttempts times
	// running. It is held back rather than quarantined outright: a store that is
	// DOWN rejects every line, and the only way to tell that apart from a line
	// the store will never accept is to try the lines BEHIND it and see.
	suspectIdx, provenUp := -1, false
	var suspect types.AuditEvent
	for i, line := range lines {
		if replayed >= batch {
			break
		}
		if len(bytes.TrimSpace(line)) == 0 {
			drop[i] = true
			continue
		}
		var ev types.AuditEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			// A corrupt/partial line (e.g. a torn last write) can never be
			// replayed; drop it with a loud log rather than wedging the drain.
			// Counted (D30) so a corruption episode is observable on /metrics, not
			// only in the logs — the Append separator now confines the loss to the
			// torn fragment itself, never the good event written after it.
			a.tornDrops.Add(1)
			slog.WarnContext(ctx, "wardynd: dropping unparseable audit spool line", slog.Any("err", err))
			drop[i] = true
			continue
		}
		if err := rec.Record(ctx, ev); err != nil {
			replayErr = err
			// A second rejection in the same pass answers the question the
			// probe was asking: the store is refusing more than one line, so it
			// is down (or degraded), not poisoned by this one. Nothing moves.
			if suspectIdx >= 0 {
				break
			}
			if a.strikeLine(line) < spoolPoisonAttempts {
				break
			}
			suspectIdx, suspect = i, ev
			continue
		}
		drop[i] = true
		replayed++
		if suspectIdx >= 0 {
			// The store just accepted a line from BEHIND the suspect: it is up,
			// and the suspect is the problem.
			provenUp = true
		}
	}
	if suspectIdx >= 0 && provenUp {
		// Only ever reached when something was actually blocked behind it. A
		// suspect at the very END of the spool blocks nothing, cannot be proven
		// against a live store, and is simply retried next tick.
		if qerr := a.quarantineLine(ctx, lines[suspectIdx], suspect, replayErr); qerr != nil {
			// Moving it aside failed (read-only or full disk). Leaving it in
			// place is the only honest option left: the alternative is dropping
			// an audit event to unblock the queue.
			replayErr = qerr
		} else {
			drop[suspectIdx] = true
			replayErr = errSpoolLineQuarantined
		}
	}

	keep := make([][]byte, 0, len(lines))
	consumed := 0
	for i, line := range lines {
		if drop[i] {
			consumed++
			continue
		}
		keep = append(keep, line)
	}
	if consumed == 0 {
		return 0, replayErr
	}
	var out []byte
	if len(keep) > 0 {
		out = append(bytes.Join(keep, []byte{'\n'}), '\n')
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

// strikeLine records one more consecutive rejection of exactly this line and
// returns the running count. A line different from the last rejected one resets
// the counter to 1: whatever was being retried has drained, been quarantined,
// or been overtaken, so its strikes no longer describe this one. Keyed by the line's digest rather than the
// event id, because the id is caller-supplied and two lines with one id (an
// at-least-once re-append) are still two lines to the store.
//
// Caller must hold a.mu (Drain does).
func (a *AuditSpool) strikeLine(line []byte) int {
	key := sha256.Sum256(line)
	if key != a.poisonKey {
		a.poisonKey, a.poisonHits = key, 0
	}
	a.poisonHits++
	return a.poisonHits
}

// quarantineLine appends line verbatim to the sidecar quarantine file and
// fsyncs it BEFORE Drain consumes it from the spool, so the event exists in two
// places at once rather than in none at any instant. Verbatim, because the file
// is then a valid JSONL spool an operator can move back onto the spool path once
// the cause is fixed; the reason lives in the log line and the /metrics counter,
// not smuggled into the payload.
//
// Deliberately NOT recorded as an audit event through rec: the store just
// refused a write from this very spool, and an integrity report written into the
// log it is about — through the path that is failing — is the worst of both. The
// operator-facing signals are the WARN below, wardyn_audit_spool_quarantined_total,
// and the file itself.
//
// Caller must hold a.mu (Drain does).
func (a *AuditSpool) quarantineLine(ctx context.Context, line []byte, ev types.AuditEvent, cause error) error {
	qf, err := os.OpenFile(a.quarantinePath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open audit spool quarantine: %w", err)
	}
	if _, err := qf.Write(append(append([]byte{}, line...), '\n')); err != nil {
		qf.Close()
		return fmt.Errorf("write audit spool quarantine: %w", err)
	}
	if err := qf.Sync(); err != nil {
		qf.Close()
		return fmt.Errorf("sync audit spool quarantine: %w", err)
	}
	if err := qf.Close(); err != nil {
		return fmt.Errorf("close audit spool quarantine: %w", err)
	}
	a.quarantined.Add(1)
	// The event's identifying fields, never its data payload: this line is
	// already being written to a file, and a log is not the place to widen who
	// can read an audit event's contents.
	slog.ErrorContext(ctx, "wardynd: audit spool line quarantined after repeated store rejection; the queryable trail is missing this event until it is re-fed",
		slog.String("quarantine_file", a.quarantinePath()),
		slog.String("event_id", ev.ID.String()),
		slog.String("action", ev.Action),
		slog.Int("attempts", a.poisonHits),
		slog.Any("err", cause))
	return nil
}

// quarantinePath is the sidecar the spool moves rejected lines to. Beside the
// spool on purpose: same directory, same 0600, same operator.
func (a *AuditSpool) quarantinePath() string { return a.path + ".quarantine" }

// Quarantined reports how many lines have been moved aside as permanently
// rejected. A nil spool reports 0. Served as the
// wardyn_audit_spool_quarantined_total counter on /metrics — the signal that
// "the spool drained" no longer implies "the trail is complete".
func (a *AuditSpool) Quarantined() int64 {
	if a == nil {
		return 0
	}
	return a.quarantined.Load()
}

// Lines reports how many events are sitting in the spool right now — the audit
// backlog a store outage builds up, and the count that has to drain back before
// the queryable trail is complete again. Served as the wardyn_audit_spool_lines
// gauge on /metrics: a spool that never returns to 0 is a drain that is not
// working, which nothing else on the scrape surface shows. A nil spool (spooling
// disabled) reports 0, and so does an unreadable file — this is an observability
// gauge, not a correctness path.
//
// ponytail: re-reads and counts the file per call, O(spool size). The spool is
// empty in steady state and /metrics is scraped, not hot-looped; track a counter
// alongside f only if a long outage ever makes this show up in a profile.
func (a *AuditSpool) Lines() int {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	buf, err := os.ReadFile(a.path)
	if err != nil {
		return 0
	}
	// Same line semantics as Drain: trailing newline is a terminator, not a line.
	trimmed := bytes.TrimRight(buf, "\n")
	if len(bytes.TrimSpace(trimmed)) == 0 {
		return 0
	}
	return bytes.Count(trimmed, []byte{'\n'}) + 1
}

// TornDrops reports how many spool lines Drain has dropped as unparseable (a
// torn tail from an ENOSPC/partial write). A nil spool reports 0. Served as the
// wardyn_audit_spool_torn_total counter on /metrics.
func (a *AuditSpool) TornDrops() int64 {
	if a == nil {
		return 0
	}
	return a.tornDrops.Load()
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
				if errors.Is(err, errSpoolLineQuarantined) {
					// quarantineLine already logged what was moved aside. The
					// head of the spool advanced, so keep draining this tick
					// instead of making every line behind it wait an interval
					// per poison line.
					continue
				}
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
