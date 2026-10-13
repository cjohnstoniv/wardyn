// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerspool

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
)

func (s *Spool) readJSON(name string, limit int64, out any) error {
	f, err := s.root.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	link, err := s.root.Lstat(name)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || link.Mode()&os.ModeSymlink != 0 || !os.SameFile(st, link) || st.Mode().Perm() != 0600 || st.Size() > limit {
		return errors.New("unsafe or oversized spool file")
	}
	dec := json.NewDecoder(io.LimitReader(f, limit+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("trailing spool data")
	}
	return nil
}

func (s *Spool) writeJSON(name string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.writeBytes(name, b)
}

func (s *Spool) writeBytes(name string, b []byte) error {
	// The process-shared lock gives this fixed temporary name one writer. A
	// prior crash's temporary file is never mistaken for committed state.
	tmp := "write.tmp"
	if err := s.root.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := s.root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	// Before the rename: the file that replaces name is the spool owner's, as the one it replaces was.
	ownErr := s.own(f)
	_, writeErr := io.Copy(f, bytes.NewReader(b))
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(ownErr, writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := s.root.Rename(tmp, name); err != nil {
		return err
	}
	d, err := s.root.Open(".")
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
