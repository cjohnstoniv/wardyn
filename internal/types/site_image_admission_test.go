// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func testPEM(t *testing.T, pub any) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func testKeys(t *testing.T) (p256, p224, ed, rsa2048, rsa1024 string) {
	t.Helper()
	k256, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	k224, _ := ecdsa.GenerateKey(elliptic.P224(), rand.Reader)
	_, edk, _ := ed25519.GenerateKey(rand.Reader)
	r2048, _ := rsa.GenerateKey(rand.Reader, 2048)
	r1024, _ := rsa.GenerateKey(rand.Reader, 1024)
	return testPEM(t, &k256.PublicKey), testPEM(t, &k224.PublicKey), testPEM(t, edk.Public()), testPEM(t, &r2048.PublicKey), testPEM(t, &r1024.PublicKey)
}

func anyPrefix(string) bool { return true }

func keylessSigner(name string) ImageSigner {
	return ImageSigner{
		Name:   name,
		Images: []string{"registry.corp.example/team/*"},
		Keyless: &ImageKeyless{
			Issuer:   "https://token.actions.githubusercontent.com",
			Identity: "https://github.com/corp/images/.github/workflows/build.yml@refs/heads/main",
		},
	}
}

func TestSiteImagesIsZero(t *testing.T) {
	for _, tc := range []struct {
		name string
		im   SiteImages
		zero bool
	}{
		{"nothing", SiteImages{}, true},
		{"an empty admission block", SiteImages{Admission: &SiteImageAdmission{}}, true},
		{"mode off and scope member spelled out", SiteImages{Admission: &SiteImageAdmission{Mode: "off", Scope: "member"}}, true},
		{"require_digest", SiteImages{RequireDigest: true}, false},
		{"audit", SiteImages{Admission: &SiteImageAdmission{Mode: "audit"}}, false},
		{"scope all", SiteImages{Admission: &SiteImageAdmission{Scope: "all"}}, false},
		{"signers defined while off", SiteImages{Admission: &SiteImageAdmission{Signers: []ImageSigner{keylessSigner("corp-ci")}}}, false},
		{"a trusted root while off", SiteImages{Admission: &SiteImageAdmission{TrustedRoots: []ImageTrustedRoot{{Name: "r", Path: "/x"}}}}, false},
	} {
		if got := tc.im.IsZero(); got != tc.zero {
			t.Errorf("%s: IsZero = %v, want %v", tc.name, got, tc.zero)
		}
	}
}

func TestSiteImageAdmissionDefaults(t *testing.T) {
	var a *SiteImageAdmission
	if a.EffectiveMode() != ImageAdmissionOff || a.EffectiveScope() != ImageAdmissionScopeMember {
		t.Errorf("a nil block reads mode %q scope %q, want off and member", a.EffectiveMode(), a.EffectiveScope())
	}
	a = &SiteImageAdmission{}
	if a.EffectiveMode() != ImageAdmissionOff || a.EffectiveScope() != ImageAdmissionScopeMember {
		t.Errorf("an empty block reads mode %q scope %q, want off and member", a.EffectiveMode(), a.EffectiveScope())
	}
}

func TestSiteImageAdmissionWireKeys(t *testing.T) {
	raw := `{"require_digest":true,"admission":{"mode":"audit","scope":"all","trusted_roots":[{"name":"gh","path":"/r.json"}],` +
		`"signers":[{"name":"corp-ci","images":["a.example/x/*"],"keyless":{"issuer":"https://i.example","identity_regexp":"^x$","source_repository":"https://github.com/o/r","trusted_root":"gh"},` +
		`"require_attestations":["https://slsa.dev/provenance/v1"],"signature_predicates":["https://sigstore.dev/cosign/sign/v1"]},` +
		`{"name":"vendor-key","images":["b.example/y/*"],"public_key":"K","transparency_log":false}]}}`
	var im SiteImages
	if err := json.Unmarshal([]byte(raw), &im); err != nil {
		t.Fatal(err)
	}
	a := im.Admission
	if a == nil || a.Mode != "audit" || a.Scope != "all" || len(a.TrustedRoots) != 1 || a.TrustedRoots[0].Path != "/r.json" || len(a.Signers) != 2 {
		t.Fatalf("decoded %+v", a)
	}
	s := a.Signers[0]
	if s.Keyless == nil || s.Keyless.IdentityRegexp != "^x$" || s.Keyless.SourceRepository != "https://github.com/o/r" || s.Keyless.TrustedRoot != "gh" ||
		!slices.Equal(s.RequireAttestations, []string{"https://slsa.dev/provenance/v1"}) || !slices.Equal(s.SignaturePredicates, []string{"https://sigstore.dev/cosign/sign/v1"}) {
		t.Errorf("keyless signer decoded %+v", s)
	}
	if k := a.Signers[1]; k.PublicKey != "K" || k.TransparencyLog == nil || *k.TransparencyLog {
		t.Errorf("key signer decoded %+v", k)
	}
	out, err := json.Marshal(SiteImages{})
	if err != nil || string(out) != "{}" {
		t.Errorf("an empty SiteImages marshals %s (%v), want {}", out, err)
	}
}

func TestSiteImageAdmissionValidate(t *testing.T) {
	p256, p224, ed, rsa2048, rsa1024 := testKeys(t)
	keySigner := func(name, pem string) ImageSigner {
		return ImageSigner{Name: name, Images: []string{"docker.io/vendor/*"}, PublicKey: pem}
	}
	with := func(mut func(a *SiteImageAdmission)) *SiteImageAdmission {
		a := &SiteImageAdmission{Mode: "audit", Signers: []ImageSigner{keylessSigner("corp-ci")}}
		mut(a)
		return a
	}
	signer := func(mut func(s *ImageSigner)) *SiteImageAdmission {
		return with(func(a *SiteImageAdmission) { mut(&a.Signers[0]) })
	}
	ok := []struct {
		name string
		a    *SiteImageAdmission
	}{
		{"nil", nil},
		{"empty", &SiteImageAdmission{}},
		{"a keyless signer", with(func(*SiteImageAdmission) {})},
		{"identity_regexp anchored", signer(func(s *ImageSigner) {
			s.Keyless.Identity, s.Keyless.IdentityRegexp = "", "^https://github.com/corp/.*$"
		})},
		{"a source repository and the public-good root named", signer(func(s *ImageSigner) {
			s.Keyless.SourceRepository, s.Keyless.TrustedRoot = "https://github.com/corp/images", "public-good"
		})},
		{"a P-256 key", with(func(a *SiteImageAdmission) { a.Signers = []ImageSigner{keySigner("k", p256)} })},
		{"an Ed25519 key", with(func(a *SiteImageAdmission) { a.Signers = []ImageSigner{keySigner("k", ed)} })},
		{"an RSA 2048 key", with(func(a *SiteImageAdmission) { a.Signers = []ImageSigner{keySigner("k", rsa2048)} })},
		{"signers defined while mode is off", with(func(a *SiteImageAdmission) { a.Mode = "off" })},
		{"enforce with a signer", with(func(a *SiteImageAdmission) { a.Mode = "enforce" })},
		{"enforce with no signer under scope all", &SiteImageAdmission{Mode: "enforce", Scope: "all"}},
		{"a defined trusted root", with(func(a *SiteImageAdmission) {
			a.TrustedRoots = []ImageTrustedRoot{{Name: "github-private", Path: "/etc/wardyn/root.json"}}
			a.Signers[0].Keyless.TrustedRoot = "github-private"
		})},
		{"predicate lists", signer(func(s *ImageSigner) {
			s.RequireAttestations = []string{"https://slsa.dev/provenance/v1"}
			s.SignaturePredicates = []string{"https://sigstore.dev/cosign/sign/v1"}
		})},
	}
	for _, tc := range ok {
		if err := tc.a.Validate(anyPrefix); err != nil {
			t.Errorf("%s: refused: %v", tc.name, err)
		}
	}
	bad := []struct {
		name string
		a    *SiteImageAdmission
		want string
	}{
		{"unknown mode", with(func(a *SiteImageAdmission) { a.Mode = "block" }), "mode"},
		{"unknown scope", with(func(a *SiteImageAdmission) { a.Scope = "everyone" }), "scope"},
		{"enforce with no signer under scope member", &SiteImageAdmission{Mode: "enforce"}, "audit"},
		{"a reserved signer name", signer(func(s *ImageSigner) { s.Name = "wardyn-release" }), "wardyn-release"},
		{"a reserved root name", with(func(a *SiteImageAdmission) {
			a.TrustedRoots = []ImageTrustedRoot{{Name: "public-good", Path: "/r.json"}}
		}), "public-good"},
		{"a duplicate signer name", with(func(a *SiteImageAdmission) { a.Signers = append(a.Signers, keylessSigner("corp-ci")) }), "corp-ci"},
		{"a duplicate root name", with(func(a *SiteImageAdmission) {
			a.TrustedRoots = []ImageTrustedRoot{{Name: "r", Path: "/a.json"}, {Name: "r", Path: "/b.json"}}
		}), "twice"},
		{"a malformed signer name", signer(func(s *ImageSigner) { s.Name = "Corp CI" }), "name"},
		{"an empty signer name", signer(func(s *ImageSigner) { s.Name = "" }), "name"},
		{"neither keyless nor key", signer(func(s *ImageSigner) { s.Keyless = nil }), "keyless"},
		{"both keyless and key", signer(func(s *ImageSigner) { s.PublicKey = p256 }), "both"},
		{"identity and identity_regexp both set", signer(func(s *ImageSigner) { s.Keyless.IdentityRegexp = "^x$" }), "identity"},
		{"neither identity nor identity_regexp", signer(func(s *ImageSigner) { s.Keyless.Identity = "" }), "identity"},
		{"an unanchored identity_regexp", signer(func(s *ImageSigner) { s.Keyless.Identity, s.Keyless.IdentityRegexp = "", "https://github.com/corp/.*" }), "anchored"},
		{"a regexp anchored only at the start", signer(func(s *ImageSigner) { s.Keyless.Identity, s.Keyless.IdentityRegexp = "", "^https://github.com/corp/.*" }), "anchored"},
		{"an uncompilable identity_regexp", signer(func(s *ImageSigner) { s.Keyless.Identity, s.Keyless.IdentityRegexp = "", "^(unclosed$" }), "identity_regexp"},
		{"a non-https issuer", signer(func(s *ImageSigner) { s.Keyless.Issuer = "http://token.example" }), "issuer"},
		{"an empty issuer", signer(func(s *ImageSigner) { s.Keyless.Issuer = "" }), "issuer"},
		{"a non-https source repository", signer(func(s *ImageSigner) { s.Keyless.SourceRepository = "git@github.com:o/r" }), "source_repository"},
		{"an unparsable PEM", with(func(a *SiteImageAdmission) { a.Signers = []ImageSigner{keySigner("k", "not a pem")} }), "public_key"},
		{"a weak RSA key", with(func(a *SiteImageAdmission) { a.Signers = []ImageSigner{keySigner("k", rsa1024)} }), "2048"},
		{"an unsupported curve", with(func(a *SiteImageAdmission) { a.Signers = []ImageSigner{keySigner("k", p224)} }), "public_key"},
		{"an undefined trusted root", signer(func(s *ImageSigner) { s.Keyless.TrustedRoot = "nowhere" }), "trusted_root"},
		{"a root with a relative path", with(func(a *SiteImageAdmission) { a.TrustedRoots = []ImageTrustedRoot{{Name: "r", Path: "root.json"}} }), "absolute"},
		{"a root with no name", with(func(a *SiteImageAdmission) { a.TrustedRoots = []ImageTrustedRoot{{Path: "/r.json"}} }), "name"},
		{"an images entry failing the grammar", signer(func(s *ImageSigner) { s.Images = []string{"registry.corp.example/team/*", "Bad Entry"} }), "images[1]"},
		{"a predicate that is not a URI", signer(func(s *ImageSigner) { s.SignaturePredicates = []string{"has space"} }), "signature_predicates"},
		{"an empty attestation type", signer(func(s *ImageSigner) { s.RequireAttestations = []string{""} }), "require_attestations"},
	}
	for _, tc := range bad {
		err := tc.a.Validate(func(p string) bool { return p != "Bad Entry" })
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not mention %q", tc.name, err, tc.want)
		}
	}
}

func TestSiteImageAdmissionSignerNames(t *testing.T) {
	a := &SiteImageAdmission{Signers: []ImageSigner{keylessSigner("corp-ci"), keylessSigner("vendor")}}
	if got := a.SignerNames(); !slices.Equal(got, []string{"corp-ci", "vendor"}) {
		t.Errorf("SignerNames = %v", got)
	}
	var nilA *SiteImageAdmission
	if got := nilA.SignerNames(); len(got) != 0 {
		t.Errorf("a nil block names %v", got)
	}
}

func TestValidRequiredImageSigners(t *testing.T) {
	for _, tc := range []struct {
		name string
		list []string
		ok   bool
	}{
		{"none", nil, true},
		{"two names", []string{"corp-ci", "wardyn-release"}, true},
		{"an empty name", []string{""}, false},
		{"upper case", []string{"Corp"}, false},
		{"a space", []string{"corp ci"}, false},
		{"a path", []string{"../x"}, false},
		{"a duplicate", []string{"corp-ci", "corp-ci"}, false},
		{"too many", func() []string {
			var l []string
			for i := 0; i <= MaxRequiredImageSigners; i++ {
				l = append(l, fmt.Sprintf("s%d", i))
			}
			return l
		}(), false},
	} {
		if err := ValidRequiredImageSigners(tc.list); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

// The restriction's limits carry required_image_signers into the overlay the composer meets, so a restriction
// can add a requirement.
func TestRestrictionLimitsCarryRequiredImageSigners(t *testing.T) {
	names := []string{"corp-ci"}
	l := RestrictionLimits{RequiredImageSigners: &names}
	if got := l.Overlay().RequiredImageSigners; got == nil || !reflect.DeepEqual(*got, names) {
		t.Errorf("Overlay().RequiredImageSigners = %v, want %v", got, names)
	}
	var raw RestrictionLimits
	if err := json.Unmarshal([]byte(`{"required_image_signers":["corp-ci"]}`), &raw); err != nil || raw.RequiredImageSigners == nil {
		t.Errorf("decode: %v %+v", err, raw)
	}
	var ov LimitsOverlay
	if err := json.Unmarshal([]byte(`{"required_image_signers":["corp-ci"]}`), &ov); err != nil || ov.RequiredImageSigners == nil {
		t.Errorf("overlay decode: %v %+v", err, ov)
	}
}
