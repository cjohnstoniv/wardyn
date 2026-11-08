// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/imageadmit"
	"github.com/cjohnstoniv/wardyn/internal/releasesig/releasesigtest"
)

// The admitter reads the image's signatures from its registry, checks them against the signers that cover the
// image, and keeps the verdict for the image digest and those signers.

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// signedImage is the test image at tag 1.4, signed by the corp CI identity, behind a fresh admitter.
func signedImage(t *testing.T) (*releasesigtest.Signer, *fakeRegistry, *imageadmit.Admitter, *clock) {
	t.Helper()
	s := releasesigtest.New(t)
	f := newFake()
	f.image("1.4", digest)
	f.bundle(s.SignDSSE(statement(t, imageadmit.PredicateCosignSign, imageHex), ciOpts()))
	c := &clock{now: time.Unix(1_800_000_000, 0)}
	return s, f, imageadmit.NewAdmitter(f, imageadmit.AdmitterOptions{Now: c.Now}), c
}

func TestAdmitKeylessSignatureThroughTheRegistry(t *testing.T) {
	s, f, a, _ := signedImage(t)
	v, err := admit(t, a, compile(t, s, corpCI()), ref)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	wantReason(t, v, "")
	if v.Digest != digest || v.Signer != "corp-ci" || v.Identity != ciIdentity || v.Issuer != ciIssuer || v.Cached {
		t.Fatalf("verdict = %+v", v)
	}
	for _, r := range f.refs {
		if r.Registry != "registry.corp.example" || r.Repo != "team/app" {
			t.Fatalf("a read named %+v, not the image's repository", r)
		}
	}
	if f.n("Manifest:1.4") != 1 {
		t.Fatalf("tag reads = %d, want 1", f.n("Manifest:1.4"))
	}
}

// No admin-authored signer covers the image, so nothing is read from any registry: a member cannot make the
// control plane contact a host by naming it.
func TestAdmitUncoveredImageMakesNoRegistryCalls(t *testing.T) {
	s, f, a, _ := signedImage(t)
	p := compile(t, s, corpCI())
	for _, uncovered := range []string{"169.254.169.254/x:y", "internal.host/x", "registry.corp.example/other/app:1"} {
		v, err := admit(t, a, p, uncovered)
		if err != nil {
			t.Fatalf("%s: %v", uncovered, err)
		}
		wantReason(t, v, imageadmit.NoPolicy)
		// A caller that skips Require's refusal still gets no read.
		v, err = a.Admit(context.Background(), imageadmit.Requirements{Ref: uncovered})
		if err != nil {
			t.Fatalf("%s: %v", uncovered, err)
		}
		wantReason(t, v, imageadmit.NoPolicy)
	}
	if n := f.total(); n != 0 {
		t.Fatalf("registry reads = %d, want 0", n)
	}
}

// The tag is resolved on every check; the verdict is kept for the digest it resolved to.
func TestAdmitResolvesTheTagEveryTime(t *testing.T) {
	s, f, a, _ := signedImage(t)
	p := compile(t, s, corpCI())
	if v, _ := admit(t, a, p, ref); !v.Admitted() || v.Cached {
		t.Fatalf("first = %+v", v)
	}
	v, _ := admit(t, a, p, ref)
	if !v.Admitted() || !v.Cached {
		t.Fatalf("second = %+v, want a cached admission", v)
	}
	if f.n("Manifest:1.4") != 2 || f.n("Referrers") != 2 {
		t.Fatalf("tag reads = %d, referrer reads = %d; want 2 tag reads and one check's referrer reads (2)", f.n("Manifest:1.4"), f.n("Referrers"))
	}
	// The tag moves to an unsigned image: the cached admission is for the old digest, not the tag.
	f.image("1.4", "sha256:"+otherHex)
	v, _ = admit(t, a, p, ref)
	wantReason(t, v, imageadmit.SignatureMissing)
	if v.Digest != "sha256:"+otherHex || v.Cached {
		t.Fatalf("after the tag moved = %+v", v)
	}
}

// A digest reference whose verdict is kept needs no registry read at all (a revive with a warm cache).
func TestAdmitDigestReferenceCacheHitReadsNothing(t *testing.T) {
	s, f, a, _ := signedImage(t)
	p := compile(t, s, corpCI())
	byDigest := "registry.corp.example/team/app@" + digest
	if v, err := admit(t, a, p, byDigest); err != nil || !v.Admitted() {
		t.Fatalf("first = %+v, %v", v, err)
	}
	if f.n("Manifest:"+digest) != 1 {
		t.Fatalf("a digest reference was not fetched by digest on a miss: %v", f.calls)
	}
	before := f.total()
	if v, _ := admit(t, a, p, byDigest); !v.Admitted() || !v.Cached {
		t.Fatalf("second = %+v", v)
	}
	if f.total() != before {
		t.Fatalf("a cache hit read the registry: %d reads, want %d", f.total(), before)
	}
}

// The key is the requirements' fingerprint and the digest: a changed signer or trust root misses.
func TestAdmitCacheMissesOnPolicyOrRootChange(t *testing.T) {
	s, f, a, _ := signedImage(t)
	if v, _ := admit(t, a, compile(t, s, corpCI()), ref); !v.Admitted() {
		t.Fatalf("first = %+v", v)
	}
	changed := corpCI()
	changed.Images = append(changed.Images, "registry.corp.example/tools/app")
	if v, _ := admit(t, a, compile(t, s, changed), ref); !v.Admitted() || v.Cached {
		t.Fatalf("after a signer change = %+v, want a fresh admission", v)
	}
	other := releasesigtest.New(t)
	p, err := imageadmit.Compile(imageadmit.Config{Signers: []imageadmit.SignerSpec{corpCI()}},
		imageadmit.Options{Version: "1.2.3", PublicGoodRoot: testRoot(t, other)})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := admit(t, a, p, ref)
	if v.Cached {
		t.Fatalf("after a trust root change the verdict came from the cache: %+v", v)
	}
	wantReason(t, v, imageadmit.SignatureMissing) // the bundle is from the first root's CA
	if f.n("Referrers") != 6 {
		t.Fatalf("referrer reads = %d, want 3 checks x 2 artifact types", f.n("Referrers"))
	}
}

func TestAdmitPositiveVerdictKeptOneHour(t *testing.T) {
	s, f, a, c := signedImage(t)
	p := compile(t, s, corpCI())
	admit(t, a, p, ref)
	c.add(imageadmit.PositiveTTL - time.Second)
	if v, _ := admit(t, a, p, ref); !v.Cached {
		t.Fatalf("within the hour = %+v, want cached", v)
	}
	c.add(2 * time.Second)
	if v, _ := admit(t, a, p, ref); v.Cached || !v.Admitted() {
		t.Fatalf("after the hour = %+v, want a fresh admission", v)
	}
	if imageadmit.PositiveTTL != time.Hour || f.n("Referrers") != 4 {
		t.Fatalf("PositiveTTL = %v, referrer reads = %d", imageadmit.PositiveTTL, f.n("Referrers"))
	}
}

func TestAdmitNegativeVerdictKeptSixtySeconds(t *testing.T) {
	s := releasesigtest.New(t)
	f := newFake()
	f.image("1.4", digest) // no signature
	c := &clock{now: time.Unix(1_800_000_000, 0)}
	a := imageadmit.NewAdmitter(f, imageadmit.AdmitterOptions{Now: c.Now})
	p := compile(t, s, corpCI())
	v, _ := admit(t, a, p, ref)
	wantReason(t, v, imageadmit.SignatureMissing)
	c.add(imageadmit.NegativeTTL - time.Second)
	if v, _ := admit(t, a, p, ref); !v.Cached || v.Reason != imageadmit.SignatureMissing {
		t.Fatalf("within the minute = %+v, want a cached refusal", v)
	}
	c.add(2 * time.Second)
	if v, _ := admit(t, a, p, ref); v.Cached {
		t.Fatalf("after the minute = %+v, want a fresh check", v)
	}
	if imageadmit.NegativeTTL != time.Minute || f.n("Referrers") != 4 {
		t.Fatalf("NegativeTTL = %v, referrer reads = %d", imageadmit.NegativeTTL, f.n("Referrers"))
	}
}

func TestAdmitUnavailableIsNeverKept(t *testing.T) {
	s, f, a, _ := signedImage(t)
	p := compile(t, s, corpCI())
	f.setErr("Referrers", &imageadmit.Unavailable{Cause: imageadmit.CauseForbidden, Host: "registry.corp.example", Registry: "registry.corp.example"})
	v, err := admit(t, a, p, ref)
	wantReason(t, v, imageadmit.AdmissionUnavailable)
	var u *imageadmit.Unavailable
	if !errors.As(err, &u) || v.Detail != "checking images from registry.corp.example got 403" {
		t.Fatalf("verdict %+v, error %v", v, err)
	}
	f.setErr("Referrers", nil)
	if v, err := admit(t, a, p, ref); err != nil || !v.Admitted() || v.Cached {
		t.Fatalf("after the registry recovered = %+v, %v; want a fresh admission", v, err)
	}
}

// Each way a registry read fails names its cause in the 503 sentence, never a credential.
func TestAdmitUnavailableSentences(t *testing.T) {
	const h = "registry.corp.example"
	un := func(c imageadmit.Cause, cred bool) error {
		return &imageadmit.Unavailable{Cause: c, Host: h, Registry: h, HasCredential: cred, Err: errors.New("detail")}
	}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"401 without a credential", un(imageadmit.CauseUnauthorized, false), "checking images from " + h + " got 401 — no credential for it in WARDYN_REGISTRY_AUTH_FILE"},
		{"401 with a credential", un(imageadmit.CauseUnauthorized, true), "checking images from " + h + " got 401 for its credential in WARDYN_REGISTRY_AUTH_FILE"},
		{"401 where a redirect led", &imageadmit.Unavailable{Cause: imageadmit.CauseUnauthorized, Host: "cdn.example", Registry: h, HasCredential: true, Withheld: true},
			"checking images from " + h + " got 401 from an address it redirected to, where its credential is not sent"},
		{"403", un(imageadmit.CauseForbidden, true), "checking images from " + h + " got 403"},
		{"unreachable", un(imageadmit.CauseUnreachable, false), "a host met while checking images from " + h + " could not be reached"},
		{"untrusted TLS", un(imageadmit.CauseTLS, false), "a TLS certificate met while checking images from " + h + " is not trusted (WARDYN_TRUSTED_CA_FILE)"},
		{"timeout", un(imageadmit.CauseTimeout, false), "the check took longer than 20 seconds"},
		{"deadline", context.DeadlineExceeded, "the check took longer than 20 seconds"},
		{"server error", un(imageadmit.CauseServer, false), "checking images from " + h + " got an error answer"},
		{"refused target", un(imageadmit.CauseRefused, false), h + " or an address it named is not one Wardyn contacts"},
		{"no such tag", imageadmit.ErrNotFound, h + " has no image team/app:1.4 (check the name and tag)"},
		{"lying registry", imageadmit.ErrDigestMismatch, h + " answered content that does not match its digest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f, a, _ := signedImage(t)
			f.setErr("Manifest:1.4", tc.err)
			v, err := admit(t, a, compile(t, s, corpCI()), ref)
			wantReason(t, v, imageadmit.AdmissionUnavailable)
			if err == nil || v.Detail != tc.want {
				t.Fatalf("detail = %q (error %v), want %q", v.Detail, err, tc.want)
			}
		})
	}
}

// A referrers index that does not parse is the registry's content, not an outage: the image has no
// signature to read, and the refusal says so rather than that the registry could not be read.
func TestAdmitMalformedReferrersIndexIsAMissingSignature(t *testing.T) {
	s, f, a, _ := signedImage(t)
	f.setErr("Referrers", fmt.Errorf("%w: manifest is not JSON", imageadmit.ErrMalformed))
	v, err := admit(t, a, compile(t, s, corpCI()), ref)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	wantReason(t, v, imageadmit.SignatureMissing)
}

// Concurrent checks of one image under one policy share one set of signature reads.
func TestAdmitConcurrentChecksShareOneFlight(t *testing.T) {
	s, f, a, _ := signedImage(t)
	p := compile(t, s, corpCI())
	f.gate = make(chan struct{})
	const n = 8
	var wg sync.WaitGroup
	verdicts := make([]imageadmit.Verdict, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			verdicts[i], _ = admit(t, a, p, ref)
		}()
	}
	for f.n("Manifest:1.4") < n {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // let every check reach the flight
	close(f.gate)
	wg.Wait()
	for i, v := range verdicts {
		if !v.Admitted() {
			t.Fatalf("check %d = %+v", i, v)
		}
	}
	if f.n("Referrers:"+bundleV03) != 1 || f.n("Blob") != 1 {
		t.Fatalf("referrer reads = %d, blob reads = %d; want 1 each for %d concurrent checks", f.n("Referrers:"+bundleV03), f.n("Blob"), n)
	}
}

// One check reads at most MaxReferrers referrers, whatever the registry lists.
func TestAdmitReadsAtMost64Referrers(t *testing.T) {
	s := releasesigtest.New(t)
	f := newFake()
	f.image("1.4", digest)
	for range imageadmit.MaxReferrers + 1 {
		f.referrer(bundleV03, nil, nil) // a referrer manifest with no bundle layer
	}
	a := imageadmit.NewAdmitter(f, imageadmit.AdmitterOptions{})
	v, _ := admit(t, a, compile(t, s, corpCI()), ref)
	wantReason(t, v, imageadmit.SignatureMissing)
	if got := f.n("Manifest") - f.n("Manifest:1.4"); got != imageadmit.MaxReferrers {
		t.Fatalf("referrer manifests read = %d, want %d", got, imageadmit.MaxReferrers)
	}
}

// Only the bundles the check verifies are fetched.
func TestAdmitFetchesAtMost16Bundles(t *testing.T) {
	s := releasesigtest.New(t)
	f := newFake()
	f.image("1.4", digest)
	for i := range imageadmit.MaxCandidatesPerSigner + 4 {
		f.bundle([]byte(fmt.Sprintf(`{"junk":%d}`, i)))
	}
	a := imageadmit.NewAdmitter(f, imageadmit.AdmitterOptions{})
	v, _ := admit(t, a, compile(t, s, corpCI()), ref)
	wantReason(t, v, imageadmit.SignatureMissing)
	if f.n("Blob") != imageadmit.MaxCandidatesPerSigner {
		t.Fatalf("bundles fetched = %d, want %d", f.n("Blob"), imageadmit.MaxCandidatesPerSigner)
	}
}

// A blob that does not match its descriptor is not a candidate; the next one still counts.
func TestAdmitSkipsABlobThatFailsItsDigest(t *testing.T) {
	s := releasesigtest.New(t)
	f := newFake()
	f.image("1.4", digest)
	bad := []byte(`{"not":"a bundle"}`)
	f.bundle(bad)
	f.setErr("Blob:"+sha(bad), imageadmit.ErrDigestMismatch)
	f.bundle(s.SignDSSE(statement(t, imageadmit.PredicateCosignSign, imageHex), ciOpts()))
	a := imageadmit.NewAdmitter(f, imageadmit.AdmitterOptions{})
	v, err := admit(t, a, compile(t, s, corpCI()), ref)
	if err != nil {
		t.Fatal(err)
	}
	wantReason(t, v, "")
}

// A caller that gives up does not fail the check other callers share.
func TestAdmitCallerCancelDoesNotFailTheSharedCheck(t *testing.T) {
	s, f, a, _ := signedImage(t)
	p := compile(t, s, corpCI())
	f.gate = make(chan struct{})
	req, refused := p.Require(ref, true, nil)
	if refused != nil {
		t.Fatal(refused)
	}
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan imageadmit.Verdict, 1)
	go func() { v, _ := a.Admit(ctx, req); first <- v }()
	for f.n("Referrers") < 1 {
		time.Sleep(time.Millisecond)
	}
	second := make(chan imageadmit.Verdict, 1)
	go func() { v, _ := a.Admit(context.Background(), req); second <- v }()
	for f.n("Manifest:1.4") < 2 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	cancel()
	if v := <-first; v.Reason != imageadmit.AdmissionUnavailable {
		t.Fatalf("cancelled caller = %+v, want unavailable", v)
	}
	close(f.gate)
	if v := <-second; !v.Admitted() {
		t.Fatalf("other caller = %+v, want the shared admission", v)
	}
}
