// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestSinkLapsedFeedsObligation: a lapsed frame records the lapse and the ProxyExited obligation,
// and a failure of one still records the other.
func TestSinkLapsedFeedsObligation(t *testing.T) {
	id := uuid.New()
	var noted uuid.UUID
	var exited string
	f := onSinkLapsed(func(r uuid.UUID) error { noted = r; return errors.New("disk") }, func(r uuid.UUID, ev string) error {
		if r == id {
			exited = ev
		}
		return nil
	})
	f(id, "deadline")
	if noted != id || exited != "lapsed:deadline" {
		t.Fatalf("noted %v exited %q", noted, exited)
	}
}
