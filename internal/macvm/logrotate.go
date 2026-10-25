// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"os"
	"sync"
)

// HelperLogMax is the size at which helper.log rotates. Two files are kept
// (helper.log and helper.log.1), so the log never passes 2 x HelperLogMax.
const HelperLogMax = 16 << 20

// RotatingLog is an append-only file that rotates to <path>.1 at Max bytes,
// replacing the previous .1.
type RotatingLog struct {
	Path string
	Max  int64

	mu   sync.Mutex
	f    *os.File
	size int64
}

// OpenRotatingLog opens (or creates, mode 0600) path for appending.
func OpenRotatingLog(path string, max int64) (*RotatingLog, error) {
	l := &RotatingLog{Path: path, Max: max}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *RotatingLog) open() error {
	f, err := os.OpenFile(l.Path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	l.f, l.size = f, st.Size()
	return nil
}

// Write appends p, rotating first when p would take the file past Max. One
// write longer than Max is truncated to Max, so the cap holds.
func (l *RotatingLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(p)
	if int64(len(p)) > l.Max {
		p = p[:l.Max]
	}
	if l.size > 0 && l.size+int64(len(p)) > l.Max {
		if err := l.rotate(); err != nil {
			return 0, err
		}
	}
	w, err := l.f.Write(p)
	l.size += int64(w)
	if err != nil {
		return w, err
	}
	return n, nil
}

func (l *RotatingLog) rotate() error {
	if err := l.f.Close(); err != nil {
		return err
	}
	if err := os.Rename(l.Path, l.Path+".1"); err != nil && !os.IsNotExist(err) {
		return err
	}
	return l.open()
}

func (l *RotatingLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}
