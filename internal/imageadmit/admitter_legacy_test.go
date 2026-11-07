// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit_test

import (
	"errors"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/imageadmit"
	"github.com/cjohnstoniv/wardyn/internal/releasesig/releasesigtest"
)

// An image signed only the cosign v2 way (a layer on its sha256-<hex>.sig tag, no referrer) is checked through
// the registry like a referrer bundle.

// legacyImage is the test image at tag 1.4 with no referrers and l on its .sig tag, behind a fresh admitter.
func legacyImage(t *testing.T, l releasesigtest.LegacyLayer) (*fakeRegistry, *imageadmit.Admitter) {
	t.Helper()
	f := newFake()
	f.image("1.4", digest)
	f.legacyTag(".sig", layer(l))
	c := &clock{now: time.Unix(1_800_000_000, 0)}
	return f, imageadmit.NewAdmitter(f, imageadmit.AdmitterOptions{Now: c.Now})
}

func TestAdmitLegacySignatureAlone(t *testing.T) {
	s := releasesigtest.New(t)
	f, a := legacyImage(t, s.LegacySignature(sigPayload(), ciOpts()))
	v, err := admit(t, a, compile(t, s, corpCI()), ref)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	wantReason(t, v, "")
	if v.Digest != digest || v.Signer != "corp-ci" || v.Identity != ciIdentity || v.Issuer != ciIssuer {
		t.Fatalf("verdict = %+v", v)
	}
	if f.n("LegacyTag:"+digest+".sig") != 1 {
		t.Fatalf(".sig tag reads = %d, want 1", f.n("LegacyTag:"+digest+".sig"))
	}
}

func TestAdmitLegacySignatureByAnotherSignerIsInvalid(t *testing.T) {
	s := releasesigtest.New(t)
	o := ciOpts()
	o.Identity = "https://github.com/corp/images/.github/workflows/build.yml@refs/heads/feature"
	_, a := legacyImage(t, s.LegacySignature(sigPayload(), o))
	v, err := admit(t, a, compile(t, s, corpCI()), ref)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	wantReason(t, v, imageadmit.SignatureInvalid)
	if v.Found != o.Identity || v.Want != ciIdentity {
		t.Fatalf("found %q want %q, verdict %+v", v.Found, v.Want, v)
	}
}

// A legacy tag the registry cannot serve fails the whole check, as a referrer read does, and the verdict is
// not kept: the next check reads again and admits.
func TestAdmitLegacyReadFailureIsUnavailableAndNotKept(t *testing.T) {
	s := releasesigtest.New(t)
	f, a := legacyImage(t, s.LegacySignature(sigPayload(), ciOpts()))
	p := compile(t, s, corpCI())
	f.setErr("LegacyTag", &imageadmit.Unavailable{Cause: imageadmit.CauseForbidden, Host: "registry.corp.example", Registry: "registry.corp.example"})
	v, err := admit(t, a, p, ref)
	wantReason(t, v, imageadmit.AdmissionUnavailable)
	var u *imageadmit.Unavailable
	if !errors.As(err, &u) || v.Detail != "checking images from registry.corp.example got 403" {
		t.Fatalf("verdict %+v, error %v", v, err)
	}
	f.setErr("LegacyTag", nil)
	if v, err := admit(t, a, p, ref); err != nil || !v.Admitted() || v.Cached {
		t.Fatalf("after the registry recovered = %+v, %v; want a fresh admission", v, err)
	}
}

// A legacy layer whose blob the registry has lost is one bad candidate, skipped as a broken referrer is: the
// signature beside it still admits.
func TestAdmitLegacyLayerWithoutItsBlobIsSkipped(t *testing.T) {
	s := releasesigtest.New(t)
	f, a := legacyImage(t, s.LegacySignature(sigPayload(), ciOpts()))
	f.legacyTag(".att", imageadmit.LegacyLayer{Descriptor: imageadmit.Descriptor{MediaType: imageadmit.LegacyAttestationMediaType,
		Digest: "sha256:" + otherHex, Size: 10}})
	v, err := admit(t, a, compile(t, s, corpCI()), ref)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	wantReason(t, v, "")
}

// A legacy tag manifest that cannot be parsed is one bad source, skipped as a malformed referrer is: the .sig
// beside it still admits. A registry failure on the same tag still fails the check (above).
func TestAdmitMalformedLegacyTagIsSkipped(t *testing.T) {
	s := releasesigtest.New(t)
	f, a := legacyImage(t, s.LegacySignature(sigPayload(), ciOpts()))
	f.setErr("LegacyTag:"+digest+".att", errors.New("manifest is not JSON"))
	v, err := admit(t, a, compile(t, s, corpCI()), ref)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	wantReason(t, v, "")
}
