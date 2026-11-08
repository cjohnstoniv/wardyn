// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package auditreceipt

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// Fold is a checkpoint's digest over its rows' hashes, in seq order:
//
//	d_0 = sha256(seed)            d_i = sha256(d_{i-1} || row_hash_i)
//
// every value lowercase hex text. A checkpoint's seed is its prev (the previous statement's hash, or "" for
// the first). It is the step the partition digest uses (store.PartitionDigest), so the same sha256sum recipe
// checks both.
func Fold(seed string, rowHashes []string) string {
	sum := sha256.Sum256([]byte(seed))
	d := hex.EncodeToString(sum[:])
	for _, h := range rowHashes {
		sum = sha256.Sum256([]byte(d + h))
		d = hex.EncodeToString(sum[:])
	}
	return d
}

// SeqDigestTag starts every seq digest, so no other sha256 in the audit log can stand in for one.
const SeqDigestTag = "wardyn-audit-seq-digest/1"

// SeqDigest is a checkpoint's digest over its rows' (seq, row_hash) pairs, in seq order:
//
//	s_0 = sha256(SeqDigestTag)    s_i = sha256(s_{i-1} || len(seq_i) ":" seq_i || len(row_hash_i) ":" row_hash_i)
//
// seq_i is decimal text, len is a decimal byte count, every digest lowercase hex text. A row's hash does not
// cover its seq, so this digest, signed in the statement, is what ties each seq to its row; Fold alone would
// let seqs be relabelled inside the signed range. The hand recipe is one line per row:
//
//	s=$(printf '%s%d:%s%d:%s' "$s" ${#seq} "$seq" ${#h} "$h" | sha256sum | cut -c1-64)
//
// Lists of different lengths have no digest: it returns "", which no statement carries.
func SeqDigest(seqs []int64, rowHashes []string) string {
	if len(seqs) != len(rowHashes) {
		return ""
	}
	sum := sha256.Sum256([]byte(SeqDigestTag))
	d := hex.EncodeToString(sum[:])
	for i, h := range rowHashes {
		seq := strconv.FormatInt(seqs[i], 10)
		sum = sha256.Sum256([]byte(d + strconv.Itoa(len(seq)) + ":" + seq + strconv.Itoa(len(h)) + ":" + h))
		d = hex.EncodeToString(sum[:])
	}
	return d
}
