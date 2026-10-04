// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package keydomain_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/keydomain"
)

// With Postgres unanswering, the audit-window and root-key counts fail as
// secretstore.ErrUnavailable, name the operation that failed, and return no
// count: a caller must not read a zero as "none".
func TestW4CovServiceCountsFailAsUnavailableWithNoCount(t *testing.T) {
	s := downService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	changes, err := s.ChangesSince(ctx, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	if !errors.Is(err, secretstore.ErrUnavailable) || !strings.Contains(err.Error(), "count the assignment changes") {
		t.Errorf("ChangesSince = %v, want ErrUnavailable naming the count", err)
	}
	if changes != (keydomain.Changes{}) {
		t.Errorf("ChangesSince returned %+v with its error, want the zero value", changes)
	}

	n, err := s.RootKeyCredentials(ctx)
	if !errors.Is(err, secretstore.ErrUnavailable) || !strings.Contains(err.Error(), "count the credentials under the credential key") {
		t.Errorf("RootKeyCredentials = %v, want ErrUnavailable naming the count", err)
	}
	if n != 0 {
		t.Errorf("RootKeyCredentials returned %d with its error, want 0", n)
	}
}

// A person's placement reads the login's groups first; a Postgres that does
// not answer there is an outage, never a placement in Default.
func TestW4CovServicePlaceFailsAsUnavailableWithNoPlacement(t *testing.T) {
	s := downService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	place, err := s.Place(ctx, "alice")
	if !errors.Is(err, secretstore.ErrUnavailable) || !strings.Contains(err.Error(), "read the login's groups") {
		t.Errorf("Place = %v, want ErrUnavailable naming the groups read", err)
	}
	if place != (keydomain.Placement{}) {
		t.Errorf("Place returned %+v with its error, want the zero value", place)
	}
}
