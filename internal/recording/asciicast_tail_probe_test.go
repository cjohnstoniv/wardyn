// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// PROBE (F4-run-wait-upload-audit, probe b1).
// Intended destination: internal/recording/asciicast_tail_probe_test.go (package recording).
//
// Invariant pinned: a CastWriter whose LAST Write ends mid-ANSI-escape-sequence
// (e.g. ESC "[" with the parameter bytes never arriving) emits that write as a
// complete, well-formed asciicast "o" event — nothing withheld, nothing
// dropped, every line valid JSON, reassembled bytes == input. The only bytes
// CastWriter may hold back are an INCOMPLETE UTF-8 RUNE (asciicast.go:97-107);
// ESC is ASCII, so the incompleteTailLen scan (:137-148) must return 0 for it.
//
// Fails if: incompleteTailLen starts treating ESC/CSI as a rune lead byte; Write
// stops emitting when DecodeLastRune reports RuneError on an ASCII tail; the
// event's JSON escaping of ESC breaks the line; or a future Flush/Close
// regresses the ASCII path.
package recording

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const esc = "\x1b"

// castLines returns the per-line JSON payloads (skipping the header) and fails
// the test on any malformed line — a mid-line cut in the persisted audit tail.
func castLines(t *testing.T, cast string) []string {
	t.Helper()
	if !strings.HasSuffix(cast, "\n") {
		t.Fatalf("cast does not end in a newline: the last event line is cut mid-line:\n%q", cast)
	}
	lines := strings.Split(strings.TrimRight(cast, "\n"), "\n")
	var out []string
	for i, ln := range lines {
		if i == 0 {
			var hdr CastHeader
			if err := json.Unmarshal([]byte(ln), &hdr); err != nil || hdr.Version != 2 {
				t.Fatalf("header line invalid: %v (%q)", err, ln)
			}
			continue
		}
		var ev []json.RawMessage
		if err := json.Unmarshal([]byte(ln), &ev); err != nil || len(ev) != 3 {
			t.Fatalf("event line %d is not a 3-element JSON array: %v (%q)", i, err, ln)
		}
		var data string
		if err := json.Unmarshal(ev[2], &data); err != nil {
			t.Fatalf("event line %d payload not a JSON string: %v (%q)", i, err, ln)
		}
		out = append(out, data)
	}
	return out
}

func TestProbeF4_CastWriter_MidEscapeSequenceTailIsFlushed(t *testing.T) {
	for _, tc := range []struct {
		name string
		last string // the final write, ending mid-escape-sequence
	}{
		{"bare ESC", "prompt$ " + esc},
		{"CSI introducer", "prompt$ " + esc + "["},
		{"CSI with partial params", esc + "[38;5;2"},
		{"OSC title start", esc + "]0;wardyn"},
		{"DCS", esc + "P1$"},
		{"ESC after a full rune", "世 " + esc + "["},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			cw := NewCastWriter(&buf, 80, 24, time.Unix(1700000000, 0))
			first := "echo hi\r\n"
			if _, err := cw.Write([]byte(first)); err != nil {
				t.Fatalf("Write 1: %v", err)
			}
			n, err := cw.Write([]byte(tc.last))
			if err != nil {
				t.Fatalf("Write 2: %v", err)
			}
			if n != len(tc.last) {
				t.Fatalf("Write 2 accepted %d bytes, want %d", n, len(tc.last))
			}
			// No Flush/Close exists (asciicast.go:40-44): whatever is in buf NOW is
			// the persisted audit tail. The ESC-terminated write must already be there.
			payloads := castLines(t, buf.String())
			if len(payloads) != 2 {
				t.Fatalf("got %d event lines, want 2 (the mid-escape write was withheld or split):\n%s", len(payloads), buf.String())
			}
			if got := strings.Join(payloads, ""); got != first+tc.last {
				t.Fatalf("reassembled output = %q, want %q (audit tail corrupted)", got, first+tc.last)
			}
			if !utf8.ValidString(payloads[1]) {
				t.Fatalf("final event payload is not valid UTF-8: %q", payloads[1])
			}
			if !strings.HasSuffix(payloads[1], tc.last[len(tc.last)-1:]) {
				t.Fatalf("final event lost its last byte: %q", payloads[1])
			}
			if !cw.HadOutput() {
				t.Fatal("HadOutput false after two writes")
			}
		})
	}
}

// TestProbeF4_CastWriter_MidRuneTailDoesNotCorruptPrecedingEvents pins the
// DOCUMENTED residual (asciicast.go:40-44): a final write ending in a lone UTF-8
// lead byte drops THAT byte (there is no Flush), but everything before it must
// still be a complete, valid event and the cast must still end on a line
// boundary. If a Flush is ever added this test still passes (it asserts the
// prefix, not the drop); if the pending logic ever starts corrupting the line
// BEFORE the lead byte, it fails.
func TestProbeF4_CastWriter_MidRuneTailDoesNotCorruptPrecedingEvents(t *testing.T) {
	var buf bytes.Buffer
	cw := NewCastWriter(&buf, 80, 24, time.Now())
	rune3 := []byte("世") // 3 bytes
	if _, err := cw.Write([]byte("box: ")); err != nil {
		t.Fatalf("Write 1: %v", err)
	}
	// Final write: an ESC sequence then the first byte of a 3-byte rune.
	last := append([]byte(esc+"[0m"), rune3[0])
	if _, err := cw.Write(last); err != nil {
		t.Fatalf("Write 2: %v", err)
	}
	payloads := castLines(t, buf.String())
	got := strings.Join(payloads, "")
	if !strings.HasPrefix(got, "box: "+esc+"[0m") {
		t.Fatalf("bytes preceding the dangling lead byte were corrupted or dropped: %q", got)
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Fatalf("a U+FFFD landed in the cast: the lead byte was emitted as a broken fragment instead of held: %q", got)
	}
	// Explicitly record the residual so a reviewer sees it in -v output.
	if len(got) == len("box: "+esc+"[0m") {
		t.Logf("RESIDUAL (asciicast.go:40-44): trailing lone lead byte 0x%02x dropped at end-of-stream; no Flush exists", rune3[0])
	}
}
