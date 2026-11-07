// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package auditreceipt

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// testKey is a deterministic signing key and its pinned public entry.
func testKey(b byte) (ed25519.PrivateKey, PublicKey) {
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{b}, ed25519.SeedSize))
	pub := priv.Public().(ed25519.PublicKey)
	return priv, PublicKey{Kid: KeyID(pub), PublicKey: pub, Fingerprint: Fingerprint(pub), CreatedAt: time.Unix(0, 0).UTC()}
}

// testChain makes n chained rows starting at seq, the first linked to prev ("" for the chain's first row).
func testChain(seq int64, n int, prev string) []Row {
	rows := make([]Row, n)
	for i := range rows {
		r := Row{Seq: seq + int64(i), ID: uuid.New(), TimeUS: 1700000000000000 + int64(i), ActorType: "system", Actor: "test",
			Action: "test.row", Target: strp(fmt.Sprintf("t%d", i)), Outcome: "success", SourceIP: strp(""),
			Data: strp(fmt.Sprintf(`{"n": %d}`, i))}
		if prev != "" {
			p := prev
			r.PrevHash = &p
		}
		h := RowHash(r)
		r.RowHash = &h
		prev = h
		rows[i] = r
	}
	return rows
}

func hashesOf(rows []Row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = *r.RowHash
	}
	return out
}

// testCheckpoint signs a checkpoint over rows, chained to the previous statement text ("" for the first).
func testCheckpoint(priv ed25519.PrivateKey, rows []Row, prevStatement string) Checkpoint {
	pub := priv.Public().(ed25519.PublicKey)
	prev := ""
	if prevStatement != "" {
		prev = StatementHash(prevStatement)
	}
	seqs := make([]int64, len(rows))
	for i, r := range rows {
		seqs[i] = r.Seq
	}
	s := Statement{Format: CheckpointFormat, Kid: KeyID(pub), SeqLo: rows[0].Seq, SeqHi: rows[len(rows)-1].Seq,
		Rows: int64(len(rows)), HeadHash: *rows[len(rows)-1].RowHash, Digest: Fold(prev, hashesOf(rows)),
		SeqDigest: SeqDigest(seqs, hashesOf(rows)), Prev: prev, SignedAt: "2026-11-07T10:00:00Z"}
	text := string(s.Bytes())
	return Checkpoint{Statement: text, Signature: ed25519.Sign(priv, []byte(text)), Seqs: seqs, RowHashes: hashesOf(rows)}
}

func withEvents(c Checkpoint, rows []Row, idx ...int) Checkpoint {
	for _, i := range idx {
		c.Events = append(c.Events, Event{Index: i, Row: rows[i]})
	}
	return c
}

func headerOnly(c Checkpoint) Checkpoint {
	c.Seqs, c.RowHashes, c.Events = nil, nil, nil
	return c
}

func keysOf(pubs ...PublicKey) Keys { return Keys{Format: KeysFormat, Keys: pubs} }

func hasReason(rep Report, reason string) bool {
	for _, f := range rep.Failures {
		if f.Reason == reason {
			return true
		}
	}
	return false
}

// twoCheckpoints is a valid two-checkpoint receipt: rows 1-5 and 6-9, events at seq 3 and seq 8.
func twoCheckpoints(priv ed25519.PrivateKey) (Bundle, [][]Row) {
	r1 := testChain(1, 5, "")
	r2 := testChain(6, 4, *r1[4].RowHash)
	c1 := testCheckpoint(priv, r1, "")
	c2 := testCheckpoint(priv, r2, c1.Statement)
	return Bundle{Format: ReceiptFormat, Checkpoints: []Checkpoint{withEvents(c1, r1, 2), withEvents(c2, r2, 2)}}, [][]Row{r1, r2}
}

func TestVerifyBundle_ValidReceipt(t *testing.T) {
	priv, pub := testKey(1)
	b, _ := twoCheckpoints(priv)
	rep := VerifyBundle(b, keysOf(pub))
	if !rep.OK() || !rep.Complete() || rep.Withheld() != 0 {
		t.Fatalf("a valid receipt: failures %+v", rep.Failures)
	}
	if len(rep.Events) != 2 || len(rep.Checkpoints) != 2 || !rep.Checkpoints[0].Inclusion || !rep.Checkpoints[1].Inclusion {
		t.Fatalf("report = %+v, want two events in two inclusion-checked checkpoints", rep)
	}
}

func TestVerifyBundle_TamperedRow(t *testing.T) {
	priv, pub := testKey(1)
	b, _ := twoCheckpoints(priv)
	b.Checkpoints[1].Events[0].Row.Action = "test.rewritten"
	rep := VerifyBundle(b, keysOf(pub))
	if rep.OK() || !hasReason(rep, ReasonRowHash) {
		t.Fatalf("an edited row: failures %+v, want %s", rep.Failures, ReasonRowHash)
	}

	// A rewritten row re-hashed by the forger no longer matches the signed hash list.
	b, _ = twoCheckpoints(priv)
	ev := &b.Checkpoints[0].Events[0]
	ev.Row.Data = strp(`{"n": 999}`)
	h := RowHash(ev.Row)
	ev.Row.RowHash = &h
	if rep := VerifyBundle(b, keysOf(pub)); rep.OK() || !hasReason(rep, ReasonHashList) {
		t.Fatalf("a re-hashed rewrite: failures %+v, want %s", rep.Failures, ReasonHashList)
	}
}

func TestVerifyBundle_TamperedHashList(t *testing.T) {
	priv, pub := testKey(1)
	b, rows := twoCheckpoints(priv)
	// Another row's hash changed: the fold no longer gives the signed digest.
	b.Checkpoints[0].RowHashes[0] = *rows[1][0].RowHash
	if rep := VerifyBundle(b, keysOf(pub)); rep.OK() || !hasReason(rep, ReasonDigest) {
		t.Fatalf("an edited hash list: failures %+v, want %s", rep.Failures, ReasonDigest)
	}

	// The event's own entry and its row_hash swapped together still breaks the fold.
	b, _ = twoCheckpoints(priv)
	ev := &b.Checkpoints[0].Events[0]
	ev.Row.Target = strp("forged")
	h := RowHash(ev.Row)
	ev.Row.RowHash = &h
	b.Checkpoints[0].RowHashes[ev.Index] = h
	if rep := VerifyBundle(b, keysOf(pub)); rep.OK() || !hasReason(rep, ReasonDigest) {
		t.Fatalf("a list edited to match a forged row: failures %+v, want %s", rep.Failures, ReasonDigest)
	}

	// A list one entry short no longer matches rows.
	b, _ = twoCheckpoints(priv)
	b.Checkpoints[1].RowHashes = b.Checkpoints[1].RowHashes[:3]
	b.Checkpoints[1].Events = nil
	if rep := VerifyBundle(b, keysOf(pub)); rep.OK() || !hasReason(rep, ReasonRows) {
		t.Fatalf("a short hash list: failures %+v, want %s", rep.Failures, ReasonRows)
	}
}

func TestVerifyBundle_WrongKey(t *testing.T) {
	priv, pub := testKey(1)
	other, otherPub := testKey(2)
	b, _ := twoCheckpoints(priv)
	if rep := VerifyBundle(b, keysOf(otherPub)); rep.OK() || !hasReason(rep, ReasonUnknownKid) {
		t.Fatalf("no pinned key for the kid: failures %+v, want %s", rep.Failures, ReasonUnknownKid)
	}
	// Signed by another key under the pinned key's kid.
	b.Checkpoints[0].Signature = ed25519.Sign(other, []byte(b.Checkpoints[0].Statement))
	if rep := VerifyBundle(b, keysOf(pub)); rep.OK() || !hasReason(rep, ReasonBadSignature) {
		t.Fatalf("a signature by another key: failures %+v, want %s", rep.Failures, ReasonBadSignature)
	}
	// A pinned entry whose kid is not its key's is refused rather than trusted.
	b, _ = twoCheckpoints(priv)
	forged := otherPub
	forged.Kid = pub.Kid
	if rep := VerifyBundle(b, keysOf(forged)); rep.OK() || !hasReason(rep, ReasonBadKey) {
		t.Fatalf("a keys file with a mislabelled key: failures %+v, want %s", rep.Failures, ReasonBadKey)
	}
}

func TestVerifyBundle_RetiredKey(t *testing.T) {
	priv, pub := testKey(1)
	b, _ := twoCheckpoints(priv)
	pub.Retired = true
	rep := VerifyBundle(b, keysOf(pub))
	if rep.OK() || !hasReason(rep, ReasonKeyRetired) {
		t.Fatalf("a retired key: failures %+v, want %s", rep.Failures, ReasonKeyRetired)
	}
}

func TestVerifyBundle_PrevLinkBreak(t *testing.T) {
	priv, pub := testKey(1)
	r1 := testChain(1, 5, "")
	r2 := testChain(6, 4, *r1[4].RowHash)
	c1 := testCheckpoint(priv, r1, "")
	c2 := testCheckpoint(priv, r2, "") // signed, but not chained to c1
	b := Bundle{Format: ReceiptFormat, Checkpoints: []Checkpoint{withEvents(c1, r1, 0), withEvents(c2, r2, 0)}}
	if rep := VerifyBundle(b, keysOf(pub)); rep.OK() || !hasReason(rep, ReasonPrev) {
		t.Fatalf("a checkpoint not chained to the one before: failures %+v, want %s", rep.Failures, ReasonPrev)
	}
}

func TestVerifyBundle_RedactedRowWithheldNotFailed(t *testing.T) {
	priv, pub := testKey(1)
	rows := testChain(1, 3, "")
	// The stored row carries host detail; the served row withholds it (the projector's marker in data).
	stored := rows[1]
	stored.Action = "drive.write"
	stored.Data = strp(`{"backend": "local", "host_root": "/srv/drives"}`)
	rows = relink(rows, 1, stored)
	c := withEvents(testCheckpoint(priv, rows, ""), rows, 1)
	c.Events[0].Row.Data = strp(`{"backend":"local","redacted_fields":["host_root"]}`)
	c.Events[0].RedactedFields = []string{"host_root"}
	b := Bundle{Format: ReceiptFormat, Checkpoints: []Checkpoint{c}}

	rep := VerifyBundle(b, keysOf(pub))
	if !rep.OK() || rep.Withheld() != 1 || rep.Complete() || !rep.Events[0].Withheld || rep.Events[0].OK {
		t.Fatalf("a host-redacted row: failures %+v, withheld %d, complete %v, event %+v; want linked, withheld, the "+
			"event not OK, the receipt not complete", rep.Failures, rep.Withheld(), rep.Complete(), rep.Events[0])
	}

	// Anyone holding the receipt can rewrite a withheld row's shown fields and keep the marker: nothing re-hashes
	// them, so the event is never OK and the receipt never complete.
	forged := c
	forged.Events = append([]Event(nil), c.Events...)
	forged.Events[0].Row.Actor = "someone-else"
	forged.Events[0].Row.Target = strp("forged-target")
	forged.Events[0].Row.Data = strp(`{"backend":"s3","redacted_fields":["host_root"]}`)
	rep = VerifyBundle(Bundle{Format: ReceiptFormat, Checkpoints: []Checkpoint{forged}}, keysOf(pub))
	if rep.Complete() || rep.Events[0].OK || !rep.Events[0].Withheld {
		t.Fatalf("a withheld row with rewritten actor, target and data: complete %v, event %+v; want withheld, "+
			"the event not OK, the receipt not complete", rep.Complete(), rep.Events[0])
	}

	// redacted_fields on an action the projector never redacts is a forgery, not a withheld row.
	c.Events[0].Row.Action = "test.row"
	if rep := VerifyBundle(Bundle{Format: ReceiptFormat, Checkpoints: []Checkpoint{c}}, keysOf(pub)); rep.OK() || !hasReason(rep, ReasonRedaction) {
		t.Fatalf("redacted_fields on an unredactable action: failures %+v, want %s", rep.Failures, ReasonRedaction)
	}
	// The marker must match the one inside the served data.
	c.Events[0].Row.Action = "drive.write"
	c.Events[0].RedactedFields = []string{"backend"}
	if rep := VerifyBundle(Bundle{Format: ReceiptFormat, Checkpoints: []Checkpoint{c}}, keysOf(pub)); rep.OK() || !hasReason(rep, ReasonRedaction) {
		t.Fatalf("redacted_fields unlike the data's marker: failures %+v, want %s", rep.Failures, ReasonRedaction)
	}
	// A withheld row still has to sit in the signed list.
	c.Events[0].RedactedFields = []string{"host_root"}
	bogus := *rows[0].RowHash
	c.Events[0].Row.RowHash = &bogus
	if rep := VerifyBundle(Bundle{Format: ReceiptFormat, Checkpoints: []Checkpoint{c}}, keysOf(pub)); rep.OK() || !hasReason(rep, ReasonHashList) {
		t.Fatalf("a withheld row not in the list: failures %+v, want %s", rep.Failures, ReasonHashList)
	}
}

// relink replaces rows[i] and re-chains every row from i on.
func relink(rows []Row, i int, r Row) []Row {
	rows[i] = r
	for j := i; j < len(rows); j++ {
		if j > 0 {
			p := *rows[j-1].RowHash
			rows[j].PrevHash = &p
		}
		h := RowHash(rows[j])
		rows[j].RowHash = &h
	}
	return rows
}

// TestVerifyBundle_CheckpointExportSignatureOnly: a checkpoint export verifies each signature and the prev
// chain, and says inclusion was not checked; it never carries rows.
func TestVerifyBundle_CheckpointExportSignatureOnly(t *testing.T) {
	priv, pub := testKey(1)
	b, _ := twoCheckpoints(priv)
	b.Format = CheckpointsFormat
	for i := range b.Checkpoints {
		b.Checkpoints[i] = headerOnly(b.Checkpoints[i])
	}
	rep := VerifyBundle(b, keysOf(pub))
	if !rep.OK() || len(rep.Events) != 0 || rep.Checkpoints[0].Inclusion || rep.Checkpoints[1].Inclusion {
		t.Fatalf("a checkpoint export: failures %+v, report %+v; want signatures checked, inclusion not checked", rep.Failures, rep)
	}

	bad := b
	bad.Checkpoints = append([]Checkpoint(nil), b.Checkpoints...)
	bad.Checkpoints[1].Signature = append([]byte(nil), bad.Checkpoints[1].Signature...)
	bad.Checkpoints[1].Signature[0] ^= 1
	if rep := VerifyBundle(bad, keysOf(pub)); rep.OK() || !hasReason(rep, ReasonBadSignature) {
		t.Fatalf("an export with a bad signature: failures %+v", rep.Failures)
	}

	withRows, _ := twoCheckpoints(priv)
	withRows.Format = CheckpointsFormat
	if rep := VerifyBundle(withRows, keysOf(pub)); rep.OK() || !hasReason(rep, ReasonFormat) {
		t.Fatalf("an export carrying rows: failures %+v, want %s", rep.Failures, ReasonFormat)
	}
}

// TestVerifyBundle_SparseRunReceipt: a run whose events sit in checkpoints 1 and 3 carries checkpoint 2's
// signed header (no rows), so the prev chain verifies across it; leaving it out breaks the chain.
func TestVerifyBundle_SparseRunReceipt(t *testing.T) {
	priv, pub := testKey(1)
	r1 := testChain(1, 4, "")
	r2 := testChain(5, 4, *r1[3].RowHash)
	r3 := testChain(9, 4, *r2[3].RowHash)
	c1 := testCheckpoint(priv, r1, "")
	c2 := testCheckpoint(priv, r2, c1.Statement)
	c3 := testCheckpoint(priv, r3, c2.Statement)
	b := Bundle{Format: ReceiptFormat, Checkpoints: []Checkpoint{withEvents(c1, r1, 1), headerOnly(c2), withEvents(c3, r3, 3)}}
	rep := VerifyBundle(b, keysOf(pub))
	if !rep.OK() || !rep.Complete() || len(rep.Events) != 2 {
		t.Fatalf("a sparse run receipt: failures %+v", rep.Failures)
	}
	if !rep.Checkpoints[0].Inclusion || rep.Checkpoints[1].Inclusion || !rep.Checkpoints[2].Inclusion {
		t.Fatalf("checkpoint results = %+v, want the header-only one reported without inclusion", rep.Checkpoints)
	}

	b.Checkpoints = []Checkpoint{b.Checkpoints[0], b.Checkpoints[2]}
	if rep := VerifyBundle(b, keysOf(pub)); rep.OK() || !hasReason(rep, ReasonPrev) {
		t.Fatalf("the intervening header left out: failures %+v, want %s", rep.Failures, ReasonPrev)
	}

	// A receipt that names no event proves nothing about any event.
	empty := Bundle{Format: ReceiptFormat, Checkpoints: []Checkpoint{headerOnly(c1)}}
	if rep := VerifyBundle(empty, keysOf(pub)); rep.OK() || !hasReason(rep, ReasonFormat) {
		t.Fatalf("a receipt with no event: failures %+v, want %s", rep.Failures, ReasonFormat)
	}
}

// TestVerifyBundle_EventPosition: an event must sit inside its checkpoint and link to the row before it.
func TestVerifyBundle_EventPosition(t *testing.T) {
	priv, pub := testKey(1)
	b, _ := twoCheckpoints(priv)
	b.Checkpoints[0].Events[0].Index = 9
	if rep := VerifyBundle(b, keysOf(pub)); rep.OK() || !hasReason(rep, ReasonIndex) {
		t.Fatalf("an index past the list: failures %+v, want %s", rep.Failures, ReasonIndex)
	}
	b, _ = twoCheckpoints(priv)
	b.Checkpoints[0].Events[0].Row.Seq = 42
	if rep := VerifyBundle(b, keysOf(pub)); rep.OK() || !hasReason(rep, ReasonSeq) {
		t.Fatalf("a seq outside the checkpoint: failures %+v, want %s", rep.Failures, ReasonSeq)
	}
	b, _ = twoCheckpoints(priv)
	p := *b.Checkpoints[0].Events[0].Row.RowHash
	b.Checkpoints[0].Events[0].Row.PrevHash = &p
	if rep := VerifyBundle(b, keysOf(pub)); rep.OK() || !hasReason(rep, ReasonPrevHash) {
		t.Fatalf("a prev_hash unlike the row before: failures %+v, want %s", rep.Failures, ReasonPrevHash)
	}
}

// TestVerifyBundle_SeqBoundToSignedList: seq is not part of a row's hash, so the signed checkpoint must bind
// each seq to its row. Relabelling a receipted event with another seq inside the signed range fails it.
func TestVerifyBundle_SeqBoundToSignedList(t *testing.T) {
	priv, pub := testKey(1)
	for _, seq := range []int64{4, 5} {
		b, _ := twoCheckpoints(priv)
		b.Checkpoints[0].Events[0].Row.Seq = seq // the event is seq 3, at index 2 of checkpoint 1-5
		rep := VerifyBundle(b, keysOf(pub))
		if rep.OK() || rep.Complete() || rep.Events[0].OK {
			t.Fatalf("seq 3 relabelled %d: failures %+v, events %+v; want the event failed and the receipt incomplete",
				seq, rep.Failures, rep.Events)
		}
	}
}

// gapCheckpoint is one signed checkpoint over four rows whose seqs skip numbers (1, 2, 4, 7), every row an
// event.
func gapCheckpoint(priv ed25519.PrivateKey) (Bundle, []Row) {
	rows := testChain(1, 4, "")
	for i, seq := range []int64{1, 2, 4, 7} {
		rows[i].Seq = seq // seq is not hashed: the chain stays valid
	}
	c := withEvents(testCheckpoint(priv, rows, ""), rows, 0, 1, 2, 3)
	return Bundle{Format: ReceiptFormat, Checkpoints: []Checkpoint{c}}, rows
}

func TestVerifyBundle_SeqGaps(t *testing.T) {
	priv, pub := testKey(1)
	b, _ := gapCheckpoint(priv)
	if rep := VerifyBundle(b, keysOf(pub)); !rep.OK() || !rep.Complete() || len(rep.Events) != 4 {
		t.Fatalf("a checkpoint with seq gaps: failures %+v, want every event verified", rep.Failures)
	}

	// Two events' seqs swapped: 4 and 7 are both inside the signed range, but neither is its row's seq.
	b, _ = gapCheckpoint(priv)
	evs := b.Checkpoints[0].Events
	evs[2].Row.Seq, evs[3].Row.Seq = evs[3].Row.Seq, evs[2].Row.Seq
	if rep := VerifyBundle(b, keysOf(pub)); rep.OK() || rep.Complete() || rep.Events[2].OK || rep.Events[3].OK {
		t.Fatalf("seqs 4 and 7 swapped: failures %+v, events %+v; want both events failed", rep.Failures, rep.Events)
	}

	// A seq the log skipped (3) is inside the range but names no row.
	b, _ = gapCheckpoint(priv)
	b.Checkpoints[0].Events[2].Row.Seq = 3
	if rep := VerifyBundle(b, keysOf(pub)); rep.OK() || rep.Events[2].OK {
		t.Fatalf("seq 4 relabelled as the skipped 3: failures %+v, want the event failed", rep.Failures)
	}
}

// TestVerifyBundle_OlderFormatRefused: a receipt or statement in the layout before seqs were signed is refused
// by name, not misread.
func TestVerifyBundle_OlderFormatRefused(t *testing.T) {
	priv, pub := testKey(1)
	b, _ := twoCheckpoints(priv)
	b.Format = "wardyn-audit-receipt/1"
	if rep := VerifyBundle(b, keysOf(pub)); rep.OK() || !hasReason(rep, ReasonFormat) {
		t.Fatalf("an older receipt: failures %+v, want %s", rep.Failures, ReasonFormat)
	}

	old := `{"format":"wardyn-audit-checkpoint/1","kid":"` + strings.Repeat("0f", 16) + `","seq_lo":1,"seq_hi":5,"rows":5,` +
		`"head_hash":"` + strings.Repeat("1a", 32) + `","digest":"` + strings.Repeat("2b", 32) + `","prev":"",` +
		`"signed_at":"2026-11-07T10:00:00Z"}`
	_, err := ParseStatement(old)
	if err == nil || !strings.Contains(err.Error(), `"wardyn-audit-checkpoint/1"`) || strings.Contains(err.Error(), "canonical") {
		t.Fatalf("an older statement: err %v, want a refusal naming its format", err)
	}
}

// TestVerifyBundle_TamperedSeqList: the seqs beside the hash list are themselves signed, through seq_digest.
func TestVerifyBundle_TamperedSeqList(t *testing.T) {
	priv, pub := testKey(1)

	// The list and the event relabelled together (4 as the skipped 3) still break the signed seq digest.
	b, _ := gapCheckpoint(priv)
	b.Checkpoints[0].Seqs = []int64{1, 2, 3, 7}
	b.Checkpoints[0].Events[2].Row.Seq = 3
	if rep := VerifyBundle(b, keysOf(pub)); rep.OK() || !hasReason(rep, ReasonSeqDigest) || rep.Events[2].OK {
		t.Fatalf("a relabelled seq list: failures %+v, want %s", rep.Failures, ReasonSeqDigest)
	}

	for name, seqs := range map[string][]int64{
		"not ascending":   {1, 4, 2, 7},
		"outside range":   {1, 2, 4, 8},
		"one short":       {1, 2, 7},
		"left out":        nil,
		"duplicated seqs": {1, 4, 4, 7},
	} {
		b, _ := gapCheckpoint(priv)
		b.Checkpoints[0].Seqs = seqs
		if rep := VerifyBundle(b, keysOf(pub)); rep.OK() || !hasReason(rep, ReasonSeqList) || rep.Checkpoints[0].Inclusion {
			t.Errorf("seqs %s: failures %+v, want %s and no inclusion", name, rep.Failures, ReasonSeqList)
		}
	}

	// A checkpoint export carries no seqs either.
	b, _ = twoCheckpoints(priv)
	b.Format = CheckpointsFormat
	b.Checkpoints[0] = headerOnly(b.Checkpoints[0])
	b.Checkpoints[1].RowHashes, b.Checkpoints[1].Events = nil, nil
	if rep := VerifyBundle(b, keysOf(pub)); rep.OK() || !hasReason(rep, ReasonFormat) {
		t.Fatalf("an export carrying seqs: failures %+v, want %s", rep.Failures, ReasonFormat)
	}
}
