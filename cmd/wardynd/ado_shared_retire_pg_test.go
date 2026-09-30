// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
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

// TestPG_SweepRetiredADOSharedCredentials: the sweep deletes every shared Azure
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
			if err := deleteRetiredADOSharedCredentials(t.Context(), st, adoRetireSite(), rec); err != nil {
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
			if err := deleteRetiredADOSharedCredentials(t.Context(), open(again), adoRetireSite(), again); err != nil {
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

// TestPG_SweepRetiredADOSharedCredentials_SparesOtherForges (#1429 review F1):
// the sweep never deletes another forge's secret. A GHES row on the very host an
// Azure DevOps row names, and a host that only slugs alike, keep their tokens in
// every namespace; the Azure DevOps host with no such claim is still swept. Each
// skipped host is named in a warning.
func TestPG_SweepRetiredADOSharedCredentials_SparesOtherForges(t *testing.T) {
	pool := envelopeDB(t)
	st, err := buildSecretStore(t.Context(), pool, mustAgeIdentity(t).String(), nil, "", storeClients{}, &capturingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	sc := types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{
		// The same host, once as Azure DevOps Server and once as GHES.
		{ID: "ados", Kind: types.GitProviderAzureDevOps, Disabled: true, BaseURLs: []string{"https://git.corp.example/tfs"}},
		{ID: "ghes", Kind: types.GitProviderGitHub, BaseURLs: []string{"https://git.corp.example/org"}},
		// Two hosts with one slug: tfs.corp.example and tfs-corp.example.
		{ID: "ados2", Kind: types.GitProviderAzureDevOps, Disabled: true, BaseURLs: []string{"https://tfs.corp.example/acme"}},
		{ID: "ghes2", Kind: types.GitProviderGitHub, BaseURLs: []string{"https://tfs-corp.example/org"}},
		// An Azure DevOps host nothing else claims.
		{ID: "ados3", Kind: types.GitProviderAzureDevOps, Disabled: true, BaseURLs: []string{"https://ado.corp.example/acme"}},
	}}}
	const alice = "alice@example.com"
	spared := []string{"git-pat-git-corp-example", "ssh-key-git-corp-example", "git-pat-tfs-corp-example", "git-pat-github-com"}
	swept := []string{"git-pat-ado-corp-example", "git-pat-dev-azure-com"}
	for _, owner := range []string{"", alice} {
		for _, name := range append(slices.Clone(spared), swept...) {
			if err := st.For(owner).Put(t.Context(), name, []byte("synthetic-credential-value")); err != nil {
				t.Fatalf("seed %s/%s: %v", owner, name, err)
			}
		}
	}
	if err := deleteRetiredADOSharedCredentials(t.Context(), st, sc, &capturingRecorder{}); err != nil {
		t.Fatal(err)
	}
	holders, err := st.Holders(t.Context(), append(slices.Clone(spared), swept...))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range spared {
		if !slices.Equal(holders[name], []string{"", alice}) {
			t.Errorf("%s held by %q after the sweep, want it kept for the operator and alice", name, holders[name])
		}
	}
	for _, name := range swept {
		if len(holders[name]) != 0 {
			t.Errorf("%s still held by %q, want it swept", name, holders[name])
		}
	}
}

// TestPG_ADOSharedSweepIsOneShot (#1429 review F2): the sweep runs at the first
// start after the migration only. A person's own token stored under an enabled
// per-person Server row's conventional name afterwards survives every later
// start, and a later start audits nothing.
func TestPG_ADOSharedSweepIsOneShot(t *testing.T) {
	pool := envelopeDB(t)
	ageKey := mustAgeIdentity(t).String()
	seed, err := buildSecretStore(t.Context(), pool, ageKey, nil, "", storeClients{}, &capturingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO site_config (config) VALUES ($1)`,
		`{"workspace_providers":{"git":[{"id":"ados","kind":"azure_devops","base_urls":["https://tfs.corp.example/acme"],"lanes":["pat"],"credential_source":"per_user"}]}}`); err != nil {
		t.Fatal(err)
	}
	if err := seed.For("").Put(t.Context(), "git-pat-tfs-corp-example", []byte("synthetic-credential-value")); err != nil {
		t.Fatal(err)
	}

	t.Setenv("WARDYN_AGE_KEY", ageKey)
	oldArgs := os.Args
	os.Args = []string{"wardynd-test"}
	t.Cleanup(func() { os.Args = oldArgs })
	boot := func() (secretstore.Store, *capturingRecorder) {
		t.Helper()
		resetFlags(t) // parseBootFlags defines its flags on the process-wide set
		rec := &capturingRecorder{}
		st, err := openSecretStore(t.Context(), pool, parseBootFlags(), rec)
		if err != nil {
			t.Fatalf("openSecretStore: %v", err)
		}
		return st, rec
	}

	st, rec := boot()
	if rows := adoRetireRows(t, rec); len(rows) != 1 || rows["operator"] == nil {
		t.Fatalf("the first start audited %v, want the operator's namespace", rows)
	}
	const alice = "alice@example.com"
	if err := st.For(alice).Put(t.Context(), "git-pat-tfs-corp-example", []byte("alices-own-token-value")); err != nil {
		t.Fatal(err)
	}
	st, rec = boot()
	holders, err := st.Holders(t.Context(), []string{"git-pat-tfs-corp-example"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(holders["git-pat-tfs-corp-example"], []string{alice}) {
		t.Fatalf("after the second start the token is held by %q, want alice's own kept", holders["git-pat-tfs-corp-example"])
	}
	if rows := adoRetireRows(t, rec); len(rows) != 0 {
		t.Fatalf("the second start audited %v, want nothing", rows)
	}
	var done bool
	if err := pool.QueryRow(t.Context(), `SELECT done_at IS NOT NULL FROM boot_once WHERE name = 'ado_shared_credential_retire'`).Scan(&done); err != nil || !done {
		t.Fatalf("marker done = %v, %v; want it set by the first start", done, err)
	}
}

// holdersFails is a store whose Holders errors, as an unreachable store would.
type holdersFails struct{ secretstore.Store }

func (holdersFails) Holders(context.Context, []string) (map[string][]string, error) {
	return nil, errors.New("store unreachable")
}

// TestPG_ADOSharedSweepMarkerSetOnlyAfterTheDelete (#1440 R2-2): a first sweep
// that fails leaves the marker pending, so the next start tries again; the
// marker is set only once the delete has succeeded.
func TestPG_ADOSharedSweepMarkerSetOnlyAfterTheDelete(t *testing.T) {
	pool := envelopeDB(t)
	st, err := buildSecretStore(t.Context(), pool, mustAgeIdentity(t).String(), nil, "", storeClients{}, &capturingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.For("").Put(t.Context(), "git-pat-dev-azure-com", []byte("synthetic-credential-value")); err != nil {
		t.Fatal(err)
	}
	pending := func() bool {
		t.Helper()
		var p bool
		if err := pool.QueryRow(t.Context(), `SELECT done_at IS NULL FROM boot_once WHERE name = 'ado_shared_credential_retire'`).Scan(&p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if err := sweepRetiredADOSharedCredentials(t.Context(), pool, holdersFails{st}, types.SiteConfig{}, &capturingRecorder{}); err == nil {
		t.Fatal("a sweep over a store that cannot answer returned nil, want it to refuse boot")
	}
	if !pending() {
		t.Fatal("the marker was set although the sweep failed: the shared credentials would never be retried")
	}
	if err := sweepRetiredADOSharedCredentials(t.Context(), pool, st, types.SiteConfig{}, &capturingRecorder{}); err != nil {
		t.Fatal(err)
	}
	if pending() {
		t.Error("the marker is still pending after a successful sweep")
	}
	if holders, _ := st.Holders(t.Context(), []string{"git-pat-dev-azure-com"}); len(holders) != 0 {
		t.Errorf("the credential survived the retried sweep: %v", holders)
	}
}
