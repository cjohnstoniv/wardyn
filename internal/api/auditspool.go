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
	"strconv"
	"strings"
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
	//
	// SEEDED FROM THE SIDECAR at NewAuditSpool, because the condition it reports
	// is PERSISTENT ON DISK while a process-local counter is not. Any restart
	// after a quarantine returned it to 0 while the sidecar still held the
	// missing events, and /metrics then showed a completely healthy audit
	// surface over a trail that is permanently incomplete.
	//
	// AS DURABLE AS THE DIRECTORY, AND NO MORE — the earlier wording promised
	// "a deploy, a crash loop, a pod reschedule" flatly, and that promise is
	// only true on a substrate that gives the spool directory durable storage:
	// compose's `audit` named volume, or a chart install with
	// persistence.enabled=true. On the chart's DEFAULT (persistence.enabled
	// false) WARDYN_AUDIT_SPOOL is /tmp/audit-spool.jsonl and /tmp is an
	// emptyDir, so a rolling deploy or a reschedule discards the sidecar with
	// the pod: the counter reads 0 again and the permanently-refused events it
	// accounts for are gone with it. A process restart in place still keeps
	// both, on every substrate. ephemeralSpoolDir names the case that does not,
	// and NewAuditSpool WARNs about it at boot rather than leaving the operator
	// to discover the difference from a counter that silently reset.
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
	a := &AuditSpool{f: f, path: path}
	// Both counters describe state that is ON DISK and outlives this process, so
	// both are read back from it rather than started at zero. For the backlog
	// that is merely correct; for the quarantine it is the whole point - a
	// restart used to clear the only scrape-surface signal that the queryable
	// trail is permanently incomplete, while the sidecar holding the missing
	// events sat untouched beside the spool.
	// THE CURSOR IS ON-DISK STATE TOO, for the same reason the two counters
	// above are: it describes the file, not this process. Drain retires work by
	// advancing `consumed` and only REWRITES the file when the reclaim pays for
	// itself (at least half), so between compactions the cursor is the only
	// record of what has already reached the store. A restart that started it at
	// zero re-replayed every line back to the last compaction — measured: one
	// pass of 100 out of 400 spooled events, then a restart, replayed all 400,
	// i.e. 100 duplicate audit_events rows, and the compaction rule bounds that
	// at ~50% of the spool, so a 64k backlog can produce ~32k duplicates on one
	// restart. The file's own doc claimed the bound was "a crash between
	// rec.Record succeeding and the on-disk trim", i.e. the in-flight batch;
	// this makes that sentence true.
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
// is discarded with the container, and names it.
//
// A PATH TEST, deliberately, and its limits are the reason it is one. The case
// that matters is the Helm chart's own default — persistence.enabled=false
// points WARDYN_AUDIT_SPOOL at /tmp/audit-spool.jsonl and mounts /tmp as an
// emptyDir — and an emptyDir is NOT a distinguishable filesystem: it is the
// node's disk, so statfs sees ext4/overlayfs and reports nothing unusual. There
// is no syscall that answers "will this survive a pod reschedule". What there IS
// is a convention every substrate this ships on honours: /tmp is scratch.
//
// It therefore UNDER-reports rather than over-reports: an operator who points
// the spool at some other ephemeral mount gets no warning, and that is the safe
// direction for a heuristic — a false alarm on a durable path would teach
// operators to ignore the line.
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

// consumedPath is the CURSOR sidecar: the byte offset of the first un-replayed
// line. Beside the spool and the quarantine sidecar, named the same way, so an
// operator looking at the spool directory sees the three files as one set.
func (a *AuditSpool) consumedPath() string { return a.path + ".consumed" }

// saveSpoolCursor fsyncs the cursor beside the spool. Best effort BY DESIGN: a
// failure here costs a duplicate replay after a restart, which is the
// at-least-once residual this file already documents and accepts, while failing
// the drain over it would stop replaying events that HAVE landed. Logged rather
// than returned so the cost is visible without being fatal.
func (a *AuditSpool) saveSpoolCursor() { a.writeSpoolCursor(a.consumed) }

// writeSpoolCursor persists one offset AND the identity of the file it
// describes. Split out from saveSpoolCursor so compact can retire the cursor
// (0) BEFORE the rename that invalidates it, without first mutating a.consumed
// on a compaction that may still fail.
func (a *AuditSpool) writeSpoolCursor(offset int64) {
	tmp := a.consumedPath() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err == nil {
		_, err = f.WriteString(strconv.FormatInt(offset, 10) + " " + spoolCursorFingerprint(a.path, offset))
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Rename(tmp, a.consumedPath())
		}
	}
	if err != nil {
		_ = os.Remove(tmp)
		slog.Warn("wardynd: audit spool cursor not persisted; a restart may replay already-recorded events",
			slog.String("path", a.consumedPath()), slog.Any("err", err))
	}
}

// spoolCursorAnchor bounds how many bytes before the offset the sidecar
// fingerprints. The anchor sits at the DECISION POINT — it is the content the
// cursor claims to have already replayed, ending exactly where Drain would
// resume — because that is the byte the answer turns on. 4 KiB is a dozen-odd
// audit lines: enough that two different spools agreeing on it at the same
// offset means they agree about the events, and small enough that the check
// costs one pread on a path Drain takes once per pass.
const spoolCursorAnchor = 4096

// spoolCursorNoAnchor is the fingerprint of offset 0. Zero needs no identity:
// "replay everything" is correct over any file, which is the direction this
// whole mechanism errs in.
const spoolCursorNoAnchor = "-"

// spoolCursorFingerprint digests the bytes the offset claims are already
// replayed — at most spoolCursorAnchor of them, ending AT the offset. "" when
// the spool cannot be read there, which no sidecar field can equal (the reader
// splits on whitespace, so an empty field cannot survive), so an unreadable
// anchor refuses the cursor rather than accepting it unverified.
func spoolCursorFingerprint(spoolPath string, offset int64) string {
	if offset <= 0 {
		return spoolCursorNoAnchor
	}
	n := offset
	if n > spoolCursorAnchor {
		n = spoolCursorAnchor
	}
	f, err := os.Open(spoolPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, offset-n); err != nil {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256(buf))
}

// seedSpoolCursor reads the cursor back, and honours it only while it still
// DESCRIBES the file it was written against.
//
// A cursor past the end of the file describes a file that no longer exists — the
// spool was compacted, truncated or replaced while this sidecar was stale — and
// honouring it would SKIP un-replayed events, which is the one direction this
// file never errs in. So an out-of-range or unreadable cursor reads as 0: replay
// from the start, at-least-once, exactly the pre-cursor behaviour.
//
// A SIZE BOUND CANNOT SAY THAT, and this is the whole of R1 F280: the test used
// to be `n < 0 || n > size`, so a cursor left over a REPLACED spool was honoured
// whenever the replacement happened to be at least as large — and a replacement
// usually IS, because the two cases that produce one are a compaction (a smaller
// file, but a smaller CURSOR too, so the old larger one often still fits) and an
// operator moving a `.quarantine` file back onto the spool path. Executed, the
// hole replayed 4 of 5 events and silently dropped a credential.mint: the offset
// was in range for bytes that were never the bytes it was measured against.
//
// So the sidecar carries the offset AND a fingerprint of the content ending
// there, and a mismatch reads as 0 like any other unusable cursor. A sidecar in
// the PRE-IDENTITY one-field format is unusable by the same rule: it asserts an
// offset over a file nothing can tie it to. That costs one extra replay of an
// in-flight batch on the upgrade that first reads it — at-least-once, the
// residual C1 accepts — and never the loss it would license.
func seedSpoolCursor(cursorPath, spoolPath string, size int64) int64 {
	buf, err := os.ReadFile(cursorPath)
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(buf))
	if len(fields) != 2 {
		return 0
	}
	n, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || n <= 0 || n > size {
		return 0
	}
	if fields[1] != spoolCursorFingerprint(spoolPath, n) {
		return 0
	}
	return n
}

// countSpoolLinesFrom counts the UN-REPLAYED lines: those after the cursor. The
// gauge means "events still to replay", so seeding it from the whole file would
// report a backlog that is already in the store.
func countSpoolLinesFrom(path string, from int64) int64 {
	buf, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	if from > 0 && from <= int64(len(buf)) {
		buf = buf[from:]
	}
	return countSpoolLinesIn(buf)
}

// countSpoolLines counts the JSONL lines in path, 0 for a missing or unreadable
// one. Same line semantics as Drain: a trailing newline is a terminator, and a
// torn tail with no newline is still a line.
func countSpoolLines(path string) int64 {
	buf, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return countSpoolLinesIn(buf)
}

// countSpoolLinesIn is countSpoolLines over bytes already in hand.
func countSpoolLinesIn(buf []byte) int64 {
	trimmed := bytes.TrimRight(buf, "\n")
	if len(bytes.TrimSpace(trimmed)) == 0 {
		return 0
	}
	return int64(bytes.Count(trimmed, []byte{'\n'}) + 1)
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
// aborted rather than a judgement about the event.
//
// SQLSTATE, not a message match and not a driver type: the codes are the
// database's own closed vocabulary, stable across driver versions, and
// documented for exactly this purpose.
//   - 55P03 lock_not_available — the audit-chain advisory lock was held past
//     `SET LOCAL lock_timeout` (internal/db's AuditChainLockTimeoutSQL). The
//     transaction never got to evaluate this event.
//   - 57014 query_canceled — statement_timeout, or an administrator cancelling
//     the backend. Same argument: aborted, not refused.
//
// Read through the `SQLState() string` method rather than *pgconn.PgError so
// this package keeps its layering — internal/api talks to store.Store and has
// never imported the driver — and so a wrapped or re-typed error from a future
// store implementation still classifies, as long as it carries the code.
// errors.As walks the %w chain the store builds around the driver error.
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
// KNOWN LIMIT: two or more ADJACENT unacceptable lines still wedge, because the
// second rejection in a pass is read as "the store is down" — which is the right
// reading for every other cause of two rejections in a row, and the price of
// never quarantining during an outage. A run like that is what "an event shape
// from another binary version" produces, so it is not hypothetical. It is not
// silent: the suspect is logged by event id every tick (above), which reads
// differently from StartDrain's store-still-failing line, and the spool gauge
// stays flat. The manual remedy is in docs/OPERATIONS.md.
//
// rec MUST be a raw durable recorder (e.g. store.Recorder) — NOT the spooling
// chain: Drain holds the spool lock across the whole operation, so a recorder that
// re-entered Append on failure would deadlock. Holding the lock also makes it safe
// against concurrent Append (a failed write during a drain blocks briefly instead
// of racing the truncate); the bounded batch keeps that hold short.
//
// at-least-once, and it stays that way. A crash between rec.Record succeeding
// and the cursor's fsync can re-replay that batch on the next Drain.
//
// THE BOUND IS THE IN-FLIGHT BATCH, and it is a bound the code holds rather
// than a sentence about one. The cursor used to live only in memory while the
// file was rewritten at most every other pass (see the compaction rule below),
// so a restart replayed everything back to the LAST COMPACTION — up to half the
// spool, measured at 100 duplicates from a 400-event backlog after one 100-event
// pass. It is now fsynced beside the spool at every advance (saveSpoolCursor)
// and seeded at open (seedSpoolCursor), so the window really is one batch.
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
	// corrupt, or quarantined. It is a per-line mark rather than the prefix
	// count it used to be, because the poison probe below can record a line
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
			// Counted (D30) so a corruption episode is observable on /metrics, not
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
			// toward quarantine.
			//
			// TWO KINDS OF TIMEOUT, and only one of them was checked. A
			// CLIENT-side deadline shows up on passCtx. A DATABASE-side abort
			// does not: the audit-chain insert runs under `SET LOCAL
			// lock_timeout` (db.AuditChainLockTimeoutSQL), so a contended chain
			// lock comes back as an ordinary statement error with
			// passCtx.Err() == nil and earned a strike. Three contended ticks on
			// the same head line and the first line to land behind it proved
			// "the store is up", moving a replayable event — a credential.mint,
			// say — into <spool>.quarantine, after which the spool gauge reads 0
			// (fully recovered) over a permanently incomplete trail. Per
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
		// PERSISTED WITH THE WORK IT DESCRIBES. This is the only record that
		// these lines already reached the store until a compaction rewrites the
		// file, which happens at most every other pass by design.
		a.saveSpoolCursor()
		// COMPACT ONLY WHEN THE RECLAIM PAYS FOR ITSELF: once the consumed
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
// prefix. Atomic via rename.
//
// THE NEW FD IS OPENED ON THE TEMP FILE BEFORE THE RENAME and swapped in after
// it, so there is no window in which a.f points at an inode the rename has
// already unlinked. Reopening the path AFTER the rename left one: a failure
// there (fd exhaustion, most likely under exactly the load that produced the
// backlog) returned with a.f still on the unlinked inode, and every later
// Append then wrote and fsynced into a file with no directory entry and
// returned nil - so spoolingRecorder logged nothing, Lines() read the new file
// and reported 0, and the runbook's "the gauge is back to 0" meant the events
// were gone. That inverted C1, the invariant this whole file exists to hold,
// into silent loss behind a healthy-looking gauge. Opening first removes the
// failure mode instead of handling it: nothing between the rename and the swap
// can fail. O_RDWR, not O_WRONLY, so endsUnterminated's ReadAt probe keeps
// working on the new fd - the torn-tail separator silently stopped being
// applied after the first compaction when the reopen used O_WRONLY.
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
	// THE CURSOR IS RETIRED BEFORE THE RENAME, not after it. The window between
	// them is a real crash window, and what it used to leave behind is the OLD
	// offset standing over the NEW, compacted file — which is SMALLER, so an
	// offset that was in range for the file it was measured against is very
	// often in range for the replacement too. That is R1 F280's own shape, and a
	// size bound cannot see it; seedSpoolCursor's fingerprint now can, but the
	// ordering removes the window rather than relying on the check that catches
	// it. Zero over the OLD file is safe in the only direction that matters: it
	// replays lines that already landed (at-least-once, the residual C1
	// accepts), where a stale offset over the new file SKIPS lines that did not.
	//
	// a.consumed is NOT moved here: this rename can still fail, and a compaction
	// that failed must leave the in-memory cursor describing the file that is
	// still on disk.
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
// TAKES NO LOCK, ON PURPOSE, and this is the reason the count is an atomic
// rather than a re-read of the file. Drain holds a.mu across every rec.Record,
// bounded only by spoolDrainDeadline, and a blocked store is exactly when this
// gauge is worth reading: an external session that inserted into audit_events
// and left its transaction open holds the chain lock, so the pass runs the full
// 15 seconds. A Lines() that waited on that mutex blocked the whole /metrics
// handler with it - past Prometheus' 10s default scrape_timeout, so the scrape
// was cancelled and the ENTIRE response was lost, wardyn_store_up and the run
// counters included, once per tick for as long as the outage lasted. The two
// gauges docs/OPERATIONS.md names for telling a dead store from an idle cluster
// disappeared during precisely the event they exist to report.
//
// Reading an atomic instead makes the scrape O(1) and structurally free of the
// drain: no shared memory beyond the counter is touched, so there is nothing to
// race on. The count can be momentarily stale against the file (an Append that
// has fsynced but not yet incremented), which is what a gauge is for.
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
// landed. Extracted so StartDrain and its tests drive the SAME loop: the bug
// below was invisible partly because the tests drove a hand-written copy of it.
//
// THE TERMINATION TEST ASKS THE BACKLOG, NOT THE BATCH. It used to be
// `n < batch`, commented "backlog cleared", and that was true while a pass read
// the whole file: replaying fewer than a full batch could only mean there was
// nothing left to replay. Once a pass reads a bounded WINDOW, a short pass far
// more often means the WINDOW ended, so the old test stopped the tick early and
// left the rest of a large backlog for the next one — draining a window per
// interval instead of continuously. Nothing was lost, but recovery slowed to a
// crawl on exactly the backlogs the paging exists to make fast, which is the
// opposite of what that change was for.
//
// The no-progress guard is what makes this loop safe to run to exhaustion: a
// pass that cannot consume anything ends the tick rather than spinning inside
// it.
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
