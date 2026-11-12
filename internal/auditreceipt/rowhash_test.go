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

func strp(s string) *string { return &s }

// TestRowHash_GoldenVectors pins the exact bytes audit_row_hash hashes, written out by hand from the SQL
// (jsonb_build_array(...)::text after coalesce(prev, ”)): time_us is a quoted string, a NULL run_id is a
// quoted empty string, a NULL text column or NULL data is a bare null, data is embedded as its stored jsonb
// text, elements are joined by ", ", and strings are escaped as jsonb prints them (\" \\ \b \f \n \r \t,
// other bytes below 0x20 as \u00xx, everything else raw: non-ASCII, U+2028, <>&, DEL).
func TestRowHash_GoldenVectors(t *testing.T) {
	id1 := uuid.MustParse("00000000-0000-4000-8000-000000000001")
	id2 := uuid.MustParse("6f1c2b3a-4d5e-4f60-8a1b-2c3d4e5f6a7b")
	run := uuid.MustParse("0a0b0c0d-0e0f-4a1b-9c2d-3e4f5a6b7c8d")
	prev := strings.Repeat("ab", 32)
	cases := []struct {
		name string
		row  Row
		want string
	}{{
		name: "plain row, first in the chain, nil run_id",
		row: Row{ID: id1, TimeUS: 1700000000123456, ActorType: "system", Actor: "wardynd", Action: "audit.checkpoint.create",
			Target: strp("audit_checkpoints"), Outcome: "success", SourceIP: strp(""), Data: strp(`{"a": 1}`)},
		want: `["00000000-0000-4000-8000-000000000001", "1700000000123456", "", "system", "wardynd", "audit.checkpoint.create", "audit_checkpoints", "success", "", {"a": 1}]`,
	}, {
		name: "quote, backslash, control bytes, non-ASCII, U+2028, <>&, DEL; NULL data; prev prefixed",
		row: Row{ID: id2, TimeUS: 1, RunID: &run, ActorType: "human", Actor: `a"b\c`, Action: "x.y",
			Target: strp("l1\nl2\tt\r\b\f<>& é日本"), Outcome: "denied", SourceIP: strp("\x01\x1f\x7f"),
			PrevHash: &prev},
		want: prev + `["6f1c2b3a-4d5e-4f60-8a1b-2c3d4e5f6a7b", "1", "0a0b0c0d-0e0f-4a1b-9c2d-3e4f5a6b7c8d", "human", "a\"b\\c", "x.y", ` +
			`"l1\nl2\tt\r\b\f<>&` + " é日本" + `", "denied", "\u0001\u001f` + "\x7f" + `", null]`,
	}, {
		name: "NULL target and source_ip render bare null; jsonb null data renders null",
		row:  Row{ID: id1, TimeUS: -123, ActorType: "agent", Actor: "", Action: "a", Outcome: "failure", Data: strp("null")},
		want: `["00000000-0000-4000-8000-000000000001", "-123", "", "agent", "", "a", null, "failure", null, null]`,
	}, {
		name: "data is embedded verbatim, its own escapes kept",
		row: Row{ID: id1, TimeUS: 0, ActorType: "system", Actor: "s", Action: "a", Target: strp(""), Outcome: "success",
			SourceIP: strp(""), Data: strp(`{"k": "a\"b\u0001", "n": [1, 2.5, null], "u": "é"}`)},
		want: `["00000000-0000-4000-8000-000000000001", "0", "", "system", "s", "a", "", "success", "", {"k": "a\"b\u0001", "n": [1, 2.5, null], "u": "é"}]`,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(HashInput(tc.row)); got != tc.want {
				t.Fatalf("hash input:\n got %q\nwant %q", got, tc.want)
			}
			sum := sha256.Sum256([]byte(tc.want))
			if got, want := RowHash(tc.row), hex.EncodeToString(sum[:]); got != want {
				t.Fatalf("RowHash = %s, want sha256 of the input %s", got, want)
			}
		})
	}

	// One end-to-end constant, from `printf '%s' '<input>' | sha256sum`, so a change in how the digest is
	// printed (case, length) fails too.
	if got := RowHash(cases[0].row); got != "661d578378968a9d5a59f7fc98c5c48c7cd5dbcce311dd4efb95521c42801912" {
		t.Fatalf("RowHash(plain row) = %s", got)
	}
}
