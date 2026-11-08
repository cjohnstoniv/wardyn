// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/imageadmit"
	"github.com/cjohnstoniv/wardyn/internal/releasesig/releasesigtest"
)

func pemKey(t *testing.T, pub any) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func TestCompileRefusesBadPolicies(t *testing.T) {
	s := releasesigtest.New(t)
	rootJSON, err := testRoot(t, s).MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"/etc/wardyn/good.json": rootJSON, "/etc/wardyn/junk.json": []byte(`{"mediaType":`)}
	read := func(p string) ([]byte, error) {
		if b, ok := files[p]; ok {
			return b, nil
		}
		return nil, os.ErrNotExist
	}
	rsa1024, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	p521, err := ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyless := func(mut func(*imageadmit.KeylessSpec)) imageadmit.SignerSpec {
		spec := corpCI()
		k := *spec.Keyless
		mut(&k)
		spec.Keyless = &k
		return spec
	}
	signer := func(mut func(*imageadmit.SignerSpec)) imageadmit.SignerSpec {
		spec := corpCI()
		mut(&spec)
		return spec
	}
	good := imageadmit.RootSpec{Name: "github-private", Path: "/etc/wardyn/good.json"}
	cases := map[string]imageadmit.Config{
		"unanchored regexp":       {Signers: []imageadmit.SignerSpec{keyless(func(k *imageadmit.KeylessSpec) { k.Identity, k.IdentityRegexp = "", "https://github.com/corp/.*" })}},
		"regexp without $":        {Signers: []imageadmit.SignerSpec{keyless(func(k *imageadmit.KeylessSpec) { k.Identity, k.IdentityRegexp = "", "^https://github.com/corp/.*" })}},
		"uncompilable regexp":     {Signers: []imageadmit.SignerSpec{keyless(func(k *imageadmit.KeylessSpec) { k.Identity, k.IdentityRegexp = "", "^https://(corp$" })}},
		"identity and regexp":     {Signers: []imageadmit.SignerSpec{keyless(func(k *imageadmit.KeylessSpec) { k.IdentityRegexp = "^https://github.com/corp/.*$" })}},
		"neither identity":        {Signers: []imageadmit.SignerSpec{keyless(func(k *imageadmit.KeylessSpec) { k.Identity = "" })}},
		"http issuer":             {Signers: []imageadmit.SignerSpec{keyless(func(k *imageadmit.KeylessSpec) { k.Issuer = "http://issuer.example" })}},
		"undefined root":          {Signers: []imageadmit.SignerSpec{keyless(func(k *imageadmit.KeylessSpec) { k.TrustedRoot = "nowhere" })}},
		"keyless and key":         {Signers: []imageadmit.SignerSpec{signer(func(s *imageadmit.SignerSpec) { s.PublicKey = pemKey(t, &p521.PublicKey) })}},
		"neither keyless nor key": {Signers: []imageadmit.SignerSpec{signer(func(s *imageadmit.SignerSpec) { s.Keyless = nil })}},
		"unparsable key": {Signers: []imageadmit.SignerSpec{signer(func(s *imageadmit.SignerSpec) {
			s.Keyless, s.PublicKey = nil, "-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----\n"
		})}},
		"rsa 1024":                {Signers: []imageadmit.SignerSpec{signer(func(s *imageadmit.SignerSpec) { s.Keyless, s.PublicKey = nil, pemKey(t, &rsa1024.PublicKey) })}},
		"p-521":                   {Signers: []imageadmit.SignerSpec{signer(func(s *imageadmit.SignerSpec) { s.Keyless, s.PublicKey = nil, pemKey(t, &p521.PublicKey) })}},
		"no images":               {Signers: []imageadmit.SignerSpec{signer(func(s *imageadmit.SignerSpec) { s.Images = nil })}},
		"hostless prefix":         {Signers: []imageadmit.SignerSpec{signer(func(s *imageadmit.SignerSpec) { s.Images = []string{"team/*"} })}},
		"star inside a segment":   {Signers: []imageadmit.SignerSpec{signer(func(s *imageadmit.SignerSpec) { s.Images = []string{"registry.corp.example/te*m/*"} })}},
		"empty attestation":       {Signers: []imageadmit.SignerSpec{signer(func(s *imageadmit.SignerSpec) { s.RequireAttestations = []string{""} })}},
		"relative predicate":      {Signers: []imageadmit.SignerSpec{signer(func(s *imageadmit.SignerSpec) { s.SignaturePredicates = []string{"approved"} })}},
		"empty name":              {Signers: []imageadmit.SignerSpec{signer(func(s *imageadmit.SignerSpec) { s.Name = "" })}},
		"reserved signer name":    {Signers: []imageadmit.SignerSpec{signer(func(s *imageadmit.SignerSpec) { s.Name = imageadmit.BuiltinSigner })}},
		"reserved root as signer": {Signers: []imageadmit.SignerSpec{signer(func(s *imageadmit.SignerSpec) { s.Name = imageadmit.PublicGood })}},
		"duplicate signer":        {Signers: []imageadmit.SignerSpec{corpCI(), corpCI()}},
		"reserved root name":      {TrustedRoots: []imageadmit.RootSpec{{Name: imageadmit.PublicGood, Path: "/etc/wardyn/good.json"}}},
		"duplicate root":          {TrustedRoots: []imageadmit.RootSpec{good, good}},
		"relative root path":      {TrustedRoots: []imageadmit.RootSpec{{Name: "r", Path: "good.json"}}},
		"unreadable root":         {TrustedRoots: []imageadmit.RootSpec{{Name: "r", Path: "/etc/wardyn/missing.json"}}},
		"unparsable root":         {TrustedRoots: []imageadmit.RootSpec{{Name: "r", Path: "/etc/wardyn/junk.json"}}},
	}
	for name, cfg := range cases {
		if _, err := imageadmit.Compile(cfg, imageadmit.Options{Version: "1.2.3", PublicGoodRoot: testRoot(t, s), ReadFile: read}); err == nil {
			t.Errorf("%s: Compile accepted %+v", name, cfg)
		} else if !errors.Is(err, imageadmit.ErrPolicy) {
			t.Errorf("%s: error %v does not wrap ErrPolicy", name, err)
		}
	}
	okCfg := imageadmit.Config{TrustedRoots: []imageadmit.RootSpec{good}, Signers: []imageadmit.SignerSpec{
		keyless(func(k *imageadmit.KeylessSpec) {
			k.Identity, k.IdentityRegexp, k.TrustedRoot = "", "^https://github.com/corp/.*$", "github-private"
		}),
		{Name: "rsa", Images: []string{"docker.io/vendor/app:1"}, PublicKey: pemKey(t, &mustRSA(t, 2048).PublicKey)},
	}}
	if _, err := imageadmit.Compile(okCfg, imageadmit.Options{Version: "1.2.3", PublicGoodRoot: testRoot(t, s), ReadFile: read}); err != nil {
		t.Fatalf("Compile refused a good policy: %v", err)
	}
}

func mustRSA(t *testing.T, bits int) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestRequireCoverage(t *testing.T) {
	s := releasesigtest.New(t)
	exact := imageadmit.SignerSpec{Name: "exact", Images: []string{"docker.io/vendor/app:1"}, Keyless: corpCI().Keyless}
	p := compile(t, s, corpCI(), exact)
	covered := map[string]string{
		"registry.corp.example/team/app:1.4":                    "corp-ci",
		"registry.corp.example/team/a/b@sha256:" + imageHex:     "corp-ci",
		"registry.corp.example/team/app:1.4@sha256:" + imageHex: "corp-ci",
		"docker.io/vendor/app:1":                                "exact",
	}
	for r, want := range covered {
		req, refused := p.Require(r, true, nil)
		if refused != nil || len(req.AnyOf) != 1 || req.AnyOf[0].Name() != want || len(req.AllOf) != 0 {
			t.Errorf("Require(%q) = %+v, %+v; want any of [%s]", r, req, refused, want)
		}
	}
	// No admin-authored entry covers these, so the check refuses without reading any registry.
	for _, r := range []string{
		"169.254.169.254/x:y", "internal.host/x", "registry.corp.example/other/app", "registry.corp.example/team",
		"REGISTRY.corp.example/team/app", "registry.corp.example:443/team/app", "docker.io/vendor/app:2", "vendor/app:1",
		"registry.corp.example/team/../x", "registry.corp.example/team/a b",
	} {
		_, refused := p.Require(r, true, nil)
		if refused == nil || refused.Reason != imageadmit.NoPolicy || refused.Signer != "" || refused.Undefined {
			t.Errorf("Require(%q) refusal = %+v, want site no_policy", r, refused)
		}
	}
}

func TestRequireProfileSigners(t *testing.T) {
	s := releasesigtest.New(t)
	other := imageadmit.SignerSpec{Name: "other", Images: []string{"registry.other.example/*"}, Keyless: corpCI().Keyless}
	p := compile(t, s, corpCI(), other)
	_, refused := p.Require(ref, true, []string{"gone"})
	if refused == nil || refused.Reason != imageadmit.NoPolicy || refused.Signer != "gone" || !refused.Undefined {
		t.Fatalf("undefined profile signer: %+v", refused)
	}
	_, refused = p.Require(ref, true, []string{"other"})
	if refused == nil || refused.Reason != imageadmit.NoPolicy || refused.Signer != "other" || refused.Undefined {
		t.Fatalf("profile signer that does not cover the image: %+v", refused)
	}
	// With the site's own coverage off, only the profile's signers apply.
	req, refused := p.Require(ref, false, []string{"corp-ci"})
	if refused != nil || len(req.AnyOf) != 0 || len(req.AllOf) != 1 || req.AllOf[0].Name() != "corp-ci" {
		t.Fatalf("profile only: %+v, %+v", req, refused)
	}
	if _, refused = p.Require(ref, false, nil); refused == nil || refused.Reason != imageadmit.NoPolicy {
		t.Fatalf("nothing to check must refuse, got %+v", refused)
	}
}

func TestFingerprintFollowsPolicyRootAndProfile(t *testing.T) {
	s := releasesigtest.New(t)
	rootA, err := testRoot(t, s).MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	rootB, err := testRoot(t, releasesigtest.New(t)).MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	current := rootA
	read := func(string) ([]byte, error) { return current, nil }
	spec := corpCI()
	spec.Keyless.TrustedRoot = "private"
	fp := func(signer imageadmit.SignerSpec, site bool, profile []string) [32]byte {
		t.Helper()
		p, err := imageadmit.Compile(imageadmit.Config{TrustedRoots: []imageadmit.RootSpec{{Name: "private", Path: "/etc/wardyn/root.json"}},
			Signers: []imageadmit.SignerSpec{signer}}, imageadmit.Options{Version: "1.2.3", PublicGoodRoot: testRoot(t, s), ReadFile: read})
		if err != nil {
			t.Fatal(err)
		}
		req, refused := p.Require(ref, site, profile)
		if refused != nil {
			t.Fatalf("Require: %+v", *refused)
		}
		return req.Fingerprint()
	}
	base := fp(spec, true, nil)
	if fp(spec, true, nil) != base {
		t.Fatal("the same policy gave two fingerprints")
	}
	changed := spec
	k := *spec.Keyless
	k.Identity += "x"
	changed.Keyless = &k
	if fp(changed, true, nil) == base {
		t.Error("an identity change kept the fingerprint")
	}
	if fp(withAttestation(spec, imageadmit.PredicateSLSAProvenanceV1), true, nil) == base {
		t.Error("an attestation change kept the fingerprint")
	}
	if fp(spec, true, []string{"corp-ci"}) == base {
		t.Error("a profile change kept the fingerprint")
	}
	current = rootB
	if fp(spec, true, nil) == base {
		t.Error("a trust root change at the same path kept the fingerprint")
	}
}

// The reason a root could not be read comes from the reader, which may be the caller's: the refusal says
// which root, never the reader's own text.
func TestCompileRootReadErrorIsAFixedMessage(t *testing.T) {
	s := releasesigtest.New(t)
	read := func(string) ([]byte, error) { return nil, errors.New("secret-from-the-reader") }
	_, err := imageadmit.Compile(imageadmit.Config{TrustedRoots: []imageadmit.RootSpec{{Name: "r", Path: "/etc/wardyn/r.json"}}},
		imageadmit.Options{Version: "1.2.3", PublicGoodRoot: testRoot(t, s), ReadFile: read})
	if err == nil || !errors.Is(err, imageadmit.ErrPolicy) || strings.Contains(err.Error(), "secret-from-the-reader") {
		t.Fatalf("err = %v, want an ErrPolicy that names the root and not the reader's text", err)
	}
}
