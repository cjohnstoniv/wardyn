// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
)

// TokenDigest is hex(sha256(token)), lower case: the form a minted token is stored, bound and checked in. The side
// that mints a token, the store that keeps it and the proxy that checks it all compute it here, so they cannot drift
// apart on its encoding.
func TokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// MatchDigest returns the index of the last of n digests, digest(0) to digest(n-1), equal to got, or -1 for none. Each
// is compared in constant time and every one is compared, so how long the scan takes does not depend on which one
// matched.
func MatchDigest(got string, n int, digest func(i int) string) int {
	match := -1
	for i := range n {
		if subtle.ConstantTimeCompare([]byte(got), []byte(digest(i))) == 1 {
			match = i
		}
	}
	return match
}
