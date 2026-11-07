// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// PUT /site-config accepts an images.admission block only when run create's compiler accepts it, so a stored
// setting never fails every later run instead of the write.

func compileParityCases(t *testing.T) []struct {
	name string
	mut  func(a *types.SiteImageAdmission)
	want string
} {
	return []struct {
		name string
		mut  func(a *types.SiteImageAdmission)
		want string
	}{
		{"a relative signature predicate", func(a *types.SiteImageAdmission) { a.Signers[0].SignaturePredicates = []string{"approved"} }, `"approved"`},
		{"an empty images list", func(a *types.SiteImageAdmission) { a.Signers[0].Images = []string{} }, "images"},
		{"a signer named public-good", func(a *types.SiteImageAdmission) { a.Signers[0].Name = "public-good" }, `"public-good"`},
		{"a trusted root named wardyn-release", func(a *types.SiteImageAdmission) {
			a.TrustedRoots = []types.ImageTrustedRoot{{Name: "wardyn-release", Path: mustAbs(t, publicGoodRootFile)}}
		}, `"wardyn-release"`},
	}
}

func TestValidateSiteImageAdmissionMatchesTheCompiler(t *testing.T) {
	for _, tc := range compileParityCases(t) {
		a := &types.SiteImageAdmission{Mode: "audit", Signers: []types.ImageSigner{admissionSigner("corp-ci")}}
		tc.mut(a)
		err := validateSiteImageAdmission(&types.SiteImages{Admission: a})
		if err == nil || !strings.Contains(err.Error(), "images.admission") || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v, want a refusal naming images.admission and %s", tc.name, err, tc.want)
		}
	}
}

func TestPutSiteConfigRefusesWhatTheCompilerRefuses(t *testing.T) {
	fake := &fakeSiteConfigStore{}
	srv, _ := newSiteConfigHarness(t, fake)
	put := func(a *types.SiteImageAdmission) (int, string) {
		body, _ := json.Marshal(map[string]any{"images": map[string]any{"admission": a}})
		w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, string(body))
		return w.Code, w.Body.String()
	}
	for _, tc := range compileParityCases(t) {
		a := &types.SiteImageAdmission{Mode: "audit", Signers: []types.ImageSigner{admissionSigner("corp-ci")}}
		tc.mut(a)
		if code, body := put(a); code != http.StatusBadRequest || !strings.Contains(body, "images.admission") {
			t.Errorf("%s: PUT = %d %s, want 400 naming images.admission", tc.name, code, body)
		}
		if fake.putSeen != nil {
			t.Fatalf("%s: a refused block was stored: %+v", tc.name, fake.putSeen.Images)
		}
	}
	// identity_regexp is matched whole at run create; the write keeps the same rule: an anchored alternation is
	// stored, an unanchored pattern is refused.
	a := &types.SiteImageAdmission{Mode: "audit", Signers: []types.ImageSigner{admissionSigner("corp-ci")}}
	a.Signers[0].Keyless.Identity = ""
	a.Signers[0].Keyless.IdentityRegexp = "^https://github\\.com/corp/images/\\.github/workflows/build\\.yml@refs/heads/main|https://github\\.com/corp/images/\\.github/workflows/build\\.yml@refs/heads/release$"
	if code, body := put(a); code != http.StatusOK {
		t.Fatalf("PUT anchored identity_regexp = %d %s, want 200", code, body)
	}
	fake.putSeen = nil
	a.Signers[0].Keyless.IdentityRegexp = "https://github\\.com/corp/images/.*"
	if code, body := put(a); code != http.StatusBadRequest || !strings.Contains(body, "identity_regexp") {
		t.Fatalf("PUT unanchored identity_regexp = %d %s, want 400 naming identity_regexp", code, body)
	}
	if fake.putSeen != nil {
		t.Fatalf("an unanchored identity_regexp was stored")
	}
}
