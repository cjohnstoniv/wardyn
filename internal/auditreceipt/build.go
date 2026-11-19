// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package auditreceipt

import (
	"errors"
	"fmt"
	"sort"
)

// Caps on one receipt response. A receipt carries the full hash list of every checkpoint that holds one of its
// events (up to 10,000 hashes each) and only the signed header of every checkpoint between them, so its size
// is bounded by these three numbers: 1,000 events, 10 checkpoints with hash lists (at most 100,000 hashes)
// and 500 header-only checkpoints in between. An explicit request past a cap is refused; a run's receipt is
// cut at the cap and the caller continues from the last event it holds.
const (
	MaxReceiptEvents            = 1000
	MaxReceiptEventCheckpoints  = 10
	MaxReceiptHeaderCheckpoints = 500
	// CheckpointLookahead is how many checkpoints, counted from the first one that can hold an event, the
	// assembler is given: enough to reach the farthest event the caps allow.
	CheckpointLookahead = MaxReceiptEventCheckpoints + MaxReceiptHeaderCheckpoints
	// MaxCheckpointPage is the most checkpoints one export page carries.
	MaxCheckpointPage = 1000
)

// Why a receipt cannot be assembled.
var (
	// ErrPending: an event is newer than the last checkpoint. It is covered by the next one.
	ErrPending = errors.New("auditreceipt: an event is newer than the last checkpoint")
	// ErrUnchained: an event is in no checkpoint and will never be: it has no row hash, or it is older than
	// the first checkpoint (signing began after it).
	ErrUnchained = errors.New("auditreceipt: an event is in no checkpoint")
	// ErrTooLarge: the events span more checkpoints than one receipt carries.
	ErrTooLarge = errors.New("auditreceipt: the events span more checkpoints than one receipt carries")
	// ErrInconsistent: the stored hash list does not hold the event's own row hash. The chain is broken or
	// the read raced a rewrite; no receipt is better than a wrong one.
	ErrInconsistent = errors.New("auditreceipt: a checkpoint's hash list does not hold the event")
	// ErrProofUnavailable: the stored rows of a checkpoint holding an event no longer match its signed
	// statement, so the receipt would fail verification. Retention dropped a partition the checkpoint also
	// covers, or the log changed after it was signed.
	ErrProofUnavailable = errors.New("auditreceipt: a checkpoint's stored rows no longer match its signed statement")
)

// SignedCheckpoint is a stored checkpoint: the signed statement text, its signature and the seq range the
// statement covers (read from the stored columns).
type SignedCheckpoint struct {
	SeqLo, SeqHi int64
	Statement    string
	Signature    []byte
}

// SeqHash is one row of a checkpoint's hash list.
type SeqHash struct {
	Seq     int64
	RowHash string
}

// AssembleInput is one receipt to build.
type AssembleInput struct {
	// Events are the rows to receipt, in seq order, with RedactedFields set where the reader was served the
	// row without host detail. Index is filled in here.
	Events []Event
	// Checkpoints are the stored checkpoints from the first one with SeqHi >= Events[0].Row.Seq, oldest
	// first, at most CheckpointLookahead of them.
	Checkpoints []SignedCheckpoint
	// Hashes returns the hash list (seq and row hash, in seq order) of the checkpoint covering lo..hi.
	Hashes func(lo, hi int64) ([]SeqHash, error)
	// Page, for a run's receipt, ends the receipt before the first event it cannot carry (a cap, or an event
	// not yet checkpointed) instead of refusing the request, as long as one event is in. Without it every
	// event must fit.
	Page bool
}

// Assembled is a receipt and how many of the leading events it holds.
type Assembled struct {
	Bundle   Bundle
	Included int
}

// Assemble builds the receipt for in.Events: for each checkpoint holding an event, its signed statement, its
// hash list with the seqs and the events at their positions in it; for each checkpoint between two of those,
// the signed statement alone, so the prev chain verifies across it. The bundle is a ReceiptFormat bundle; the
// signatures are the signer's and are not checked here.
func Assemble(in AssembleInput) (Assembled, error) {
	if len(in.Events) == 0 {
		return Assembled{}, errors.New("auditreceipt: no event to receipt")
	}
	type slot struct{ cp, first, end int } // events[first:end] are in checkpoint cp
	var slots []slot
	cps := in.Checkpoints
	included := 0
	j := 0
	for i, ev := range in.Events {
		seq := ev.Row.Seq
		if ev.Row.RowHash == nil {
			return Assembled{}, fmt.Errorf("%w: seq %d has no row hash", ErrUnchained, seq)
		}
		for j < len(cps) && cps[j].SeqHi < seq {
			j++
		}
		var cut error
		switch {
		case j == len(cps) && len(cps) >= CheckpointLookahead:
			cut = fmt.Errorf("%w: seq %d is more than %d checkpoints on", ErrTooLarge, seq, CheckpointLookahead)
		case j == len(cps):
			cut = fmt.Errorf("%w: seq %d", ErrPending, seq)
		case seq < cps[j].SeqLo:
			return Assembled{}, fmt.Errorf("%w: seq %d lies before checkpoint %d-%d", ErrUnchained, seq, cps[j].SeqLo, cps[j].SeqHi)
		case len(slots) == 0 || slots[len(slots)-1].cp != j:
			n, span := len(slots)+1, 1
			if len(slots) > 0 {
				span = j - slots[0].cp + 1
			}
			if n > MaxReceiptEventCheckpoints || span-n > MaxReceiptHeaderCheckpoints {
				cut = fmt.Errorf("%w: seq %d would make %d checkpoints with hash lists and %d header-only", ErrTooLarge, seq, n, span-n)
			}
		}
		if cut != nil {
			if in.Page && i > 0 {
				break
			}
			return Assembled{}, cut
		}
		if len(slots) > 0 && slots[len(slots)-1].cp == j {
			slots[len(slots)-1].end = i + 1
		} else {
			slots = append(slots, slot{cp: j, first: i, end: i + 1})
		}
		included = i + 1
	}

	out := Bundle{Format: ReceiptFormat}
	next := 0
	for c := slots[0].cp; c <= slots[len(slots)-1].cp; c++ {
		sc := cps[c]
		cp := Checkpoint{Statement: sc.Statement, Signature: sc.Signature}
		if next < len(slots) && slots[next].cp == c {
			if err := fillHashList(&cp, sc, in.Hashes); err != nil {
				return Assembled{}, err
			}
			for _, ev := range in.Events[slots[next].first:slots[next].end] {
				k := sort.Search(len(cp.Seqs), func(k int) bool { return cp.Seqs[k] >= ev.Row.Seq })
				if k == len(cp.Seqs) || cp.Seqs[k] != ev.Row.Seq {
					return Assembled{}, fmt.Errorf("%w: seq %d is not in checkpoint %d-%d", ErrUnchained, ev.Row.Seq, sc.SeqLo, sc.SeqHi)
				}
				if cp.RowHashes[k] != *ev.Row.RowHash {
					return Assembled{}, fmt.Errorf("%w: seq %d", ErrInconsistent, ev.Row.Seq)
				}
				ev.Index = k
				cp.Events = append(cp.Events, ev)
			}
			next++
		}
		out.Checkpoints = append(out.Checkpoints, cp)
	}
	return Assembled{Bundle: out, Included: included}, nil
}

// fillHashList reads sc's hash list into cp and runs the verifier's own hash-list checks on it: a list that
// fails them proves nothing, so it is never served.
func fillHashList(cp *Checkpoint, sc SignedCheckpoint, hashes func(lo, hi int64) ([]SeqHash, error)) error {
	list, err := hashes(sc.SeqLo, sc.SeqHi)
	if err != nil {
		return err
	}
	cp.Seqs, cp.RowHashes = make([]int64, len(list)), make([]string, len(list))
	for k, e := range list {
		cp.Seqs[k], cp.RowHashes[k] = e.Seq, e.RowHash
	}
	st, err := ParseStatement(sc.Statement)
	var rep Report
	if err == nil && !rep.checkHashList(0, st, *cp, ReceiptFormat) {
		err = fmt.Errorf("%+v", rep.Failures)
	}
	if err != nil {
		return fmt.Errorf("%w: checkpoint %d-%d: %v", ErrProofUnavailable, sc.SeqLo, sc.SeqHi, err)
	}
	return nil
}

// CheckpointExport is the signed statements alone, for a reader who wants to pin or compare heads: verifying
// it checks signatures and the prev links, never that any row is included.
func CheckpointExport(cps []SignedCheckpoint) Bundle {
	out := Bundle{Format: CheckpointsFormat, Checkpoints: make([]Checkpoint, len(cps))}
	for i, c := range cps {
		out.Checkpoints[i] = Checkpoint{Statement: c.Statement, Signature: c.Signature}
	}
	return out
}
