// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"crypto/sha256"
	"encoding/base32"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const goOIDCImportPath = "github.com/coreos/go-oidc/v3/oidc"

// portalConfigPackages are the packages whose own Config type a portal file may name: go-oidc's verifier
// settings, and the standard library's TLS settings for the portal transport.
var portalConfigPackages = map[string]bool{goOIDCImportPath: true, "crypto/tls": true}

// portalBannedIdents are names a portal file never uses: the console's sign-in configuration and role
// derivation, and go-oidc's weakening fields.
var portalBannedIdents = map[string]bool{
	"cfg": true, "RoleMap": true, "RoleMappings": true, "DefaultRole": true, "LegacyAdminEmails": true,
	"UserTypes": true, "deriveLogin": true, "deriveRole": true, "admit": true, "resolvePerson": true,
	"admitIdentity": true, "PreviewRole": true,
	"SkipIssuerCheck": true, "SkipExpiryCheck": true, "InsecureSkipSignatureCheck": true,
	"InsecureSkipVerify": true, "InsecureIssuerURLContext": true, "NewProvider": true,
}

// portalGuardViolations parses one source file and lists the banned uses in it. Config is banned unqualified
// (the console's type) and allowed only as a name selected from go-oidc or crypto/tls, resolved from the import
// path and not the alias; Now is banned as a config field.
func portalGuardViolations(t *testing.T, name string, src any) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	configPkg := map[string]bool{}
	for _, imp := range file.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		if !portalConfigPackages[path] {
			continue
		}
		local := path[strings.LastIndex(path, "/")+1:]
		if path == goOIDCImportPath {
			local = "oidc"
		}
		if imp.Name != nil {
			local = imp.Name.Name
		}
		configPkg[local] = true
	}
	var out []string
	okSel := map[*ast.Ident]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok && x.Sel.Name == "Config" && configPkg[id.Name] {
				okSel[x.Sel] = true
			}
		case *ast.KeyValueExpr:
			if id, ok := x.Key.(*ast.Ident); ok && id.Name == "Now" {
				out = append(out, name+": go-oidc Now field at "+fset.Position(id.Pos()).String())
			}
		case *ast.Ident:
			if portalBannedIdents[x.Name] || (x.Name == "Config" && !okSel[x]) {
				out = append(out, name+": "+x.Name+" at "+fset.Position(x.Pos()).String())
			}
		}
		return true
	})
	return out
}

func TestPortalIssuer_GuardNamesNoConsoleConfig(t *testing.T) {
	files, err := filepath.Glob("portal_*.go")
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, f := range files {
		if !strings.HasSuffix(f, "_test.go") {
			found = append(found, f)
		}
	}
	sort.Strings(found)
	for _, need := range []string{"portal_issuer.go", "portal_fetch.go", "portal_keyset.go", "portal_profile.go", "portal_admission.go"} {
		if !slices.Contains(found, need) {
			t.Errorf("the guard's file set %v lacks %s", found, need)
		}
	}
	for _, f := range found {
		if v := portalGuardViolations(t, f, nil); len(v) > 0 {
			t.Errorf("portal file names banned identifiers: %v", v)
		}
	}

	// The guard catches what it claims to.
	planted := `package x
import gooidc "github.com/coreos/go-oidc/v3/oidc"
import other "example.com/other"
var ok = &gooidc.Config{SkipClientIDCheck: true}
var bad1 = &gooidc.Config{Now: nil, SkipExpiryCheck: true}
type bad2 Config
var bad3 other.Config
var cfg int
func f() { admit(); deriveLogin() }
`
	got := portalGuardViolations(t, "planted.go", planted)
	for _, want := range []string{"Now", "SkipExpiryCheck", "Config at planted.go:6", "Config at planted.go:7", "cfg", "admit", "deriveLogin"} {
		if !slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, want) }) {
			t.Errorf("planted source: no violation mentions %q in %v", want, got)
		}
	}
	if slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, "planted.go:4") }) {
		t.Errorf("a go-oidc Config literal with no weakening field was flagged: %v", got)
	}
}

func TestPortalIssuer_PrincipalFormat(t *testing.T) {
	const issuer = "https://example.okta.com/oauth2/default"
	sum := sha256.Sum256([]byte("wardyn-portal-v2\n" + issuer))
	wantK := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:]))[:26]
	if got := PortalIssuerKey(issuer); got != wantK || len(got) != 26 {
		t.Fatalf("k = %q, want %q (26 chars)", got, wantK)
	}
	p := PortalPrincipal(issuer, "abc")
	if p != "delegate:person:"+wantK+":mfrgg" {
		t.Fatalf("principal = %q", p)
	}
	re := regexp.MustCompile(`^delegate:person:[a-z2-7]{26}:[a-z2-7]+$`)
	if !re.MatchString(PortalPrincipal(issuer, "00u1ABCD~!")) {
		t.Fatalf("principal %q has the wrong shape", PortalPrincipal(issuer, "00u1ABCD~!"))
	}
	if n := len(PortalPrincipal(issuer, strings.Repeat("A", 128))); n > 248 {
		t.Fatalf("a 128-byte person id gives a %d-character principal; the limit is 248", n)
	}
}

func TestPortalIssuer_PrincipalCaseVariantsDistinct(t *testing.T) {
	const issuer = "https://example.okta.com"
	a, b := PortalPrincipal(issuer, "Alice"), PortalPrincipal(issuer, "alice")
	if a == b {
		t.Fatal("ids that differ only in case gave one principal")
	}
	if strings.ToLower(a) != a || strings.ToLower(b) != b || strings.ToLower(a) == strings.ToLower(b) {
		t.Fatalf("principals are not case-stable and distinct under lowercasing: %q %q", a, b)
	}
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	for _, id := range []string{"00uAbC", "00uabc"} {
		c := h.accessClaims()
		c["uid"] = id
		if got := h.mustAdmit(h.token(c)).Principal; got != PortalPrincipal(h.reg.Issuer, id) {
			t.Fatalf("verified principal %q for %q", got, id)
		}
	}
}

func TestPortalIssuer_ExactIssuerBytesHashed(t *testing.T) {
	base := "https://example.okta.com/oauth2/a"
	seen := map[string]string{}
	for _, iss := range []string{base, base + "/", "https://EXAMPLE.okta.com/oauth2/a", "https://example.okta.com:443/oauth2/a", "http://example.okta.com/oauth2/a"} {
		k := PortalIssuerKey(iss)
		if prev, dup := seen[k]; dup {
			t.Fatalf("issuers %q and %q share the key %q", prev, iss, k)
		}
		seen[k] = iss
	}
	if PortalIssuerKey(base) != PortalIssuerKey(base) {
		t.Fatal("the key is not deterministic")
	}
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	subj := h.mustAdmit(h.token(h.accessClaims()))
	if subj.Issuer != h.reg.Issuer || subj.K != PortalIssuerKey(h.reg.Issuer) {
		t.Fatalf("subject names issuer %q key %q, want the registered bytes %q", subj.Issuer, subj.K, h.reg.Issuer)
	}
}

func TestPortalIssuer_SubShapeRefused(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "")
	cases := []struct {
		name string
		id   string
		ok   bool
	}{
		{"128 bytes", strings.Repeat("a", 128), true},
		{"one byte", "x", true},
		{"printable punctuation", "u~!#$%&'()*+,-./:;<=>?@[]^_`{|}", true},
		{"empty", "", false},
		{"129 bytes", strings.Repeat("a", 129), false},
		{"space inside", "a b", false},
		{"tab", "a\tb", false},
		{"newline", "a\nb", false},
		{"non-ASCII", "café", false},
		{"control", "a\x01b", false},
		{"delete", "a\x7fb", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			claims := h.accessClaims()
			claims["uid"] = c.id
			_, r := h.verify(h.token(claims))
			switch {
			case c.ok && !r.OK():
				t.Fatalf("refused: %+v", r)
			case !c.ok && c.id == "" && r.Reason != SubjectTokenNoUser:
				t.Fatalf("empty id: %+v, want %s", r, SubjectTokenNoUser)
			case !c.ok && c.id != "" && r.Reason != SubjectTokenInvalid:
				t.Fatalf("refusal = %+v, want %s", r, SubjectTokenInvalid)
			}
		})
	}
}

func TestPortalVerifier_TokenIssuerExactAfterVerify(t *testing.T) {
	// go-oidc accepts "accounts.google.com" for "https://accounts.google.com"; the portal verifier does not.
	h := newPortalHarness(t, "accounts.google.com", false, "")
	good := h.accessClaims()
	h.mustAdmit(h.token(good))
	scheme := h.accessClaims()
	scheme["iss"] = "accounts.google.com"
	h.wantRefusal(h.token(scheme), SubjectTokenInvalid)
	slash := h.accessClaims()
	slash["iss"] = h.reg.Issuer + "/"
	h.wantRefusal(h.token(slash), SubjectTokenInvalid)
}

func TestPortalVerifier_RefreshFailureKeepsLastGood(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	h.mustAdmit(h.token(h.accessClaims()))
	h.clock.Advance(time.Hour)
	h.idp.set(func(f *fakeIdP) { f.discStatus = 500 })
	tok := func() string { return h.token(h.accessClaims()) }
	h.wantUnavailable(tok(), portalCauseUnreachable)
	v, err := h.pi.verifierFor(h.reg)
	if err != nil {
		t.Fatal(err)
	}
	if v.disc == nil || len(v.ks.keys) == 0 {
		t.Fatal("the last good discovery result and key set were not kept")
	}
	// Kept, but not used: every token answers 503 until a refresh passes.
	h.clock.Advance(5 * time.Second)
	h.wantUnavailable(tok(), portalCauseIssuerDown)
	h.clock.Advance(portalKeyCooldown)
	h.wantUnavailable(tok(), portalCauseUnreachable)
	h.idp.set(func(f *fakeIdP) { f.discStatus = 0 })
	h.clock.Advance(portalKeyCooldown)
	h.mustAdmit(tok())
}

func TestPortalVerifier_RefreshToOffOriginDropsVerifier(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	h.mustAdmit(h.token(h.accessClaims()))
	h.clock.Advance(time.Hour)
	h.idp.set(func(f *fakeIdP) { f.jwksURI = "https://other.example.org/oauth2/a/keys" })
	tok := func() string { return h.token(h.accessClaims()) }
	h.wantUnavailable(tok(), portalCauseKeysOffOrigin)
	v, err := h.pi.verifierFor(h.reg)
	if err != nil {
		t.Fatal(err)
	}
	if v.disc != nil || len(v.ks.keys) != 0 {
		t.Fatal("a document that turned hostile was kept as the last good result")
	}
	// The same answer is repeated inside the cooldown with no new request, then a passing refresh recovers.
	disc, _ := h.idp.hits()
	h.clock.Advance(5 * time.Second)
	h.wantUnavailable(tok(), portalCauseKeysOffOrigin)
	if after, _ := h.idp.hits(); after != disc {
		t.Fatalf("discovery was fetched again inside the cooldown (%d -> %d)", disc, after)
	}
	h.idp.set(func(f *fakeIdP) { f.jwksURI = h.reg.Issuer + "/keys" })
	h.clock.Advance(portalKeyCooldown)
	h.mustAdmit(tok())
}
