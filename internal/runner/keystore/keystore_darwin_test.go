// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package keystore

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// A value whose keychain command would pass 4096 bytes is refused before
// go-keyring starts `security -i`, so no child process is left behind; the
// longest value that fits still reaches the keyring.
func TestSet_OversizeRefusedBeforeStart(t *testing.T) {
	prev := keyringSet
	t.Cleanup(func() { keyringSet = prev })
	reached := 0
	keyringSet = func(string, string, string) error { reached++; return nil }
	s := For(uuid.NewString())
	account := uuid.NewString() + "/" + uuid.NewString()
	fits := 0
	for n := 1; macSetCommandLen(s.service, account, strings.Repeat("v", n)) <= macCommandLimit; n++ {
		fits = n
	}
	if fits < 2800 || fits > 3100 {
		t.Fatalf("longest value that fits = %d bytes, want about 3 KB", fits)
	}
	if err := s.Set(account, strings.Repeat("v", fits)); err != nil || reached != 1 {
		t.Fatalf("Set of the longest value that fits = %v (keyring reached %d times)", err, reached)
	}
	if err := s.Set(account, strings.Repeat("v", fits+3)); !errors.Is(err, ErrUnavailable) || reached != 1 {
		t.Fatalf("Set of an oversize value = %v (keyring reached %d times), want ErrUnavailable before the keyring", err, reached)
	}
	if err := s.Precheck(account, strings.Repeat("v", fits+3)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Precheck of an oversize value = %v, want ErrUnavailable", err)
	}
}
