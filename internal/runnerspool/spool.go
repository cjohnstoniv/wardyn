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
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"github.com/cjohnstoniv/wardyn/internal/ackcursor"
	"github.com/cjohnstoniv/wardyn/internal/egress"
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
}

// Spool is safe across goroutines and independently opened proxy/daemon handles.
type Spool struct {
	mu    sync.Mutex
	root  *os.Root
	lock  *os.File
	runID uuid.UUID
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
		return nil, err
	}
	st, statErr := lock.Stat()
	link, linkErr := root.Lstat("lock")
	if statErr != nil || linkErr != nil || !st.Mode().IsRegular() || link.Mode()&os.ModeSymlink != 0 || !os.SameFile(st, link) || st.Mode().Perm() != 0600 {
		lock.Close()
		root.Close()
		return nil, errors.New("unsafe spool lock file")
	}
	s := &Spool{root: root, lock: lock, runID: runID}
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

// Close releases this process's handles; it never deletes pending decisions.
func (s *Spool) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return errors.Join(s.lock.Close(), s.root.Close())
}

func (s *Spool) locked(ctx context.Context, fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	if m.Sealed && (m.Last != m.Ack.Seq || m.Drops != m.Reported || m.Overflow != nil) {
		return manifest{}, errors.New("sealed spool has unsettled evidence")
	}
	if r := m.Overflow; r != nil && (r.Seq != 0 || !r.ValidHash() || r.SpoolID != m.SpoolID || r.Decision.Request.RunID != s.runID || r.Count == 0 || r.Count > m.Drops-m.Reported || r.DropEnd != m.Reported+r.Count || r.Decision.Decision != egress.Deny || r.Decision.RuleSource != "builtin:spool-full") {
		return m, errors.New("invalid spool overflow record")
	}
	return m, nil
}

func (s *Spool) writeManifest(m manifest) error { return s.writeJSON("state.json", m) }

// Append commits before returning. An unreserved producer must refuse on any
// error; the proxy separately reserves capacity before forwarding requests.
func (s *Spool) Append(ctx context.Context, decision egress.DecisionLog) error {
	return s.append(ctx, decision, false)
}

// AppendAdmitted finishes a request whose proxy already reserved capacity.
// Overflow refuses new requests, never discards this previously admitted tail.
func (s *Spool) AppendAdmitted(ctx context.Context, decision egress.DecisionLog) error {
	return s.append(ctx, decision, true)
}
func (s *Spool) append(ctx context.Context, decision egress.DecisionLog, admitted bool) error {
	return s.locked(ctx, func() error {
		m, err := s.readManifest()
		if err != nil {
			return err
		}
		if m.Sealed {
			return ErrSealed
		}
		if decision.Request.RunID != s.runID {
			return errors.New("decision belongs to another run")
		}
		r := Record{SpoolID: m.SpoolID, Seq: m.Last + 1, Decision: decision}
		r.Hash = r.digest()
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if m.Last-m.Ack.Seq >= MaxRecords || m.Bytes+int64(len(b)) > MaxBytes || len(b) > MaxRecordBytes || (!admitted && m.Drops > m.Reported) {
			if m.Drops == ^uint64(0) {
				return errors.New("spool overflow counter exhausted")
			}
			m.Drops++
			if err := s.writeManifest(m); err != nil {
				return err
			}
			return ErrFull
		}
		// A crash after the record rename but before the manifest commit left only
		// an uncommitted tail. Replacing it is safe: Append never acknowledged it.
		if err := s.writeBytes(recordName(r.Seq), b); err != nil {
			return err
		}
		m.Last = r.Seq
		m.Bytes += int64(len(b))
		return s.writeManifest(m)
	})
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

// CanRecord checks durable overflow state. Concurrent producers must also
// reserve capacity; checking alone cannot guarantee space for a later append.
func (s *Spool) CanRecord(ctx context.Context) error {
	return s.locked(ctx, func() error {
		m, err := s.readManifest()
		if err != nil {
			return err
		}
		if m.Sealed {
			return ErrSealed
		}
		if m.Last-m.Ack.Seq >= MaxRecords || m.Bytes >= MaxBytes || m.Drops > m.Reported {
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
