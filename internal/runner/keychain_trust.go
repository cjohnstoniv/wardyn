// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

// KeychainTrust is a macOS guest's answer to trusting the run's proxy CA: a status and, when not
// trusted, a fixed reason code. It never carries the guest's own output.
type KeychainTrust struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// The KeychainTrust codes. The guest agent sends the same strings (guestagent.KeychainTrusted and
// its siblings).
const (
	KeychainTrusted    = "trusted"
	KeychainNotTrusted = "not_trusted"
	// KeychainRefused: macOS refused the trust change.
	KeychainRefused = "refused"
	// KeychainFailed: the security command failed for another reason.
	KeychainFailed = "failed"
	// KeychainUnavailable: the command could not run, the CA was not one certificate, or the guest
	// sent a code this host does not know.
	KeychainUnavailable = "unavailable"
)

// Known is k with any code this host does not know read as not trusted, unavailable. Nil stays nil:
// absent means not attempted, never failed.
func (k *KeychainTrust) Known() *KeychainTrust {
	if k == nil {
		return nil
	}
	if k.Status == KeychainTrusted && k.Reason == "" {
		return &KeychainTrust{Status: KeychainTrusted}
	}
	switch k.Reason {
	case KeychainRefused, KeychainFailed, KeychainUnavailable:
		if k.Status == KeychainNotTrusted {
			return &KeychainTrust{Status: KeychainNotTrusted, Reason: k.Reason}
		}
	}
	return &KeychainTrust{Status: KeychainNotTrusted, Reason: KeychainUnavailable}
}
