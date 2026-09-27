// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestCredentialInventoryFill_AddsEmailAndProviderName pins design F-2: the
// server adds a row's email and provider name alongside its bare subject and
// provider id. RED with either projection dropped from fill.
func TestCredentialInventoryFill_AddsEmailAndProviderName(t *testing.T) {
	now := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	providerOf := map[string]string{"cred-key": "corp-gw"}
	providerNames := map[string]string{"corp-gw": "Corp gateway"}
	emailOf := map[string]string{"sub-alice": "alice@corp.example"}
	metas := []secretstore.Meta{{Name: "cred-key", Owner: "sub-alice", Store: "pg", AddedAt: now}}

	inv := credentialInventory{Counts: credentialInventoryCounts{ByProvider: map[string]int{"corp-gw": 0}}}
	inv.fill(metas, providerOf, providerNames, emailOf, now)

	if len(inv.Credentials) != 1 {
		t.Fatalf("credentials = %+v, want exactly 1", inv.Credentials)
	}
	row := inv.Credentials[0]
	if row.Person != "sub-alice" || row.Email != "alice@corp.example" ||
		row.Provider != "corp-gw" || row.ProviderName != "Corp gateway" {
		t.Fatalf("row = %+v, want person=sub-alice email=alice@corp.example provider=corp-gw provider_name=Corp gateway", row)
	}

	// Neither is secret, but neither is invented either: a subject with no
	// paired email, or a provider this deployment never named, gets none —
	// omitempty drops it from the wire rather than shipping "".
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"email":"alice@corp.example"`) || !strings.Contains(string(raw), `"provider_name":"Corp gateway"`) {
		t.Fatalf("wire = %s, want email and provider_name on the wire", raw)
	}
}

// TestCredentialInventoryFill_NoPairedEmailOmitsField: a principal with no
// paired email (e.g. known only through workspaces.owned_by) gets no email
// invented — never falls back to the bare subject.
func TestCredentialInventoryFill_NoPairedEmailOmitsField(t *testing.T) {
	now := time.Now()
	providerOf := map[string]string{"cred-key": "corp-gw"}
	providerNames := map[string]string{"corp-gw": "Corp gateway"}
	metas := []secretstore.Meta{{Name: "cred-key", Owner: "sub-bob", Store: "pg", AddedAt: now}}

	inv := credentialInventory{Counts: credentialInventoryCounts{ByProvider: map[string]int{"corp-gw": 0}}}
	inv.fill(metas, providerOf, providerNames, map[string]string{}, now)

	if len(inv.Credentials) != 1 || inv.Credentials[0].Email != "" {
		t.Fatalf("credentials = %+v, want one row with no email", inv.Credentials)
	}
	raw, err := json.Marshal(inv.Credentials[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"email"`) {
		t.Fatalf("wire = %s, want no email key at all (omitempty)", raw)
	}
}

// TestEmailsByPrincipal_FirstPairingWins mirrors resolveSecretOwner's OWN
// directory (knownPrincipals): API tokens pair a principal with an email,
// workspaces.owned_by names a principal with none. A principal named by
// both keeps whichever email it's first paired with; one named only by a
// workspace gets none.
func TestEmailsByPrincipal_FirstPairingWins(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Store = secretOwnerDirectory{toks: []types.APIToken{
		{Principal: "sub-alice", Email: "alice@corp.example"},
	}}
	got := h.srv.emailsByPrincipal(context.Background())
	if got["sub-alice"] != "alice@corp.example" {
		t.Fatalf("emailsByPrincipal = %+v, want sub-alice paired", got)
	}
	if _, ok := got["sub-bob"]; ok {
		t.Fatalf("emailsByPrincipal invented an email for an unknown principal: %+v", got)
	}
}

// TestCredInventoryNoMeta_Canon pins the packet-F canon 503 sentence — was
// DRAFT and lower-case; a reader must never see either the old wording or
// see it regress.
func TestCredInventoryNoMeta_Canon(t *testing.T) {
	const want = "This deployment's secret store keeps no credential metadata, so there is nothing to list."
	if credInventoryNoMeta != want {
		t.Fatalf("credInventoryNoMeta = %q, want %q", credInventoryNoMeta, want)
	}
}
