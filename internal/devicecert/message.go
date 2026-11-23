// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package devicecert holds what a Mac's MDM device key signs to prove it holds the key, and the bounds both
// ends keep to. It imports only the standard library, so the cgo helper that signs and the control plane that
// verifies share one definition of the message.
package devicecert

import (
	"errors"
	"time"
)

// Prefix starts every message a device key signs for Wardyn, so a signature over it cannot stand for
// anything another protocol signs with the same identity.
const Prefix = "wardyn-mdm-device-v1\n"

// Bounds the control plane enforces and the helper keeps to.
const (
	MaxProofs   = 8
	MaxChainLen = 4
	MaxDER      = 8 << 10
	MaxSkew     = 10 * time.Minute
)

// hexLen is the length of a SHA-256 in hex.
const hexLen = 64

// ErrMalformed is the refusal for a fingerprint or organisation hash that is not 64 lower-case hex characters.
var ErrMalformed = errors.New("devicecert: the runner fingerprint and the organisation hash must each be 64 lower-case hex characters")

func isLowerHex64(s string) bool {
	if len(s) != hexLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Message is what the device key signs: it binds the identity to this runner's key and organisation.
// fingerprint and orgURLSHA256 are 64 lower-case hex characters; signedAt is truncated to the second.
func Message(fingerprint, orgURLSHA256 string, signedAt time.Time) ([]byte, error) {
	if !isLowerHex64(fingerprint) || !isLowerHex64(orgURLSHA256) {
		return nil, ErrMalformed
	}
	return []byte(Prefix + fingerprint + "\n" + orgURLSHA256 + "\n" + signedAt.UTC().Format(time.RFC3339) + "\n"), nil
}
