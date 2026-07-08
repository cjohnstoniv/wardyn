// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sinks

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// FileConfig holds the configuration for a FileSink.
type FileConfig struct {
	// Path is the log file path (required). Rotated files are named
	// <path>.1, <path>.2, … <path>.N.
	Path string `json:"path"`
	// MaxBytes is the maximum size of the active log file before rotation
	// (default 100 MiB). 0 means no rotation.
	MaxBytes int64 `json:"max_bytes,omitempty"`
	// Keep is the number of rotated files to retain (default 5). The active
	// file is not counted; older files beyond Keep are deleted.
	Keep int `json:"keep,omitempty"`
}

func (c *FileConfig) withDefaults() FileConfig {
	out := *c
	if out.MaxBytes == 0 {
		out.MaxBytes = 100 * 1024 * 1024 // 100 MiB
	}
	if out.Keep == 0 {
		out.Keep = 5
	}
	return out
}

// FileSink appends JSON-lines audit events to a file, rotating when the file
// exceeds MaxBytes. Up to Keep rotated files are retained; older ones are
// deleted. All operations are serialised by a mutex — the sink is safe for
// concurrent Emit calls.
type FileSink struct {
	cfg  FileConfig
	mu   sync.Mutex
	file *os.File
	size int64 // bytes written to the current active file
}

// NewFileSink opens (or creates) the log file at cfg.Path.
// Returns an error if the file cannot be opened.
func NewFileSink(cfg FileConfig) (*FileSink, error) {
	cfg = cfg.withDefaults()
	if cfg.Path == "" {
		return nil, fmt.Errorf("sinks.file: path is required")
	}
	s := &FileSink{cfg: cfg}
	if err := s.openFile(); err != nil {
		return nil, err
	}
	return s, nil
}

// Name implements audit.Sink.
func (s *FileSink) Name() string { return "file" }

// Emit serialises ev as a JSON line and appends it to the active log file,
// rotating if necessary.
func (s *FileSink) Emit(_ context.Context, ev types.AuditEvent) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("sinks.file: marshal: %w", err)
	}
	b = append(b, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cfg.MaxBytes > 0 && s.size+int64(len(b)) > s.cfg.MaxBytes {
		if err := s.rotate(); err != nil {
			return fmt.Errorf("sinks.file: rotate: %w", err)
		}
	}

	n, err := s.file.Write(b)
	if err != nil {
		return fmt.Errorf("sinks.file: write: %w", err)
	}
	s.size += int64(n)
	return nil
}

// Close flushes and closes the active log file.
func (s *FileSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

// openFile opens the active log file for appending, recording its current size.
func (s *FileSink) openFile() error {
	f, err := os.OpenFile(s.cfg.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("sinks.file: open %q: %w", s.cfg.Path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("sinks.file: stat %q: %w", s.cfg.Path, err)
	}
	s.file = f
	s.size = info.Size()
	return nil
}

// rotate renames the active file to <path>.1, shifts older rotated files up,
// removes any beyond Keep, then opens a fresh active file. Caller must hold mu.
func (s *FileSink) rotate() error {
	if s.file != nil {
		if err := s.file.Close(); err != nil {
			return fmt.Errorf("close active file: %w", err)
		}
		s.file = nil
	}

	// Shift rotated files: .N-1 → .N (drop if N > Keep), then .1 → .2, etc.
	for i := s.cfg.Keep; i >= 1; i-- {
		older := rotatedName(s.cfg.Path, i)
		newer := rotatedName(s.cfg.Path, i-1)
		if i-1 == 0 {
			newer = s.cfg.Path
		}
		if i > s.cfg.Keep {
			// Already past Keep; shouldn't happen in normal flow but guard anyway.
			_ = os.Remove(older)
			continue
		}
		// If .N slot is already occupied, remove it first so Rename is atomic.
		if _, err := os.Lstat(older); err == nil {
			if i == s.cfg.Keep {
				_ = os.Remove(older)
			}
		}
		if _, err := os.Lstat(newer); err == nil {
			_ = os.Rename(newer, older)
		}
	}
	// After rotation the active path is free; open a fresh file.
	return s.openFile()
}

// rotatedName returns the path for the i-th rotated file.
func rotatedName(base string, i int) string {
	dir := filepath.Dir(base)
	name := filepath.Base(base)
	return filepath.Join(dir, fmt.Sprintf("%s.%d", name, i))
}
