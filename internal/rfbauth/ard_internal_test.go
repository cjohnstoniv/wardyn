// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package rfbauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"math/big"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/rfbauth/rfbauthtest"
)

// A random private key gives a shared key with a zero first byte once in 256 sign-ins, so this picks
// one that does: the client must left-pad the shared key and its public key to the key length, or
// the server derives another AES key and refuses.
func TestARDKeysAreLeftPaddedToTheKeyLength(t *testing.T) {
	p := rfbauthtest.Group2
	n := (p.BitLen() + 7) / 8
	y := big.NewInt(0x5eed5eed)
	ys := new(big.Int).Exp(big.NewInt(2), y, p)
	limit := new(big.Int).Lsh(big.NewInt(1), uint(8*(n-1)))
	x := big.NewInt(2)
	for new(big.Int).Exp(ys, x, p).Cmp(limit) >= 0 {
		x.Add(x, big.NewInt(1))
	}
	pick := new(big.Int).Sub(x, big.NewInt(2)).FillBytes(make([]byte, n+8))
	srv := &rfbauthtest.Server{User: "agent", Password: "pw", Private: y}
	conn, _ := srv.Dial(context.Background())
	defer conn.Close()
	si, err := ardLogin(conn, "agent", []byte("pw"), io.MultiReader(bytes.NewReader(pick), rand.Reader))
	if err != nil || !bytes.Equal(si, rfbauthtest.ServerInit()) {
		t.Fatalf("sign-in with a shared key that starts with a zero byte = %x, %v", si, err)
	}
}
