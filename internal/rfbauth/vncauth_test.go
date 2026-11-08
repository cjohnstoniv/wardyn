// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package rfbauth_test

import (
	"encoding/hex"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/rfbauth"
)

// The answers the helper's shim gave before VncAuth's answer moved to this package: the host's shim
// and the guest check one another with it, so a changed answer would refuse every session.
func TestVNCAuthResponseVectorsUnchanged(t *testing.T) {
	for _, c := range []struct{ password, challenge, want string }{
		{"Abcd2345", "30313233343536373839616263646566", "c351cf1f67f2525ee7be1f5a2a756efb"},
		{"Abcd2345-and-more", "30313233343536373839616263646566", "c351cf1f67f2525ee7be1f5a2a756efb"},
		{"", "30313233343536373839616263646566", "a068dbeab73d140b56541eb3c7625177"},
		{"pw", "ff00800102030405060708090a0b0cfe", "a57446a0bee01401297be3dbecd7e3b8"},
		{"\xff\x80é", "ff00800102030405060708090a0b0cfe", "2f7e4236d1fa5f11613338c958492428"},
	} {
		challenge, _ := hex.DecodeString(c.challenge)
		got, err := rfbauth.VNCAuthResponse(c.password, challenge)
		if err != nil || hex.EncodeToString(got) != c.want {
			t.Errorf("VNCAuthResponse(%q, %s) = %x, %v; want %s", c.password, c.challenge, got, err, c.want)
		}
	}
}
