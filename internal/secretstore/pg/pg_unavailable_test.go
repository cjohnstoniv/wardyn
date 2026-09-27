// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"context"
	"errors"
	"testing"

	"filippo.io/age"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// A database that does not answer is transient (ErrUnavailable), like an
// external store's outage: the injection sink answers it with the 503 a
// running run rides out on its last-good value, never with the definitive
// refusal that stops the run's credential at once (design K8).
func TestGetAndList_ADatabaseThatDoesNotAnswerIsTransient(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, "postgres://wardyn@127.0.0.1:1/wardyn?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(pool, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "anthropic-api-key"); !errors.Is(err, secretstore.ErrUnavailable) || errors.Is(err, secretstore.ErrNotFound) {
		t.Errorf("Get with the database down = %v, want ErrUnavailable and not ErrNotFound", err)
	}
	if _, err := s.For("alice").List(ctx); !errors.Is(err, secretstore.ErrUnavailable) {
		t.Errorf("List with the database down = %v, want ErrUnavailable", err)
	}
}
