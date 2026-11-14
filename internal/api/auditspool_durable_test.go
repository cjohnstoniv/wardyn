// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// spoolDirSyncs swaps fsyncSpoolDir for fn for the rest of the test.
func spoolDirSyncs(t *testing.T, fn func(dir string) error) {
	t.Helper()
	old := fsyncSpoolDir
	fsyncSpoolDir = fn
	t.Cleanup(func() { fsyncSpoolDir = old })
}

// Boot counts the backlog and the quarantine before the drain starts, over a spool that grows
// without bound while the store is down. What it allocates must not grow with them, or a backlog
// larger than memory stops every restart.
func TestAuditSpoolBootCountMemoryIsFlatInTheBacklog(t *testing.T) {
	sp, size := spoolBigTail(t)
	b, err := os.ReadFile(sp.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sp.quarantinePath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
	const cursor = 1 << 20 // a line boundary: 1024 lines of 1024 bytes
	sp.writeSpoolCursor(cursor)
	const bound = 4 << 20 // far under the two 24 MiB files, far over the fixed buffer
	var re *AuditSpool
	if got := allocatedBy(func() { re, err = NewAuditSpool(sp.path) }); err != nil || got > bound {
		t.Fatalf("NewAuditSpool: err %v, allocated %d bytes over a %d-byte spool and quarantine, want nil and <= %d", err, got, size, bound)
	}
	t.Cleanup(func() { re.f.Close() })
	if re.consumed != cursor || re.Lines() != 23<<10 || re.Quarantined() != 24<<10 {
		t.Errorf("boot read cursor %d, lines %d, quarantined %d; want %d, %d, %d",
			re.consumed, re.Lines(), re.Quarantined(), cursor, 23<<10, 24<<10)
	}
}

// The streamed count keeps the line semantics of the whole-file count it replaced, across the
// boundaries of its read buffer: a trailing newline is a terminator, a torn tail is a line, and
// nothing but whitespace is no line.
func TestAuditSpoolLineCountSemantics(t *testing.T) {
	x := func(n int) string { return strings.Repeat("x", n) }
	nl := func(n int) string { return strings.Repeat("\n", n) }
	for _, c := range []struct {
		name, body string
		from       int64
		want       int64
	}{
		{"empty", "", 0, 0},
		{"blank lines only", " \n\t\n\n", 0, 0},
		{"one line", "a\n", 0, 1},
		{"torn tail", "a\nb", 0, 2},
		{"trailing newlines are terminators", "a\nb\n\n\n", 0, 2},
		{"a blank line inside counts", "a\n \nb\n", 0, 3},
		{"from the cursor", "aa\nbb\ncc\n", 3, 2},
		{"cursor at the end", "aa\nbb\n", 6, 0},
		{"cursor past the end counts the file", "aa\nbb\n", 99, 2},
		{"newline run across a buffer edge", x(spoolCopyChunk-1) + nl(3), 0, 1},
		{"line ends on a buffer edge", x(spoolCopyChunk-1) + "\n" + "b\n", 0, 2},
		{"newlines only past a buffer", nl(spoolCopyChunk + 7), 0, 0},
		{"content then newlines past a buffer", "a" + nl(spoolCopyChunk+7), 0, 1},
		{"content after a buffer of newlines", nl(spoolCopyChunk+7) + "a", 0, spoolCopyChunk + 8},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "spool.jsonl")
			if err := os.WriteFile(path, []byte(c.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := countSpoolLinesFrom(path, c.from); got != c.want {
				t.Errorf("countSpoolLinesFrom(%d) = %d, want %d", c.from, got, c.want)
			}
		})
	}
	if got := countSpoolLines(filepath.Join(t.TempDir(), "missing")); got != 0 {
		t.Errorf("countSpoolLines(missing) = %d, want 0", got)
	}
}

// A newly created spool's name is synced at boot: Append syncs the file, not its directory entry.
func TestNewAuditSpoolSyncsItsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit-spool.jsonl")
	var synced []string
	spoolDirSyncs(t, func(dir string) error {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("directory synced before the spool existed: %v", err)
		}
		synced = append(synced, dir)
		return nil
	})
	sp, err := NewAuditSpool(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sp.f.Close() })
	if len(synced) != 1 || synced[0] != filepath.Dir(path) {
		t.Errorf("directory syncs %q, want one of %q", synced, filepath.Dir(path))
	}
}

// compact renames a new file over the spool and appends go to it from then on, so the rename must
// be durable before compact returns, or a host crash brings the old file back and loses every
// event acknowledged into the new one. A failed directory sync does not undo a compaction that has
// happened: the new file stays in use.
func TestAuditSpoolCompactionSyncsTheDirectoryAfterTheRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit-spool.jsonl")
	sp, err := NewAuditSpool(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sp.f.Close() })
	for _, action := range []string{"run.create", "run.start"} {
		if err := sp.Append(newTestEvent(action)); err != nil {
			t.Fatal(err)
		}
	}
	size := spoolFileSize(path)
	var renamed []bool
	spoolDirSyncs(t, func(dir string) error {
		b, _ := os.ReadFile(path)
		renamed = append(renamed, dir == filepath.Dir(path) && bytes.Equal(b, []byte("{\"kept\":1}\n")))
		return nil
	})
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if err := sp.compact([][]byte{[]byte(`{"kept":1}`)}, size, size); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if len(renamed) != 1 || !renamed[0] {
		t.Fatalf("directory syncs after the rename %v, want exactly one, after it", renamed)
	}

	spoolDirSyncs(t, func(string) error { return syscall.EIO })
	old := sp.f
	if err := sp.compact([][]byte{[]byte(`{"kept":2}`)}, spoolFileSize(path), spoolFileSize(path)); err != nil {
		t.Fatalf("compact with a failed directory sync = %v, want nil: the rename has happened", err)
	}
	if sp.f == old || sp.consumed != 0 {
		t.Errorf("compact with a failed directory sync left the old file in use (f swapped %v, consumed %d)", sp.f != old, sp.consumed)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, []byte("{\"kept\":2}\n")) {
		t.Errorf("spool after the second compact = %q", b)
	}
}

// A quarantine's directory is synced after the file is written and before the spool gives the
// line up. A failed sync keeps the line in the spool; a filesystem with no directory sync does not
// block the move.
func TestAuditSpoolQuarantineSyncsTheDirectoryBeforeTheLineLeaves(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit-spool.jsonl")
	sp, err := NewAuditSpool(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sp.f.Close() })
	poison := newTestEvent("poison")
	for _, ev := range []types.AuditEvent{poison, newTestEvent("good")} {
		if err := sp.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	var ordered []bool
	spoolDirSyncs(t, func(string) error {
		q, _ := os.ReadFile(sp.quarantinePath())
		s, _ := os.ReadFile(path)
		id := []byte(poison.ID.String())
		ordered = append(ordered, bytes.Contains(q, id) && bytes.Contains(s, id))
		return syscall.EIO
	})
	rec := &rejectingRecorder{reject: "poison"}
	for i := 0; i < spoolPoisonAttempts; i++ {
		if _, err := sp.Drain(context.Background(), rec, 100); err == nil || errors.Is(err, errSpoolLineQuarantined) {
			t.Fatalf("tick %d: Drain = %v, want a failure that is not a quarantine", i, err)
		}
	}
	if len(ordered) == 0 || !ordered[0] {
		t.Fatalf("quarantine directory syncs %v; want the first with the line in both files", ordered)
	}
	if s, _ := os.ReadFile(path); sp.Quarantined() != 0 || !bytes.Contains(s, []byte(poison.ID.String())) {
		t.Fatalf("a failed directory sync gave the line up: quarantined %d", sp.Quarantined())
	}

	spoolDirSyncs(t, func(string) error { return syscall.EINVAL })
	if _, err := sp.Drain(context.Background(), rec, 100); !errors.Is(err, errSpoolLineQuarantined) {
		t.Fatalf("Drain with no directory sync on offer = %v, want the line quarantined", err)
	}
	if sp.Quarantined() != 1 || sp.Lines() != 0 {
		t.Errorf("after the move: quarantined %d, lines %d; want 1 and 0", sp.Quarantined(), sp.Lines())
	}
}

// A directory sync the filesystem does not offer is not a failure; every other error is.
func TestAuditSpoolSyncDirNotOfferedIsNotAFailure(t *testing.T) {
	sp := &AuditSpool{path: filepath.Join(t.TempDir(), "audit-spool.jsonl")}
	for _, c := range []struct {
		err    error
		failed bool
	}{
		{nil, false},
		{syscall.EINVAL, false},
		{syscall.ENOTSUP, false},
		{syscall.EOPNOTSUPP, false},
		{&os.PathError{Op: "sync", Path: ".", Err: syscall.ENOTSUP}, false},
		{syscall.EIO, true},
		{syscall.ENOSPC, true},
		{syscall.EACCES, true},
	} {
		spoolDirSyncs(t, func(string) error { return c.err })
		if err := sp.syncDir(); (err != nil) != c.failed {
			t.Errorf("syncDir when the directory sync answers %v = %v, want a failure: %v", c.err, err, c.failed)
		}
	}
}
