// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/vaultkv"
)

// TestPG_SweepRetiredModelCredentials: boot deletes every retired
// operator-lane model credential from every namespace — the operator's and
// each member's, in local sealing and in store mode (whose value lives in the
// organisation's store) — audits it once, keeps every other secret, and finds
// nothing on the next boot.
func TestPG_SweepRetiredModelCredentials(t *testing.T) {
	for _, mode := range []string{"local", "store"} {
		t.Run(mode, func(t *testing.T) {
			pool := envelopeDB(t)
			ext := &memExternal{vals: map[string][]byte{}}
			ageKey := mustAgeIdentity(t).String() // one key for both boots
			open := func(rec *capturingRecorder) secretstore.Store {
				t.Helper()
				clients, key, sel := storeClients{}, ageKey, ""
				if mode == "store" {
					clients, key, sel = storeClients{ext: ext}, "", vaultkv.Name
				}
				s, err := buildSecretStore(t.Context(), pool, key, nil, sel, clients, rec)
				if err != nil {
					t.Fatal(err)
				}
				return s
			}
			st := open(&capturingRecorder{})
			const member = "alice@example.com"
			seed := map[string][]string{
				"":     {"anthropic-api-key", "bedrock-api-key", "aws-access-key-id", "aws-secret-access-key", "wardyn-harness-anthropic-oauth", "npm-token"},
				member: {"anthropic-api-key", "openai-api-key", "wardyn-harness-aws-oauth", "wardyn-provider-uid-1-key"},
			}
			for owner, names := range seed {
				for _, name := range names {
					if err := st.For(owner).Put(t.Context(), name, []byte("synthetic-credential-value")); err != nil {
						t.Fatalf("seed %s/%s: %v", owner, name, err)
					}
				}
			}

			rec := &capturingRecorder{}
			if err := sweepRetiredModelCredentials(t.Context(), st, rec); err != nil {
				t.Fatalf("sweep: %v", err)
			}
			holders, err := st.Holders(t.Context(), []string{"anthropic-api-key", "openai-api-key", "bedrock-api-key",
				"aws-access-key-id", "aws-secret-access-key", "wardyn-harness-anthropic-oauth", "wardyn-harness-aws-oauth",
				"npm-token", "wardyn-provider-uid-1-key"})
			if err != nil {
				t.Fatal(err)
			}
			if len(holders) != 2 || !slices.Equal(holders["npm-token"], []string{""}) ||
				!slices.Equal(holders["wardyn-provider-uid-1-key"], []string{member}) {
				t.Fatalf("after the sweep holders = %v, want only npm-token (operator) and alice's own provider key", holders)
			}
			if mode == "store" {
				for k := range ext.vals {
					if k != "/npm-token" && k != member+"/wardyn-provider-uid-1-key" {
						t.Errorf("the organisation's store still holds %s", k)
					}
				}
			}
			var retire []map[string]any
			for _, ev := range rec.got {
				if ev.Action == "model_credential.retire" {
					var d map[string]any
					_ = json.Unmarshal(ev.Data, &d)
					retire = append(retire, d)
				}
			}
			if len(retire) != 1 || retire[0]["count"] != float64(8) {
				t.Fatalf("model_credential.retire rows = %v, want one row counting 8 deletions", retire)
			}

			again := &capturingRecorder{}
			if err := sweepRetiredModelCredentials(t.Context(), open(again), again); err != nil {
				t.Fatalf("second boot: %v", err)
			}
			for _, ev := range again.got {
				if ev.Action == "model_credential.retire" {
					t.Fatalf("the second boot audited a sweep of nothing: %s", ev.Data)
				}
			}
		})
	}
}
