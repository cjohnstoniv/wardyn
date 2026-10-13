// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerspool

import (
	"context"
	"errors"
	"os"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"golang.org/x/sys/unix"
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
	writer, err := s.root.OpenFile("writer.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, writer.Close()) }()
	if err = s.own(writer); err != nil {
		return false, err
	}
	st, err := writer.Stat()
	link, linkErr := s.root.Lstat("writer.lock")
	if err != nil || linkErr != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || link.Mode()&os.ModeSymlink != 0 || !os.SameFile(st, link) {
		return false, errors.New("unsafe spool writer lock")
	}
	if err = unix.Flock(int(writer.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return false, nil
		}
		return false, err
	}
	defer unix.Flock(int(writer.Fd()), unix.LOCK_UN) //nolint:errcheck // close also releases the lock
	err = s.locked(ctx, func() error {
		m, err := s.readManifest()
		if err != nil {
			return err
		}
		if receipt(m) != expected || m.Last != m.Ack.Seq || m.Drops != m.Reported || m.Overflow != nil {
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
