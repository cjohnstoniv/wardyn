// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adoscope

import (
	"net/http"
	"slices"
	"testing"
)

// The own-token door asks Graph for the token owner's originId
// (GET .../_apis/graph/users/{descriptor}). That read is identity_read, a
// capability the default profile leaves out: the pasted token's vso.graph is
// wider than the row's ceiling, and the proxy holds a run to the ceiling.
func TestGraphUserLookupIsIdentityReadAndNotInTheDefaultProfile(t *testing.T) {
	req := onHost("vssps.dev.azure.com", http.MethodGet, "/acme/_apis/graph/users/aad.abc", "")
	v, err := Classify(req)
	if err != nil || v.Capability != CapIdentityRead {
		t.Fatalf("GET graph/users/{descriptor} = %+v, %v, want identity_read", v, err)
	}
	if slices.Contains(ProfileDefault(), CapIdentityRead) {
		t.Fatalf("ProfileDefault %v holds identity_read", ProfileDefault())
	}
	if Permits(ProfileDefault(), v) {
		t.Fatal("a run on the default profile may read Graph users")
	}
}
