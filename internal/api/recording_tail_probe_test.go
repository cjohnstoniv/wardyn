// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// PROBE (F4-run-wait-upload-audit, probe b2 + b3).
// Intended destination: internal/api/recording_tail_probe_test.go (package api).
// Reuses decodeCastOutput (attach_recording_test.go), errReader
// (recording_upload_test.go) and recRecorder / adminToken from the
// package's existing tests. No PG, no build tags.
//
// Invariant pinned, on BOTH recording sinks:
//
//	b2  Upload path (buildMaskingBody in recording.go): with a registered
//	    secret (so the MaskingWriter pipe branch is taken), a body whose LAST
//	    bytes end mid-escape-sequence and carry no trailing newline reaches
//	    SaveCast byte-for-byte — the (maxLen-1)-byte retained tail
//	    MaskingWriter.Write holds back is flushed by mw.Close()
//	    (MaskingWriter.Close in internal/secretmask) and arrives BEFORE the
//	    clean EOF. Fails if the copy goroutine stops calling Close, if Close
//	    stops flushing, or if CloseWithError is stamped before the flush lands.
//
//	b3  Live attach path (newSessionRecorder in attach.go): a session whose
//	    final PTY write ends in an escape prefix that is ALSO a strict prefix of
//	    a registered secret is withheld by pendingTailLen and must be emitted
//	    by finish's liveMaskWriter.flushLocked (both in attach.go) — the cast
//	    must contain the escape bytes, decode as valid JSON lines, and must NOT
//	    contain the secret. Fails if finish stops flushing, if flushLocked drops
//	    the tail, or if the flushed tail is appended outside a well-formed
//	    event line.
package api

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const probeESC = "\x1b"

func TestProbeF4_BuildMaskingBody_MidEscapeTailReachesStoreVerbatim(t *testing.T) {
	runID := uuid.New()
	reg := secretmask.NewRegistry()
	// Long secret => long retained tail (maxLen-1 bytes), so the ESC-terminated
	// tail below is entirely inside the withheld window.
	secret := []byte("wardyn-long-secret-value-0123456789abcdef")
	reg.Add(runID, secret)

	// asciinema-shaped body, last line NOT newline-terminated and ending mid-CSI —
	// the shape a recorder killed mid-write, or an agent's final partial frame, leaves.
	// asciinema json-escapes ESC as \u001b inside the string; the raw form is used
	// here too (a .log fallback is raw bytes) so both renderings cross the masker.
	body := `{"version":2,"width":80,"height":24}` + "\n" +
		`[0.10,"o","echo hi\r\n"]` + "\n" +
		`[0.20,"o","\u001b[38;5;2` + "\n" +
		probeESC + `[38;5;2`

	for _, tc := range []struct {
		name string
		src  func() io.Reader
	}{
		{"one read", func() io.Reader { return strings.NewReader(body) }},
		// Byte-at-a-time source: every byte crosses the retained-tail boundary.
		{"byte-at-a-time", func() io.Reader { return oneByteReader{r: strings.NewReader(body)} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, cleanup := buildMaskingBody(tc.src(), reg, runID)
			defer cleanup()
			out, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("read masked body: %v (a clean EOF is required after the tail flush)", err)
			}
			if string(out) != body {
				t.Fatalf("masked body != input (no secret present, so it must round-trip verbatim)\n got: %q\nwant: %q", out, body)
			}
			if !bytes.HasSuffix(out, []byte(probeESC+`[38;5;2`)) {
				t.Fatalf("the mid-escape tail was dropped or altered: %q", out)
			}
		})
	}
}

// TestProbeF4_BuildMaskingBody_TailFlushBeforeError pins the ordering the
// handler depends on: when the SOURCE errors (MaxBytesError shape), the
// retained tail is still flushed first (in buildMaskingBody, mw.Close() runs
// before pw.CloseWithError), and the reader sees the error — never a clean EOF
// that would let a truncated cast be audited as `recording.upload success`.
func TestProbeF4_BuildMaskingBody_TailFlushBeforeError(t *testing.T) {
	runID := uuid.New()
	reg := secretmask.NewRegistry()
	reg.Add(runID, []byte("wardyn-long-secret-value-0123456789abcdef"))
	srcErr := io.ErrUnexpectedEOF
	prefix := `[0.20,"o","` + probeESC + `[`
	src := io.MultiReader(strings.NewReader(prefix), errReader{srcErr})

	r, cleanup := buildMaskingBody(src, reg, runID)
	defer cleanup()
	out, err := io.ReadAll(r)
	if err == nil || err != srcErr {
		t.Fatalf("reader err = %v, want the source error %v (never a clean EOF)", err, srcErr)
	}
	if string(out) != prefix {
		t.Fatalf("bytes before the error = %q, want %q (tail must be flushed before CloseWithError)", out, prefix)
	}
}

func TestProbeF4_SessionRecorder_MidEscapeTailWithheldThenFlushed(t *testing.T) {
	store, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	runID := uuid.New()
	// The secret STARTS with the escape prefix the session ends on, so the final
	// ESC "[0m" is a strict prefix of a registered secret and pendingTailLen
	// withholds it (pendingTailLen in attach.go). Only finish's
	// liveMaskWriter.flushLocked can emit it.
	escPrefix := probeESC + "[0m"
	secret := escPrefix + "TOKEN-super-secret-value-9876"
	reg := secretmask.NewRegistry()
	reg.Add(runID, []byte(secret))

	srv := New(Config{RecordingStore: store, MaskRegistry: reg, Audit: &recRecorder{}, AdminToken: adminToken})
	tee, finish := srv.newSessionRecorder(types.AgentRun{ID: runID}, "esc", runner.AttachOptions{Cols: 80, Rows: 24})
	if tee == nil {
		t.Fatal("tee writer is nil with a RecordingStore configured")
	}

	_, _ = tee.Write([]byte("ls -la\r\n"))
	_, _ = tee.Write([]byte("total 0" + escPrefix)) // ends mid-escape AND mid-secret-prefix

	finish(context.Background(), types.ActorHuman, "erin@example.com")

	rc, err := store.OpenCast(context.Background(), recording.CastKey(runID.String(), "esc"))
	if err != nil {
		t.Fatalf("OpenCast: %v", err)
	}
	defer rc.Close()
	raw, _ := io.ReadAll(rc)
	cast := string(raw)

	if !strings.HasSuffix(cast, "\n") {
		t.Fatalf("persisted cast does not end on a line boundary (flushed tail landed outside an event line):\n%q", cast)
	}
	out := decodeCastOutput(t, cast)
	want := "ls -la\r\n" + "total 0" + escPrefix
	if out != want {
		t.Fatalf("decoded cast output = %q, want %q (withheld escape tail dropped or corrupted by finish)", out, want)
	}
	if strings.Contains(cast, "TOKEN-super-secret-value-9876") {
		t.Fatalf("secret leaked into the cast:\n%s", cast)
	}
}

// oneByteReader hands out one byte per Read so every byte crosses the masker's
// retained-tail boundary.
type oneByteReader struct{ r io.Reader }

func (o oneByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return o.r.Read(p[:1])
}
