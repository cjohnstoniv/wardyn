// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// The log --log-file names is rotated at logFileMax, keeping logFileKeep older files (.1 the newest):
// 40 MiB on disk at most. At the backed-off retry lines' rate one file holds weeks of a failing
// runner; at the once-a-second rate an older runnerd logged it still held a quarter of an hour.
const (
	logFileMax  = 10 << 20
	logFileKeep = 3
)

// rotatingLog is the file --log-file names, rotated by size. The LaunchAgent passes it: launchd's
// StandardErrorPath cannot be rotated (newsyslog needs root), so under launchd runnerd keeps its own log.
// launchd still holds that path open as stdout and stderr, so a panic or a line not written through slog
// lands in the file that was current at start; the size is read again at each start.
type rotatingLog struct {
	mu   sync.Mutex
	path string
	max  int64
	keep int
	f    *os.File
	size int64
}

func openRotatingLog(path string, max int64, keep int) (*rotatingLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	l := &rotatingLog{path: path, max: max, keep: keep}
	if err := l.open(); err != nil {
		return nil, err
	}
	if l.size >= max {
		if err := l.rotate(); err != nil {
			return nil, err
		}
	}
	return l, nil
}

func (l *rotatingLog) open() error {
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
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

// rotate moves path.1 to path.2 and so on, the oldest over the one past keep, then path to path.1, and
// starts path afresh.
func (l *rotatingLog) rotate() error {
	if err := l.f.Close(); err != nil {
		return err
	}
	for i := l.keep - 1; i >= 1; i-- {
		if err := os.Rename(fmt.Sprintf("%s.%d", l.path, i), fmt.Sprintf("%s.%d", l.path, i+1)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := os.Rename(l.path, l.path+".1"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return l.open()
}

func (l *rotatingLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size > 0 && l.size+int64(len(p)) > l.max {
		if err := l.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

func (l *rotatingLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}
