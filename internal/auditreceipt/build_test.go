// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package auditreceipt

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"testing"
)

// signedOf is a checkpoint as the store holds it, built from a test checkpoint.
func signedOf(c Checkpoint) SignedCheckpoint {
	st, err := ParseStatement(c.Statement)
	if err != nil {
		panic(err)
	}
	return SignedCheckpoint{SeqLo: st.SeqLo, SeqHi: st.SeqHi, Statement: c.Statement, Signature: c.Signature}
}

// threeCheckpoints is rows 1-3, 4-6 and 7-9 in three chained checkpoints.
func threeCheckpoints(priv ed25519.PrivateKey) (cps []Checkpoint, rows [][]Row) {
	r1 := testChain(1, 3, "")
	r2 := testChain(4, 3, *r1[2].RowHash)
	r3 := testChain(7, 3, *r2[2].RowHash)
	c1 := testCheckpoint(priv, r1, "")
	c2 := testCheckpoint(priv, r2, c1.Statement)
	c3 := testCheckpoint(priv, r3, c2.Statement)
	return []Checkpoint{c1, c2, c3}, [][]Row{r1, r2, r3}
}

// hashLists answers the hash list of a stored checkpoint range from the test checkpoints.
func hashLists(cps []Checkpoint) func(lo, hi int64) ([]SeqHash, error) {
	return func(lo, hi int64) ([]SeqHash, error) {
		for _, c := range cps {
			if len(c.Seqs) > 0 && c.Seqs[0] == lo && c.Seqs[len(c.Seqs)-1] == hi {
				out := make([]SeqHash, len(c.Seqs))
				for i := range out {
					out[i] = SeqHash{Seq: c.Seqs[i], RowHash: c.RowHashes[i]}
				}
				return out, nil
			}
		}
		return nil, fmt.Errorf("no checkpoint covers %d-%d", lo, hi)
	}
}

func signedAll(cps []Checkpoint) []SignedCheckpoint {
	out := make([]SignedCheckpoint, len(cps))
	for i, c := range cps {
		out[i] = signedOf(c)
	}
	return out
}

func evs(rows ...Row) []Event {
	out := make([]Event, len(rows))
	for i, r := range rows {
		out[i] = Event{Row: r}
	}
	return out
}

// A receipt for rows in the first and third checkpoints carries the second as a signed header only, so the
// prev chain verifies across it; the second proves no inclusion and the receipt says so.
func TestAssemble_SparseReceiptCarriesTheCheckpointInBetween(t *testing.T) {
	priv, pub := testKey(1)
	cps, rows := threeCheckpoints(priv)
	got, err := Assemble(AssembleInput{
		Events: evs(rows[0][1], rows[2][0]), Checkpoints: signedAll(cps), Hashes: hashLists(cps),
	})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if got.Included != 2 || got.Bundle.Format != ReceiptFormat || len(got.Bundle.Checkpoints) != 3 {
		t.Fatalf("included %d, format %q, %d checkpoints; want 2 events in a 3-checkpoint %s", got.Included, got.Bundle.Format, len(got.Bundle.Checkpoints), ReceiptFormat)
	}
	mid := got.Bundle.Checkpoints[1]
	if len(mid.Events) != 0 || len(mid.RowHashes) != 0 || len(mid.Seqs) != 0 || mid.Statement != cps[1].Statement {
		t.Fatalf("the unrelated checkpoint must be a header only, got %d events, %d hashes", len(mid.Events), len(mid.RowHashes))
	}
	if e := got.Bundle.Checkpoints[0].Events; len(e) != 1 || e[0].Index != 1 {
		t.Fatalf("first checkpoint events = %+v, want the row at index 1", e)
	}
	if e := got.Bundle.Checkpoints[2].Events; len(e) != 1 || e[0].Index != 0 {
		t.Fatalf("third checkpoint events = %+v, want the row at index 0", e)
	}
	rep := VerifyBundle(got.Bundle, keysOf(pub))
	if !rep.Complete() {
		t.Fatalf("sparse receipt failures %+v", rep.Failures)
	}
	if !rep.Checkpoints[0].Inclusion || rep.Checkpoints[1].Inclusion || !rep.Checkpoints[1].Signed || !rep.Checkpoints[2].Inclusion {
		t.Fatalf("checkpoint verdicts %+v: the middle one is signed, inclusion not checked", rep.Checkpoints)
	}

	// Without the middle header the chain does not link: that is what the header is for.
	gap := got.Bundle
	gap.Checkpoints = []Checkpoint{got.Bundle.Checkpoints[0], got.Bundle.Checkpoints[2]}
	if rep := VerifyBundle(gap, keysOf(pub)); !hasReason(rep, ReasonPrev) {
		t.Fatalf("a receipt missing the checkpoint between: failures %+v, want %s", rep.Failures, ReasonPrev)
	}
}

// A checkpoint-only export is signatures and links, never inclusion, and carries no rows.
func TestCheckpointExport_SignatureOnly(t *testing.T) {
	priv, pub := testKey(1)
	cps, _ := threeCheckpoints(priv)
	b := CheckpointExport(signedAll(cps))
	if b.Format != CheckpointsFormat || len(b.Checkpoints) != 3 {
		t.Fatalf("export = %q with %d checkpoints", b.Format, len(b.Checkpoints))
	}
	for i, c := range b.Checkpoints {
		if len(c.Events)+len(c.RowHashes)+len(c.Seqs) != 0 {
			t.Fatalf("checkpoint %d carries rows in an export", i)
		}
	}
	rep := VerifyBundle(b, keysOf(pub))
	if !rep.Complete() || len(rep.Events) != 0 {
		t.Fatalf("export failures %+v, events %d", rep.Failures, len(rep.Events))
	}
	for i, c := range rep.Checkpoints {
		if !c.Signed || c.Inclusion {
			t.Fatalf("checkpoint %d = %+v, want signed with inclusion unchecked", i, c)
		}
	}
	// Dropping the second one is still seen through the prev link.
	b.Checkpoints = []Checkpoint{b.Checkpoints[0], b.Checkpoints[2]}
	if rep := VerifyBundle(b, keysOf(pub)); !hasReason(rep, ReasonPrev) {
		t.Fatalf("an export with a checkpoint removed: failures %+v, want %s", rep.Failures, ReasonPrev)
	}
}

func TestAssemble_Refusals(t *testing.T) {
	priv, _ := testKey(1)
	cps, rows := threeCheckpoints(priv)
	run := func(events []Event, have []Checkpoint, page bool) error {
		_, err := Assemble(AssembleInput{Events: events, Checkpoints: signedAll(have), Hashes: hashLists(cps), Page: page})
		return err
	}

	// Newer than the last checkpoint: pending, never a partial answer for an explicit request.
	newer := testChain(10, 1, *rows[2][2].RowHash)
	if err := run(evs(rows[0][0], newer[0]), cps, false); !errors.Is(err, ErrPending) {
		t.Fatalf("a row past the last checkpoint: %v, want ErrPending", err)
	}
	if err := run(evs(newer[0]), nil, false); !errors.Is(err, ErrPending) {
		t.Fatalf("no checkpoint at all: %v, want ErrPending", err)
	}

	// A hashless row is in no checkpoint and never will be.
	hashless := rows[0][0]
	hashless.RowHash, hashless.PrevHash = nil, nil
	if err := run(evs(hashless), cps, false); !errors.Is(err, ErrUnchained) {
		t.Fatalf("a hashless row: %v, want ErrUnchained", err)
	}

	// A row before the first checkpoint's range (the signer started after it) is unchained, not pending.
	early := rows[0][0]
	early.Seq = 0
	if err := run(evs(early), cps, false); !errors.Is(err, ErrUnchained) {
		t.Fatalf("a row before the first checkpoint: %v, want ErrUnchained", err)
	}

	// A hash list that disagrees with the row is a server fault, not a receipt.
	bad := rows[1][1]
	other := "ff" + (*bad.RowHash)[2:]
	if other == *bad.RowHash { // a hash that already starts with ff (1 in 256 runs)
		other = "00" + (*bad.RowHash)[2:]
	}
	bad.RowHash = &other
	if err := run(evs(bad), cps, false); !errors.Is(err, ErrUnchained) && !errors.Is(err, ErrInconsistent) {
		t.Fatalf("a row whose hash is not in the list: %v", err)
	}
}

// Past the event-checkpoint or header caps an explicit request is too large; a run page is cut there and
// the cut is reported, always with progress.
func TestAssemble_CapsRefuseOrCut(t *testing.T) {
	priv, _ := testKey(1)
	var all []Checkpoint
	var firsts []Row
	prevText, prevHash := "", ""
	for i := 0; i < MaxReceiptEventCheckpoints+2; i++ {
		rs := testChain(int64(i*3+1), 3, prevHash)
		c := testCheckpoint(priv, rs, prevText)
		all = append(all, c)
		firsts = append(firsts, rs[0])
		prevText, prevHash = c.Statement, *rs[2].RowHash
	}
	_, err := Assemble(AssembleInput{Events: evs(firsts...), Checkpoints: signedAll(all), Hashes: hashLists(all)})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("events in %d checkpoints, cap %d: %v, want ErrTooLarge", len(firsts), MaxReceiptEventCheckpoints, err)
	}
	got, err := Assemble(AssembleInput{Events: evs(firsts...), Checkpoints: signedAll(all), Hashes: hashLists(all), Page: true})
	if err != nil {
		t.Fatalf("a run page: %v", err)
	}
	if got.Included != MaxReceiptEventCheckpoints || len(got.Bundle.Checkpoints) != MaxReceiptEventCheckpoints {
		t.Fatalf("page included %d events in %d checkpoints, want %d each", got.Included, len(got.Bundle.Checkpoints), MaxReceiptEventCheckpoints)
	}

	// Two events far apart: the checkpoints between are headers, and past the header cap the page is cut
	// before the second event rather than carrying thousands of them.
	var long []Checkpoint
	prevText, prevHash = "", ""
	for i := 0; i < MaxReceiptHeaderCheckpoints+5; i++ {
		rs := testChain(int64(i*2+1), 2, prevHash)
		c := testCheckpoint(priv, rs, prevText)
		long = append(long, c)
		prevText, prevHash = c.Statement, *rs[1].RowHash
	}
	first, last := long[0], long[len(long)-1]
	pair := evs(rowsOf(first)[0], rowsOf(last)[0])
	if _, err := Assemble(AssembleInput{Events: pair, Checkpoints: signedAll(long), Hashes: hashLists(long)}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("events %d checkpoints apart: %v, want ErrTooLarge", len(long), err)
	}
	got, err = Assemble(AssembleInput{Events: pair, Checkpoints: signedAll(long), Hashes: hashLists(long), Page: true})
	if err != nil || got.Included != 1 || len(got.Bundle.Checkpoints) != 1 {
		t.Fatalf("a page across the header cap: included %d, %d checkpoints, %v; want the first event alone", got.Included, len(got.Bundle.Checkpoints), err)
	}
}

// A run page that reaches rows no checkpoint covers yet ends at the last covered row; only a page with
// nothing covered is pending.
func TestAssemble_PageEndsAtTheNewestCheckpoint(t *testing.T) {
	priv, _ := testKey(1)
	cps, rows := threeCheckpoints(priv)
	newer := testChain(10, 1, *rows[2][2].RowHash)
	got, err := Assemble(AssembleInput{Events: evs(rows[0][0], rows[2][2], newer[0]), Checkpoints: signedAll(cps), Hashes: hashLists(cps), Page: true})
	if err != nil || got.Included != 2 {
		t.Fatalf("included %d, %v; want the two covered rows", got.Included, err)
	}
	if _, err := Assemble(AssembleInput{Events: evs(newer[0]), Checkpoints: signedAll(cps), Hashes: hashLists(cps), Page: true}); !errors.Is(err, ErrPending) {
		t.Fatalf("a page of only uncovered rows: %v, want ErrPending", err)
	}
}

func rowsOf(c Checkpoint) []Row {
	// The test checkpoints are built from testChain rows; rebuild the first row of a checkpoint from its list.
	rs := make([]Row, len(c.Seqs))
	for i := range rs {
		h := c.RowHashes[i]
		rs[i] = Row{Seq: c.Seqs[i], RowHash: &h}
	}
	return rs
}
