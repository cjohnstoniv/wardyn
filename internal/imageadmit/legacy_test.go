// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/imageadmit"
	"github.com/cjohnstoniv/wardyn/internal/releasesig"
	"github.com/cjohnstoniv/wardyn/internal/releasesig/releasesigtest"
)

const dockerRef = "registry.corp.example/team/app"

// layer is l as the registry client hands it over: the descriptor from the tag manifest and the blob.
func layer(l releasesigtest.LegacyLayer) imageadmit.LegacyLayer {
	sum := sha256.Sum256(l.Blob)
	return imageadmit.LegacyLayer{Descriptor: imageadmit.Descriptor{MediaType: l.MediaType, Digest: "sha256:" + hex.EncodeToString(sum[:]),
		Size: int64(len(l.Blob)), Annotations: l.Annotations}, Blob: l.Blob}
}

func layers(ls ...releasesigtest.LegacyLayer) []imageadmit.LegacyLayer {
	out := make([]imageadmit.LegacyLayer, len(ls))
	for i, l := range ls {
		out[i] = layer(l)
	}
	return out
}

// checkLegacyAt runs the site's check on image at digest over legacy layers alone.
func checkLegacyAt(t *testing.T, p *imageadmit.Policy, image string, ls ...releasesigtest.LegacyLayer) imageadmit.Verdict {
	t.Helper()
	req, refused := p.Require(image, true, nil)
	if refused != nil {
		return *refused
	}
	return imageadmit.CheckCandidates(req, digest, imageadmit.Candidates{Legacy: layers(ls...)})
}

func checkLegacy(t *testing.T, p *imageadmit.Policy, ls ...releasesigtest.LegacyLayer) imageadmit.Verdict {
	t.Helper()
	return checkLegacyAt(t, p, ref, ls...)
}

func sigPayload() []byte { return releasesigtest.SimpleSigningPayload(dockerRef, digest) }

func TestLegacyKeylessSignaturePasses(t *testing.T) {
	s := releasesigtest.New(t)
	v := checkLegacy(t, compile(t, s, corpCI()), s.LegacySignature(sigPayload(), ciOpts()))
	wantReason(t, v, "")
	if v.Signer != "corp-ci" || v.Identity != ciIdentity || v.Issuer != ciIssuer || v.Digest != digest {
		t.Fatalf("verdict = %+v", v)
	}
}

func TestLegacySignatureForAnotherDigestFails(t *testing.T) {
	s := releasesigtest.New(t)
	other := releasesigtest.SimpleSigningPayload(dockerRef, "sha256:"+otherHex)
	v := checkLegacy(t, compile(t, s, corpCI()), s.LegacySignature(other, ciOpts()))
	wantReason(t, v, imageadmit.SignatureMissing)
	mustContain(t, v.Detail, "signs another image")
}

func TestLegacySignaturePayloadOfAnotherTypeFails(t *testing.T) {
	s := releasesigtest.New(t)
	payload, err := json.Marshal(map[string]any{"critical": map[string]any{"image": map[string]any{"docker-manifest-digest": digest},
		"type": "something else"}})
	if err != nil {
		t.Fatal(err)
	}
	wantReason(t, checkLegacy(t, compile(t, s, corpCI()), s.LegacySignature(payload, ciOpts())), imageadmit.SignatureMissing)
}

func TestLegacySETFromAnotherLogKeyFails(t *testing.T) {
	s, other := releasesigtest.New(t), releasesigtest.New(t)
	forged := other.ResignLegacySET(s.LegacySignature(sigPayload(), ciOpts()))
	wantReason(t, checkLegacy(t, compile(t, s, corpCI()), forged), imageadmit.SignatureMissing)
}

func TestLegacySignatureWithoutTlogEntryFails(t *testing.T) {
	s := releasesigtest.New(t)
	o := ciOpts()
	o.NoTlog = true
	wantReason(t, checkLegacy(t, compile(t, s, corpCI()), s.LegacySignature(sigPayload(), o)), imageadmit.SignatureMissing)
}

// Shipped-policy parity: a root with CT logs requires the certificate's SCT for a legacy signature too.
func TestLegacySignatureWithoutSCTFails(t *testing.T) {
	s := releasesigtest.New(t)
	o := ciOpts()
	o.NoSCT = true
	wantReason(t, checkLegacy(t, compile(t, s, corpCI()), s.LegacySignature(sigPayload(), o)), imageadmit.SignatureMissing)
}

func TestLegacySignatureByAnotherIdentityIsInvalid(t *testing.T) {
	s := releasesigtest.New(t)
	o := ciOpts()
	o.Identity = "https://github.com/corp/images/.github/workflows/build.yml@refs/heads/feature"
	v := checkLegacy(t, compile(t, s, corpCI()), s.LegacySignature(sigPayload(), o))
	wantReason(t, v, imageadmit.SignatureInvalid)
	if v.Found != o.Identity || v.Want != ciIdentity {
		t.Fatalf("found %q want %q, verdict %+v", v.Found, v.Want, v)
	}
}

// Swapping the certificate annotation for the trusted identity's certificate does not move the signature to
// that identity: the signature and the Rekor entry are bound to the key that made them.
func TestLegacySwappedCertificateAnnotationFails(t *testing.T) {
	s := releasesigtest.New(t)
	o := ciOpts()
	o.Identity = "https://github.com/attacker/x/.github/workflows/x.yml@refs/heads/main"
	forged := s.LegacySignature(sigPayload(), o)
	trusted := s.LegacySignature(releasesigtest.SimpleSigningPayload(dockerRef, "sha256:"+otherHex), ciOpts())
	forged.Annotations[releasesigtest.AnnotationCertificate] = trusted.Annotations[releasesigtest.AnnotationCertificate]
	v := checkLegacy(t, compile(t, s, corpCI()), forged)
	if v.Admitted() || v.Identity == ciIdentity {
		t.Fatalf("a swapped certificate admitted the image: %+v", v)
	}
	wantReason(t, v, imageadmit.SignatureMissing)
}

// The chain annotation comes from the registry, so it never adds trust: a certificate from another CA fails
// even when the annotation names that CA and the trusted logs logged it.
func TestLegacyChainAnnotationIsNotTrusted(t *testing.T) {
	s, other := releasesigtest.New(t), releasesigtest.New(t)
	v := checkLegacy(t, compile(t, s, corpCI()), other.LoggingTo(s).LegacySignature(sigPayload(), ciOpts()))
	wantReason(t, v, imageadmit.SignatureMissing)
	mustContain(t, v.Detail, "leaf certificate")
}

// vendorKeyTlog is vendorKey with transparency_log set.
func vendorKeyTlog(k *releasesigtest.Key, tlog bool) imageadmit.SignerSpec {
	spec := vendorKey(k)
	spec.TransparencyLog = &tlog
	return spec
}

func TestLegacyKeySignaturePasses(t *testing.T) {
	s := releasesigtest.New(t)
	k := s.NewKey()
	v := checkLegacy(t, compile(t, s, vendorKeyTlog(k, true)), k.LegacySignature(sigPayload(), releasesigtest.Options{}))
	wantReason(t, v, "")
	if v.Signer != "vendor-key" || v.KeyID != k.Hint() {
		t.Fatalf("verdict = %+v", v)
	}
}

func TestLegacyKeySignatureOfflinePasses(t *testing.T) {
	s := releasesigtest.New(t)
	k := s.NewKey()
	offline := k.LegacySignature(sigPayload(), releasesigtest.Options{NoTlog: true})
	wantReason(t, checkLegacy(t, compile(t, s, vendorKeyTlog(k, false)), offline), "")
	// With the transparency log required, the same signature has no Rekor entry, so it fails.
	wantReason(t, checkLegacy(t, compile(t, s, vendorKeyTlog(k, true)), offline), imageadmit.SignatureMissing)
}

func TestLegacySignatureByAnotherKeyFails(t *testing.T) {
	s := releasesigtest.New(t)
	k, other := s.NewKey(), s.NewKey()
	wantReason(t, checkLegacy(t, compile(t, s, vendorKeyTlog(k, true)), other.LegacySignature(sigPayload(), releasesigtest.Options{})),
		imageadmit.SignatureMissing)
}

func TestLegacyAttestationSatisfiesRequiredAttestation(t *testing.T) {
	s := releasesigtest.New(t)
	spec := corpCI()
	spec.RequireAttestations = []string{imageadmit.PredicateSLSAProvenanceV1}
	p := compile(t, s, spec)
	sig := s.LegacySignature(sigPayload(), ciOpts())

	v := checkLegacy(t, p, sig)
	wantReason(t, v, imageadmit.AttestationMissing)
	if v.Want != imageadmit.PredicateSLSAProvenanceV1 {
		t.Fatalf("want = %q", v.Want)
	}

	att := s.LegacyAttestation(statement(t, imageadmit.PredicateSLSAProvenanceV1, imageHex), ciOpts())
	v = checkLegacy(t, p, sig, att)
	wantReason(t, v, "")
	if !contains(v.Attestations, imageadmit.PredicateSLSAProvenanceV1) {
		t.Fatalf("attestations = %v", v.Attestations)
	}
}

func TestLegacyAttestationForAnotherDigestFails(t *testing.T) {
	s := releasesigtest.New(t)
	att := s.LegacyAttestation(statement(t, imageadmit.PredicateSLSAProvenanceV1, otherHex), ciOpts())
	wantReason(t, checkLegacy(t, compile(t, s, corpCI()), att), imageadmit.SignatureMissing)
}

// A legacy SBOM attestation by the signer's identity is about the image, not a signature of it.
func TestLegacySBOMAttestationIsNotASignature(t *testing.T) {
	s := releasesigtest.New(t)
	att := s.LegacyAttestation(statement(t, predicateCycloneDX, imageHex), ciOpts())
	wantReason(t, checkLegacy(t, compile(t, s, corpCI()), att), imageadmit.SignatureMissing)
}

// The built-in signer accepts the release workflow's legacy cosign signature as well as its provenance.
func TestBuiltinAcceptsLegacySignature(t *testing.T) {
	s := releasesigtest.New(t)
	const image = "ghcr.io/cjohnstoniv/wardynd:1.2.3"
	sig := s.LegacySignature(releasesigtest.SimpleSigningPayload("ghcr.io/cjohnstoniv/wardynd", digest), releaseOpts("1.2.3"))
	v := checkLegacyAt(t, compile(t, s), image, sig)
	wantReason(t, v, "")
	if v.Signer != imageadmit.BuiltinSigner || v.Identity != releasesig.Identity("1.2.3") {
		t.Fatalf("verdict = %+v", v)
	}
	// The same workflow at another tag is not this build's release.
	other := s.LegacySignature(releasesigtest.SimpleSigningPayload("ghcr.io/cjohnstoniv/wardynd", digest), releaseOpts("1.2.2"))
	wantReason(t, checkLegacyAt(t, compile(t, s), image, other), imageadmit.SignatureInvalid)
}

func TestBuiltinLegacySBOMAttestationIsNotASignature(t *testing.T) {
	s := releasesigtest.New(t)
	att := s.LegacyAttestation(statement(t, predicateCycloneDX, imageHex), releaseOpts("1.2.3"))
	wantReason(t, checkLegacyAt(t, compile(t, s), "ghcr.io/cjohnstoniv/wardynd:1.2.3", att), imageadmit.SignatureMissing)
}

// A referrer bundle and a legacy layer are candidates of one check: the signature from one and the required
// attestation from the other admit together.
func TestBundleAndLegacyCandidatesCombine(t *testing.T) {
	s := releasesigtest.New(t)
	spec := corpCI()
	spec.RequireAttestations = []string{predicateCycloneDX}
	req, refused := compile(t, s, spec).Require(ref, true, nil)
	if refused != nil {
		t.Fatalf("Require: %+v", *refused)
	}
	sbom := s.SignDSSE(statement(t, predicateCycloneDX, imageHex), ciOpts())
	v := imageadmit.CheckCandidates(req, digest, imageadmit.Candidates{Bundles: [][]byte{sbom},
		Legacy: layers(s.LegacySignature(sigPayload(), ciOpts()))})
	wantReason(t, v, "")
}

func TestLegacyBlobNotMatchingItsDescriptorFails(t *testing.T) {
	s := releasesigtest.New(t)
	l := layer(s.LegacySignature(sigPayload(), ciOpts()))
	l.Descriptor.Digest = "sha256:" + otherHex
	req, refused := compile(t, s, corpCI()).Require(ref, true, nil)
	if refused != nil {
		t.Fatalf("Require: %+v", *refused)
	}
	wantReason(t, imageadmit.CheckCandidates(req, digest, imageadmit.Candidates{Legacy: []imageadmit.LegacyLayer{l}}), imageadmit.SignatureMissing)
}

func TestLegacyOversizeBlobFails(t *testing.T) {
	s := releasesigtest.New(t)
	// Valid JSON naming the digest, padded past the bound: only the size refuses it.
	payload := append(sigPayload(), bytes.Repeat([]byte(" "), imageadmit.MaxBundleBytes)...)
	wantReason(t, checkLegacy(t, compile(t, s, corpCI()), s.LegacySignature(payload, ciOpts())), imageadmit.SignatureMissing)
}

// Only the first MaxCandidatesPerSigner legacy layers are verified.
func TestLegacyLayersAreBounded(t *testing.T) {
	s := releasesigtest.New(t)
	p := compile(t, s, corpCI())
	junk := s.LegacySignature(releasesigtest.SimpleSigningPayload(dockerRef, "sha256:"+otherHex), ciOpts())
	good := s.LegacySignature(sigPayload(), ciOpts())
	var ls []releasesigtest.LegacyLayer
	for range imageadmit.MaxCandidatesPerSigner - 1 {
		ls = append(ls, junk)
	}
	wantReason(t, checkLegacy(t, p, append(ls, good)...), "")
	wantReason(t, checkLegacy(t, p, append(append(ls, junk), good)...), imageadmit.SignatureMissing)
}

// fakeLegacy serves legacy tag manifests by suffix and blobs by digest.
type fakeLegacy struct {
	tags      map[string]*imageadmit.Manifest
	blobs     map[string][]byte
	tagErr    error
	blobReads int
}

func (f *fakeLegacy) LegacyTag(_ context.Context, _ imageadmit.Ref, subject, suffix string) (*imageadmit.Manifest, error) {
	if subject != digest {
		return nil, imageadmit.ErrNotFound
	}
	if f.tagErr != nil {
		return nil, f.tagErr
	}
	if m := f.tags[suffix]; m != nil {
		return m, nil
	}
	return nil, imageadmit.ErrNotFound
}

// Blob refuses a descriptor over the bound without reading, as RegistryClient.Blob does.
func (f *fakeLegacy) Blob(_ context.Context, _ imageadmit.Ref, d imageadmit.Descriptor) ([]byte, error) {
	if d.Size > imageadmit.MaxBlobBytes {
		return nil, imageadmit.ErrTooLarge
	}
	f.blobReads++
	return f.blobs[d.Digest], nil
}

func (f *fakeLegacy) serve(suffix string, ls ...releasesigtest.LegacyLayer) {
	m := &imageadmit.Manifest{}
	for _, l := range layers(ls...) {
		m.Layers = append(m.Layers, l.Descriptor)
		f.blobs[l.Descriptor.Digest] = l.Blob
	}
	f.tags[suffix] = m
}

func newFakeLegacy() *fakeLegacy {
	return &fakeLegacy{tags: map[string]*imageadmit.Manifest{}, blobs: map[string][]byte{}}
}

func TestReadLegacyReadsBothTags(t *testing.T) {
	s := releasesigtest.New(t)
	f := newFakeLegacy()
	f.serve(".sig", s.LegacySignature(sigPayload(), ciOpts()))
	repo := imageadmit.Ref{Registry: "registry.corp.example", Repo: "team/app"}

	got, err := imageadmit.ReadLegacy(context.Background(), f, repo, digest)
	if err != nil || len(got) != 1 || got[0].Descriptor.MediaType != imageadmit.LegacySignatureMediaType {
		t.Fatalf("no .att tag: got %d layers, err %v", len(got), err)
	}

	f.serve(".att", s.LegacyAttestation(statement(t, imageadmit.PredicateSLSAProvenanceV1, imageHex), ciOpts()))
	got, err = imageadmit.ReadLegacy(context.Background(), f, repo, digest)
	if err != nil || len(got) != 2 || got[1].Descriptor.MediaType != imageadmit.LegacyAttestationMediaType {
		t.Fatalf("both tags: got %d layers, err %v", len(got), err)
	}
	spec := corpCI()
	spec.RequireAttestations = []string{imageadmit.PredicateSLSAProvenanceV1}
	req, refused := compile(t, s, spec).Require(ref, true, nil)
	if refused != nil {
		t.Fatalf("Require: %+v", *refused)
	}
	wantReason(t, imageadmit.CheckCandidates(req, digest, imageadmit.Candidates{Legacy: got}), "")

	got, err = imageadmit.ReadLegacy(context.Background(), f, repo, "sha256:"+otherHex)
	if err != nil || len(got) != 0 {
		t.Fatalf("an unsigned digest: got %d layers, err %v", len(got), err)
	}
}

func TestReadLegacyIsBounded(t *testing.T) {
	s := releasesigtest.New(t)
	f := newFakeLegacy()
	var ls []releasesigtest.LegacyLayer
	for range imageadmit.MaxCandidatesPerSigner + 4 {
		ls = append(ls, s.LegacySignature(sigPayload(), ciOpts()))
	}
	f.serve(".sig", ls...)
	f.serve(".att", s.LegacyAttestation(statement(t, imageadmit.PredicateSLSAProvenanceV1, imageHex), ciOpts()))
	got, err := imageadmit.ReadLegacy(context.Background(), f, imageadmit.Ref{Registry: "registry.corp.example", Repo: "team/app"}, digest)
	if err != nil || len(got) != imageadmit.MaxCandidatesPerSigner || f.blobReads != imageadmit.MaxCandidatesPerSigner {
		t.Fatalf("got %d layers and %d blob reads (err %v), want %d", len(got), f.blobReads, err, imageadmit.MaxCandidatesPerSigner)
	}
}

func TestReadLegacyReturnsRegistryErrors(t *testing.T) {
	f := newFakeLegacy()
	f.tagErr = &imageadmit.Unavailable{Cause: imageadmit.CauseForbidden, Host: "registry.corp.example", Registry: "registry.corp.example"}
	_, err := imageadmit.ReadLegacy(context.Background(), f, imageadmit.Ref{Registry: "registry.corp.example", Repo: "team/app"}, digest)
	var u *imageadmit.Unavailable
	if !errors.As(err, &u) || u.Cause != imageadmit.CauseForbidden {
		t.Fatalf("err = %v, want the registry's 403", err)
	}
}

// A layer over the blob bound (the release workflow's SBOM attestation is a few MB) is not read and does not
// stop the check: the other tag's signature still admits, and the layer explains itself when nothing else does.
func TestReadLegacySkipsOversizeLayers(t *testing.T) {
	s := releasesigtest.New(t)
	f := newFakeLegacy()
	f.serve(".sig", s.LegacySignature(sigPayload(), ciOpts()))
	f.tags[".att"] = &imageadmit.Manifest{Layers: []imageadmit.Descriptor{{MediaType: imageadmit.LegacyAttestationMediaType,
		Digest: "sha256:" + otherHex, Size: imageadmit.MaxBlobBytes + 1}}}
	got, err := imageadmit.ReadLegacy(context.Background(), f, imageadmit.Ref{Registry: "registry.corp.example", Repo: "team/app"}, digest)
	if err != nil || len(got) != 2 || f.blobReads != 1 {
		t.Fatalf("got %d layers and %d blob reads, err %v; want 2 layers and 1 read", len(got), f.blobReads, err)
	}
	req, refused := compile(t, s, corpCI()).Require(ref, true, nil)
	if refused != nil {
		t.Fatalf("Require: %+v", *refused)
	}
	wantReason(t, imageadmit.CheckCandidates(req, digest, imageadmit.Candidates{Legacy: got}), "")
	v := imageadmit.CheckCandidates(req, digest, imageadmit.Candidates{Legacy: got[1:]})
	wantReason(t, v, imageadmit.SignatureMissing)
	mustContain(t, v.Detail, "larger than")
}

// A bundle and a legacy layer count against the same bound: one signer verifies at most
// MaxCandidatesPerSigner candidates in all, bundles first.
func TestBundlesAndLegacyLayersShareOneBound(t *testing.T) {
	s := releasesigtest.New(t)
	p := compile(t, s, corpCI())
	req, refused := p.Require(ref, true, nil)
	if refused != nil {
		t.Fatal(refused)
	}
	junk := make([][]byte, imageadmit.MaxCandidatesPerSigner-1)
	for i := range junk {
		junk[i] = []byte(`{}`)
	}
	good := s.LegacySignature(sigPayload(), ciOpts())
	check := func(nJunk int) imageadmit.Verdict {
		return imageadmit.CheckCandidates(req, digest, imageadmit.Candidates{Bundles: junk[:nJunk], Legacy: layers(good)})
	}
	wantReason(t, check(0), "")
	wantReason(t, check(len(junk)), "")
	over := append(junk, []byte(`{}`))
	wantReason(t, imageadmit.CheckCandidates(req, digest, imageadmit.Candidates{Bundles: over, Legacy: layers(good)}), imageadmit.SignatureMissing)
}
