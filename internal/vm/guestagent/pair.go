// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"time"
)

// Pairing. Before a guest connection carries anything, the helper sends a challenge on it and the guest
// answers with a proof that it holds the base's pairing key, which stage 1 wrote root-only into the
// guest. Only the guest agent (root) reads that file, so no other process in the guest pairs with the
// helper, on any port.
const (
	// MacPairKeyPath is the guest's copy of the base's pairing key: root:wheel 0600, written by stage 1.
	MacPairKeyPath = "/Library/Wardyn/pair.key"
	// PairMagic opens every challenge; PairChallengeLen random bytes follow it.
	PairMagic        = "WDYNPAIR"
	PairChallengeLen = 32
	// PairKeyLen is the key's length; the proof is sha256.Size bytes.
	PairKeyLen = 32
	pairLabel  = "wardyn-guest-pair-v1"
)

// errOlderHost: the first bytes on the connection were not a challenge, so the host is an older
// runner's helper, which pairs any connection. The guest serves nothing on it.
var errOlderHost = errors.New("guestagent: the host did not authenticate the pairing (an older runner)")

// pairChallengeWait bounds the guest's wait for the challenge, which a helper sends as it accepts.
var pairChallengeWait = 60 * time.Second

// PairProof is HMAC-SHA256(key, "wardyn-guest-pair-v1" || port (big-endian, 32 bits) || challenge). The
// port is inside it, so an answer on one channel is no answer on another.
func PairProof(key []byte, port uint32, challenge []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(pairLabel))
	mac.Write(binary.BigEndian.AppendUint32(nil, port))
	mac.Write(challenge)
	return mac.Sum(nil)
}

// ReadPairKey reads the guest's pairing key.
func ReadPairKey(path string) ([]byte, error) {
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("guestagent: the pairing key: %w", err)
	}
	if len(key) != PairKeyLen {
		return nil, fmt.Errorf("guestagent: the pairing key at %s is %d bytes, not %d", path, len(key), PairKeyLen)
	}
	return key, nil
}

// PairedDial wraps dial so that each connection answers the host's challenge before it is handed on: a
// session, the desktop's open byte and the tools channel only ever start on a paired connection.
func PairedDial(dial DialFunc, port uint32, key []byte) DialFunc {
	return func(ctx context.Context) (net.Conn, error) {
		c, err := dial(ctx)
		if err != nil {
			return nil, err
		}
		if err := answerPairing(ctx, c, port, key); err != nil {
			_ = c.Close()
			return nil, err
		}
		return c, nil
	}
}

// answerPairing reads the challenge and writes the proof. The magic is read a byte at a time, so an
// older helper's first byte (a session's 0x00, the desktop's open byte) is refused at once.
func answerPairing(ctx context.Context, c net.Conn, port uint32, key []byte) error {
	_ = c.SetDeadline(time.Now().Add(pairChallengeWait))
	stop := context.AfterFunc(ctx, func() { _ = c.SetDeadline(time.Unix(1, 0)) })
	err := exchange(c, port, key)
	if !stop() {
		return fmt.Errorf("guestagent: pairing: %w", ctx.Err())
	}
	if errors.Is(err, errOlderHost) || errors.Is(err, os.ErrDeadlineExceeded) {
		slog.Warn(errOlderHost.Error(), "port", port)
		return errOlderHost
	}
	if err != nil {
		return fmt.Errorf("guestagent: pairing on port %d: %w", port, err)
	}
	return c.SetDeadline(time.Time{})
}

func exchange(c net.Conn, port uint32, key []byte) error {
	var b [1]byte
	for i := range len(PairMagic) {
		if _, err := io.ReadFull(c, b[:]); err != nil {
			return err
		}
		if b[0] != PairMagic[i] {
			return errOlderHost
		}
	}
	challenge := make([]byte, PairChallengeLen)
	if _, err := io.ReadFull(c, challenge); err != nil {
		return err
	}
	_, err := c.Write(PairProof(key, port, challenge))
	return err
}
