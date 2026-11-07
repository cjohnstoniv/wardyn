// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit_test

import (
	"testing"

	"github.com/sigstore/sigstore-go/pkg/root"

	"github.com/cjohnstoniv/wardyn/internal/imageadmit"
	"github.com/cjohnstoniv/wardyn/internal/releasesig/releasesigtest"
)

// A private Sigstore may timestamp signatures with an RFC 3161 TSA instead of logging them in Rekor. cosign
// keeps that timestamp in the layer's rfc3161timestamp annotation.

// privatePolicy compiles corp-ci against tr, an admin's trust root file.
func privatePolicy(t *testing.T, s *releasesigtest.Signer, tr *root.TrustedRoot) *imageadmit.Policy {
	t.Helper()
	j, err := tr.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	spec := corpCI()
	spec.Keyless.TrustedRoot = "private"
	p, err := imageadmit.Compile(imageadmit.Config{TrustedRoots: []imageadmit.RootSpec{{Name: "private", Path: "/etc/wardyn/private.json"}},
		Signers: []imageadmit.SignerSpec{spec}}, imageadmit.Options{Version: "1.2.3", PublicGoodRoot: testRoot(t, s),
		ReadFile: func(string) ([]byte, error) { return j, nil }})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return p
}

// timestamped signs with a TSA timestamp and no Rekor entry.
func timestamped() releasesigtest.Options {
	o := ciOpts()
	o.NoTlog, o.RFC3161 = true, true
	return o
}

func TestLegacyTimestampedSignatureOnATSAOnlyRootPasses(t *testing.T) {
	s := releasesigtest.New(t)
	p := privatePolicy(t, s, s.TrustedRootWithTSA(false))
	v := checkLegacy(t, p, s.LegacySignature(sigPayload(), timestamped()))
	wantReason(t, v, "")
	if v.Signer != "corp-ci" || v.Identity != ciIdentity || v.Issuer != ciIssuer {
		t.Fatalf("verdict = %+v", v)
	}
	// With neither a timestamp nor a log entry there is no time to check the certificate at.
	o := ciOpts()
	o.NoTlog = true
	wantReason(t, checkLegacy(t, p, s.LegacySignature(sigPayload(), o)), imageadmit.SignatureMissing)
}

func TestLegacyTimestampFromAnotherTSAFails(t *testing.T) {
	s := releasesigtest.New(t)
	sig := s.WithOtherTSA().LegacySignature(sigPayload(), timestamped())
	v := checkLegacy(t, privatePolicy(t, s, s.TrustedRootWithTSA(false)), sig)
	wantReason(t, v, imageadmit.SignatureMissing)
	mustContain(t, v.Detail, "unable to verify signed timestamps")
}

// A root that names a Rekor log still requires the log entry: a TSA timestamp does not stand in for it.
func TestLegacyTimestampedSignatureOnARekorRootFails(t *testing.T) {
	s := releasesigtest.New(t)
	v := checkLegacy(t, privatePolicy(t, s, s.TrustedRootWithTSA(true)), s.LegacySignature(sigPayload(), timestamped()))
	wantReason(t, v, imageadmit.SignatureMissing)
	mustContain(t, v.Detail, "not enough verified log entries")
}
