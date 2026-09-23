// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package kek_test

import (
	"testing"

	"filippo.io/age"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek/kektest"
)

func TestLocalKEK_Conformance(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	kektest.Run(t, func(t *testing.T) kek.KEK {
		k, err := kek.NewLocal(id)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}, kektest.Hooks{})
}
