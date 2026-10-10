// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

func TestLocalDispatchMetadataClosedWorld(t *testing.T) {
	if got := localDispatchUnclassified(); len(got) != 0 {
		t.Fatalf("unclassified authoring metadata: %v", got)
	}
	type child struct{ secret string }
	type parent struct{ captures []child }
	_, _ = child{}.secret, parent{}.captures
	schema := map[reflect.Type][]string{reflect.TypeFor[parent](): {"captures"}, reflect.TypeFor[child](): {}}
	if got := placement.SchemaUnclassified(reflect.TypeFor[parent](), "plan", schema); !slices.Equal(got, []string{"plan.captures.secret"}) {
		t.Fatalf("empty private child escaped schema: %v", got)
	}
}

func TestLocalLateGrantWriterForcesOwnerOnlyBeforeWrite(t *testing.T) {
	f := newComponentFixture(t)
	run := types.AgentRun{ID: uuid.New(), CreatedBy: capSub, Placement: types.PlacementLocal}
	g := types.CredentialGrant{ID: uuid.New(), RunID: run.ID, Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"host":"api.example","secret_name":"person-secret"}`)}}
	if _, err := f.srv.createDispatchGrant(context.Background(), run, g, false); err != nil {
		t.Fatal(err)
	}
	if len(f.st.grants) != 1 || !f.st.grants[0].Spec.OwnerOnly || f.st.grants[0].Delivery != types.GrantDeliveryOwn {
		t.Fatalf("late writer did not bind own delivery: %+v", f.st.grants)
	}
	if _, err := f.srv.createDispatchGrant(context.Background(), run, g, true); err == nil || len(f.st.grants) != 1 {
		t.Fatal("operator redirect became own through colliding namespace")
	}
	g.Spec.Scope = json.RawMessage(`{"host":"api.example","secret_name":"operator-only-secret"}`)
	if _, err := f.srv.createDispatchGrant(context.Background(), run, g, false); err == nil || len(f.st.grants) != 1 {
		t.Fatal("late grant allowed operator fallback")
	}
}

func TestLocalCapturedSnapshotRefusesUnknownOrForeignOwner(t *testing.T) {
	p := types.ModelProvider{ID: "key", UID: "uid", Kind: types.ModelProviderOpenAIAPIKey}
	sc := types.SiteConfig{ModelProviders: &types.ModelProviders{Providers: []types.ModelProvider{p}}}
	name := providerSecretName(p.UID, providerKeyPart)
	if _, err := localCapturedSecretName(types.SiteConfig{}, "owner", name, json.RawMessage(`{"provider_uid":"uid","owner_subject":"owner"}`)); err == nil {
		t.Fatal("absent provider configuration accepted capture")
	}
	for _, raw := range []string{
		`{"provider_uid":"uid","owner_subject":"other"}`,
		`{"provider_uid":"uid","owner_subject":"owner","new_secret":"hidden"}`,
		`{"provider_uid":"uid","owner_subject":"owner"} {}`,
		`null`,
	} {
		if _, err := localCapturedSecretName(sc, "owner", name, json.RawMessage(raw)); err == nil {
			t.Fatalf("capture allowed %s", raw)
		}
	}
	if got, err := localCapturedSecretName(sc, "owner", name, json.RawMessage(`{"provider_uid":"uid","owner_subject":"owner"}`)); err != nil || got != name {
		t.Fatalf("matching capture=%q %v", got, err)
	}
}
