// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package runnerspool stores proxy decisions until the authenticated runner
// stream acknowledges their durable org ingestion. The proxy and daemon use
// the same private directory, serialized by a process-shared file lock.
package runnerspool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"github.com/cjohnstoniv/wardyn/internal/ackcursor"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/spoolowner"
)

const (
	// ReplayBatch bounds the uncommitted local ACK window retained by org receipts.
	ReplayBatch = 64
	MaxRecords  = 50000
	MaxBytes    = 32 << 20
	// MaxRecordBytes leaves the record envelope below the wire control bound.
	MaxRecordBytes = (1 << 20) - 4096
)

var ErrFull = errors.New("runner decision spool is full")

// Record is one stable, replayable decision. Seq zero is an overflow summary,
// whose durable identity is its hash; ordinary records have positive sequences.
type Record struct {
	SpoolID  uuid.UUID          `json:"spool_id"`
	Seq      int64              `json:"seq"`
	Hash     string             `json:"hash"`
	Decision egress.DecisionLog `json:"decision"`
	Count    uint64             `json:"count,omitempty"`
	DropEnd  uint64             `json:"drop_end,omitempty"`
}

// digest hashes the record as this release encodes it, so a row a later release wrote with a field this one
// does not know fails ValidHash after a rollback. A new field changes what older runners can replay.
func (r Record) digest() string {
	r.Hash = ""
	b, _ := json.Marshal(r)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ValidHash refuses modified or cross-stream rows before durable ingestion.
func (r Record) ValidHash() bool { return r.SpoolID != uuid.Nil && r.Hash == r.digest() }

// Position is the acknowledgement cursor's sequence and content identity.
func (r Record) Position() ackcursor.Pos { return ackcursor.Pos{Seq: r.Seq, Hash: r.Hash} }

type manifest struct {
	Sealed   bool          `json:"sealed,omitempty"`
	RunID    uuid.UUID     `json:"run_id"`
	SpoolID  uuid.UUID     `json:"spool_id"`
	Last     int64         `json:"last"`
	Ack      ackcursor.Pos `json:"ack"`
	Bytes    int64         `json:"bytes"`
	Drops    uint64        `json:"drops"`
	Reported uint64        `json:"reported"`
	Overflow *Record       `json:"overflow,omitempty"`
	// The remote-sink fields below are written only by runnerd's sink (k8s
	// runners); a spool the proxy writes directly never carries them.
	Producer    *Producer    `json:"producer,omitempty"`
	Grant       *Grant       `json:"grant,omitempty"`
	Gen         uint64       `json:"gen,omitempty"`
	PendingLoss *PendingLoss `json:"pending_loss,omitempty"`
}

// Disposition is the one durable outcome of a producer sequence.
type Disposition string

const (
	Appended Disposition = "appended"
	Refused  Disposition = "refused"
	Lost     Disposition = "lost"
)

// LossBasis says whether a pending loss count is exact or an upper bound.
type LossBasis string

const (
	LossExact      LossBasis = "exact"
	LossUpperBound LossBasis = "upper_bound"
)

// Producer is the remote proxy's durable position: the last sequence with a
// disposition, its body hash, and the refusal total already counted in Drops.
type Producer struct {
	Instance    string      `json:"instance"`
	Seq         int64       `json:"seq"`
	Hash        string      `json:"hash,omitempty"`
	Disposition Disposition `json:"disposition,omitempty"`
	Refused     uint64      `json:"refused,omitempty"`
}

// Grant is write-ahead credit: sequences Base+1..Through carrying at most Bytes
// of record bodies. Its liability is Through - Producer.Seq.
type Grant struct {
	Gen     uint64 `json:"gen"`
	Base    int64  `json:"base"`
	Through int64  `json:"through"`
	Bytes   int64  `json:"bytes"`
}

// PendingLoss is settled loss not yet emitted as a builtin:spool-lost row.
type PendingLoss struct {
	Count uint64    `json:"count"`
	Basis LossBasis `json:"basis"`
}

// Spool is safe across goroutines and independently opened proxy/daemon handles.
type Spool struct {
	// sem is the handle's lock, a one-slot channel: a waiter gives up when its context ends, and waiters
	// are served in the order they arrived.
	sem   chan struct{}
	root  *os.Root
	lock  *os.File
	runID uuid.UUID
	// dir is the path Open was given, which names the spool in an error (inDir).
	dir string

	// Group commit (append): appenders queue under qmu, and whichever holds commitMu writes every
	// queued record under one manifest write. Each appender returns once the manifest covering its
	// record is durable, so a burst of decisions shares the fsyncs one decision used to pay alone.
	qmu      sync.Mutex
	queue    []*queuedAppend
	commitMu sync.Mutex
	// manifestWrites counts this handle's manifest commits, under sem (the group-commit pin reads it).
	manifestWrites uint64
}

// queuedAppend is one appender's record and the channel its outcome is sent on.
type queuedAppend struct {
	decision egress.DecisionLog
	done     chan error
}

// Open refuses unsafe or corrupt existing state. A missing directory starts a
// new private stream; an existing stream never silently resets its cursor.
func Open(dir string, runID uuid.UUID) (*Spool, error) {
	if runID == uuid.Nil {
		return nil, errors.New("spool requires a run id")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0077 != 0 {
		return nil, errors.New("spool directory must be private and not a symlink")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	lock, err := root.OpenFile("lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		root.Close()
		return nil, inDir(dir, err)
	}
	st, statErr := lock.Stat()
	link, linkErr := root.Lstat("lock")
	if statErr != nil || linkErr != nil || !st.Mode().IsRegular() || link.Mode()&os.ModeSymlink != 0 || !os.SameFile(st, link) || st.Mode().Perm() != 0600 {
		lock.Close()
		root.Close()
		return nil, errors.New("unsafe spool lock file")
	}
	s := &Spool{sem: make(chan struct{}, 1), root: root, lock: lock, runID: runID, dir: dir}
	// After the checks above: only a regular 0600 file that is the name's own (and, in Keep, has no
	// other link) is ever handed to the spool's owner.
	if err := spoolowner.Keep(root, lock); err != nil {
		lock.Close()
		root.Close()
		return nil, err
	}
	err = s.locked(context.Background(), func() error {
		m, err := s.readManifest()
		if errors.Is(err, os.ErrNotExist) {
			entries, e := s.root.Open(".")
			if e != nil {
				return e
			}
			names, e := entries.Readdirnames(-1)
			entries.Close()
			if e != nil {
				return e
			}
			// A crash inside the first manifest write leaves its temporary file, which writeBytes replaces.
			names = slices.DeleteFunc(names, func(n string) bool { return n == "write.tmp" })
			if len(names) != 1 || names[0] != "lock" {
				return errors.New("spool metadata missing beside existing data")
			}
			return s.writeManifest(manifest{RunID: runID, SpoolID: uuid.New()})
		}
		if err != nil {
			return err
		}
		return s.reclaim(m.Ack.Seq)
	})
	if err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// inDir puts dir in front of the name an os.Root error carries, which is relative to the spool, so the
// error says which directory it could not use: "openat /var/lib/wardyn/decision-spool/state.json".
func inDir(dir string, err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) && !filepath.IsAbs(pe.Path) {
		pe.Path = filepath.Join(dir, pe.Path)
	}
	var le *os.LinkError
	if errors.As(err, &le) && !filepath.IsAbs(le.Old) {
		le.Old, le.New = filepath.Join(dir, le.Old), filepath.Join(dir, le.New)
	}
	return err
}

// Close releases this process's handles; it never deletes pending decisions.
func (s *Spool) Close() error {
	s.sem <- struct{}{}
	defer func() { <-s.sem }()
	return errors.Join(s.lock.Close(), s.root.Close())
}

// locked runs fn holding the spool lock. Both waits end with ctx: a commit may hold sem while it waits
// on the file lock, and a proxy's admission check must still answer within its own deadline.
func (s *Spool) locked(ctx context.Context, fn func() error) error {
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.sem }()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := unix.Flock(int(s.lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	defer unix.Flock(int(s.lock.Fd()), unix.LOCK_UN) //nolint:errcheck // descriptor close also releases the lock
	return fn()
}

func recordName(seq int64) string { return fmt.Sprintf("%020d.json", seq) }

func (s *Spool) readManifest() (manifest, error) {
	var m manifest
	if err := s.readJSON("state.json", 8192, &m); err != nil {
		return m, err
	}
	if m.RunID != s.runID || m.SpoolID == uuid.Nil || m.Last < 0 || m.Ack.Seq < 0 || m.Ack.Seq > m.Last || m.Last-m.Ack.Seq > MaxRecords || m.Bytes < 0 || m.Bytes > MaxBytes || m.Reported > m.Drops {
		return m, errors.New("invalid spool manifest")
	}
	if (m.Ack.Seq == 0 && m.Ack.Hash != "") || (m.Ack.Seq > 0 && len(m.Ack.Hash) != 64) {
		return m, errors.New("invalid spool acknowledgement")
	}
	// Before the sealed check, which reads Producer through remoteSettled.
	if err := m.validRemote(); err != nil {
		return m, err
	}
	if m.Sealed && (m.Last != m.Ack.Seq || m.Drops != m.Reported || m.Overflow != nil || !m.remoteSettled()) {
		return manifest{}, errors.New("sealed spool has unsettled evidence")
	}
	if !m.validOverflow(s.runID) {
		return m, errors.New("invalid spool overflow record")
	}
	return m, nil
}

// validOverflow reports whether the manifest's overflow record, when it has one, is the spool-full
// deny for runID that accounts for the drops not yet reported.
func (m manifest) validOverflow(runID uuid.UUID) bool {
	r := m.Overflow
	return r == nil || !(r.Seq != 0 || !r.ValidHash() || r.SpoolID != m.SpoolID || r.Decision.Request.RunID != runID || r.Count == 0 || r.Count > m.Drops-m.Reported ||
		r.DropEnd != m.Reported+r.Count || r.Decision.Decision != egress.Deny || r.Decision.RuleSource != "builtin:spool-full")
}

func (s *Spool) writeManifest(m manifest) error {
	s.manifestWrites++
	return s.writeJSON("state.json", m)
}

// Append commits before returning. An unreserved producer must refuse on any
// error; the proxy separately reserves capacity before forwarding requests.
func (s *Spool) Append(ctx context.Context, decision egress.DecisionLog) error {
	return s.append(ctx, decision)
}

// AppendAdmitted finishes a request whose proxy already reserved capacity.
// Overflow refuses new requests, never discards this previously admitted tail.
func (s *Spool) AppendAdmitted(ctx context.Context, decision egress.DecisionLog) error {
	return s.append(ctx, decision)
}

// append refuses only a record that does not fit. A refusal counted earlier
// waits in Drops for replay to report it; it does not close a spool with room,
// which a dropped runner link would otherwise keep closed until reconnect.
func (s *Spool) append(ctx context.Context, decision egress.DecisionLog) error {
	q := &queuedAppend{decision: decision, done: make(chan error, 1)}
	s.qmu.Lock()
	s.queue = append(s.queue, q)
	s.qmu.Unlock()
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	select {
	case err := <-q.done: // the committer before this one wrote it
		return err
	default:
	}
	s.qmu.Lock()
	batch := s.queue
	s.queue = nil
	s.qmu.Unlock()
	errs := make([]error, len(batch))
	err := s.locked(ctx, func() error { return s.commit(batch, errs) })
	for i, b := range batch {
		b.done <- errors.Join(err, errs[i])
	}
	return <-q.done
}

// commit writes batch's records and then one manifest that covers them all; errs[i] is record i's own
// refusal. Each record file is synced and renamed, one directory sync makes the renames durable, and
// only then does the manifest name them: the manifest stays the single commit point, and a crash
// before it leaves an uncommitted tail that the next append replaces, as before.
func (s *Spool) commit(batch []*queuedAppend, errs []error) error {
	m, err := s.readManifest()
	if err != nil {
		return err
	}
	if m.Sealed {
		return ErrSealed
	}
	read := m
	wrote, changed := false, false
	for i, q := range batch {
		if q.decision.Request.RunID != s.runID {
			errs[i] = errors.New("decision belongs to another run")
			continue
		}
		r := Record{SpoolID: m.SpoolID, Seq: m.Last + 1, Decision: q.decision}
		r.Hash = r.digest()
		b, err := json.Marshal(r)
		if err != nil {
			errs[i] = err
			continue
		}
		if m.Last-m.Ack.Seq >= MaxRecords || m.Bytes+int64(len(b)) > MaxBytes || len(b) > MaxRecordBytes {
			if m.Drops == ^uint64(0) {
				errs[i] = errors.New("spool overflow counter exhausted")
				continue
			}
			m.Drops++
			errs[i], changed = ErrFull, true
			continue
		}
		if err := s.writeFile(recordName(r.Seq), b); err != nil {
			// The batch fails, but a drop counted in it is still kept: its caller was told ErrFull, and
			// replay must report it. Nothing written in the batch is committed.
			if m.Drops != read.Drops {
				read.Drops = m.Drops
				err = errors.Join(err, s.writeManifest(read))
			}
			return inDir(s.dir, err)
		}
		m.Last = r.Seq
		m.Bytes += int64(len(b))
		wrote, changed = true, true
	}
	if !changed {
		return nil
	}
	if wrote {
		if err := s.syncDir(); err != nil {
			return err
		}
	}
	return s.writeManifest(m)
}

// Stats reports unacknowledged decisions and the total refused-overflow count.
func (s *Spool) Stats(ctx context.Context) (depth int, bytes int64, drops uint64, err error) {
	err = s.locked(ctx, func() error {
		m, e := s.readManifest()
		if e != nil {
			return e
		}
		depth = int(m.Last - m.Ack.Seq)
		bytes = m.Bytes
		drops = m.Drops
		return nil
	})
	return
}

// CanRecord checks durable capacity. Concurrent producers must also reserve
// capacity; checking alone cannot guarantee space for a later append. Refusals
// not yet reported do not refuse a spool with room (see append).
func (s *Spool) CanRecord(ctx context.Context) error {
	return s.locked(ctx, func() error {
		m, err := s.readManifest()
		if err != nil {
			return err
		}
		if m.Sealed {
			return ErrSealed
		}
		if m.Last-m.Ack.Seq >= MaxRecords || m.Bytes >= MaxBytes {
			return ErrFull
		}
		return nil
	})
}

// Refuse durably counts one egress admission refused because capacity is full.
func (s *Spool) Refuse(ctx context.Context) error {
	return s.locked(ctx, func() error {
		m, err := s.readManifest()
		if err != nil {
			return err
		}
		if m.Sealed {
			return ErrSealed
		}
		if m.Drops == ^uint64(0) {
			return errors.New("spool overflow counter exhausted")
		}
		m.Drops++
		return s.writeManifest(m)
	})
}

// Remote-sink grant ceilings (design §3.2 item 2). The last record slot and the
// last LossReserveBytes stay free for the builtin:spool-lost row. The byte
// credit covers GrantReservations requests reserved at the largest record
// (a scanned body-bearing request reserves MaxRecordBytes), as the local
// spool admits them, within the spool's own MaxBytes.
const (
	GrantRecords      = 5000
	GrantReservations = 16
	GrantBytes        = GrantReservations * MaxRecordBytes
	LossReserveBytes  = 4 << 10
	// grantRecordEnvelope is what a spool row adds to a record body: the
	// stream id, sequence and hash. A grant's byte credit leaves it per record.
	grantRecordEnvelope = 512
)

// A full grant, its record envelopes and the loss reserve fit an empty spool:
// this constant underflows, failing the build, if they ever do not.
const _ = uint64(MaxBytes - GrantBytes - GrantRecords*grantRecordEnvelope - LossReserveBytes)

var (
	// ErrProducer refuses a frame from a foreign, settled or out-of-order producer.
	ErrProducer = errors.New("spool refuses this producer sequence")
	// ErrStaleGen refuses a frame from a superseded sink session.
	ErrStaleGen = errors.New("spool sink session superseded")
)

func validInstance(s string) bool {
	if len(s) != 32 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

func (m manifest) validRemote() error {
	if p := m.Producer; p != nil {
		okDisp := p.Disposition == Appended || p.Disposition == Refused || p.Disposition == Lost
		if !validInstance(p.Instance) || p.Seq < 0 || (p.Seq == 0) != (p.Hash == "" && p.Disposition == "") || (p.Seq > 0 && (len(p.Hash) != 64 || !okDisp)) {
			return errors.New("invalid spool producer")
		}
	}
	if g := m.Grant; g != nil && (m.Producer == nil || g.Gen > m.Gen || g.Base < 0 || g.Base > g.Through || g.Through < m.Producer.Seq || g.Bytes < 0) {
		return errors.New("invalid spool grant")
	}
	if l := m.PendingLoss; l != nil && (l.Count == 0 || (l.Basis != LossExact && l.Basis != LossUpperBound)) {
		return errors.New("invalid spool pending loss")
	}
	return nil
}

// remoteSettled is the remote producer's part of Seal: no pending loss row and
// no write-ahead liability left.
func (m manifest) remoteSettled() bool {
	return m.PendingLoss == nil && (m.Grant == nil || m.Grant.Through == m.Producer.Seq)
}

// nextGrant is the credit a live session may hold now; it never lowers an
// outstanding ceiling (design Q5).
func nextGrant(m manifest) Grant {
	free := max(int64(MaxRecords)-(m.Last-m.Ack.Seq)-1, 0)
	n := min(int64(GrantRecords), free)
	bytes := max(min(int64(GrantBytes), int64(MaxBytes)-m.Bytes-LossReserveBytes-n*grantRecordEnvelope), 0)
	g := Grant{Gen: m.Gen, Base: m.Producer.Seq, Through: m.Producer.Seq + n, Bytes: bytes}
	if old := m.Grant; old != nil && old.Through > g.Through {
		g.Through = old.Through
	}
	return g
}

// Session is an accepted sink session's durable start.
type Session struct {
	Gen   uint64
	Seq   int64
	Grant Grant
}

// Accept starts a session for instance at a new generation and, in the same
// write, issues its grant or re-issues the outstanding one no lower. A settled
// producer is never reopened; only Succeed opens the spool to a new instance.
func (s *Spool) Accept(ctx context.Context, instance string) (out Session, err error) {
	if !validInstance(instance) {
		return out, ErrProducer
	}
	err = s.locked(ctx, func() error {
		m, err := s.readManifest()
		if err != nil {
			return err
		}
		if m.Sealed {
			return ErrSealed
		}
		if p := m.Producer; p != nil && (p.Instance != instance || m.Grant == nil) {
			return ErrProducer
		}
		m.Gen++
		if m.Producer == nil {
			m.Producer = &Producer{Instance: instance}
		}
		g := nextGrant(m)
		m.Grant = &g
		if err := s.writeManifest(m); err != nil {
			return err
		}
		out = Session{Gen: m.Gen, Seq: m.Producer.Seq, Grant: g}
		return nil
	})
	return
}

// Regrant persists fresh credit for the current session before it is sent,
// returning changed=false when the outstanding grant already equals it.
func (s *Spool) Regrant(ctx context.Context, gen uint64, instance string) (g Grant, changed bool, err error) {
	err = s.locked(ctx, func() error {
		m, err := s.readManifest()
		if err != nil {
			return err
		}
		if err := m.live(gen, instance); err != nil {
			return err
		}
		g = nextGrant(m)
		if g == *m.Grant {
			return nil
		}
		m.Grant, changed = &g, true
		return s.writeManifest(m)
	})
	return
}

func (m manifest) live(gen uint64, instance string) error {
	if m.Sealed {
		return ErrSealed
	}
	if gen != m.Gen {
		return ErrStaleGen
	}
	if m.Producer == nil || m.Producer.Instance != instance || m.Grant == nil {
		return ErrProducer
	}
	return nil
}

// Produced is one record frame from the remote proxy.
type Produced struct {
	Gen      uint64
	Instance string
	Seq      int64
	Hash     string
	Admitted bool
	Decision egress.DecisionLog
}

// Produce gives a producer sequence its one durable disposition, in the same
// manifest write as its effect: appended (Last/Bytes), refused (Drops, an
// unreserved record only) or lost (pending_loss, an admitted record that still
// did not fit). A resend of the last sequence returns the stored disposition
// with fresh=false; an older one returns "" (durable, nothing to redo).
func (s *Spool) Produce(ctx context.Context, p Produced) (disp Disposition, fresh bool, err error) {
	err = s.locked(ctx, func() error {
		m, err := s.readManifest()
		if err != nil {
			return err
		}
		if err := m.live(p.Gen, p.Instance); err != nil {
			return err
		}
		prod := m.Producer
		switch {
		case p.Seq < prod.Seq:
			return nil
		case p.Seq == prod.Seq && p.Seq > 0:
			if p.Hash != prod.Hash {
				return ErrProducer
			}
			disp = prod.Disposition
			return nil
		case p.Seq != prod.Seq+1 || p.Seq > m.Grant.Through || len(p.Hash) != 64 || p.Decision.Request.RunID != s.runID:
			return ErrProducer
		}
		fresh = true
		next := *prod
		next.Seq, next.Hash = p.Seq, p.Hash
		r := Record{SpoolID: m.SpoolID, Seq: m.Last + 1, Decision: p.Decision}
		r.Hash = r.digest()
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		full := m.Last-m.Ack.Seq >= MaxRecords || m.Bytes+int64(len(b)) > MaxBytes || len(b) > MaxRecordBytes
		switch {
		case !full && (p.Admitted || m.Drops == m.Reported):
			if err := s.writeBytes(recordName(r.Seq), b); err != nil {
				return err
			}
			m.Last, m.Bytes, next.Disposition = r.Seq, m.Bytes+int64(len(b)), Appended
		case !p.Admitted && m.Drops != ^uint64(0):
			m.Drops++
			next.Disposition = Refused
		default:
			// Admitted evidence that did not fit, or a refusal the exhausted
			// counter cannot hold: lost, never a second Drops meaning.
			m.PendingLoss = addLoss(m.PendingLoss, 1, LossExact)
			next.Disposition = Lost
		}
		m.Producer, disp = &next, next.Disposition
		return s.writeManifest(m)
	})
	return
}

// ProducerRefused counts the instance's cumulative refused admissions into
// Drops once: a resent or older total changes nothing.
func (s *Spool) ProducerRefused(ctx context.Context, gen uint64, instance string, total uint64) error {
	return s.locked(ctx, func() error {
		m, err := s.readManifest()
		if err != nil {
			return err
		}
		if err := m.live(gen, instance); err != nil {
			return err
		}
		if total <= m.Producer.Refused {
			return nil
		}
		delta := total - m.Producer.Refused
		if m.Drops > ^uint64(0)-delta {
			return errors.New("spool overflow counter exhausted")
		}
		next := *m.Producer
		next.Refused = total
		m.Drops += delta
		m.Producer = &next
		return s.writeManifest(m)
	})
}

func addLoss(l *PendingLoss, n uint64, basis LossBasis) *PendingLoss {
	if l == nil {
		return &PendingLoss{Count: n, Basis: basis}
	}
	out := *l
	out.Count += n
	if basis == LossUpperBound {
		out.Basis = LossUpperBound
	}
	return &out
}
