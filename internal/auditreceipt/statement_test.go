// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package auditreceipt

import (
	"strings"
	"testing"
)

func sampleStatement() Statement {
	return Statement{Format: CheckpointFormat, Kid: strings.Repeat("0f", 16), SeqLo: 101, SeqHi: 250, Rows: 150,
		HeadHash: strings.Repeat("1a", 32), Digest: strings.Repeat("2b", 32), SeqDigest: strings.Repeat("4d", 32), Prev: strings.Repeat("3c", 32),
		SignedAt: "2026-11-07T10:00:00.123456789Z"}
}

// TestStatement_ByteStableKeyOrder: the signed bytes are compact JSON with the keys in exactly the documented
// order, nothing HTML-escaped, and ParseStatement accepts only that form, so the bytes a verifier reads are the
// bytes that were signed.
func TestStatement_ByteStableKeyOrder(t *testing.T) {
	s := sampleStatement()
	want := `{"format":"wardyn-audit-checkpoint/2","kid":"0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f","seq_lo":101,"seq_hi":250,"rows":150,` +
		`"head_hash":"` + strings.Repeat("1a", 32) + `","digest":"` + strings.Repeat("2b", 32) + `","seq_digest":"` + strings.Repeat("4d", 32) + `","prev":"` + strings.Repeat("3c", 32) +
		`","signed_at":"2026-11-07T10:00:00.123456789Z"}`
	if got := string(s.Bytes()); got != want {
		t.Fatalf("statement bytes:\n got %s\nwant %s", got, want)
	}
	if got := string(s.Bytes()); got != want {
		t.Fatal("a second encoding differs from the first")
	}
	back, err := ParseStatement(want)
	if err != nil || back != s {
		t.Fatalf("ParseStatement(canonical) = %+v, %v; want %+v", back, err, s)
	}

	genesis := s
	genesis.Prev = ""
	if _, err := ParseStatement(string(genesis.Bytes())); err != nil {
		t.Fatalf("the first checkpoint (empty prev) does not parse: %v", err)
	}

	for name, text := range map[string]string{
		"reordered keys":      strings.Replace(want, `"seq_lo":101,"seq_hi":250`, `"seq_hi":250,"seq_lo":101`, 1),
		"spaces":              strings.Replace(want, `,"rows"`, `, "rows"`, 1),
		"duplicate key":       strings.Replace(want, `"rows":150,`, `"rows":150,"rows":150,`, 1),
		"unknown key":         strings.Replace(want, `"rows":150,`, `"rows":150,"extra":1,`, 1),
		"trailing newline":    want + "\n",
		"other format":        strings.Replace(want, "wardyn-audit-checkpoint/2", "wardyn-audit-checkpoint/3", 1),
		"no seq_digest":       strings.Replace(want, `"seq_digest":"`+strings.Repeat("4d", 32)+`",`, "", 1),
		"short seq_digest":    strings.Replace(want, strings.Repeat("4d", 32), "4d", 1),
		"short kid":           strings.Replace(want, `"kid":"0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f"`, `"kid":"0f"`, 1),
		"upper-case hash":     strings.Replace(want, strings.Repeat("1a", 32), strings.Repeat("1A", 32), 1),
		"seq_hi below lo":     strings.Replace(want, `"seq_hi":250`, `"seq_hi":100`, 1),
		"zero rows":           strings.Replace(want, `"rows":150`, `"rows":0`, 1),
		"more rows than seqs": strings.Replace(want, `"rows":150`, `"rows":151`, 1),
		"bad time":            strings.Replace(want, "2026-11-07T10:00:00.123456789Z", "yesterday", 1),
	} {
		if _, err := ParseStatement(text); err == nil {
			t.Errorf("%s: ParseStatement accepted %s", name, text)
		}
	}
}

// TestStatementHash is the prev link: lowercase hex sha256 of the exact statement text.
func TestStatementHash(t *testing.T) {
	// printf '%s' 'x' | sha256sum
	if got := StatementHash("x"); got != "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881" {
		t.Fatalf("StatementHash(x) = %s", got)
	}
}
