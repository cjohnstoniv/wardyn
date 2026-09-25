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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// AuditSpool durably records audit events whose PRIMARY store write failed, so a
// security event is never silently lost. The control-plane audit log is the
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
	// an ENOSPC/partial write, dropped rather than replayed. Surfaced on
	// /metrics so a corruption episode is visible, not just logged.
	tornDrops atomic.Int64
	// quarantined counts lines moved aside because the store rejected them
	// spoolPoisonAttempts times running (see Drain). Surfaced on /metrics: the
	// spool draining back to 0 must not be the same signal as the spool being
	// COMPLETE, and after a quarantine those two differ. Seeded from the sidecar
	// at NewAuditSpool so a restart cannot show a healthy surface over a trail
	// that is permanently incomplete. As durable as the directory, and no more:
	// on the chart's DEFAULT (persistence.enabled=false, /tmp an emptyDir) a
	// redeploy or reschedule discards the sidecar and the counter reads 0 again;
	// compose's `audit` volume or persistence.enabled=true keep it. NewAuditSpool
	// WARNs at boot when ephemeralSpoolDir names that case.
	quarantined atomic.Int64
	// lines is the number of un-replayed events in the spool: the
	// wardyn_audit_spool_lines gauge. An atomic maintained by Append and Drain
	// rather than a per-scrape recount of the file, because /metrics must not
	// queue behind a drain pass - see Lines().
	lines atomic.Int64
	// consumed is the byte offset of the first un-replayed line: everything
	// before it is already in the durable store and is waiting only to be
	// reclaimed. Drain advances it instead of rewriting the whole file on every
	// pass - see the compaction rule in Drain. Guarded by mu.
	consumed int64
	// windowBase is the file offset the current Drain window was read from.
	// Guarded by mu.
	windowBase int64
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

// spoolDrainDeadline bounds ONE Drain pass, and with it how long the spool lock
// can be held. Half the 30-second drain tick (auditSpoolDrainInterval,
// server.go), so a pass that is getting nowhere has released the lock well
// before the next tick starts one, and the events it did not reach are simply
// retried then.
const spoolDrainDeadline = 15 * time.Second

// spoolReadWindow bounds how many bytes ONE Drain pass reads. A pass replays at
// most `batch` events, so reading the WHOLE file to find them made a pass cost
// O(backlog) rather than O(batch) - and since clearing a backlog of N takes
// ceil(N/batch) passes, the recovery the spool exists to perform cost O(N^2).
// 1 MiB holds several thousand audit lines, far more than any sane batch.
const spoolReadWindow = 1 << 20

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
// disabling the fallback spool exactly when a deploy's own defaults haven't
// pre-created the path for it.
func NewAuditSpool(path string) (*AuditSpool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	a := &AuditSpool{f: f, path: path}
	// The cursor and both counters describe state ON DISK that outlives this
	// process, so they are read back rather than started at zero. For the
	// quarantine that keeps the only scrape signal of a permanently incomplete
	// trail. Drain only REWRITES the file when the reclaim is at least half, so
	// between compactions the cursor is the only record of what reached the
	// store; starting it at zero would replay up to ~50% of the spool as
	// duplicate audit_events rows. With it, the duplicate bound is the in-flight
	// batch (a crash between rec.Record succeeding and the on-disk trim).
	a.consumed = seedSpoolCursor(a.consumedPath(), path, spoolFileSize(path))
	a.lines.Store(countSpoolLinesFrom(path, a.consumed))
	a.quarantined.Store(countSpoolLines(a.quarantinePath()))
	// Said once, at boot, where an operator reads it — not left to be inferred
	// from a quarantine counter that came back 0 after a deploy. WARN rather
	// than a refusal: an ephemeral spool is still better than no spool (it
	// survives the store outage it exists for, which is the common case), and
	// refusing to boot over it would take the deployment down for a durability
	// property the deployment may not need.
	if dir, ephemeral := ephemeralSpoolDir(path); ephemeral {
		slog.Warn("api: the audit spool is on ephemeral storage; a redeploy or reschedule discards un-drained events AND the quarantine sidecar",
			"path", path, "dir", dir,
			"remedy", "point WARDYN_AUDIT_SPOOL at durable storage (helm: persistence.enabled=true; compose: the `audit` named volume)")
	}
	return a, nil
}

// ephemeralSpoolDir reports whether path's directory is one this project KNOWS
// is discarded with the container, and names it. A path test, deliberately: the
// chart default mounts /tmp as an emptyDir, which statfs cannot tell from the
// node's disk, and no syscall answers "will this survive a reschedule" — but
// every substrate this ships on treats /tmp as scratch. It UNDER-reports (another
// ephemeral mount gets no warning), the safe direction for a heuristic: a false
// alarm on a durable path would teach operators to ignore the line.
func ephemeralSpoolDir(path string) (string, bool) {
	dir := filepath.Clean(filepath.Dir(path))
	if dir == os.TempDir() || dir == "/tmp" || strings.HasPrefix(dir, "/tmp/") {
		return dir, true
	}
	return dir, false
}

// spoolFileSize is the spool's size on disk, 0 for a missing or unreadable file.
func spoolFileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
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
	// If the spool ends in a torn tail — a newline-less fragment left by an
	// ENOSPC episode or a partial Write that landed bytes then errored — separate
	// it from this event with a leading newline. Without it, Drain splits on '\n'
	// and reads `fragment{this event}` as ONE line, fails to unmarshal it, and
	// drops BOTH: this good event (whose primary-store write had ALREADY failed)
	// is silently destroyed, exactly the "never silently lost" invariant is
	// meant to hold. With the separator the fragment is its own (dropped, counted)
	// line and this event survives on its own line.
	payload := append(b, '\n')
	if a.endsUnterminated() {
		payload = append([]byte{'\n'}, payload...)
	}
	if _, err := a.f.Write(payload); err != nil {
		return err
	}
	if err := a.f.Sync(); err != nil {
		return err
	}
	// Exactly one line either way: a torn fragment was ALREADY counted as a line
	// (a tail with no newline is a line), and the separator only terminates it.
	a.lines.Add(1)
	return nil
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

// auditWriteTimedOut reports whether a store rejection was a WAIT the DATABASE
// aborted rather than a judgement about the event. SQLSTATE, not a message or
// driver type: the codes are the database's own stable, closed vocabulary.
//   - 55P03 lock_not_available — the audit-chain advisory lock was held past
//     `SET LOCAL lock_timeout` (db.AuditChainLockTimeoutSQL); the event was
//     never evaluated.
//   - 57014 query_canceled — statement_timeout or an admin cancel: aborted.
//
// Read via a `SQLState() string` method, not *pgconn.PgError, so internal/api
// never imports the driver and any store's wrapped error classifies (errors.As).
func auditWriteTimedOut(err error) bool {
	var coded interface{ SQLState() string }
	if !errors.As(err, &coded) {
		return false
	}
	switch coded.SQLState() {
	case sqlStateLockNotAvailable, sqlStateQueryCanceled:
		return true
	}
	return false
}

const (
	sqlStateLockNotAvailable = "55P03"
	sqlStateQueryCanceled    = "57014"
)

// Drain replays up to batch spooled events into rec (the DURABLE store recorder), removes exactly
// those it confirmed and returns the count. On a replay error it stops and keeps every unconfirmed
// line, so a down store leaves the spool to retry. A line rejected spoolPoisonAttempts times running
// moves to the quarantine sidecar (fsynced there first) and Drain returns errSpoolLineQuarantined, so
// the move is never silent. Known limit: two ADJACENT unacceptable lines still wedge (the second reads
// as an outage, the price of never quarantining during one); the suspect is logged by event id every
// tick, and docs/OPERATIONS.md has the remedy. rec MUST NOT be the spooling chain: Drain holds the
// spool lock throughout (safe against concurrent Append), so re-entering Append would deadlock.
// At-least-once, bounded by the in-flight batch (saveSpoolCursor fsyncs at every advance). Not
// `ON CONFLICT (id)`: audit_events has no unique id (42P10), and a unique index cannot be a boot
// migration without disabling what db.AuditDDLProtected verifies. A duplicate is the benign direction;
// exactly-once would be an operator-run CREATE UNIQUE INDEX CONCURRENTLY behind a flag.
func (a *AuditSpool) Drain(ctx context.Context, rec audit.Recorder, batch int) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	fi, err := os.Stat(a.path)
	if err != nil {
		return 0, err
	}
	size := fi.Size()
	if a.consumed >= size {
		// Everything on disk is already in the store; reclaim it and stop.
		return 0, a.reclaimAll(size)
	}
	lines, ends, chunkLen, err := a.readWindow(size)
	if err != nil {
		return 0, err
	}
	if len(lines) == 0 {
		return 0, nil
	}

	// drop marks the lines this pass removes from the file: recorded, empty,
	// corrupt, or quarantined. It is a per-line mark rather than a prefix
	// count, because the poison probe below can record a line
	// while an EARLIER one stays behind (that is the whole point of it).
	drop := make([]bool, len(lines))
	replayed := 0 // real events landed in the store (bounds the batch, logged)
	var replayErr error
	// The whole pass is bounded, because a.mu is held across every Record and a
	// blocked Record blocks the SPOOL, not just the drain: every request whose
	// own audit write fails then queues on Append behind it. Since migration
	// 0056 that is reachable without any Wardyn code misbehaving — an external
	// session that inserted into audit_events and left its transaction open
	// holds the chain lock, so the store call waits on it indefinitely. One idle
	// psql transaction must not become a process-wide stall. A deadline on the
	// PASS rather than per record keeps the bound independent of batch size.
	passCtx, cancelPass := context.WithTimeout(ctx, spoolDrainDeadline)
	defer cancelPass()
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
			// Counted so a corruption episode is observable on /metrics, not
			// only in the logs — the Append separator now confines the loss to the
			// torn fragment itself, never the good event written after it.
			a.tornDrops.Add(1)
			slog.WarnContext(ctx, "wardynd: dropping unparseable audit spool line", slog.Any("err", err))
			drop[i] = true
			continue
		}
		if err := rec.Record(passCtx, ev); err != nil {
			replayErr = err
			// A timeout is NOT a rejection: the store never answered, so this
			// line has proved nothing about itself and must not earn a strike
			// toward quarantine. A CLIENT-side deadline shows on passCtx; a
			// DATABASE-side abort (the chain insert's `SET LOCAL lock_timeout`,
			// db.AuditChainLockTimeoutSQL) is an ordinary statement error with
			// passCtx.Err() == nil, so auditWriteTimedOut checks it too. Per
			// docs/OPERATIONS.md an outage, chain-lock contention included, must
			// quarantine nothing.
			if passCtx.Err() != nil || auditWriteTimedOut(err) {
				break
			}
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
	if suspectIdx >= 0 && !provenUp {
		// The pass ended without the store accepting anything behind the
		// suspect, so it stays. Logged DISTINCTLY, because StartDrain's own
		// "store still failing" line tells the wrong story here: this shape is
		// also what a RUN of adjacent unacceptable lines looks like (the second
		// one breaks the pass before anything can prove the store is up), and
		// that run is the documented limit of the poison probe — an operator
		// seeing this line every tick with a live store should read the
		// quarantine paragraph in docs/OPERATIONS.md and triage the spool by
		// hand.
		slog.WarnContext(ctx, "wardynd: audit spool line held back after repeated store rejection; nothing behind it has landed yet, so it is not yet provably unacceptable",
			slog.String("event_id", suspect.ID.String()),
			slog.String("action", suspect.Action),
			slog.Int("attempts", a.poisonHits),
			slog.Any("err", replayErr))
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

	// The drop set is a PREFIX in every ordinary pass: the loop stops at the
	// first line the store would not take, so everything it dropped lies in
	// front of everything it kept. That is what lets a pass retire its work by
	// advancing a byte offset instead of rewriting the file. The one exception
	// is a blank or torn line sitting BEHIND a held-back poison suspect - real,
	// but rare - and it falls through to the rewrite below.
	prefix, dropped := 0, 0
	for i := range lines {
		if drop[i] {
			dropped++
			if prefix == i {
				prefix = i + 1
			}
		}
	}
	if dropped == 0 {
		return replayed, replayErr
	}
	a.lines.Add(-int64(dropped))
	if dropped == prefix {
		a.consumed += ends[prefix-1]
		// Persisted with the work it describes. This is the only record that
		// these lines already reached the store until a compaction rewrites the
		// file, which happens at most every other pass by design.
		a.saveSpoolCursor()
		// Compact only when the reclaim pays for itself: once the consumed
		// prefix is at least as big as what is left, a rewrite halves the file,
		// so the rewrites over a whole backlog sum to at most 2N instead of the
		// N^2/(2*batch) that rewriting every pass cost (measured: 159x write
		// amplification at 64k spooled events, and the fsyncs land on the same
		// volume as the database that has just come back). Reaching the end
		// always reclaims, so an emptied spool still reads 0 on disk.
		if a.consumed >= size {
			return replayed, errors.Join(replayErr, a.reclaimAll(size))
		}
		if a.consumed*2 < size {
			return replayed, replayErr
		}
	}
	keep := make([][]byte, 0, len(lines))
	for i, line := range lines {
		if !drop[i] {
			keep = append(keep, line)
		}
	}
	return replayed, errors.Join(replayErr, a.compact(keep, a.consumedBase()+chunkLen, size))
}

// consumedBase is the file offset the current window was read from.
func (a *AuditSpool) consumedBase() int64 { return a.windowBase }

// readWindow reads at most spoolReadWindow bytes of un-replayed spool, splits
// it into whole lines, and returns those lines, the offset just past each one
// (relative to the window start) and the window's byte length. A window that
// does not reach EOF is cut back to its last newline, so a line is never split
// across passes; the remainder is simply the next pass's problem.
//
// Caller must hold a.mu.
func (a *AuditSpool) readWindow(size int64) ([][]byte, []int64, int64, error) {
	a.windowBase = a.consumed
	end := size
	if size-a.consumed > spoolReadWindow {
		end = a.consumed + spoolReadWindow
	}
	f, err := os.Open(a.path)
	if err != nil {
		return nil, nil, 0, err
	}
	defer f.Close()
	buf := make([]byte, end-a.consumed)
	if _, err := f.ReadAt(buf, a.consumed); err != nil {
		return nil, nil, 0, err
	}
	if end < size {
		i := bytes.LastIndexByte(buf, '\n')
		if i < 0 {
			// One line longer than the window. Take the whole remainder rather
			// than making no progress for ever.
			buf = make([]byte, size-a.consumed)
			if _, err := f.ReadAt(buf, a.consumed); err != nil {
				return nil, nil, 0, err
			}
		} else {
			buf = buf[:i+1]
		}
	}
	var lines [][]byte
	var ends []int64
	start := 0
	for i := 0; i < len(buf); i++ {
		if buf[i] == '\n' {
			lines = append(lines, buf[start:i])
			ends = append(ends, int64(i+1))
			start = i + 1
		}
	}
	if start < len(buf) { // torn tail: only reachable at EOF, and still a line
		lines = append(lines, buf[start:])
		ends = append(ends, int64(len(buf)))
	}
	return lines, ends, int64(len(buf)), nil
}

// reclaimAll empties a fully-drained spool in place. No temp file, no rename,
// so the fd stays valid and the common case costs one truncate.
//
// Caller must hold a.mu.
func (a *AuditSpool) reclaimAll(size int64) error {
	a.consumed = 0
	// The cursor describes the file, so it is reset with it — and BEFORE the
	// truncate, so a crash in between leaves a cursor of 0 over the old file
	// (replay, at-least-once) rather than a stale offset over an empty one.
	a.saveSpoolCursor()
	if size == 0 {
		return nil
	}
	return os.Truncate(a.path, 0)
}

// compact rewrites the spool as head (the kept lines of the processed window)
// followed by the raw bytes from tailStart to size, dropping the consumed
// prefix. Atomic via rename. The new fd is opened on the temp file BEFORE the
// rename and swapped in after it, so nothing between rename and swap can fail:
// a reopen after the rename could fail (fd exhaustion under backlog load) and
// leave a.f on the unlinked inode, where every later Append would write into a
// file with no directory entry and return nil — silent loss behind a gauge
// reading 0, the inverse of this file's "never silently lost" invariant.
// O_RDWR, not O_WRONLY, so endsUnterminated's ReadAt probe works on the new fd.
//
// Caller must hold a.mu.
func (a *AuditSpool) compact(head [][]byte, tailStart, size int64) error {
	var out []byte
	if len(head) > 0 {
		out = append(bytes.Join(head, []byte{'\n'}), '\n')
	}
	if tailStart < size {
		tail := make([]byte, size-tailStart)
		rf, err := os.Open(a.path)
		if err != nil {
			return err
		}
		_, err = rf.ReadAt(tail, tailStart)
		rf.Close()
		if err != nil {
			return err
		}
		out = append(out, tail...)
	}
	tmp := a.path + ".tmp"
	tf, err := os.OpenFile(tmp, os.O_CREATE|os.O_RDWR|os.O_TRUNC|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := tf.Write(out); err != nil {
		tf.Close()
		return err
	}
	if err := tf.Sync(); err != nil {
		tf.Close()
		return err
	}
	// The cursor is retired BEFORE the rename: a crash between them would
	// otherwise leave the OLD offset over the NEW, smaller file, where it often
	// still fits. seedSpoolCursor's fingerprint would catch that, but the
	// ordering removes the window. Zero over the OLD file only replays lines that
	// already landed (at-least-once); a stale offset over the new file SKIPS
	// lines that did not. a.consumed is NOT moved here: the rename can still
	// fail, and the in-memory cursor must describe the file still on disk.
	a.writeSpoolCursor(0)
	if err := os.Rename(tmp, a.path); err != nil {
		tf.Close()
		return err
	}
	_ = a.f.Close()
	a.f = tf
	a.consumed = 0
	return nil
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
// places at once rather than in none at any instant. Verbatim, so the file is a
// valid JSONL spool an operator can move back once the cause is fixed; the
// reason lives in the log line and the /metrics counter, not the payload.
// Deliberately NOT recorded as an audit event through rec: the store just
// refused a write from this spool, and a report written through the failing path
// into the log it is about is the worst of both. The signals are the log line
// below, wardyn_audit_spool_quarantined_total, and the file itself.
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
// disabled) reports 0 — an observability gauge, not a correctness path.
//
// Takes no lock, on purpose (hence the atomic): Drain holds a.mu across every
// rec.Record for up to spoolDrainDeadline, and a blocked store is exactly when
// this gauge matters. Waiting on the mutex would push /metrics past Prometheus'
// 10s default scrape_timeout and lose the ENTIRE scrape, wardyn_store_up
// included, during the very outage it reports. Briefly lagging the file is fine.
func (a *AuditSpool) Lines() int {
	if a == nil {
		return 0
	}
	n := a.lines.Load()
	if n < 0 {
		return 0
	}
	return int(n)
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

// drainUntilIdle replays until the spool is empty, a pass makes no progress, or
// a replay error defers the rest to the next tick. Returns how many events
// landed. Shared by StartDrain and its tests so both drive the SAME loop.
//
// It terminates on the backlog, not `n < batch`: a pass reads a bounded WINDOW,
// so a short pass usually means the window ended, and stopping there would
// drain one window per interval — recovery crawling on exactly the backlogs
// paging exists to make fast. The no-progress guard makes running to
// exhaustion safe: a pass that consumes nothing ends the tick, never spins.
func (a *AuditSpool) drainUntilIdle(ctx context.Context, rec audit.Recorder, batch int) (int, error) {
	total := 0
	for {
		before := a.Lines()
		n, err := a.Drain(ctx, rec, batch)
		total += n
		if errors.Is(err, errSpoolLineQuarantined) {
			// quarantineLine already logged what was moved aside. The head of
			// the spool advanced, so keep draining this tick instead of making
			// every line behind it wait an interval per poison line.
			continue
		}
		if err != nil {
			return total, err
		}
		if a.Lines() == 0 || a.Lines() >= before {
			return total, nil
		}
	}
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
			total, err := a.drainUntilIdle(ctx, rec, batch)
			if err != nil {
				slog.WarnContext(ctx, "wardynd: audit spool drain deferred (store still failing)",
					slog.Int("drained", total), slog.Any("err", err))
			}
			if total > 0 {
				slog.InfoContext(ctx, "wardynd: drained audit spool into durable store", slog.Int("events", total))
			}
		}
	}
}
