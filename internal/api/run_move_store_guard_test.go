// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// TestMoveStoreReadersMatchPG: the store readers a Resume and a move are refused through keep one shape on
// store.PG, so an optional-interface assertion on the store never silently reads false. RunMoveInFlight is the
// move's own; RunLocallyContained arrives with the containment hold and is checked once it exists.
func TestMoveStoreReadersMatchPG(t *testing.T) {
	want := reflect.TypeFor[func(context.Context, uuid.UUID) (bool, error)]()
	pg := reflect.ValueOf(store.PG{})
	for _, tc := range []struct {
		name     string
		required bool
	}{{"RunMoveInFlight", true}, {"RunLocallyContained", false}} {
		m := pg.MethodByName(tc.name)
		switch {
		case !m.IsValid() && tc.required:
			t.Errorf("store.PG has no %s", tc.name)
		case m.IsValid() && m.Type() != want:
			t.Errorf("store.PG.%s is %s, want %s", tc.name, m.Type(), want)
		}
	}
}
