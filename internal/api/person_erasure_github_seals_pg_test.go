// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A person's erasure deletes the GitHub tokens the broker keeps sealed under their masking-manifest owner key. The
// run's manifest owner (a local principal on the dispatch context) differs from its created_by, so erasing the
// creator leaves the row and erasing the manifest owner removes it: a seal keyed on created_by fails this.
func TestPersonErasure_DeletesGitHubTokenSeals_PG(t *testing.T) {
	l := newMaskLab(t)
	rp := l.replica()
	run := l.run()
	const manifestOwner = "carol@example.com"
	ctx := withLocalPrincipal(t.Context(), manifestOwner)
	if !rp.srv.beginMaskManifest(ctx, run) || !rp.srv.completeMaskManifest(ctx, run) {
		t.Fatal("the manifest could not begin and complete")
	}
	var owner string
	if err := l.pool.QueryRow(t.Context(), `SELECT owner FROM run_mask_manifest WHERE run_id = $1`, run.ID).Scan(&owner); err != nil || owner != manifestOwner || owner == run.CreatedBy {
		t.Fatalf("manifest owner = %q, %v; want %q, not the creator %q", owner, err, manifestOwner, run.CreatedBy)
	}

	brk := broker.New(broker.NewPgxStore(l.pool), nil, l.rec, nil, &broker.FakeGitHubMinter{Token: "ghs_erasure_" + uuid.NewString()}).
		WithTokenSeal(broker.NewTokenSeal(l.pool, rp.sec.SubjectKeys()))
	rp.srv.cfg.Broker = brk
	scope, _ := json.Marshal(map[string]any{"repos": []string{"acme/widgets"}, "permissions": map[string]string{"contents": "read"}})
	spec, _ := json.Marshal(types.GrantSpec{Kind: types.GrantGitHubToken, Scope: scope, TTLSeconds: 600})
	grantID := uuid.New()
	if _, err := l.pool.Exec(t.Context(), `INSERT INTO credential_grants (id, run_id, spec) VALUES ($1, $2, $3)`, grantID, run.ID, spec); err != nil {
		t.Fatal(err)
	}
	if _, err := brk.MintForGrant(t.Context(), &identity.Claims{RunID: run.ID, SPIFFEID: run.SPIFFEID}, grantID); err != nil {
		t.Fatalf("mint: %v", err)
	}
	rows := func() int {
		return l.count(`SELECT count(*) FROM run_github_token_seals WHERE run_id = $1`, run.ID)
	}
	if n := rows(); n != 1 {
		t.Fatalf("seal rows after the mint = %d, want 1", n)
	}

	if _, err := rp.srv.eraseMaskCopies(t.Context(), run.CreatedBy, nil); err != nil {
		t.Fatal(err)
	}
	if n := rows(); n != 1 {
		t.Fatalf("seal rows after erasing the creator = %d, want 1: the seal is the manifest owner's", n)
	}
	detail, err := rp.srv.eraseMaskCopies(t.Context(), manifestOwner, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := rows(); n != 0 {
		t.Fatalf("seal rows after erasing the manifest owner = %d, want 0", n)
	}
	if d, _ := detail.(map[string]any); d["run_tokens"] != 1 {
		t.Fatalf("erasure detail = %v, want run_tokens 1", detail)
	}
}
