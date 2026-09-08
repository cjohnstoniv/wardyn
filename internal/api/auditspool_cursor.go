// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
)

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
