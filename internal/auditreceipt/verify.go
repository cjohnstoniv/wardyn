// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package auditreceipt

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/google/uuid"
)

// Bundle formats. A receipt carries rows for at least one event; a checkpoint export carries only signed
// statements, so verifying it checks signatures and the prev chain, never inclusion.
const (
	ReceiptFormat     = "wardyn-audit-receipt/1"
	CheckpointsFormat = "wardyn-audit-checkpoints/1"
)

// Bundle is a receipt or a checkpoint export. It never carries keys.
type Bundle struct {
	Format      string       `json:"format"`
	Checkpoints []Checkpoint `json:"checkpoints"`
}

// Checkpoint is one signed statement, with its rows' hash list and events when the bundle proves inclusion.
// A receipt also carries, header only, every checkpoint between two that hold its events, so the prev chain
// verifies across them.
type Checkpoint struct {
	Statement string   `json:"statement"`
	Signature []byte   `json:"signature"`
	RowHashes []string `json:"row_hashes,omitempty"`
	Events    []Event  `json:"events,omitempty"`
}

// Event is one receipted row and its position in its checkpoint's hash list. RedactedFields is set when the
// row's data was served without host detail (a reader below the super-admin tier); its content then cannot
// be re-hashed, and it is reported withheld, never verified.
type Event struct {
	Index          int      `json:"index"`
	Row            Row      `json:"row"`
	RedactedFields []string `json:"redacted_fields,omitempty"`
}

// Failure reasons.
const (
	ReasonFormat       = "bad_format"
	ReasonBadKey       = "bad_key"
	ReasonStatement    = "bad_statement"
	ReasonUnknownKid   = "unknown_kid"
	ReasonBadSignature = "bad_signature"
	ReasonKeyRetired   = "key_retired"
	ReasonPrev         = "prev_mismatch"
	ReasonRows         = "rows_mismatch"
	ReasonDigest       = "digest_mismatch"
	ReasonHead         = "head_mismatch"
	ReasonSeq          = "seq_out_of_range"
	ReasonIndex        = "index_out_of_range"
	ReasonHashList     = "hash_list_mismatch"
	ReasonPrevHash     = "prev_hash_mismatch"
	ReasonRowHash      = "row_hash_mismatch"
	ReasonRedaction    = "bad_redaction"
)

// RedactableActions are the actions (canonical names and their legacy aliases) whose data a reader below the
// super-admin tier is served without host detail. An event claiming redaction on any other action is a
// forgery. It mirrors the API's host-view table, which a test pins it to.
var RedactableActions = map[string]bool{
	"drive.write": true, "drive.member.create": true, "source.write": true,
	"recovery.rehearse.begin": true, "recovery.rehearse": true,
	"site_config.write": true, "site_config.redirect.test": true,
	"run.policy.resolve": true, "run.policy.effective": true,
	"policy.create": true, "policy.update": true, "policy.delete": true,
	"preset.create": true, "preset.update": true,
}

// Failure is one failed check: the bundle's checkpoint index, the seq it concerns, and why.
type Failure struct {
	Checkpoint int    `json:"checkpoint"`
	Seq        int64  `json:"seq,omitempty"`
	Reason     string `json:"reason"`
	Detail     string `json:"detail"`
}

// CheckpointResult is one checkpoint's verdict. Signed means its signature verified with a pinned, unretired
// key; Inclusion means its hash list was present, folds to the signed digest, and the checkpoint failed no
// other check. A header-only checkpoint is at most signed: inclusion was not checked.
type CheckpointResult struct {
	SeqLo     int64  `json:"seq_lo"`
	SeqHi     int64  `json:"seq_hi"`
	Kid       string `json:"kid"`
	Signed    bool   `json:"signed"`
	Inclusion bool   `json:"inclusion"`
}

// EventResult is one event's verdict. Withheld means it is linked into the signed list but its content was
// not re-hashed.
type EventResult struct {
	Checkpoint int       `json:"checkpoint"`
	Seq        int64     `json:"seq"`
	ID         uuid.UUID `json:"id"`
	Action     string    `json:"action"`
	Withheld   bool      `json:"withheld"`
	OK         bool      `json:"ok"`
}

// Report is VerifyBundle's verdict.
type Report struct {
	Format      string             `json:"format"`
	Checkpoints []CheckpointResult `json:"checkpoints"`
	Events      []EventResult      `json:"events"`
	Failures    []Failure          `json:"failures"`
}

// OK reports that no check failed.
func (r Report) OK() bool { return len(r.Failures) == 0 }

// Withheld counts the events whose content was not re-hashed.
func (r Report) Withheld() int {
	n := 0
	for _, e := range r.Events {
		if e.Withheld {
			n++
		}
	}
	return n
}

// Complete reports that no check failed and no event was withheld: the only verdict that says every receipted
// row's content is what was signed.
func (r Report) Complete() bool { return r.OK() && r.Withheld() == 0 }

// VerifyBundle checks a bundle against the keys the operator pinned, in this order per checkpoint: the
// statement's encoding; its signature with the pinned key its kid names (unknown, retired or wrong keys fail);
// its prev link to the checkpoint before it in the bundle; its hash list (count, fold to the digest, last hash
// to head_hash); and each event (seq inside the range, its row_hash at its index, its prev_hash the hash
// before it, and its content re-hashed unless withheld). It never stops early: every failure is reported.
func VerifyBundle(b Bundle, keys Keys) Report {
	rep := Report{Format: b.Format, Checkpoints: []CheckpointResult{}, Events: []EventResult{}, Failures: []Failure{}}
	fail := func(cp int, seq int64, reason, format string, args ...any) {
		rep.Failures = append(rep.Failures, Failure{Checkpoint: cp, Seq: seq, Reason: reason, Detail: fmt.Sprintf(format, args...)})
	}

	pinned := map[string]PublicKey{}
	for _, k := range keys.Keys {
		switch {
		case len(k.PublicKey) != ed25519.PublicKeySize || KeyID(k.PublicKey) != k.Kid:
			fail(-1, 0, ReasonBadKey, "pinned key %q is not the key its kid names", k.Kid)
		case pinned[k.Kid].Kid != "":
			fail(-1, 0, ReasonBadKey, "pinned key %q is listed twice", k.Kid)
		default:
			pinned[k.Kid] = k
		}
	}

	events := 0
	for _, c := range b.Checkpoints {
		events += len(c.Events)
	}
	switch {
	case b.Format != ReceiptFormat && b.Format != CheckpointsFormat:
		fail(-1, 0, ReasonFormat, "format %q is neither %q nor %q", b.Format, ReceiptFormat, CheckpointsFormat)
		return rep
	case len(b.Checkpoints) == 0:
		fail(-1, 0, ReasonFormat, "the bundle holds no checkpoint")
		return rep
	case b.Format == ReceiptFormat && events == 0:
		fail(-1, 0, ReasonFormat, "a receipt that names no event proves nothing about any event")
	}

	var prevText string
	var prevSt *Statement
	for i, c := range b.Checkpoints {
		st, err := ParseStatement(c.Statement)
		if err != nil {
			fail(i, 0, ReasonStatement, "%v", err)
			rep.Checkpoints = append(rep.Checkpoints, CheckpointResult{})
			for _, ev := range c.Events {
				rep.Events = append(rep.Events, EventResult{Checkpoint: i, Seq: ev.Row.Seq, ID: ev.Row.ID, Action: ev.Row.Action})
			}
			prevText, prevSt = c.Statement, nil
			continue
		}
		res := CheckpointResult{SeqLo: st.SeqLo, SeqHi: st.SeqHi, Kid: st.Kid}
		before := len(rep.Failures)

		// 1. The signature, with the pinned key the kid names.
		k, known := pinned[st.Kid]
		switch {
		case !known:
			fail(i, st.SeqLo, ReasonUnknownKid, "no pinned key has kid %s", st.Kid)
		case k.Retired:
			fail(i, st.SeqLo, ReasonKeyRetired, "kid %s is retired (treated as possibly stolen)", st.Kid)
		case !ed25519.Verify(k.PublicKey, []byte(c.Statement), c.Signature):
			fail(i, st.SeqLo, ReasonBadSignature, "the signature does not verify with kid %s", st.Kid)
		default:
			res.Signed = true
		}

		// 2. The prev link to the checkpoint before it in the bundle.
		linked := false
		if i > 0 {
			if want := StatementHash(prevText); st.Prev != want {
				fail(i, st.SeqLo, ReasonPrev, "prev %q is not the previous statement's hash %s (a checkpoint is missing or was replaced)", st.Prev, want)
			} else {
				linked = prevSt != nil
			}
		}

		// 3. The hash list.
		if b.Format == CheckpointsFormat && (len(c.RowHashes) > 0 || len(c.Events) > 0) {
			fail(i, st.SeqLo, ReasonFormat, "a checkpoint export carries no rows")
		}
		if len(c.Events) > 0 && len(c.RowHashes) == 0 {
			fail(i, st.SeqLo, ReasonFormat, "events without their checkpoint's hash list")
		}
		listOK := false
		if len(c.RowHashes) > 0 {
			listOK = true
			if int64(len(c.RowHashes)) != st.Rows {
				fail(i, st.SeqLo, ReasonRows, "%d hashes for %d signed rows", len(c.RowHashes), st.Rows)
				listOK = false
			}
			if d := Fold(st.Prev, c.RowHashes); d != st.Digest {
				fail(i, st.SeqLo, ReasonDigest, "the hash list folds to %s, the signed digest is %s", d, st.Digest)
				listOK = false
			}
			if c.RowHashes[len(c.RowHashes)-1] != st.HeadHash {
				fail(i, st.SeqHi, ReasonHead, "the last hash is not the signed head_hash")
				listOK = false
			}
		}
		res.Inclusion = listOK && len(rep.Failures) == before

		// 4. The events.
		for _, ev := range c.Events {
			r := EventResult{Checkpoint: i, Seq: ev.Row.Seq, ID: ev.Row.ID, Action: ev.Row.Action}
			evBefore := len(rep.Failures)
			seq := ev.Row.Seq
			if seq < st.SeqLo || seq > st.SeqHi {
				fail(i, seq, ReasonSeq, "seq %d is outside the checkpoint's %d-%d", seq, st.SeqLo, st.SeqHi)
			}
			if ev.Index < 0 || ev.Index >= len(c.RowHashes) {
				fail(i, seq, ReasonIndex, "index %d is outside the %d-entry hash list", ev.Index, len(c.RowHashes))
				rep.Events = append(rep.Events, r)
				continue
			}
			if ev.Row.RowHash == nil || *ev.Row.RowHash != c.RowHashes[ev.Index] {
				fail(i, seq, ReasonHashList, "the row's row_hash is not entry %d of the signed hash list", ev.Index)
			}
			var wantPrev string
			checkPrev := true
			switch {
			case ev.Index > 0:
				wantPrev = c.RowHashes[ev.Index-1]
			case linked:
				wantPrev = prevSt.HeadHash
			default:
				checkPrev = false // the first row of the bundle's first checkpoint: nothing here to link it to
			}
			if checkPrev && (ev.Row.PrevHash == nil || *ev.Row.PrevHash != wantPrev) {
				fail(i, seq, ReasonPrevHash, "the row's prev_hash is not the hash of the row before it")
			}
			if len(ev.RedactedFields) > 0 {
				if err := checkRedaction(ev); err != nil {
					fail(i, seq, ReasonRedaction, "%v", err)
				} else {
					r.Withheld = true
				}
			} else if ev.Row.RowHash != nil && RowHash(ev.Row) != *ev.Row.RowHash {
				fail(i, seq, ReasonRowHash, "the row's content does not hash to its row_hash (it was altered)")
			}
			r.OK = len(rep.Failures) == evBefore && res.Signed && res.Inclusion
			rep.Events = append(rep.Events, r)
		}

		rep.Checkpoints = append(rep.Checkpoints, res)
		prevText, prevSt = c.Statement, &st
	}
	return rep
}

// checkRedaction accepts a withheld event only as the projector makes one: an action it redacts, and data
// carrying the same redacted_fields marker the event names.
func checkRedaction(ev Event) error {
	if !RedactableActions[ev.Row.Action] {
		return fmt.Errorf("action %q is never served redacted", ev.Row.Action)
	}
	if ev.Row.Data == nil {
		return fmt.Errorf("a redacted row carries no data marker")
	}
	var marker struct {
		Fields []string `json:"redacted_fields"`
	}
	if err := json.Unmarshal([]byte(*ev.Row.Data), &marker); err != nil {
		return fmt.Errorf("a redacted row's data does not decode: %v", err)
	}
	got, want := slices.Clone(marker.Fields), slices.Clone(ev.RedactedFields)
	slices.Sort(got)
	slices.Sort(want)
	if len(got) == 0 || !slices.Equal(got, want) {
		return fmt.Errorf("redacted_fields %v does not match the data's marker %v", ev.RedactedFields, marker.Fields)
	}
	return nil
}
