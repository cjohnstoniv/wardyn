// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// Protocol constants of the stream (design §3.2). They are constants, not knobs.
const (
	ProtocolVersion = 1

	StreamWindow = 256 << 10 // initial credit per byte stream
	ConnWindow   = 4 << 20   // initial credit per connection
	ReplayBytes  = 1 << 20   // replay buffer per side, until ACKed

	ResumeWindow = 120 * time.Second
	PingInterval = 15 * time.Second
	MissedPings  = 3
	BackoffMin   = time.Second
	BackoffMax   = 60 * time.Second

	nonceLen = 32
	authTag  = "wardyn-runner-v1"
)

// AuthMessage is the byte string a runner signs for one connection: the tag,
// the server's fresh nonce, the runner id and the org URL hash, so a signature
// is bound to one connection, one runner and one org.
func AuthMessage(nonce []byte, runnerID, orgURLSHA256 string) []byte {
	m := make([]byte, 0, len(authTag)+len(nonce)+len(runnerID)+len(orgURLSHA256))
	m = append(m, authTag...)
	m = append(m, nonce...)
	m = append(m, runnerID...)
	return append(m, orgURLSHA256...)
}

// SignAuth is the AUTH signature over AuthMessage.
func SignAuth(key ed25519.PrivateKey, nonce []byte, runnerID, orgURLSHA256 string) []byte {
	return ed25519.Sign(key, AuthMessage(nonce, runnerID, orgURLSHA256))
}

// VerifyAuth reports whether sig proves possession of pub for this connection.
// A nonce of the wrong length never verifies.
func VerifyAuth(pub ed25519.PublicKey, nonce []byte, runnerID, orgURLSHA256 string, sig []byte) bool {
	return len(pub) == ed25519.PublicKeySize && len(nonce) == nonceLen &&
		ed25519.Verify(pub, AuthMessage(nonce, runnerID, orgURLSHA256), sig)
}

// Fingerprint is the key fingerprint a runner prints and an owner claims with:
// the hex SHA-256 of the raw public key.
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])
}

// PostAuthSequence is what follows AUTH_OK. A fresh session runs the full
// sequence; a resumed one replays its un-ACKed frames and skips all three.
func PostAuthSequence(resumed bool) []Type {
	if resumed {
		return nil
	}
	return []Type{TypePending, TypeState, TypeReady}
}

// One id space is shared by calls and byte streams: 0 is the control stream,
// even ids are org-initiated, odd ids runner-initiated.

// IDAllocator hands out ids for one side of the connection.
type IDAllocator struct{ next uint32 }

// NewIDAllocator starts the allocator for the org (even) or the runner (odd).
func NewIDAllocator(org bool) *IDAllocator {
	if org {
		return &IDAllocator{next: 2}
	}
	return &IDAllocator{next: 1}
}

// Next returns the next id. It is not safe for concurrent use.
func (a *IDAllocator) Next() uint32 {
	id := a.next
	a.next += 2
	return id
}

// InitiatedByOrg reports which side owns id; the control stream has no owner.
func InitiatedByOrg(id uint32) (org, ok bool) {
	if id == 0 {
		return false, false
	}
	return id%2 == 0, true
}
