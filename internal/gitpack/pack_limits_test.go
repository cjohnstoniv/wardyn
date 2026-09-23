// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package gitpack

import (
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"hash/adler32"
	"runtime"
	"strings"
	"testing"
)

// storedZlib frames payload as one uncompressed ("stored") deflate block, so a
// million objects build in a fraction of a second rather than a million
// compressor runs. payload must be under 64 KiB.
func storedZlib(payload []byte) []byte {
	n := uint16(len(payload))
	out := []byte{0x78, 0x01, 0x01, byte(n), byte(n >> 8), byte(^n), byte(^n >> 8)}
	out = append(out, payload...)
	return binary.BigEndian.AppendUint32(out, adler32.Checksum(payload))
}

// minimalBlobPack is issue #250's shape: a receive-pack body whose pack holds a
// commit, its empty tree, and n-2 distinct four-byte blobs that no tree names.
// Sixteen bytes an object, so 1<<20 objects are the issue's 16.8 MB body.
func minimalBlobPack(n int) []byte {
	tree := hashObject("tree", nil)
	commit := mkCommit(tree)
	pack := []byte("PACK")
	pack = binary.BigEndian.AppendUint32(pack, 2)
	pack = binary.BigEndian.AppendUint32(pack, uint32(n))
	pack = append(pack, objHeader(objCommit, int64(len(commit)))...)
	pack = append(pack, storedZlib(commit)...)
	pack = append(pack, objHeader(objTree, 0)...)
	pack = append(pack, storedZlib(nil)...)
	for i := range n - 2 {
		payload := binary.BigEndian.AppendUint32(nil, uint32(i))
		pack = append(pack, objHeader(objBlob, int64(len(payload)))...)
		pack = append(pack, storedZlib(payload)...)
	}
	sum := sha1.Sum(pack)
	cmd := strings.Repeat("0", 40) + " " + hashObject("commit", commit) + " refs/heads/main"
	return append(commandSection("report-status", cmd), append(pack, sum[:]...)...)
}

// TestPackCeiling_ObjectCountIsRefused pins the object ceiling (#250). The
// issue's pack — 1,048,576 minimal blobs, a legal 16.8 MB body — used to be
// inspected, and holding its Result kept 656 MiB live. It is refused now, as is
// one object past the ceiling, because per-object bookkeeping is what the
// ceiling bounds.
func TestPackCeiling_ObjectCountIsRefused(t *testing.T) {
	for _, n := range []int{1 << 20, maxObjects + 1} {
		body := minimalBlobPack(n)
		res, err := Inspect(body)
		if !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), "objects") {
			t.Fatalf("%d objects (%d-byte body): err = %v, want ErrTooLarge naming the object ceiling",
				n, len(body), err)
		}
		if res.idx != nil || len(res.Changes) != 0 {
			t.Errorf("%d objects: a refusal came back with a partial Result", n)
		}
	}
}

// TestPackCeiling_InflatedBytesIsRefused pins the inflate ceiling (#254): five
// distinct all-zero blobs, each exactly maxObjectBytes and individually legal,
// push the cumulative inflated total past maxInflatedBytes on the fifth. Each
// blob compresses to a handful of bytes, so the pack body stays small while
// what it declares does not — the same shape charge() exists to catch.
func TestPackCeiling_InflatedBytesIsRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("inflates over maxInflatedBytes of blob content")
	}
	var objs []rawObject
	for range 5 {
		objs = append(objs, rawObject{typ: objBlob, payload: make([]byte, maxObjectBytes)})
	}
	tree := mkTree()
	commit := mkCommit(hashObject("tree", tree))
	objs = append(objs, rawObject{typ: objTree, payload: tree}, rawObject{typ: objCommit, payload: commit})
	cmd := strings.Repeat("0", 40) + " " + hashObject("commit", commit) + " refs/heads/main"
	body := append(commandSection("report-status", cmd), buildPack(t, objs...)...)

	_, err := Inspect(body)
	if !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), "inflates") {
		t.Fatalf("5x%d-byte blobs: err = %v, want ErrTooLarge naming the inflate ceiling", maxObjectBytes, err)
	}
}

// bookkeepingPerObject is the per-object heap the package comment states an
// inspection keeps: measured at 115 bytes, with headroom for how the runtime's
// maps happen to size.
const bookkeepingPerObject = 160

// retainedBy reports how many heap bytes inspect's return value keeps live.
func retainedBy(t *testing.T, body []byte) (Result, uint64) {
	t.Helper()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	res, err := Inspect(body)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	t.Logf("heap %.1f MiB -> %.1f MiB, %.1f MiB allocated in total",
		float64(before.HeapAlloc)/(1<<20), float64(after.HeapAlloc)/(1<<20),
		float64(after.TotalAlloc-before.TotalAlloc)/(1<<20))
	if after.HeapAlloc < before.HeapAlloc {
		return res, 0
	}
	return res, after.HeapAlloc - before.HeapAlloc
}

// TestPackCeiling_MaxLegalPackRetainsUnderTheBound is the other half of #250:
// the largest pack the ceiling admits, in the shape that maximizes bookkeeping,
// keeps no more live than the package comment states.
func TestPackCeiling_MaxLegalPackRetainsUnderTheBound(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and inspects a pack of maxObjects objects")
	}
	body := minimalBlobPack(maxObjects)
	res, retained := retainedBy(t, body)
	runtime.KeepAlive(res)
	if retained > maxObjects*bookkeepingPerObject {
		t.Errorf("a %d-object pack retains %d bytes, over the stated %d (maxObjects x bookkeepingPerObject)",
			maxObjects, retained, maxObjects*bookkeepingPerObject)
	}
}

// TestPackCeiling_BlobContentIsNotRetained: once the pack is parsed nothing
// reads a blob's bytes again — only its size — so an inspection of four 8 MiB
// blobs must not keep 32 MiB alive for as long as the caller holds the Result.
func TestPackCeiling_BlobContentIsNotRetained(t *testing.T) {
	var objs []rawObject
	var lines []treeLine
	for i := range 4 {
		blob := make([]byte, 8<<20)
		blob[0] = byte(i)
		objs = append(objs, rawObject{typ: objBlob, payload: blob})
		lines = append(lines, treeLine{"100644", string(rune('a' + i)), hashObject("blob", blob)})
	}
	tree := mkTree(lines...)
	commit := mkCommit(hashObject("tree", tree))
	objs = append(objs, rawObject{typ: objTree, payload: tree}, rawObject{typ: objCommit, payload: commit})
	cmd := strings.Repeat("0", 40) + " " + hashObject("commit", commit) + " refs/heads/main"
	body := append(commandSection("report-status", cmd), buildPack(t, objs...)...)

	res, retained := retainedBy(t, body)
	if len(res.Changes) != 4 || res.Changes[0].Size != 8<<20 {
		t.Fatalf("Changes = %+v, want four 8 MiB blobs", res.Changes)
	}
	runtime.KeepAlive(res)
	if retained > 1<<20 {
		t.Errorf("the Result keeps %d bytes live: the blobs' content outlived the parse", retained)
	}
}
