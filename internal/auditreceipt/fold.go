// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package auditreceipt

import (
	"crypto/sha256"
	"encoding/hex"
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
