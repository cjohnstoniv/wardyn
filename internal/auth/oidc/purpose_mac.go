// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"crypto/hmac"
	"crypto/sha256"
)

// PurposeMAC authenticates data for one named purpose with the key every replica of this console shares.
// The key used is derived per purpose, so a value signed for one purpose is never valid for another and
// none can be replayed as a session cookie. A caller verifies by recomputing and comparing with hmac.Equal.
func (a *Authenticator) PurposeMAC(purpose string, data []byte) []byte {
	derive := hmac.New(sha256.New, a.hmacKey)
	derive.Write([]byte("wardyn/purpose-mac/v1/" + purpose))
	mac := hmac.New(sha256.New, derive.Sum(nil))
	mac.Write(data)
	return mac.Sum(nil)
}
