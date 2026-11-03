// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

const imgPrefix = "registry.example.com/team/*"

// imgDigest is a well-formed sha256 digest; a malformed one fails to parse and
// would test the parser rather than the prefix rule.
const imgDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestImagePrefixMatch pins what a prefix image grant covers: a repository
// strictly under the prefix at a segment boundary, whatever its tag or digest.
func TestImagePrefixMatch(t *testing.T) {
	for _, tc := range []struct {
		name, grant, want string
		matches           bool
	}{
		{"tag", imgPrefix, "registry.example.com/team/app:1", true},
		{"no tag", imgPrefix, "registry.example.com/team/app", true},
		{"digest", imgPrefix, "registry.example.com/team/app@" + imgDigest, true},
		{"tag and digest", imgPrefix, "registry.example.com/team/app:1@" + imgDigest, true},
		{"nested path", imgPrefix, "registry.example.com/team/sub/app:1", true},
		{"host spelled in capitals is not normalised", imgPrefix, "REGISTRY.example.com/team/app:1", false},
		{"tag on a middle segment", imgPrefix, "registry.example.com/team/app:1/x", false},
		{"slash after the digest marker", imgPrefix, "registry.example.com/team/app@x/y", false},
		{"whitespace in the wanted ref", imgPrefix, "registry.example.com/team/app :1", false},
		{"star in the wanted ref", imgPrefix, "registry.example.com/team/a*", false},
		{"the prefix itself, no more segment", imgPrefix, "registry.example.com/team", false},
		{"the prefix with a tag, no more segment", imgPrefix, "registry.example.com/team:1", false},
		{"sibling sharing the leading letters", imgPrefix, "registry.example.com/teamx/app", false},
		{"path case differs", imgPrefix, "registry.example.com/Team/app", false},
		{"other host", imgPrefix, "other.example.com/team/app", false},
		{"dot segment in the wanted ref", imgPrefix, "registry.example.com/team/../x/app", false},
		{"empty segment in the wanted ref", imgPrefix, "registry.example.com/team//app", false},
		{"wanted ref is itself a prefix", imgPrefix, "registry.example.com/team/*", false},
		{"nothing wanted", imgPrefix, "", false},
		{"host with port", "localhost:5000/team/*", "localhost:5000/team/app:1", true},
		{"other port", "localhost:5000/team/*", "localhost:5001/team/app:1", false},
		{"no port", "localhost:5000/team/*", "localhost/team/app:1", false},
		{"bare localhost", "localhost/team/*", "localhost/team/app:1", true},
		{"whole registry", "registry.example.com/*", "registry.example.com/a:1", true},
		{"whole registry, host alone wanted", "registry.example.com/*", "registry.example.com", false},
		{"hub prefix, ref as typed", "docker.io/library/*", "docker.io/library/ubuntu:22.04", true},
		{"hub prefix does not cover a bare name", "docker.io/library/*", "ubuntu:22.04", false},
		{"hub prefix does not cover the alias host", "docker.io/library/*", "index.docker.io/library/ubuntu", false},
		{"hub prefix, other namespace", "docker.io/library/*", "docker.io/acme/app", false},
		// Unchanged behaviour.
		{"exact ref, same", "ghcr.io/a/b:1", "ghcr.io/a/b:1", true},
		{"exact ref, other tag", "ghcr.io/a/b:1", "ghcr.io/a/b:2", false},
		{"exact ref, case differs", "ghcr.io/a/b:1", "GHCR.io/a/b:1", false},
		{"bare wildcard", capWildcard, "ghcr.io/a/b:1", true},
		{"bare wildcard, dot segment is capGranted's refusal, not the matcher's", capWildcard, "ghcr.io/../b", true},
		// Rows stored before the prefix form, which the write boundary now
		// refuses, match nothing at all (not even their own literal text).
		{"half-wildcard legacy row", "registry.example.com/te*", "registry.example.com/team/app", false},
		{"half-wildcard legacy row, own text", "registry.example.com/te*", "registry.example.com/te*", false},
		{"no-host legacy row", "team/*", "team/app", false},
		{"no-host legacy row, own text", "team/*", "team/*", false},
		{"legacy row with a tag", "registry.example.com/team/*:1", "registry.example.com/team/app:1", false},
		{"legacy row with a tag, own text", "registry.example.com/team/*:1", "registry.example.com/team/*:1", false},
		{"legacy dot-segment prefix row", "registry.example.com/team/../*", "registry.example.com/team/../x", false},
		{"legacy uppercase prefix row", "registry.example.com/Team/*", "registry.example.com/Team/app", false},
		{"legacy middle-star row", "registry.example.com/*/x", "registry.example.com/a/x", false},
		{"legacy middle-star row, own text", "registry.example.com/*/x", "registry.example.com/*/x", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := capValueMatches(capImage, tc.grant, tc.want); got != tc.matches {
				t.Errorf("capValueMatches(image, %q, %q) = %v, want %v", tc.grant, tc.want, got, tc.matches)
			}
		})
	}
}

// TestImagePrefixOverlap pins the deny question, in both directions: a deny
// bites when the sets intersect.
func TestImagePrefixOverlap(t *testing.T) {
	for _, tc := range []struct {
		name, deny, want string
		overlaps         bool
	}{
		{"prefix deny, exact ref under it", imgPrefix, "registry.example.com/team/app:1", true},
		{"prefix deny, exact ref outside", imgPrefix, "registry.example.com/teamx/app:1", false},
		{"prefix deny, the same prefix", imgPrefix, imgPrefix, true},
		{"prefix deny, nested prefix", imgPrefix, "registry.example.com/team/sub/*", true},
		{"nested prefix deny, outer prefix", "registry.example.com/team/sub/*", imgPrefix, true},
		{"prefix deny, whole registry", imgPrefix, "registry.example.com/*", true},
		{"whole-registry deny, narrower prefix", "registry.example.com/*", imgPrefix, true},
		{"prefix deny, boundary sibling prefix", imgPrefix, "registry.example.com/teamx/*", false},
		{"boundary sibling prefix deny", "registry.example.com/teamx/*", imgPrefix, false},
		{"prefix deny, other host prefix", imgPrefix, "other.example.com/team/*", false},
		{"exact deny, prefix grant that matches it", "registry.example.com/team/app:1", imgPrefix, true},
		{"exact deny, prefix grant that does not", "registry.example.com/teamx/app:1", imgPrefix, false},
		{"exact deny, the prefix itself", "registry.example.com/team", imgPrefix, false},
		{"prefix deny, dot-segment ref", imgPrefix, "registry.example.com/team/../x/app", false},
		{"legacy uppercase prefix deny", "registry.example.com/Team/*", "registry.example.com/Team/app", false},
		{"legacy no-host prefix deny", "team/*", "team/app", false},
		// Unchanged behaviour.
		{"exact deny, same ref", "ghcr.io/a/b:1", "ghcr.io/a/b:1", true},
		{"exact deny, other ref", "ghcr.io/a/b:1", "ghcr.io/a/b:2", false},
		{"wildcard deny", capWildcard, "ghcr.io/a/b:1", true},
		{"exact deny, wildcard want", "ghcr.io/a/b:1", capWildcard, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := capValueOverlaps(capImage, tc.deny, tc.want); got != tc.overlaps {
				t.Errorf("capValueOverlaps(image, %q, %q) = %v, want %v", tc.deny, tc.want, got, tc.overlaps)
			}
		})
	}
}

// TestImagePrefixIsImageOnly: no other kind reads "/*" as a prefix.
func TestImagePrefixIsImageOnly(t *testing.T) {
	for _, kind := range []string{capSecret, capWorkspace, capAgent, capFeature, capPolicy, capComponent} {
		if capValueMatches(kind, imgPrefix, "registry.example.com/team/app") {
			t.Errorf("%s: matched a prefix; only image takes the form", kind)
		}
		if !capValueOverlaps(kind, imgPrefix, imgPrefix) {
			t.Errorf("%s: identical values must still overlap as exact ids", kind)
		}
	}
}

// TestImagePrefixWriteTime pins the write boundary: the accepted forms are
// stored verbatim, everything else with a "*" is refused.
func TestImagePrefixWriteTime(t *testing.T) {
	for _, tc := range []struct {
		in string
		ok bool
	}{
		{imgPrefix, true},
		{"registry.example.com/*", true},
		{"registry.example.com/team/sub/*", true},
		{"localhost/team/*", true},
		{"localhost:5000/team/*", true},
		{"ghcr.io/acme/*", true},
		{"  registry.example.com/team/*  ", true},
		{capWildcard, true},
		{"ghcr.io/a/b:1", true},
		{"*/x", false},
		{"*/*", false},
		{"/*", false},
		{"registry.example.com/*/x", false},
		{"registry.example.com/te*", false},
		{"registry.example.com/team*/*", false},
		{"registry.example.com/**", false},
		{"registry.example.com/team/**/*", false},
		{"team/*", false},
		{"library/*", false},
		{"registry.example.com/team/../*", false},
		{"registry.example.com/team/./*", false},
		{"registry.example.com//team/*", false},
		{"registry.example.com/team//*", false},
		{"registry.example.com/team/*:tag", false},
		{"registry.example.com/team/*@" + imgDigest, false},
		{"registry.example.com/team:1/*", false},
		{"registry.example.com/team@" + imgDigest + "/*", false},
		{"registry.example.com/Team/*", false},
		{"Registry.example.com/team/*", false},
		{"registry.example.com/team /*", false},
		{"registry.example.com\\team/*", false},
		{"localhost:5000:1/team/*", false},
		{"localhost:port/team/*", false},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := canonicalGrantValue(capImage, tc.in)
			if tc.ok {
				if err != nil {
					t.Fatalf("canonicalGrantValue(image, %q) refused: %v", tc.in, err)
				}
				if want := strings.TrimSpace(tc.in); got != want {
					t.Errorf("canonicalGrantValue(image, %q) = %q, want it stored verbatim as %q", tc.in, got, want)
				}
				return
			}
			if err == nil {
				t.Errorf("canonicalGrantValue(image, %q) = %q, want a refusal", tc.in, got)
			}
		})
	}
	if _, err := canonicalGrantValue(capSecret, imgPrefix); err == nil {
		t.Error("a secret grant with a '/*' value must still be refused by the secret grammar")
	}
}

// TestCapGrantedImagePrefix runs the prefix through the real resolver: an
// allow widens to the repositories under it and no further, a deny prefix
// beats a wildcard allow, and the dot-segment refusal still comes first.
func TestCapGrantedImagePrefix(t *testing.T) {
	for _, tc := range []struct {
		name   string
		grants []types.CapabilityGrant
		ref    string
		want   bool
	}{
		{"allow prefix, ref under it", []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capImage, imgPrefix, types.CapabilityAllow)}, "registry.example.com/team/app:1", true},
		{"allow prefix, ref beside it", []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capImage, imgPrefix, types.CapabilityAllow)}, "registry.example.com/teamx/app:1", false},
		{"allow prefix, dot segment", []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capImage, imgPrefix, types.CapabilityAllow)}, "registry.example.com/team/../x/app", false},
		{"deny prefix beats wildcard allow", []types.CapabilityGrant{
			grant(types.CapabilitySubjectAll, "", capImage, capWildcard, types.CapabilityAllow),
			grant(types.CapabilitySubjectAll, "", capImage, imgPrefix, types.CapabilityDeny),
		}, "registry.example.com/team/app:1", false},
		{"deny prefix leaves the rest of a wildcard allow", []types.CapabilityGrant{
			grant(types.CapabilitySubjectAll, "", capImage, capWildcard, types.CapabilityAllow),
			grant(types.CapabilitySubjectAll, "", capImage, imgPrefix, types.CapabilityDeny),
		}, "registry.example.com/teamx/app:1", true},
		{"deny exact beats allow prefix", []types.CapabilityGrant{
			grant(types.CapabilitySubjectAll, "", capImage, imgPrefix, types.CapabilityAllow),
			grant(types.CapabilitySubjectAll, "", capImage, "registry.example.com/team/bad:1", types.CapabilityDeny),
		}, "registry.example.com/team/bad:1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &capStore{grants: tc.grants, enf: map[string]bool{capImage: true}}
			got, err := capServer(st).capGranted(memberCtx(nil), capImage, tc.ref)
			if err != nil || got != tc.want {
				t.Fatalf("capGranted(%q) = %v, %v; want %v", tc.ref, got, err, tc.want)
			}
		})
	}
}

// TestImagePrefixNeverSatisfiesARestriction: "Available to" stays exact. A
// restricted image is closed to everyone but an allow naming that exact value,
// so a prefix (or a wildcard) allow lists nobody for it.
func TestImagePrefixNeverSatisfiesARestriction(t *testing.T) {
	const ref = "registry.example.com/team/app:1"
	restricted := map[string]map[string]bool{capImage: {ref: true}}
	for _, tc := range []struct {
		name   string
		grants []types.CapabilityGrant
		enf    bool
		want   bool
	}{
		{"prefix allow, restricted, switch on", []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capImage, imgPrefix, types.CapabilityAllow)}, true, false},
		{"prefix allow, restricted, switch off", []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capImage, imgPrefix, types.CapabilityAllow)}, false, false},
		{"wildcard allow, restricted", []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capImage, capWildcard, types.CapabilityAllow)}, true, false},
		{"prefix allow plus an allow naming the ref", []types.CapabilityGrant{
			grant(types.CapabilitySubjectAll, "", capImage, imgPrefix, types.CapabilityAllow),
			grant(types.CapabilitySubjectAll, "", capImage, ref, types.CapabilityAllow),
		}, true, true},
		{"an unrestricted sibling under the same prefix", []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capImage, imgPrefix, types.CapabilityAllow)}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &capStore{grants: tc.grants, enf: map[string]bool{capImage: tc.enf}, restricted: restricted}
			got, err := capServer(st).capGranted(memberCtx(nil), capImage, ref)
			if err != nil || got != tc.want {
				t.Fatalf("capGranted(%q) = %v, %v; want %v", ref, got, err, tc.want)
			}
		})
	}
	// The sibling the restriction does not name is covered by the prefix alone.
	st := &capStore{
		grants:     []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capImage, imgPrefix, types.CapabilityAllow)},
		enf:        map[string]bool{capImage: true},
		restricted: restricted,
	}
	if ok, err := capServer(st).capGranted(memberCtx(nil), capImage, "registry.example.com/team/other:1"); err != nil || !ok {
		t.Errorf("an unrestricted image under the prefix = %v, %v; want granted", ok, err)
	}
}

// TestCapAllowedForSubImagePrefix covers the second scan loop (a revive knows
// the owner by sub alone and walks ListCapabilityGrants itself): the same
// prefix, deny and restriction answers as capGranted.
func TestCapAllowedForSubImagePrefix(t *testing.T) {
	const owner = "sub-bob"
	const ref = "registry.example.com/team/app:1"
	allowPrefix := grant(types.CapabilitySubjectUser, owner, capImage, imgPrefix, types.CapabilityAllow)
	for _, tc := range []struct {
		name       string
		grants     []types.CapabilityGrant
		ref        string
		restricted map[string]map[string]bool
		want       bool
	}{
		{"allow prefix covers the ref", []types.CapabilityGrant{allowPrefix}, ref, nil, true},
		{"allow prefix, sibling path", []types.CapabilityGrant{allowPrefix}, "registry.example.com/teamx/app:1", nil, false},
		{"deny prefix beats wildcard allow", []types.CapabilityGrant{
			grant(types.CapabilitySubjectAll, "", capImage, capWildcard, types.CapabilityAllow),
			grant(types.CapabilitySubjectAll, "", capImage, imgPrefix, types.CapabilityDeny),
		}, ref, nil, false},
		{"deny exact beats allow prefix", []types.CapabilityGrant{
			allowPrefix, grant(types.CapabilitySubjectAll, "", capImage, ref, types.CapabilityDeny),
		}, ref, nil, false},
		{"allow prefix, restricted ref", []types.CapabilityGrant{allowPrefix}, ref, map[string]map[string]bool{capImage: {ref: true}}, false},
		{"allow naming the restricted ref", []types.CapabilityGrant{
			allowPrefix, grant(types.CapabilitySubjectUser, owner, capImage, ref, types.CapabilityAllow),
		}, ref, map[string]map[string]bool{capImage: {ref: true}}, true},
		{"legacy star row allows nothing", []types.CapabilityGrant{
			grant(types.CapabilitySubjectUser, owner, capImage, "registry.example.com/te*", types.CapabilityAllow),
		}, "registry.example.com/te*", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &capStore{grants: tc.grants, enf: map[string]bool{capImage: true}, restricted: tc.restricted}
			got, err := capServer(st).capAllowedForSub(memberCtx(nil), owner, "", capImage, tc.ref)
			if err != nil || got != tc.want {
				t.Fatalf("capAllowedForSub(%q) = %v, %v; want %v", tc.ref, got, err, tc.want)
			}
		})
	}
}

// TestStoredImageRowsAcrossTheUpgrade: rows are seeded straight into the store,
// as an install that wrote them before the prefix form existed would hold
// them, bypassing the write validation. A well-formed "<host>/<path>/*" row,
// which matched nothing until now, matches by prefix; a row the write boundary
// would refuse today matches nothing, not even its own text.
func TestStoredImageRowsAcrossTheUpgrade(t *testing.T) {
	for _, tc := range []struct {
		name, row, ref string
		want           bool
	}{
		{"valid row, ref under it", imgPrefix, "registry.example.com/team/app:1", true},
		{"valid row, sibling path", imgPrefix, "registry.example.com/teamx/app", false},
		{"valid row, the prefix itself", imgPrefix, "registry.example.com/team", false},
		{"refused row (no host), ref under it", "team/*", "team/app:1", false},
		{"refused row (no host), own text", "team/*", "team/*", false},
		{"refused row (middle star), own text", "registry.example.com/*/x", "registry.example.com/*/x", false},
		{"refused row (uppercase), ref under it", "registry.example.com/Team/*", "registry.example.com/Team/app", false},
		{"refused row (tag), own text", "registry.example.com/team/*:1", "registry.example.com/team/*:1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := canonicalGrantValue(capImage, tc.row); (err == nil) != (tc.name[:5] == "valid") {
				t.Fatalf("canonicalGrantValue(%q) err = %v; the fixture's valid/refused label is wrong", tc.row, err)
			}
			st := &capStore{
				grants: []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capImage, tc.row, types.CapabilityAllow)},
				enf:    map[string]bool{capImage: true},
			}
			got, err := capServer(st).capGranted(memberCtx(nil), capImage, tc.ref)
			if err != nil || got != tc.want {
				t.Fatalf("capGranted(%q) with stored row %q = %v, %v; want %v", tc.ref, tc.row, got, err, tc.want)
			}
		})
	}
}
