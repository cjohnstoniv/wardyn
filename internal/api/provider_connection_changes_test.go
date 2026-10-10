// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

func TestProviderCause_ReadFailureIsNotAbsence(t *testing.T) {
	for _, kind := range []types.ModelProviderKind{types.ModelProviderAnthropicAPIKey, types.ModelProviderAnthropicSubscription, types.ModelProviderBedrockSSO} {
		t.Run(string(kind), func(t *testing.T) {
			h, _ := newSecretsHarness(t)
			p := paKeyProvider("provider", kind)
			absent := h.srv.providerAccessFor(t.Context(), p, paOwner)
			if absent.Cause != "never_connected" {
				t.Fatalf("confirmed absence: %+v", absent)
			}
			h.srv.cfg.Secrets = wedgedSecrets{err: secretstore.ErrUnavailable}
			unreadable := h.srv.providerAccessFor(t.Context(), p, paOwner)
			if unreadable.Cause != "store_unreadable" || unreadable.Action != providerAccessRecheck {
				t.Fatalf("read failure: %+v", unreadable)
			}
		})
	}
}

func changedProviderFixture(t *testing.T) (*replicaPair, types.ModelProvider, types.ModelProvider) {
	t.Helper()
	pair := newReplicaPair(t)
	pair.a.cfg.Secrets = secretstore.Audited(pair.a.cfg.Secrets, pair.audit)
	pair.b.cfg.Secrets = secretstore.Audited(pair.b.cfg.Secrets, pair.audit)
	old := paKeyProvider("gateway", types.ModelProviderAnthropicAPIKey)
	old.BaseURL = "https://old.example/api"
	next := old
	next.BaseURL = "https://new.example/private/path"
	for _, owner := range []string{"person-a", "person-b"} {
		if err := pair.a.cfg.Secrets.For(owner).Put(t.Context(), providerCredentialName(old), []byte("old-credential-private-value")); err != nil {
			t.Fatal(err)
		}
	}
	return pair, old, next
}

func purgeChangedProvider(t *testing.T, s *Server, old, next types.ModelProvider) {
	t.Helper()
	_, err := s.purgeProviderCredentials(t.Context(), &types.ModelProviders{Providers: []types.ModelProvider{old}}, &types.ModelProviders{Providers: []types.ModelProvider{next}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestProviderCausePG_AddressChangeShowsCauseAndDestinationToEachAffectedOwner(t *testing.T) {
	pair, old, next := changedProviderFixture(t)
	purgeChangedProvider(t, pair.a, old, next)
	for _, owner := range []string{"person-a", "person-b"} {
		got := pair.b.providerAccessFor(t.Context(), next, owner)
		if got.Cause != "destination_changed" || got.NewDestination != "new.example" || got.ChangedAt == nil {
			t.Fatalf("%s: %+v", owner, got)
		}
		if _, found, err := pair.b.ownSecret(t.Context(), owner, providerCredentialName(old)); err != nil || found {
			t.Fatalf("purge: found=%v err=%v", found, err)
		}
	}
	if got := pair.b.providerAccessFor(t.Context(), next, "unaffected"); got.Cause != "never_connected" || got.ChangedAt != nil {
		t.Fatalf("unaffected: %+v", got)
	}
	next2 := next
	next2.BaseURL = "https://latest.example"
	purgeChangedProvider(t, pair.a, next, next2)
	if got := pair.b.providerAccessFor(t.Context(), next2, "person-a"); got.NewDestination != "latest.example" {
		t.Fatalf("second change: %+v", got)
	}
}

func TestProviderCausePG_ReconnectingClearsRecord(t *testing.T) {
	pair, old, next := changedProviderFixture(t)
	purgeChangedProvider(t, pair.a, old, next)
	for _, part := range providerSecretParts {
		t.Run(part, func(t *testing.T) {
			if part != providerKeyPart {
				if _, err := pair.poolA.Exec(t.Context(), `INSERT INTO provider_connection_changes SELECT 'person-a', provider_id, provider_uid, reason, changed_at, new_destination FROM provider_connection_changes WHERE owner='person-b'`); err != nil {
					t.Fatal(err)
				}
			}
			if err := pair.a.cfg.Secrets.For("person-a").Put(t.Context(), providerSecretName(next.UID, part), []byte("new-private-credential-value")); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := pair.poolA.QueryRow(t.Context(), `SELECT count(*) FROM provider_connection_changes WHERE owner='person-a'`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("history after reconnect: %d %v", count, err)
			}
		})
	}
	if got := pair.b.providerAccessFor(t.Context(), next, "person-b"); got.Cause != "destination_changed" {
		t.Fatalf("other owner: %+v", got)
	}
}

func TestProviderCausePG_RecreatedProviderHasNoHistory(t *testing.T) {
	pair, old, next := changedProviderFixture(t)
	purgeChangedProvider(t, pair.a, old, next)
	recreated := next
	recreated.UID = uuid.NewString()
	if got := pair.b.providerAccessFor(t.Context(), recreated, "person-a"); got.Cause != "never_connected" || got.ChangedAt != nil {
		t.Fatalf("new UID: %+v", got)
	}
	if _, err := pair.a.purgeProviderCredentials(t.Context(), &types.ModelProviders{Providers: []types.ModelProvider{next}}, nil); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pair.poolA.QueryRow(t.Context(), `SELECT count(*) FROM provider_connection_changes`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("deleted provider retained history: %d %v", count, err)
	}
}

func TestProviderCausePG_RecordContainsNoSecret(t *testing.T) {
	pair, old, next := changedProviderFixture(t)
	purgeChangedProvider(t, pair.a, old, next)
	var columns []string
	if err := pair.poolA.QueryRow(t.Context(), `SELECT array_agg(column_name::text ORDER BY column_name) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='provider_connection_changes'`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	want := []string{"changed_at", "new_destination", "owner", "provider_id", "provider_uid", "reason"}
	if !reflect.DeepEqual(columns, want) {
		t.Fatalf("record columns: %v", columns)
	}
	history := pair.a.cfg.Secrets.For("person-a").(secretstore.ProviderChangeStore)
	change, found, err := history.ProviderChange(t.Context(), next.UID)
	if err != nil || !found {
		t.Fatalf("record: %v %v", found, err)
	}
	raw, err := json.Marshal(change)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	keys := sortedKeys(fields)
	if !slices.Equal(keys, want) {
		t.Fatalf("record JSON keys: %v", keys)
	}
	if change.NewDestination != "new.example" {
		t.Fatalf("destination contains more than the approved host: %q", change.NewDestination)
	}
}

func TestProviderCausePG_RecordWrittenAtPurgeAndRolledBackWithIt(t *testing.T) {
	pair, old, next := changedProviderFixture(t)
	_, err := pair.poolA.Exec(t.Context(), `CREATE FUNCTION reject_test_purge() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'purge refused'; END $$;
 CREATE TRIGGER reject_test_purge BEFORE DELETE ON secrets FOR EACH ROW EXECUTE FUNCTION reject_test_purge()`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pair.a.purgeProviderCredentials(t.Context(), &types.ModelProviders{Providers: []types.ModelProvider{old}}, &types.ModelProviders{Providers: []types.ModelProvider{next}})
	if err == nil {
		t.Fatal("purge unexpectedly succeeded")
	}
	var count int
	if err := pair.poolA.QueryRow(t.Context(), `SELECT count(*) FROM provider_connection_changes`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rollback: %d %v", count, err)
	}
	if _, err := pair.poolA.Exec(t.Context(), `DROP TRIGGER reject_test_purge ON secrets`); err != nil {
		t.Fatal(err)
	}
	purgeChangedProvider(t, pair.a, old, next)
	if got := pair.b.providerAccessFor(t.Context(), next, "person-a"); got.Cause != "destination_changed" {
		t.Fatalf("successful purge: %+v", got)
	}
}

func TestProviderCausePG_KindChangeUsesSuccessorUID(t *testing.T) {
	pair, old, next := changedProviderFixture(t)
	next.Kind, next.UID = types.ModelProviderOpenAIAPIKey, uuid.NewString()
	purgeChangedProvider(t, pair.a, old, next)
	if got := pair.b.providerAccessFor(t.Context(), next, "person-a"); got.Cause != "kind_changed" {
		t.Fatalf("kind change: %+v", got)
	}
	if got := pair.b.providerAccessFor(t.Context(), old, "person-a"); got.ChangedAt != nil {
		t.Fatalf("old UID: %+v", got)
	}
}

func TestProviderCause_OldClientIgnoresOptionalFields(t *testing.T) {
	raw, err := json.Marshal(SetupProviderAccess{Provider: "gateway", State: modelAccessNotConfigured, Cause: "destination_changed", NewDestination: "new.example"})
	if err != nil {
		t.Fatal(err)
	}
	var old struct {
		Provider string `json:"provider"`
		State    string `json:"state"`
	}
	if err := json.Unmarshal(raw, &old); err != nil || old.State != modelAccessNotConfigured || old.Provider != "gateway" {
		t.Fatalf("old client: %+v %v", old, err)
	}
}

type unreadableHistory struct{ secretstore.Store }

func (s unreadableHistory) DeleteProviderChanges(context.Context) error { return nil }

func (s unreadableHistory) For(owner string) secretstore.Store {
	return unreadableHistory{s.Store.For(owner)}
}
func (s unreadableHistory) ProviderChange(context.Context, string) (secretstore.ProviderChange, bool, error) {
	return secretstore.ProviderChange{}, false, errors.New("history unavailable")
}

func TestProviderCause_HistoryReadFailureIsNotAbsence(t *testing.T) {
	h, sec := newSecretsHarness(t)
	h.srv.cfg.Secrets = unreadableHistory{sec}
	if got := h.srv.providerAccessFor(t.Context(), paKeyProvider("provider", types.ModelProviderAnthropicAPIKey), paOwner); got.Cause != "store_unreadable" {
		t.Fatalf("history read failure: %+v", got)
	}
}

func TestProviderCause_UnreadableStoredSubscription(t *testing.T) {
	h, sec := newSecretsHarness(t)
	p := paKeyProvider("subscription", types.ModelProviderAnthropicSubscription)
	if err := sec.For(paOwner).Put(t.Context(), providerCredentialName(p), []byte("invalid-json")); err != nil {
		t.Fatal(err)
	}
	if got := h.srv.providerAccessFor(t.Context(), p, paOwner); got.Cause != "store_unreadable" {
		t.Fatalf("decode failure: %+v", got)
	}
}

func TestProviderCausePG_UncommittedDestinationIsNotDisclosed(t *testing.T) {
	pair, old, next := changedProviderFixture(t)
	purgeChangedProvider(t, pair.a, old, next)
	got := pair.b.providerAccessFor(t.Context(), old, "person-a")
	if got.Cause != "" || got.NewDestination != "" || got.ChangedAt != nil {
		t.Fatalf("configuration still names the old destination: %+v", got)
	}
}

func TestProviderCause_PreviewDoesNotReadHistory(t *testing.T) {
	srv, _, sec, _ := memberPreviewSrv(t)
	srv.cfg.Secrets = unreadableHistory{sec}
	got := previewSetupStatus(t, srv, memberPreviewSession(t, true, true))
	if got.Cause != "never_connected" || got.ChangedAt != nil || got.NewDestination != "" {
		t.Fatalf("preview exposed history: %+v", got)
	}
}

func TestProviderCausePG_FailedKindSaveHistoryIsCleared(t *testing.T) {
	for _, action := range []string{"reconnect", "delete", "retry"} {
		t.Run(action, func(t *testing.T) {
			pair, old, next := changedProviderFixture(t)
			if _, err := pair.a.cfg.Store.PutSiteConfig(t.Context(), types.SiteConfig{ModelProviders: &types.ModelProviders{Providers: []types.ModelProvider{old}}}); err != nil {
				t.Fatal(err)
			}
			next.Kind, next.UID = types.ModelProviderOpenAIAPIKey, uuid.NewString()
			purgeChangedProvider(t, pair.a, old, next)
			want := 0
			switch action {
			case "reconnect":
				if err := pair.a.cfg.Secrets.For("person-a").Put(t.Context(), providerCredentialName(old), []byte("reconnected-to-unchanged-provider")); err != nil {
					t.Fatal(err)
				}
			case "delete":
				if _, err := pair.a.purgeProviderCredentials(t.Context(), &types.ModelProviders{Providers: []types.ModelProvider{old}}, nil); err != nil {
					t.Fatal(err)
				}
			case "retry":
				retry := next
				retry.UID = uuid.NewString()
				purgeChangedProvider(t, pair.a, old, retry)
				if got := pair.b.providerAccessFor(t.Context(), retry, "person-a"); got.Cause != "kind_changed" {
					t.Fatalf("retried change lost cause: %+v", got)
				}
				want = 1
			}
			var count int
			if err := pair.poolA.QueryRow(t.Context(), `SELECT count(*) FROM provider_connection_changes WHERE owner='person-a'`).Scan(&count); err != nil || count != want {
				t.Fatalf("retained history: count=%d want=%d err=%v", count, want, err)
			}
		})
	}
}
