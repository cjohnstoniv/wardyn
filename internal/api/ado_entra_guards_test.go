// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// The stored Azure DevOps sign-in is per person and per provider row. Each door
// below refuses before it reads or writes anything when it has no store, no
// person, or a row id that could be spelled into another row's name.
func TestADOEntraStore_DoorsRefuseWithoutAnOwnerARowOrAStore(t *testing.T) {
	ctx := context.Background()
	bare := &Server{cfg: Config{}}
	withStore := &Server{cfg: Config{Secrets: &memSecrets{m: map[string][]byte{}}}}

	// No store, or no owner: nothing is stored for anyone, and the read is not an error.
	for name, s := range map[string]*Server{"no store": bare, "no owner": withStore} {
		owner := ""
		if name == "no store" {
			owner = "alice"
		}
		if blob, found, err := s.readADOEntraBlob(ctx, owner, "row-1"); found || err != nil || blob.valid() {
			t.Errorf("%s: read = %+v, %v, %v; want nothing found and no error", name, blob, found, err)
		}
		if rev, guarded, err := s.entraRevision(ctx, owner, adoCapture(ADOEntraConfig{RowID: "row-1"})); rev != "" || guarded || err != nil {
			t.Errorf("%s: revision = %q, %v, %v; want none", name, rev, guarded, err)
		}
	}

	// A row id that could compose another row's secret name is refused by name.
	for _, id := range []string{"", "has space", "x-oauth", "wardyn-harness-x", "UPPER_case!"} {
		if adoEntraValidRowID(id) {
			t.Errorf("row id %q was accepted", id)
		}
		if _, _, err := withStore.readADOEntraBlob(ctx, "alice", id); err == nil || !strings.Contains(err.Error(), "not a usable store name") {
			t.Errorf("read with row id %q = %v, want a refusal naming the row id", id, err)
		}
		if err := withStore.storeADOEntraBlob(ctx, "alice", id, adoEntraBlob{}); err == nil || !strings.Contains(err.Error(), "not a usable store name") {
			t.Errorf("store with row id %q = %v, want a refusal naming the row id", id, err)
		}
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("adoEntraSecretName(%q) composed a name instead of panicking", id)
				}
			}()
			adoEntraSecretName(id)
		}()
	}

	for name, err := range map[string]error{
		"store, no store":       bare.storeADOEntraBlob(ctx, "alice", "row-1", adoEntraBlob{}),
		"store, no owner":       withStore.storeADOEntraBlob(ctx, "", "row-1", adoEntraBlob{}),
		"entra store, no store": bare.storeEntraBlob(ctx, "alice", adoCapture(ADOEntraConfig{RowID: "row-1"}), adoEntraBlob{}),
		"entra store, no owner": withStore.storeEntraBlob(ctx, "", adoCapture(ADOEntraConfig{RowID: "row-1"}), adoEntraBlob{}),
	} {
		if err == nil {
			t.Errorf("%s: wrote a sign-in with nowhere or no one to put it", name)
		}
	}
	if got := withStore.cfg.Secrets.(*memSecrets); len(got.owned) != 0 || len(got.m) != 0 {
		t.Errorf("a refused store wrote: %v %v", got.owned, got.m)
	}
}

func TestADOEntraStore_AStoredBlobThatIsNotASignInIsNotOne(t *testing.T) {
	ctx := context.Background()
	sec := &memSecrets{m: map[string][]byte{}}
	s := &Server{cfg: Config{Secrets: sec}}
	name := adoEntraSecretName("row-1")

	if err := sec.For("alice").Put(ctx, name, []byte(`{not json`)); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.readADOEntraBlob(ctx, "alice", "row-1"); found || err == nil || !strings.Contains(err.Error(), "parse") {
		t.Errorf("malformed blob: found %v, err %v; want a parse error and no sign-in", found, err)
	}

	// Valid JSON that lacks what a sign-in needs reads as "nothing captured", never as a half credential.
	if err := sec.For("alice").Put(ctx, name, []byte(`{"refresh_token":"r"}`)); err != nil {
		t.Fatal(err)
	}
	if blob, found, err := s.readADOEntraBlob(ctx, "alice", "row-1"); found || err != nil || blob.RefreshToken != "" {
		t.Errorf("incomplete blob: %+v, %v, %v; want nothing found", blob, found, err)
	}
}

func TestADOEntraClassify(t *testing.T) {
	if got := ADOEntraClassify(nil); got != ADOEntraFailureNone {
		t.Errorf("ADOEntraClassify(nil) = %q, want none", got)
	}
	for want, err := range map[ADOEntraFailure]error{
		ADOEntraFailureNotCaptured:  ErrADOEntraNotCaptured,
		ADOEntraFailureStoreRefused: fmt.Errorf("wrapped: %w", ErrADOEntraStoreRefused),
		ADOEntraFailureUnavailable:  errors.New("something this lane does not know"),
	} {
		if got := ADOEntraClassify(err); got != want {
			t.Errorf("ADOEntraClassify(%v) = %q, want %q", err, got, want)
		}
	}
}

func TestADOEntraRedeemable_RefusesBeforeTheLockIsTaken(t *testing.T) {
	good := ADOEntraConfig{
		RowID: "row-1", TenantID: "tenant-1", ClientID: "client-1", RedirectURL: "https://console.example.invalid/cb",
		Scopes: []string{"499b84ac-1321-427f-aa17-267ca6975798/vso.code"},
	}
	scopes := good.Scopes
	if err := adoEntraRedeemable(ADOEntraConfig{}, "alice", scopes); err == nil {
		t.Error("an empty provider row was redeemable")
	}
	if err := adoEntraRedeemable(good, "", scopes); !errors.Is(err, ErrADOEntraNotCaptured) {
		t.Errorf("no owner = %v, want ErrADOEntraNotCaptured", err)
	}

	many := make([]string, adoEntraMaxScopes+1)
	for i := range many {
		many[i] = fmt.Sprintf("499b84ac-1321-427f-aa17-267ca6975798/s%d", i)
	}
	for name, c := range map[string]struct {
		scopes []string
		want   string
	}{
		"none named":      {nil, "must name the scopes"},
		"too many":        {many, "at most"},
		"a blank scope":   {[]string{""}, "not a single scope"},
		"a spaced scope":  {[]string{"a b"}, "not a single scope"},
		"the app ceiling": {[]string{"499b84ac-1321-427f-aa17-267ca6975798/.default"}, "every permission"},
		"a bare default":  {[]string{".default"}, "every permission"},
	} {
		if err := adoEntraCheckRequestedScopes(c.scopes); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want a refusal containing %q", name, err, c.want)
		}
	}
	if err := adoEntraCheckRequestedScopes(scopes); err != nil {
		t.Errorf("a single named scope = %v, want accepted", err)
	}
}
