// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit_test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/imageadmit"
	"github.com/cjohnstoniv/wardyn/internal/releasesig/releasesigtest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The site setting images.admission (internal/types) and the compiler's input (Config) are one contract: the
// mapping keeps every field, and the site write refuses whatever the compiler refuses.

func jsonFields(t reflect.Type) []string {
	var out []string
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		out = append(out, name)
	}
	return out
}

func TestSiteTypesAndConfigShareJSONNames(t *testing.T) {
	for _, pair := range [][2]reflect.Type{
		{reflect.TypeFor[types.ImageTrustedRoot](), reflect.TypeFor[imageadmit.RootSpec]()},
		{reflect.TypeFor[types.ImageSigner](), reflect.TypeFor[imageadmit.SignerSpec]()},
		{reflect.TypeFor[types.ImageKeyless](), reflect.TypeFor[imageadmit.KeylessSpec]()},
	} {
		if site, cfg := jsonFields(pair[0]), jsonFields(pair[1]); !reflect.DeepEqual(site, cfg) {
			t.Errorf("%s fields %v, %s fields %v", pair[0], site, pair[1], cfg)
		}
	}
}

func fullSite(t *testing.T) *types.SiteImageAdmission {
	off := false
	return &types.SiteImageAdmission{Mode: "enforce", Scope: "all",
		TrustedRoots: []types.ImageTrustedRoot{{Name: "github-private", Path: publicGoodPath(t)}},
		Signers: []types.ImageSigner{
			{Name: "corp-ci", Images: []string{"registry.corp.example/team/*"},
				Keyless:             &types.ImageKeyless{Issuer: ciIssuer, IdentityRegexp: "^" + ciIdentity + "$", SourceRepository: ciRepo, TrustedRoot: "github-private"},
				RequireAttestations: []string{imageadmit.PredicateSLSAProvenanceV1}, SignaturePredicates: []string{"https://example.com/approved/v1"}},
			{Name: "vendor-key", Images: []string{"docker.io/vendor/*", "docker.io/vendor/tool:1"}, PublicKey: "-----BEGIN PUBLIC KEY-----\nx\n-----END PUBLIC KEY-----\n", TransparencyLog: &off},
		}}
}

func publicGoodPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("../releasesig/trusted_root.json")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// SiteConfig keeps every field under the same JSON name, and copies rather than aliases.
func TestSiteConfigKeepsEveryField(t *testing.T) {
	a := fullSite(t)
	got := imageadmit.SiteConfig(a)
	raw, err := json.Marshal(struct {
		TrustedRoots []types.ImageTrustedRoot `json:"trusted_roots,omitempty"`
		Signers      []types.ImageSigner      `json:"signers,omitempty"`
	}{a.TrustedRoots, a.Signers})
	if err != nil {
		t.Fatal(err)
	}
	var want imageadmit.Config
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SiteConfig = %+v\nwant %+v", got, want)
	}
	a.Signers[0].Images[0] = "changed/*"
	a.Signers[0].Keyless.Issuer = "https://changed.example"
	*a.Signers[1].TransparencyLog = true
	a.Signers[0].SignaturePredicates[0] = "https://changed.example/v1"
	if got.Signers[0].Images[0] != "registry.corp.example/team/*" || got.Signers[0].Keyless.Issuer != ciIssuer ||
		*got.Signers[1].TransparencyLog || got.Signers[0].SignaturePredicates[0] != "https://example.com/approved/v1" {
		t.Fatalf("SiteConfig aliases the site setting: %+v", got)
	}
	if c := imageadmit.SiteConfig(nil); !reflect.DeepEqual(c, imageadmit.Config{}) {
		t.Fatalf("SiteConfig(nil) = %+v", c)
	}
}

// validSite is a block both the site write and the compiler accept.
func validSite() *types.SiteImageAdmission {
	return &types.SiteImageAdmission{Mode: "audit", Signers: []types.ImageSigner{{
		Name: "corp-ci", Images: []string{"registry.corp.example/team/*"},
		Keyless: &types.ImageKeyless{Issuer: ciIssuer, Identity: ciIdentity, SourceRepository: ciRepo},
	}}}
}

func anyImage(string) bool { return true }

// The four settings the site write once accepted and the compiler refused: each is now refused by both.
func TestCheckSiteRefusesWhatCompileRefuses(t *testing.T) {
	s := releasesigtest.New(t)
	o := imageadmit.Options{Version: "1.2.3", PublicGoodRoot: testRoot(t, s)}
	if err := validSite().Validate(anyImage); err != nil {
		t.Fatalf("valid block refused by the shape check: %v", err)
	}
	if err := imageadmit.CheckSite(validSite(), o); err != nil {
		t.Fatalf("valid block refused by CheckSite: %v", err)
	}
	if err := imageadmit.CheckSite(nil, o); err != nil {
		t.Fatalf("CheckSite(nil) = %v", err)
	}
	for _, tc := range []struct {
		name string
		mut  func(a *types.SiteImageAdmission)
		want string
	}{
		{"a relative signature predicate", func(a *types.SiteImageAdmission) { a.Signers[0].SignaturePredicates = []string{"approved"} }, `"approved"`},
		{"an empty images list", func(a *types.SiteImageAdmission) { a.Signers[0].Images = []string{} }, "images"},
		{"a signer named public-good", func(a *types.SiteImageAdmission) { a.Signers[0].Name = "public-good" }, `"public-good"`},
		{"a trusted root named wardyn-release", func(a *types.SiteImageAdmission) {
			a.TrustedRoots = []types.ImageTrustedRoot{{Name: "wardyn-release", Path: publicGoodPath(t)}}
		}, `"wardyn-release"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := validSite()
			tc.mut(a)
			_, cerr := imageadmit.Compile(imageadmit.SiteConfig(a), o)
			if !errors.Is(cerr, imageadmit.ErrPolicy) {
				t.Fatalf("Compile = %v, want ErrPolicy", cerr)
			}
			err := imageadmit.CheckSite(a, o)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CheckSite = %v, want a refusal naming %s", err, tc.want)
			}
			if strings.Contains(err.Error(), imageadmit.ErrPolicy.Error()) {
				t.Fatalf("CheckSite message %q repeats the compiler's prefix", err)
			}
		})
	}
}

// identity_regexp means the whole identity at the site write as at run create: an anchored alternation the
// write accepts cannot admit a longer identity, and an unanchored pattern is refused by both.
func TestCheckSiteKeepsTheWholeIdentityRule(t *testing.T) {
	const wf = "https://github\\.com/corp/images/\\.github/workflows/build\\.yml@refs/heads/"
	const wfURL = "https://github.com/corp/images/.github/workflows/build.yml@refs/heads/"
	s := releasesigtest.New(t)
	o := imageadmit.Options{Version: "1.2.3", PublicGoodRoot: testRoot(t, s)}
	a := validSite()
	a.Signers[0].Keyless.Identity, a.Signers[0].Keyless.IdentityRegexp = "", "^"+wf+"main|"+wfURL+"release$"
	if err := a.Validate(anyImage); err != nil {
		t.Fatalf("shape check refused an anchored pattern: %v", err)
	}
	if err := imageadmit.CheckSite(a, o); err != nil {
		t.Fatalf("CheckSite refused an anchored pattern: %v", err)
	}
	p, err := imageadmit.Compile(imageadmit.SiteConfig(a), o)
	if err != nil {
		t.Fatal(err)
	}
	for identity, admitted := range map[string]bool{wfURL + "main": true, wfURL + "release": true, wfURL + "main-untrusted": false, wfURL + "not-release": false} {
		opts := ciOpts()
		opts.Identity = identity
		v := check(t, p, nil, s.SignDSSE(statement(t, imageadmit.PredicateCosignSign, imageHex), opts))
		if v.Admitted() != admitted {
			t.Errorf("identity %s: verdict %+v, admitted want %v", identity, v, admitted)
		}
	}
	a.Signers[0].Keyless.IdentityRegexp = wf + "main"
	if a.Validate(anyImage) == nil || imageadmit.CheckSite(a, o) == nil {
		t.Fatal("an unanchored identity_regexp was accepted")
	}
}
