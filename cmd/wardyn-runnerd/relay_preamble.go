// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"io"
	"net"
	"time"

	"github.com/google/uuid"
)

// The relay preamble a keyed proxy sends first on each connection it makes to the control-plane
// host: relayPreambleMagic, relayPreambleVersion, then its run's 32-byte relay key. A TLS client
// opens with 0x16, never with relayPreambleMagic.
const (
	relayPreambleMagic   byte = 0x57
	relayPreambleVersion byte = 1
	relayPreambleKeyLen       = 32
)

// relayStart is how a relay connection opened: with the preamble (keyed, its version and key) or
// without, and first, the stream's own first byte, which relayCopy sends first.
type relayStart struct {
	keyed   bool
	version byte
	key     [relayPreambleKeyLen]byte
	first   byte
}

// readRelayStart reads c's first byte and, when it is relayPreambleMagic, the version, the key and the
// stream's own first byte after them, all before deadline. A connection that sends less is not
// admitted.
func readRelayStart(c net.Conn, deadline time.Time) (relayStart, bool) {
	var s relayStart
	_ = c.SetReadDeadline(deadline)
	var b [1]byte
	if _, err := io.ReadFull(c, b[:]); err != nil {
		return s, false
	}
	s.first = b[0]
	if s.first == relayPreambleMagic {
		var rest [1 + relayPreambleKeyLen + 1]byte
		if _, err := io.ReadFull(c, rest[:]); err != nil {
			return s, false
		}
		s.keyed, s.version, s.first = true, rest[0], rest[len(rest)-1]
		copy(s.key[:], rest[1:1+relayPreambleKeyLen])
	}
	_ = c.SetReadDeadline(time.Time{})
	return s, true
}

// relayAdmits is the k8s relay's rule for a connection whose source address the lookup mapped to a
// live proxy pod of run; keys holds each run's relay key hash. A fenced run is refused whatever it
// sends. A keyed connection is admitted only with the known version and run's own key, and the IP
// fence is not consulted for it: the key, not the address, says which run is talking. An unkeyed
// one keeps the address rule, and is refused for a run that holds a key, whose proxy sends only
// keyed connections.
func relayAdmits(run uuid.UUID, s relayStart, keys map[uuid.UUID][sha256.Size]byte, runFenced, ipFenced bool) bool {
	if runFenced {
		return false
	}
	want, held := keys[run]
	if !s.keyed {
		return !ipFenced && !held
	}
	if s.version != relayPreambleVersion || !held {
		return false
	}
	got := sha256.Sum256(s.key[:])
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}
