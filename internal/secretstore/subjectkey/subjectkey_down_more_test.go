// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package subjectkey

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/keydomain"
)

func TestResealAndVerifyOnAPostgresThatDoesNotAnswerAreOutages(t *testing.T) {
	m := downManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	aad := func(int) []byte { return []byte("aad") }

	// Reseal asks Current first; nothing is opened or sealed without a generation.
	if v, sealed, err := m.Reseal(ctx, "alice", PurposeCred, 1, []byte("sealed"), []byte("aad"), aad); !errors.Is(err, secretstore.ErrUnavailable) || v != 0 || sealed != nil {
		t.Errorf("Reseal = %d, %v, %v; want ErrUnavailable and nothing sealed", v, sealed, err)
	}
	if _, _, err := m.Reseal(ctx, "", PurposeCred, 1, nil, nil, aad); !errors.Is(err, ErrOperatorOwner) {
		t.Errorf("Reseal with no owner = %v, want ErrOperatorOwner", err)
	}

	pool, err := pgxpool.New(ctx, "postgres://wardyn@127.0.0.1:1/wardyn?connect_timeout=1&sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	reach := func(string, string) (kek.KEK, error) {
		t.Error("reach was called with no rows to check")
		return nil, nil
	}
	if err := Verify(ctx, pool, []string{"vault-a"}, reach); !errors.Is(err, secretstore.ErrUnavailable) {
		t.Errorf("Verify = %v, want ErrUnavailable: the boot check must not pass on a database it could not read", err)
	}
}

// The owner's domain is decided before any row is read, and a refusal from the
// domain resolver (ambiguous membership, an undeclared domain) is definitive: it
// is wrapped with the owner, keeps its identity, and is never an outage.
func TestCurrentRefusesWhenTheOwnersDomainCannotBeDecided(t *testing.T) {
	m := downManager(t)
	for _, cause := range []error{keydomain.ErrAmbiguous, keydomain.ErrUnknownDomain, keydomain.ErrGroupsTruncated} {
		m.keks.Domain = func(context.Context, string) (string, error) { return "", cause }
		_, _, err := m.Current(context.Background(), "alice", PurposeCred)
		if !errors.Is(err, cause) || errors.Is(err, secretstore.ErrUnavailable) {
			t.Errorf("Current with domain error %v = %v, want that error, not an outage", cause, err)
		}
	}
}

func TestCurrentAsksTheOwnersDomainBeforeReadingTheGeneration(t *testing.T) {
	m := downManager(t)
	var asked string
	m.keks.Domain = func(_ context.Context, owner string) (string, error) { asked = owner; return DomainDefault, nil }
	_, _, err := m.Current(context.Background(), "alice", PurposeAuditSeal)
	if asked != "alice" {
		t.Errorf("the domain resolver was asked about %q, want alice", asked)
	}
	if !errors.Is(err, secretstore.ErrUnavailable) {
		t.Errorf("Current = %v, want the generation read to fail as an outage", err)
	}
}
