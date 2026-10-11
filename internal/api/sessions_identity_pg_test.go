// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// countingTokenStore counts the whole-table token reads on top of the real store. It keeps the indexed
// per-identity read, which the embedded PG promotes.
type countingTokenStore struct {
	store.PG
	full atomic.Int32
}

func (s *countingTokenStore) ListAPITokens(ctx context.Context) ([]types.APIToken, error) {
	s.full.Add(1)
	return s.PG.ListAPITokens(ctx)
}

// scanOnlyTokenStore hides the indexed read, so a revocation takes the store-agnostic path.
type scanOnlyTokenStore struct{ store.Store }

func tokenNames(toks []types.APIToken) []string {
	var out []string
	for _, tk := range toks {
		out = append(out, tk.Name)
	}
	return out
}

// A per-person token revocation reads the person's own rows by index, never the whole table, and finds
// exactly the tokens the whole-table scan found: the name as a principal or as an email in any case,
// revoked rows listed but not revoked again, nothing that merely contains the name. The audit rows come
// in the scan's order (principal matches first, then email-only ones, each newest first).
func TestRevokeAPITokensMatching_IndexedReadFindsWhatTheScanFound(t *testing.T) {
	e := newSCIMEnv(t)
	ctx := context.Background()
	counting := &countingTokenStore{PG: e.st}
	cfg := e.a.srv.cfg
	cfg.Store = counting
	srv := New(cfg)

	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	n := 0
	mint := func(label, principal, email string) uuid.UUID {
		t.Helper()
		n++
		tok, err := e.st.CreateAPIToken(ctx, types.APIToken{ID: uuid.New(), Principal: principal, Email: email, Role: "user",
			UserType: "standard", Name: label, CreatedAt: base.Add(time.Duration(n) * time.Minute)}, "wdn_"+uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		return tok.ID
	}
	// Alice holds tokens under her IdP subject, under her email as a principal, and under a second subject
	// that only names her email, in another case. Bob and the lookalikes hold none of hers.
	mint("alice-sub-1", "sub-alice", "alice@corp.example")
	mint("alice-sub-2", "sub-alice", "")
	mint("alice-email-principal", "alice@corp.example", "")
	mint("alice-other-sub", "sub-alice-2", "ALICE@Corp.Example")
	retired := mint("alice-already-revoked", "alice@corp.example", "alice@corp.example")
	if _, err := e.st.RevokeAPIToken(ctx, retired, "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	mint("bob", "sub-bob", "bob@corp.example")
	mint("lookalike-prefix", "sub-malice", "malice@corp.example")
	mint("lookalike-suffix", "sub-alice-evil", "alice@corp.example.evil")
	mint("lookalike-substring", "sub-alice-3", "x-alice@corp.example")

	for _, name := range []string{"sub-alice", "alice@corp.example", "ALICE@CORP.EXAMPLE", "sub-alice-2", "sub-bob", "nobody@corp.example"} {
		want, err := srv.listAPITokensByIdentityScan(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		got, err := counting.ListAPITokensByIdentity(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(tokenNames(got), tokenNames(want)) {
			t.Errorf("%q: indexed read = %v, whole-table scan = %v", name, tokenNames(got), tokenNames(want))
		}
	}

	counting.full.Store(0)
	revoked, err := srv.revokeAPITokensMatching(ctx, "alice@corp.example", nil)
	if err != nil {
		t.Fatal(err)
	}
	if revoked != 3 {
		t.Errorf("revoked %d tokens, want the 3 live ones that name alice@corp.example", revoked)
	}
	if c := counting.full.Load(); c != 0 {
		t.Errorf("a per-person revocation read the whole token table %d times, want 0", c)
	}
	all, err := e.st.ListAPITokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, tk := range all {
		names[tk.ID.String()] = tk.Name
		wantLive := !slices.Contains([]string{"alice-email-principal", "alice-other-sub", "alice-sub-1", "alice-already-revoked"}, tk.Name)
		if (tk.RevokedAt == nil) != wantLive {
			t.Errorf("%s: live = %v after the revocation, want %v", tk.Name, tk.RevokedAt == nil, wantLive)
		}
	}
	var audited []string
	for _, ev := range e.a.h.audit.snapshot() {
		if ev.Action == "token.revoke" {
			audited = append(audited, names[ev.Target])
		}
	}
	if want := []string{"alice-email-principal", "alice-other-sub", "alice-sub-1"}; !slices.Equal(audited, want) {
		t.Errorf("token.revoke audit rows = %v, want %v", audited, want)
	}

	// The whole-deployment arm still reads everything, and a match filter still narrows it.
	counting.full.Store(0)
	if _, err := srv.revokeAPITokensMatching(ctx, "", func(tk types.APIToken) bool { return tk.Name == "bob" }); err != nil {
		t.Fatal(err)
	}
	if counting.full.Load() != 1 {
		t.Errorf("the revoke-all arm read the table %d times, want 1", counting.full.Load())
	}

	// A store with no indexed read takes the scan and revokes the other form of the same person.
	scanCfg := cfg
	scanCfg.Store = scanOnlyTokenStore{e.st}
	if n, err := New(scanCfg).revokeAPITokensMatching(ctx, "sub-alice", nil); err != nil || n != 1 {
		t.Errorf("scan path revoked %d (%v), want 1: alice-sub-2, the only live token under her subject", n, err)
	}
}
