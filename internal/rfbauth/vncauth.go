// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package rfbauth holds the RFB sign-ins both ends of the macOS desktop path share: VncAuth's answer,
// which the helper's shim gives and the guest agent checks, and Apple authentication (security type
// 30) as a client. It imports only the standard library, so the guest agent can use it.
package rfbauth

import (
	"crypto/des"
	"math/bits"
)

// VNCAuthResponse is VncAuth's answer to challenge: each half DES-encrypted under the password's
// first eight bytes, zero-padded, each byte's bits reversed (RFC 6143 7.2.2 and its errata).
func VNCAuthResponse(password string, challenge []byte) ([]byte, error) {
	var key [8]byte
	copy(key[:], password)
	for i, b := range key {
		key[i] = bits.Reverse8(b)
	}
	c, err := des.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	out := make([]byte, 16)
	c.Encrypt(out[:8], challenge[:8])
	c.Encrypt(out[8:], challenge[8:])
	return out, nil
}
