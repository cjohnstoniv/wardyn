// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/vaultkv"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// adoRetireSite is a site config with an Azure DevOps Server row and a legacy
// visualstudio.com row, whose hosts the sweep must name as well as the
// well-known Services ones.
func adoRetireSite() types.SiteConfig {
	return types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{
		{ID: "ados", Kind: types.GitProviderAzureDevOps, Disabled: true, BaseURLs: []string{"https://tfs.corp.example/acme"}},
		{ID: "adovs", Kind: types.GitProviderAzureDevOps, Disabled: true, BaseURLs: []string{"https://acme.visualstudio.com"}},
	}}}
}

// adoRetireRows are the audit rows the sweep wrote, by namespace.
func adoRetireRows(t *testing.T, rec *capturingRecorder) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, ev := range rec.got {
		if ev.Action != "ado_shared_credential.retire" {
			continue
		}
		var d map[string]any
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		ns := d["namespace"].(string)
		if _, dup := out[ns]; dup {
			t.Errorf("namespace %s audited twice", ns)
		}
		out[ns] = d
	}
	return out
}

// TestPG_SweepRetiredADOSharedCredentials: boot deletes every shared Azure
// DevOps token, SSH key and known-hosts secret from every namespace — the
// operator's and each person's (a person could store their own copy, and the
// git broker reads the owner's row first), in local sealing and in store mode —
// audits once per namespace that held any, keeps every other secret (another
// forge's, and Wardyn's own sealed Azure DevOps names), and finds nothing on
// the next boot. After it, the operator fallback has no Azure DevOps token left
// to serve to anyone.
func TestPG_SweepRetiredADOSharedCredentials(t *testing.T) {
	for _, mode := range []string{"local", "store"} {
		t.Run(mode, func(t *testing.T) {
			pool := envelopeDB(t)
			ext := &memExternal{vals: map[string][]byte{}}
			ageKey := mustAgeIdentity(t).String()
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
			const alice, bob, carol = "alice@example.com", "bob@example.com", "carol@example.com"
			seed := map[string][]string{
				"": {"git-pat-dev-azure-com", "ssh-key-ssh-dev-azure-com", "known-hosts-ssh-dev-azure-com",
					"git-pat-tfs-corp-example", "git-pat-acme-visualstudio-com",
					"git-pat-github-com", "npm-token", "wardyn-harness-ado-ado-oauth"},
				alice: {"git-pat-dev-azure-com", "ssh-key-vs-ssh-visualstudio-com", "git-pat-github-com", "wardyn-harness-ado-ado-oauth"},
				bob:   {"git-pat-tfs-corp-example"},
				carol: {"git-pat-github-com"},
			}
			for owner, names := range seed {
				for _, name := range names {
					if err := st.For(owner).Put(t.Context(), name, []byte("synthetic-credential-value")); err != nil {
						t.Fatalf("seed %s/%s: %v", owner, name, err)
					}
				}
			}

			rec := &capturingRecorder{}
			if err := sweepRetiredADOSharedCredentials(t.Context(), st, adoRetireSite(), rec); err != nil {
				t.Fatalf("sweep: %v", err)
			}
			kept := []string{"git-pat-github-com", "npm-token", "wardyn-harness-ado-ado-oauth"}
			var all []string
			for _, names := range seed {
				all = append(all, names...)
			}
			holders, err := st.Holders(t.Context(), all)
			if err != nil {
				t.Fatal(err)
			}
			for name, owners := range holders {
				if !slices.Contains(kept, name) {
					t.Errorf("%s is still held by %q after the sweep", name, owners)
				}
			}
			if !slices.Equal(holders["git-pat-github-com"], []string{"", alice, carol}) ||
				!slices.Equal(holders["wardyn-harness-ado-ado-oauth"], []string{"", alice}) ||
				!slices.Equal(holders["npm-token"], []string{""}) {
				t.Fatalf("holders after the sweep = %v: GitHub's token, Wardyn's own Azure DevOps grant and npm-token must survive", holders)
			}
			// No fallback is left to serve an Azure DevOps host: not to the person
			// who had no copy of their own, and not to the one who did.
			for _, owner := range []string{alice, bob, carol} {
				if _, err := st.For(owner).Get(secretstore.WithPurpose(t.Context(), secretstore.PurposeBrokerMint), "git-pat-dev-azure-com"); !errors.Is(err, secretstore.ErrNotFound) {
					t.Errorf("%s reading git-pat-dev-azure-com after the sweep = %v, want not found (no operator fallback)", owner, err)
				}
			}
			if mode == "store" {
				for k := range ext.vals {
					switch k {
					case "/git-pat-github-com", "/npm-token", "/wardyn-harness-ado-ado-oauth",
						alice + "/git-pat-github-com", alice + "/wardyn-harness-ado-ado-oauth", carol + "/git-pat-github-com":
					default:
						t.Errorf("the organisation's store still holds %s", k)
					}
				}
			}

			rows := adoRetireRows(t, rec)
			if len(rows) != 3 {
				t.Fatalf("audit rows by namespace = %v, want one each for operator, alice and bob (carol held none)", rows)
			}
			for ns, want := range map[string]struct {
				count float64
				names []string
			}{
				"operator": {5, []string{"git-pat-acme-visualstudio-com", "git-pat-dev-azure-com", "git-pat-tfs-corp-example", "known-hosts-ssh-dev-azure-com", "ssh-key-ssh-dev-azure-com"}},
				alice:      {2, []string{"git-pat-dev-azure-com", "ssh-key-vs-ssh-visualstudio-com"}},
				bob:        {1, []string{"git-pat-tfs-corp-example"}},
			} {
				got := rows[ns]
				if got == nil || got["count"] != want.count {
					t.Fatalf("audit row for %s = %v, want count %v", ns, got, want.count)
				}
				var names []string
				for _, n := range got["names"].([]any) {
					names = append(names, n.(string))
				}
				if !slices.Equal(names, want.names) {
					t.Errorf("audit row for %s names %v, want %v", ns, names, want.names)
				}
			}

			again := &capturingRecorder{}
			if err := sweepRetiredADOSharedCredentials(t.Context(), open(again), adoRetireSite(), again); err != nil {
				t.Fatalf("second boot: %v", err)
			}
			if rows := adoRetireRows(t, again); len(rows) != 0 {
				t.Fatalf("the second boot audited a sweep of nothing: %v", rows)
			}
		})
	}
}

// TestPG_OpenSecretStoreSweepsRetiredADOSharedCredentials pins the WIRING: the
// store a serving boot opens has already deleted the shared Azure DevOps
// credentials — including a Server row's, named by the site config's own rows —
// from every namespace, and written the audit rows, by the time boot reads a
// secret.
func TestPG_OpenSecretStoreSweepsRetiredADOSharedCredentials(t *testing.T) {
	pool := envelopeDB(t)
	ageKey := mustAgeIdentity(t).String()
	seedStore, err := buildSecretStore(t.Context(), pool, ageKey, nil, "", storeClients{}, &capturingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO site_config (config) VALUES ($1)`,
		`{"workspace_providers":{"git":[{"id":"ados","kind":"azure_devops","disabled":true,"base_urls":["https://tfs.corp.example/acme"],"lanes":["pat"],"credential_source":"per_user"}]}}`); err != nil {
		t.Fatal(err)
	}
	for owner, name := range map[string]string{"": "git-pat-dev-azure-com", "alice@example.com": "git-pat-tfs-corp-example"} {
		if err := seedStore.For(owner).Put(t.Context(), name, []byte("synthetic-credential-value")); err != nil {
			t.Fatalf("seed %s/%s: %v", owner, name, err)
		}
	}

	t.Setenv("WARDYN_AGE_KEY", ageKey)
	resetFlags(t)
	oldArgs := os.Args
	os.Args = []string{"wardynd-test"}
	t.Cleanup(func() { os.Args = oldArgs })
	rec := &capturingRecorder{}
	st, err := openSecretStore(t.Context(), pool, parseBootFlags(), rec)
	if err != nil {
		t.Fatalf("openSecretStore: %v", err)
	}
	holders, err := st.Holders(t.Context(), []string{"git-pat-dev-azure-com", "git-pat-tfs-corp-example"})
	if err != nil {
		t.Fatal(err)
	}
	if len(holders) != 0 {
		t.Fatalf("after the boot open, shared Azure DevOps credentials are still held: %v", holders)
	}
	if rows := adoRetireRows(t, rec); len(rows) != 2 {
		t.Fatalf("the boot open wrote audit rows for %v, want operator and alice", rows)
	}
}
