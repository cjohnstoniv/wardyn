// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package keydomain_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/keydomain"
)

// downService is a Service whose Postgres refuses every connection (pgxpool
// dials lazily, so it builds), declaring "vault-b" and "azure-a" in that order.
func downService(t *testing.T) *keydomain.Service {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://wardyn@127.0.0.1:1/wardyn?connect_timeout=1&sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return keydomain.NewService(pool, []string{"vault-b", "azure-a", "vault-b"})
}

func TestService_DeclaredIsSortedDeduplicatedAndNeverIncludesDefault(t *testing.T) {
	s := downService(t)
	if got, want := s.Declared(), []string{"azure-a", "vault-b"}; !slices.Equal(got, want) {
		t.Errorf("Declared = %v, want %v", got, want)
	}
	for domain, want := range map[string]bool{
		keydomain.Default: true, "vault-b": true, "azure-a": true, "vault-c": false, "": false,
	} {
		if got := s.Has(domain); got != want {
			t.Errorf("Has(%q) = %v, want %v", domain, got, want)
		}
	}
}

// An unknown domain is refused on the Set before Postgres is asked: the file's
// declaration is the only authority, and an outage must not blur it.
func TestService_SetRefusesAnUndeclaredDomainWithoutAskingPostgres(t *testing.T) {
	_, err := downService(t).Set(context.Background(), keydomain.Assignment{
		SubjectType: keydomain.SubjectUser, Subject: "alice", Domain: "vault-c", SetBy: "admin",
	})
	if !errors.Is(err, keydomain.ErrUnknownDomain) || errors.Is(err, secretstore.ErrUnavailable) {
		t.Errorf("Set = %v, want ErrUnknownDomain and not an outage", err)
	}
}

func TestService_RecordLoginGroupsOfNoOneIsANoOp(t *testing.T) {
	if err := downService(t).RecordLoginGroups(context.Background(), "", []string{"eng"}, false); err != nil {
		t.Errorf("RecordLoginGroups with no principal = %v, want nil (nothing to stamp, nothing asked)", err)
	}
}

// With Postgres unanswering, every read and write fails as secretstore.ErrUnavailable,
// which callers ride out, and never as a definitive refusal or a made-up answer.
func TestService_PostgresDownFailsEveryCallAsUnavailable(t *testing.T) {
	s := downService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for name, call := range map[string]func() error{
		"Domain":            func() error { _, err := s.Domain(ctx, "alice"); return err },
		"RecordLoginGroups": func() error { return s.RecordLoginGroups(ctx, "alice", nil, true) },
		"List":              func() error { _, err := s.List(ctx); return err },
		"Usage":             func() error { _, err := s.Usage(ctx); return err },
		"Set": func() error {
			_, err := s.Set(ctx, keydomain.Assignment{SubjectType: keydomain.SubjectAll, Domain: "vault-b"})
			return err
		},
		"Get":                 func() error { _, _, err := s.Get(ctx, keydomain.SubjectUser, "alice"); return err },
		"Delete":              func() error { _, _, err := s.Delete(ctx, keydomain.SubjectUser, "alice"); return err },
		"AmbiguousIfGroup":    func() error { _, err := s.AmbiguousIfGroup(ctx, "eng", "vault-b"); return err },
		"TruncatedUnassigned": func() error { _, err := s.TruncatedUnassigned(ctx); return err },
	} {
		if err := call(); !errors.Is(err, secretstore.ErrUnavailable) {
			t.Errorf("%s = %v, want secretstore.ErrUnavailable", name, err)
		}
	}
}
