// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"crypto/sha256"
	"encoding/hex"
	"runtime"
	"strings"
	"testing"
)

// The digest is defined in migration 0120; this is the same definition written out with nothing shared.
func referenceDigest(header string, hashes ...string) string {
	sum := sha256.Sum256([]byte(header))
	d := hex.EncodeToString(sum[:])
	for _, h := range hashes {
		sum = sha256.Sum256([]byte(d + h))
		d = hex.EncodeToString(sum[:])
	}
	return d
}

func TestPartitionManifestHeaderIsJSONBText(t *testing.T) {
	m := PartitionManifest{Partition: "audit_events_p202610", Rows: 3, SeqLo: 7, SeqHi: 9, RecordedLoUS: 1759500000000001, RecordedHiUS: 1759500000000009}
	if got, want := m.Header(), `["audit_events_p202610", "7", "9", "1759500000000001", "1759500000000009", "3"]`; got != want {
		t.Errorf("Header = %s, want %s", got, want)
	}
	// An empty partition's ranges are empty strings, whatever the zero values hold.
	empty := PartitionManifest{Partition: "audit_events_p202611"}
	if got, want := empty.Header(), `["audit_events_p202611", "", "", "", "", "0"]`; got != want {
		t.Errorf("empty Header = %s, want %s", got, want)
	}
}

func TestPartitionDigestMatchesTheReferenceFold(t *testing.T) {
	m := PartitionManifest{Partition: "audit_events_p202610", Rows: 3, SeqLo: 7, SeqHi: 9, RecordedLoUS: 1, RecordedHiUS: 2}
	hashes := []string{strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)}
	d := NewPartitionDigest(m)
	for _, h := range hashes {
		d.Add(h)
	}
	if got, want := d.Sum(), referenceDigest(m.Header(), hashes...); got != want {
		t.Errorf("Sum = %s, want %s", got, want)
	}
	// No rows: d_0.
	empty := PartitionManifest{Partition: "audit_events_p202611"}
	if got, want := NewPartitionDigest(empty).Sum(), referenceDigest(empty.Header()); got != want {
		t.Errorf("empty digest = %s, want d_0 %s", got, want)
	}
	// Order matters: the fold is a chain, not a set.
	a, b := NewPartitionDigest(m), NewPartitionDigest(m)
	a.Add(hashes[0])
	a.Add(hashes[1])
	b.Add(hashes[1])
	b.Add(hashes[0])
	if a.Sum() == b.Sum() {
		t.Error("two orders of the same hashes gave one digest")
	}
}

// A string_agg of row hashes reaches Postgres's 1 GB field limit near sixteen million rows. The fold must
// digest more than that in bounded memory, from streamed input that is never held.
func TestPartitionDigestStreamsPastTheFieldLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("streams 1.1 GB through the fold")
	}
	const fieldLimit = 1 << 30
	const rows = 17_000_000 // 17M x 64 hex characters = 1.09 GB
	h := []byte(strings.Repeat("0123456789abcdef", 4))
	d := NewPartitionDigest(PartitionManifest{Partition: "audit_events_p202610", Rows: rows, SeqLo: 1, SeqHi: rows, RecordedLoUS: 1, RecordedHiUS: 2})

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	var folded int64
	for i := 0; i < rows; i++ {
		h[0] = "0123456789abcdef"[i&15] // distinct input, no allocation
		h[1] = "0123456789abcdef"[(i>>4)&15]
		d.Add(string(h))
		folded += int64(len(h))
	}
	runtime.GC()
	runtime.ReadMemStats(&after)

	if folded <= fieldLimit {
		t.Fatalf("folded %d bytes, want more than the 1 GB field limit (%d)", folded, fieldLimit)
	}
	if growth := int64(after.HeapAlloc) - int64(before.HeapAlloc); growth > 4<<20 {
		t.Errorf("the heap grew by %d bytes while digesting %d bytes: the fold is not constant-memory", growth, folded)
	}
	if len(d.Sum()) != 64 {
		t.Errorf("Sum = %q, want 64 hex characters", d.Sum())
	}
}
