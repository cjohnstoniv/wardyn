// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package azurekv

// The live acceptance case (credential-storage design §2.3a.10): a real Key
// Vault. WARDYN_TEST_AZURE_KV is the vault's URL; the identity comes from the
// same WARDYN_AZURE_* settings wardynd reads (workload identity on AKS, or
// WARDYN_AZURE_AUTH=managed-identity on a VM), holding Key Vault Secrets
// Officer on a vault dedicated to the test. With WARDYN_TEST_PG set it also
// runs the conformance suite through the pg store in store mode.

import (
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/secretstoretest"
)

func TestLive_AzureKeyVault(t *testing.T) {
	kvURL := os.Getenv("WARDYN_TEST_AZURE_KV")
	if kvURL == "" {
		t.Skip("WARDYN_TEST_AZURE_KV not set; skipping the live Key Vault case")
	}
	auth := os.Getenv("WARDYN_AZURE_AUTH")
	if auth == "" {
		auth = AuthWorkloadIdentity
	}
	tokenFile := os.Getenv("WARDYN_AZURE_FEDERATED_TOKEN_FILE")
	if tokenFile == "" {
		tokenFile = os.Getenv("AZURE_FEDERATED_TOKEN_FILE")
	}
	s, err := New(t.Context(), Config{
		VaultURL: kvURL, Auth: auth, TenantID: os.Getenv("WARDYN_AZURE_TENANT_ID"), ClientID: os.Getenv("WARDYN_AZURE_CLIENT_ID"),
		FederatedTokenFile: tokenFile, AuthorityHost: os.Getenv("WARDYN_AZURE_AUTHORITY_HOST"),
		Prefix: "wardyn-live-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8], MaxVersions: 2, Purge: PurgeAuto,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	owner := "live-" + uuid.NewString()

	ref := ""
	for _, v := range []string{"one", "two", "three"} { // the third rolls the generation
		next, err := s.Put(ctx, owner, "pat", ref, []byte(v), false)
		if err != nil {
			t.Fatalf("Put %s: %v", v, err)
		}
		if ref != "" && secretstore.RefObject(next) != secretstore.RefObject(ref) {
			if err := s.Delete(ctx, owner, "pat", ref); err != nil { // what the pg store does after a rollover
				t.Fatalf("delete the old generation: %v", err)
			}
		}
		ref = next
	}
	if v, err := s.Get(ctx, owner, "pat", ref); err != nil || string(v) != "three" {
		t.Fatalf("Get = (%q, %v)", v, err)
	}
	if err := s.Check(ctx, owner, "pat", ref); err != nil {
		t.Fatalf("Check = %v", err)
	}
	two, err := s.Put(ctx, owner, "pat", ref, []byte("four"), false)
	if err != nil {
		t.Fatal(err)
	}
	sn, _, _, _ := s.parse(owner, "pat", two)
	all, err := s.versions(ctx, sn)
	if err != nil {
		t.Fatal(err)
	}
	enabled := 0
	for _, v := range all {
		if v.enabled() {
			enabled++
		}
	}
	if len(all) != 2 || enabled != 1 {
		t.Fatalf("versions: %d, %d enabled; want 2, 1", len(all), enabled)
	}
	found := false
	entries, err := s.Walk(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		found = found || (e.Owner == owner && e.Name == "pat")
	}
	if !found {
		t.Fatal("Walk did not list the live secret")
	}

	dctx, rep := secretstore.WithDeleteReport(ctx)
	if err := s.Delete(dctx, owner, "pat", two); err != nil {
		t.Fatal(err)
	}
	t.Logf("delete report: %+v", *rep)
	// Added again within the retention, on the same name: purged and reused,
	// or (purge withheld) a deleted secret holds it and a new name is taken.
	again, err := s.Put(ctx, owner, "pat", s.ref(sn, 1), []byte("back"), false)
	if err != nil {
		t.Fatalf("Put over a deleted name: %v", err)
	}
	if v, err := s.Get(ctx, owner, "pat", again); err != nil || string(v) != "back" {
		t.Fatalf("Get after re-adding = (%q, %v)", v, err)
	}
	if err := s.Delete(ctx, owner, "pat", again); err != nil {
		t.Fatal(err)
	}

	if os.Getenv("WARDYN_TEST_PG") != "" {
		pool := throwawayDB(t)
		secretstoretest.RunConformance(t, func(t *testing.T) secretstore.Store { return storeMode(t, pool, s, nil) })
	}
}
