// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"
)

// TestADORepoBranches_SignInListsWithTheBearerToken: a person who signed in through the real capture and
// redemption path lists branches with their own access token, sent as a Bearer credential, and the listing
// says it came through the sign-in.
func TestADORepoBranches_SignInListsWithTheBearerToken(t *testing.T) {
	f := newADOFixture(t)
	subject := f.fake.Subject()
	if w := f.capture(t, subject); w.Code != http.StatusFound {
		t.Fatalf("capture: status %d body %q", w.Code, w.Body.String())
	}
	fake := &adoBranchFake{def: "main", refs: []string{"topic-alpha", "main"}}
	fake.install(t)

	run := adoEntraRun{rowID: f.cfg.RowID, org: "contoso", owner: subject}
	out, err := f.srv.adoRepoBranches(t.Context(), adoSite(adoEntraTestRow()), run, adoTestRepo)
	if err != nil {
		t.Fatalf("adoRepoBranches: %v", err)
	}
	if out.Listing != branchListingListed || out.Credential != branchCredADOSignIn || out.Default != "main" ||
		strings.Join(out.Branches, ",") != "main,topic-alpha" {
		t.Fatalf("listing = %+v, want main,topic-alpha listed through ado_signin", out)
	}
	if len(fake.auths) != 2 {
		t.Fatalf("%d requests, want 2 (repository and refs)", len(fake.auths))
	}
	for i, auth := range fake.auths {
		if token, ok := strings.CutPrefix(auth, "Bearer "); !ok || token == "" {
			t.Errorf("request %d carried %q, want the person's access token as a Bearer credential", i, auth)
		}
	}
}
