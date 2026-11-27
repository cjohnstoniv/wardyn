// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package auditreceipt checks signed audit checkpoints and receipts offline. It links no api or store code, so
// the CLI can verify a receipt on any machine with nothing but the receipt and the public keys the operator
// saved earlier.
package auditreceipt

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strconv"

	"github.com/google/uuid"
)

// Row is one stored audit row as a receipt carries it: every column audit_row_hash covers, in the units it
// hashes them in (time as integer microseconds, data as the jsonb's own text), plus the position and hashes.
// The JSON names are the raw partition export's.
type Row struct {
	Seq       int64      `json:"seq"`
	ID        uuid.UUID  `json:"id"`
	TimeUS    int64      `json:"time_us"`
	RunID     *uuid.UUID `json:"run_id"`
	ActorType string     `json:"actor_type"`
	Actor     string     `json:"actor"`
	Action    string     `json:"action"`
	// Target and SourceIP are NOT NULL in the table; a nil one hashes as the function hashes a NULL argument.
	Target   *string `json:"target"`
	Outcome  string  `json:"outcome"`
	SourceIP *string `json:"source_ip"`
	// Data is the jsonb's stored text form, embedded verbatim; nil for SQL NULL (which hashes as jsonb null).
	Data *string `json:"data"`
	// PrevHash is nil for the chain's first row; RowHash is the stored hash.
	PrevHash *string `json:"prev_hash"`
	RowHash  *string `json:"row_hash"`
	// HashFormat is the formula the row was hashed with: absent or 1 is the original, 2 is salted. HashSalt is
	// a format 2 row's per-row secret salt; an export or receipt line of a row whose content was withheld
	// carries none, and a row without one cannot be re-hashed.
	HashFormat int     `json:"hash_format,omitempty"`
	HashSalt   *string `json:"hash_salt,omitempty"`
}

var saltPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// RowHashOf is the row's hash under its own format. It errors on a format other than absent, 1 or 2, and on
// a format 2 row whose salt is missing or is not 64 lowercase hex. The error never carries the salt.
func RowHashOf(r Row) (string, error) {
	switch r.HashFormat {
	case 0, 1:
		return RowHash(r), nil
	case 2:
		if r.HashSalt == nil || !saltPattern.MatchString(*r.HashSalt) {
			return "", errors.New("a format 2 row needs a hash_salt of 64 lowercase hex characters")
		}
		return RowHash(r), nil
	}
	return "", errors.New("hash_format must be 1 or 2")
}

// HashInput is the exact byte string audit_row_hash (migration 0047) feeds to sha256:
//
//	coalesce(prev, '') || jsonb_build_array(id::text, time_us::text, coalesce(run_id::text, ''),
//	    actor_type, actor, action, target, outcome, source_ip, data)::text
//
// jsonb prints an array as its elements joined by ", " inside brackets, a text element as a JSON string, a
// NULL element as null, and a jsonb element as its own text.
//
// A format 2 row (audit_row_hash_v2) puts the two strings "v2" and its salt first in the array. A format
// outside absent, 1 and 2, or a format 2 row without a usable salt, has no hash input: HashInput returns nil
// and RowHash returns "" (RowHashOf says why).
func HashInput(r Row) []byte {
	if r.HashFormat > 2 || r.HashFormat < 0 {
		return nil
	}
	b := make([]byte, 0, 256)
	if r.PrevHash != nil {
		b = append(b, *r.PrevHash...)
	}
	runID := ""
	if r.RunID != nil {
		runID = r.RunID.String()
	}
	b = append(b, '[')
	fields := []*string{ptr(r.ID.String()), ptr(strconv.FormatInt(r.TimeUS, 10)), &runID,
		&r.ActorType, &r.Actor, &r.Action, r.Target, &r.Outcome, r.SourceIP}
	if r.HashFormat == 2 {
		if r.HashSalt == nil || !saltPattern.MatchString(*r.HashSalt) {
			return nil
		}
		fields = append([]*string{ptr("v2"), r.HashSalt}, fields...)
	}
	for i, s := range fields {
		if i > 0 {
			b = append(b, ", "...)
		}
		b = appendJSONBString(b, s)
	}
	b = append(b, ", "...)
	if r.Data == nil {
		b = append(b, "null"...)
	} else {
		b = append(b, *r.Data...)
	}
	return append(b, ']')
}

// RowHash is audit_row_hash in Go: lowercase hex sha256 of HashInput.
func RowHash(r Row) string {
	in := HashInput(r)
	if in == nil {
		return ""
	}
	sum := sha256.Sum256(in)
	return hex.EncodeToString(sum[:])
}

func ptr(s string) *string { return &s }

// appendJSONBString appends s as jsonb prints a string (Postgres escape_json): \" \\ \b \f \n \r \t, any other
// byte below 0x20 as \u00xx in lower-case hex, and every other byte raw. That is not encoding/json's form,
// which also escapes <, >, &, U+2028 and U+2029. A nil s prints null.
func appendJSONBString(b []byte, s *string) []byte {
	if s == nil {
		return append(b, "null"...)
	}
	const hexDigits = "0123456789abcdef"
	b = append(b, '"')
	for i := 0; i < len(*s); i++ {
		c := (*s)[i]
		switch c {
		case '"':
			b = append(b, '\\', '"')
		case '\\':
			b = append(b, '\\', '\\')
		case '\b':
			b = append(b, '\\', 'b')
		case '\f':
			b = append(b, '\\', 'f')
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		case '\t':
			b = append(b, '\\', 't')
		default:
			if c < 0x20 {
				b = append(b, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
			} else {
				b = append(b, c)
			}
		}
	}
	return append(b, '"')
}
