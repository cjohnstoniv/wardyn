// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package auditreceipt

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/google/uuid"
)

const testSalt = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func v2Row(salt *string) Row {
	run := uuid.MustParse("0a0b0c0d-0e0f-4a1b-9c2d-3e4f5a6b7c8d")
	return Row{Seq: 7, ID: uuid.MustParse("6f1c2b3a-4d5e-4f60-8a1b-2c3d4e5f6a7b"), TimeUS: 1700000000123456, RunID: &run,
		ActorType: "agent", Actor: "run", Action: "test.row", Target: strp("t"), Outcome: "success", SourceIP: strp(""),
		Data: strp(`{"k": "secret-host"}`), PrevHash: strp(strings.Repeat("ab", 32)), HashFormat: 2, HashSalt: salt}
}

// TestRowHash_V1Unchanged: a row with no format, and one that says format 1, hash exactly as before the
// salted format existed (the golden vectors above), and carry no salt in their input.
func TestRowHash_V1Unchanged(t *testing.T) {
	r := Row{ID: uuid.MustParse("00000000-0000-4000-8000-000000000001"), TimeUS: 1700000000123456, ActorType: "system",
		Actor: "wardynd", Action: "audit.checkpoint.create", Target: strp("audit_checkpoints"), Outcome: "success",
		SourceIP: strp(""), Data: strp(`{"a": 1}`)}
	const want = `["00000000-0000-4000-8000-000000000001", "1700000000123456", "", "system", "wardynd", "audit.checkpoint.create", "audit_checkpoints", "success", "", {"a": 1}]`
	sum := sha256.Sum256([]byte(want))
	wantHash := hex.EncodeToString(sum[:])
	for _, f := range []int{0, 1} {
		r.HashFormat = f
		if got := string(HashInput(r)); got != want {
			t.Errorf("format %d input = %s, want %s", f, got, want)
		}
		if got := RowHash(r); got != wantHash {
			t.Errorf("format %d hash = %s, want %s", f, got, wantHash)
		}
		if got, err := RowHashOf(r); err != nil || got != wantHash {
			t.Errorf("format %d RowHashOf = %q, %v", f, got, err)
		}
	}
}

// TestHashInput_V2Golden pins the salted input: "v2" and the salt come first in the array.
func TestHashInput_V2Golden(t *testing.T) {
	r := v2Row(strp(testSalt))
	want := strings.Repeat("ab", 32) + `["v2", "` + testSalt + `", "6f1c2b3a-4d5e-4f60-8a1b-2c3d4e5f6a7b", "1700000000123456", ` +
		`"0a0b0c0d-0e0f-4a1b-9c2d-3e4f5a6b7c8d", "agent", "run", "test.row", "t", "success", "", {"k": "secret-host"}]`
	if got := string(HashInput(r)); got != want {
		t.Fatalf("v2 input = %s\nwant %s", got, want)
	}
	sum := sha256.Sum256([]byte(want))
	if got, err := RowHashOf(r); err != nil || got != hex.EncodeToString(sum[:]) {
		t.Fatalf("RowHashOf = %q, %v", got, err)
	}
	other := strings.Repeat("c", 64)
	r2 := v2Row(&other)
	if RowHash(r) == RowHash(r2) {
		t.Fatal("two salts gave one hash")
	}
}

// TestRowHashOf_V2BadSaltIsFormat: a v2 row with a nil, a short or an upper-case salt, and an unknown
// format, have no hash; verify reports them as an incomplete row, never as an altered one.
func TestRowHashOf_V2BadSaltIsFormat(t *testing.T) {
	bad := map[string]*string{"nil": nil, "63 characters": strp(testSalt[:63]), "upper case": strp(strings.ToUpper(testSalt))}
	for name, salt := range bad {
		r := v2Row(salt)
		if h, err := RowHashOf(r); err == nil || h != "" || (salt != nil && strings.Contains(err.Error(), *salt)) {
			t.Errorf("%s: RowHashOf = %q, %v, want an error without the salt", name, h, err)
		}
		if RowHash(r) != "" {
			t.Errorf("%s: RowHash is not empty", name)
		}
	}
	r := v2Row(strp(testSalt))
	r.HashFormat = 3
	if _, err := RowHashOf(r); err == nil || RowHash(r) != "" {
		t.Errorf("format 3: want an error and no hash")
	}

	priv, pub := testKey(1)
	for name, salt := range bad {
		rows := testChain(1, 3, "")
		rows[1] = v2Row(salt)
		rows[1].Seq = 2
		h := strings.Repeat("e", 64)
		rows[1].RowHash = &h
		rows[1].PrevHash = rows[0].RowHash
		rows[2].PrevHash = &h
		c := withEvents(testCheckpoint(priv, rows, ""), rows, 1)
		rep := VerifyBundle(Bundle{Format: ReceiptFormat, Checkpoints: []Checkpoint{c}}, keysOf(pub))
		if !hasReason(rep, ReasonFormat) || hasReason(rep, ReasonRowHash) {
			t.Errorf("%s: failures %+v, want %s and never %s", name, rep.Failures, ReasonFormat, ReasonRowHash)
		}
	}
}

// TestProjectedRowHashNotRecomputable: a v2 row whose data was withheld and whose salt is gone cannot be
// recomputed from its visible fields and a list of candidates that includes the true withheld value.
func TestProjectedRowHashNotRecomputable(t *testing.T) {
	truth := v2Row(strp(testSalt))
	stored := RowHash(truth)
	candidates := []string{`{"k": "secret-host"}`, `{"k": "other-host"}`, `{"k": "x"}`, `null`}
	zero := strings.Repeat("0", 64)
	for _, c := range candidates {
		visible := truth
		visible.HashSalt = nil
		visible.Data = strp(c)
		for _, f := range []int{0, 1} { // what a reader can try without the salt: the original formula
			try := visible
			try.HashFormat = f
			if RowHash(try) == stored {
				t.Fatalf("candidate %s reproduced the stored hash under format %d", c, f)
			}
		}
		try := visible
		try.HashSalt = &zero // and a guessed salt
		if RowHash(try) == stored {
			t.Fatalf("candidate %s reproduced the stored hash under a guessed salt", c)
		}
	}
	if RowHash(truth) != stored {
		t.Fatal("the row with its salt must reproduce its own hash")
	}
}

// TestUnprojectedLineVerifiesOffline: a v2 row that kept its salt verifies in a receipt with nothing but the
// receipt and the key.
func TestUnprojectedLineVerifiesOffline(t *testing.T) {
	priv, pub := testKey(1)
	rows := testChain(1, 3, "")
	r := v2Row(strp(testSalt))
	r.Seq, r.PrevHash = 2, rows[0].RowHash
	h, err := RowHashOf(r)
	if err != nil {
		t.Fatal(err)
	}
	r.RowHash = &h
	rows[1] = r
	n := rows[2]
	n.PrevHash = &h
	nh := RowHash(n)
	n.RowHash = &nh
	rows[2] = n
	c := withEvents(testCheckpoint(priv, rows, ""), rows, 1)
	rep := VerifyBundle(Bundle{Format: ReceiptFormat, Checkpoints: []Checkpoint{c}}, keysOf(pub))
	if !rep.OK() || !rep.Complete() {
		t.Fatalf("a v2 row with its salt: failures %+v", rep.Failures)
	}
	c.Events[0].Row.Actor = "someone-else"
	if rep := VerifyBundle(Bundle{Format: ReceiptFormat, Checkpoints: []Checkpoint{c}}, keysOf(pub)); rep.OK() || !hasReason(rep, ReasonRowHash) {
		t.Fatalf("an edited v2 row: failures %+v, want %s", rep.Failures, ReasonRowHash)
	}
}
