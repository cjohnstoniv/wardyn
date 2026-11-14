// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package admission

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// plantEntry writes a parsed entry file another local account made (the rig counts it as uid 666's).
func plantEntry(t *testing.T, r *rig, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A forged disk reservation must not add disk: a negative one, which is damage (a live VM holding no
// disk), and the largest one, whose sum with the free-space floor would wrap. Each run is refused for
// disk with the free space the entry leaves, and the offer has no room.
func TestForgedDiskReserveAddsNoDiskHeadroom(t *testing.T) {
	for name, tc := range map[string]struct {
		free, left int64
		entry      string
	}{
		// 50 GiB free, under the 56 GiB a run needs; -100 GiB would read as 150 free.
		"negative": {free: 50 * gib, left: 50 * gib,
			entry: `{"id":"neg","kind":"leaked","state":"leaked","slot":-1,"disk_reserve_bytes":-107374182400}`},
		// 1 GiB free; 1 GiB less the largest int64 less the 5 GiB floor wraps to plenty.
		"largest": {free: 1 * gib, left: 1*gib - math.MaxInt64,
			entry: `{"id":"big","kind":"leaked","state":"leaked","slot":-1,"disk_reserve_bytes":9223372036854775807}`},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, &fakeHost{boot: 1, session: "boot-1", mem: 64 * gib, avail: 48 * gib, free: tc.free})
			a := r.open(t, 501, "a")
			plantEntry(t, r, "e-disk.json", tc.entry)
			_, err := a.Admit(runReq(8 * gib))
			var ref *Refusal
			if !errors.As(err, &ref) || ref.DiskNeeded == 0 || ref.DiskFreeLeft != tc.left {
				t.Fatalf("Admit past a forged reservation = %v, want a disk refusal", err)
			}
			if o := offer(t, a, 8*gib); o.Headroom != 0 {
				t.Fatalf("a forged reservation raised the offer: %+v", o)
			}
		})
	}
}

// The status line's held disk sums debts the way admission does: past the largest int64 it holds there,
// and a sum that reaches it exactly is it, not past it.
func TestSumDiskHoldsAtTheLargest(t *testing.T) {
	const top = int64(math.MaxInt64)
	for name, tc := range map[string]struct {
		disks []int64
		want  int64
	}{
		"no entries":                {nil, 0},
		"zero figures":              {[]int64{0, 0, 0}, 0},
		"a plain sum":               {[]int64{2 * gib, 3 * gib}, 5 * gib},
		"the largest alone":         {[]int64{top}, top},
		"the largest and zero":      {[]int64{0, top, 0}, top},
		"an exact fit":              {[]int64{top - 1, 1}, top},
		"two past the largest":      {[]int64{top/2 + 1, top/2 + 1}, top},
		"stays once it has reached": {[]int64{top, 1, 0, 5}, top},
	} {
		t.Run(name, func(t *testing.T) {
			es := make([]Entry, len(tc.disks))
			for i, d := range tc.disks {
				es[i] = Entry{DiskReserve: d}
			}
			if got := SumDisk(es); got != tc.want {
				t.Fatalf("SumDisk(%v) = %d, want %d", tc.disks, got, tc.want)
			}
		})
	}
}

// Forged memory figures whose sum passes the largest uint64 must not wrap into free memory: a guest
// memory plus overhead that wraps to 1 GiB, and another login's Docker memory that wraps the reserve
// to nothing. Each request is refused, and the offer has no room.
func TestForgedNearOverflowMemoryAddsNoMemoryHeadroom(t *testing.T) {
	for name, tc := range map[string]struct {
		mem, avail uint64
		entry      string
		guest      uint64
	}{
		// 64 GiB less macOS's 4 and Docker's 8 leaves 52; the entry's 2^64-1 GiB plus 2 GiB wraps to 1 GiB.
		"guest memory": {mem: 64 * gib, avail: 48 * gib, guest: 8 * gib,
			entry: `{"id":"mem","kind":"leaked","state":"leaked","slot":-1,"guest_memory_bytes":18446744072635809792,"overhead_bytes":2147483648}`},
		// 24 GiB less 4 and 8 leaves 12, under the 15 asked; 4 GiB + (2^64-4 GiB) wraps to 0, leaving 16.
		"docker memory": {mem: 24 * gib, avail: 20 * gib, guest: 14 * gib,
			entry: `{"id":"dock","kind":"leaked","state":"leaked","slot":-1,"docker_memory_bytes":18446744069414584320}`},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, &fakeHost{boot: 1, session: "boot-1", mem: tc.mem, avail: tc.avail, free: 500 * gib})
			a := r.open(t, 501, "a")
			plantEntry(t, r, "e-big.json", tc.entry)
			if _, err := a.Admit(runReq(tc.guest)); !IsRefusal(err) {
				t.Fatalf("Admit past a wrapping forged memory figure = %v, want a refusal", err)
			}
			if o := offer(t, a, tc.guest); o.Headroom != 0 {
				t.Fatalf("a wrapping forged memory figure raised the offer: %+v", o)
			}
		})
	}
}
