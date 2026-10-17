// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"strings"
	"testing"
)

// chain builds n well-formed links: row hashes "h1".."hn", each linking to the
// one before it, and each re-hashing to what it stores (want == row). Seq
// starts at 100 so a reported seq can never be confused with an index.
func chain(n int) []auditChainLink {
	out := make([]auditChainLink, 0, n)
	prev := ""
	for i := 1; i <= n; i++ {
		h := string(rune('a'+i-1)) + "-hash"
		out = append(out, auditChainLink{
			seq: int64(99 + i), prev: prev, row: h, want: h, prevIsNull: prev == "",
		})
		prev = h
	}
	return out
}

func walk(links []auditChainLink) AuditChainStatus {
	w := newAuditChainWalk()
	for _, l := range links {
		if !w.step(l) {
			break
		}
	}
	return w.st
}

// TestVerifyAuditChain is the whole tamper-detection rule. The load-bearing
// cases are the MIDDLE-row ones: an edit or a splice at the head or tail is
// easy, a rewrite buried inside a long chain is what a chain exists to catch.
func TestVerifyAuditChain(t *testing.T) {
	tests := []struct {
		name      string
		links     []auditChainLink
		wantOK    bool
		wantSeq   int64
		wantCheck int64
	}{
		{name: "empty log verifies", links: nil, wantOK: true},
		{name: "intact chain", links: chain(5), wantOK: true, wantCheck: 5},
		{
			// A DB admin edited row 3's actor/action/data. Its stored row_hash
			// still says what it said at write time, so the recomputed hash no
			// longer agrees: rule 1.
			name: "tampered middle row is detected",
			links: func() []auditChainLink {
				l := chain(5)
				l[2].want = "recomputed-differently"
				return l
			}(),
			wantOK: false, wantSeq: 102, wantCheck: 3,
		},
		{
			// Row 3 was deleted outright. Rows 2 and 4 are each internally
			// consistent — only the LINK between them is gone: rule 2. This is
			// the case re-hashing alone cannot see.
			name: "spliced-out middle row is detected",
			links: func() []auditChainLink {
				l := chain(5)
				return append(l[:2:2], l[3:]...)
			}(),
			wantOK: false, wantSeq: 103, wantCheck: 3,
		},
		{
			// The oldest chained row was deleted, so what is now first still
			// carries a prev_hash pointing at a row that no longer exists.
			name: "deleted genesis row is detected",
			links: func() []auditChainLink {
				l := chain(5)
				return l[1:]
			}(),
			wantOK: false, wantSeq: 101, wantCheck: 1,
		},
		{
			// Truncating the TAIL is invisible to the chain by construction:
			// what remains is a shorter, perfectly valid chain. Detecting it
			// needs the head hash a SIEM recorded off-box, which is why the
			// sink stream carries it — asserted here so nobody later "fixes"
			// this into a false promise.
			name:   "tail truncation verifies clean (needs the off-box head hash)",
			links:  chain(5)[:3],
			wantOK: true, wantCheck: 3,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := walk(tc.links)
			if st.OK != tc.wantOK {
				t.Fatalf("OK = %v, want %v (reason %q)", st.OK, tc.wantOK, st.Reason)
			}
			if st.Checked != tc.wantCheck {
				t.Errorf("Checked = %d, want %d", st.Checked, tc.wantCheck)
			}
			if !tc.wantOK {
				if st.BrokenSeq != tc.wantSeq {
					t.Errorf("BrokenSeq = %d, want %d", st.BrokenSeq, tc.wantSeq)
				}
				if st.Reason == "" {
					t.Error("a broken chain must carry a Reason an operator can act on")
				}
			} else if st.Reason != "" || st.BrokenSeq != 0 {
				t.Errorf("clean chain leaked a finding: seq=%d reason=%q", st.BrokenSeq, st.Reason)
			}
		})
	}
}

// TestVerifyAuditChain_HeadHashIsTheNewestRow pins the field an operator
// actually compares against their SIEM's copy. Getting FirstSeq/HeadSeq
// backwards would make an alert compare the wrong end of the log.
func TestVerifyAuditChain_HeadHashIsTheNewestRow(t *testing.T) {
	links := chain(4)
	st := walk(links)
	if got, want := st.HeadHash, links[3].row; got != want {
		t.Errorf("HeadHash = %q, want the NEWEST row's hash %q", got, want)
	}
	if st.FirstSeq != 100 || st.HeadSeq != 103 {
		t.Errorf("range = [%d,%d], want [100,103]", st.FirstSeq, st.HeadSeq)
	}
}

// TestVerifyAuditChain_FromAnAnchor: after an attested retention drop the walk starts at the drop's
// recorded tail. The first retained row must chain to it; anything else is a removal the drop does not
// account for, and a hashless row can no longer be a legacy prefix (the drop removed the prefix).
func TestVerifyAuditChain_FromAnAnchor(t *testing.T) {
	// Rows 3..5 of chain(5): the oldest two were dropped, and row 2's hash was the anchor's tail.
	full := chain(5)
	tail := full[1].row
	retained := func() []auditChainLink { return append([]auditChainLink(nil), full[2:]...) }
	run := func(tail string, links []auditChainLink) AuditChainStatus {
		w := newAnchoredAuditChainWalk(tail)
		for _, l := range links {
			if !w.step(l) {
				break
			}
		}
		return w.st
	}

	if st := run(tail, retained()); !st.OK || st.Checked != 3 || st.FirstSeq != 102 {
		t.Errorf("a chain that starts at the anchor's tail: %+v, want OK, 3 checked from seq 102", st)
	}
	for name, tc := range map[string]struct {
		tail    string
		links   []auditChainLink
		wantSeq int64
	}{
		"first row chains to a different tail": {"some-other-hash", retained(), 102},
		"first row has no prev at all": {tail, func() []auditChainLink {
			l := retained()
			l[0].prev, l[0].prevIsNull = "", true
			return l
		}(), 102},
		"a hashless row first": {tail, append([]auditChainLink{{seq: 101, unchained: true}}, retained()...), 101},
		"a row removed after the anchor": {tail, func() []auditChainLink {
			l := retained()
			return append(l[:1:1], l[2:]...)
		}(), 104},
	} {
		t.Run(name, func(t *testing.T) {
			st := run(tc.tail, tc.links)
			if st.OK || st.BrokenSeq != tc.wantSeq {
				t.Fatalf("status %+v, want a break at seq %d", st, tc.wantSeq)
			}
		})
	}
	if st := run(tail, append([]auditChainLink{{seq: 101, unchained: true}}, retained()...)); st.OK {
		t.Error("a hashless row after a drop must not read as the legacy prefix")
	}
	// A drop whose last row was hashless leaves nothing to chain to: the walk starts as an unanchored one.
	if st := run("", append([]auditChainLink{{seq: 90, unchained: true}}, chain(2)...)); !st.OK || st.Legacy != 1 {
		t.Errorf("walk after a drop with no tail hash: %+v, want the legacy prefix still allowed", st)
	}
}

// Every unattested removal reports the same searchable sentence.
func TestVerifyAuditChain_RemovalReasonsNameTheMissingDrop(t *testing.T) {
	l := chain(5)
	for name, links := range map[string][]auditChainLink{
		"oldest row removed": l[1:],
		"middle row removed": append(l[:2:2], l[3:]...),
	} {
		if st := walk(links); st.OK || !strings.Contains(st.Reason, "rows removed without an attested retention drop") {
			t.Errorf("%s: %+v, want a reason naming %q", name, st, "rows removed without an attested retention drop")
		}
	}
}
