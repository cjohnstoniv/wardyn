// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/distribution/reference"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/imageadmit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Image admission at run create (images.admission, stepImageAdmission): while the site mode is enforce, or a
// governance profile requires a signer, a governed member's image must be signed by a signer the deployment
// trusts for it. audit reports without refusing. A refusal decided by coverage alone (no_policy) reads no
// registry. Operators are not refused. A macOS VM run starts no member image and is recorded not_applicable.

const (
	admitTagRef  = "registry.example.com/team/app:1"
	admitDigest  = "sha256:" + "ab12cd34ef56" + "0000000000000000000000000000000000000000000000000000"
	admitPinned  = "registry.example.com/team/app:1@" + admitDigest
	admitSigner  = "corp-ci"
	admitProfile = "release-ci"
)

type admitRequireCall struct {
	mode    string
	ref     string
	site    bool
	profile []string
}

// fakeAdmitter records every Require and Admit; require and admit decide the answers (nil = covered, admitted).
type fakeAdmitter struct {
	mu       sync.Mutex
	requires []admitRequireCall
	admits   []string
	require  func(site bool, profile []string) (*imageadmit.Verdict, error)
	admit    func(req imageadmit.Requirements) (imageadmit.Verdict, error)
}

func (a *fakeAdmitter) Require(adm *types.SiteImageAdmission, ref string, site bool, profile []string) (imageadmit.Requirements, *imageadmit.Verdict, error) {
	a.mu.Lock()
	a.requires = append(a.requires, admitRequireCall{mode: adm.EffectiveMode(), ref: ref, site: site, profile: slices.Clone(profile)})
	a.mu.Unlock()
	if a.require != nil {
		if v, err := a.require(site, profile); v != nil || err != nil {
			return imageadmit.Requirements{}, v, err
		}
	}
	return imageadmit.Requirements{Ref: ref}, nil, nil
}

func (a *fakeAdmitter) Admit(_ context.Context, req imageadmit.Requirements) (imageadmit.Verdict, error) {
	a.mu.Lock()
	a.admits = append(a.admits, req.Ref)
	a.mu.Unlock()
	if a.admit != nil {
		return a.admit(req)
	}
	return imageadmit.Verdict{Digest: admitDigest, Signer: admitSigner, Identity: "https://ci.example/build@refs/heads/main",
		Issuer: "https://token.actions.githubusercontent.com", Attestations: []string{"https://slsa.dev/provenance/v1"}}, nil
}

func (a *fakeAdmitter) calls() (requires, admits int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.requires), len(a.admits)
}

func admissionSite(mode string) types.SiteConfig {
	adm := &types.SiteImageAdmission{Mode: mode, Signers: []types.ImageSigner{{
		Name: admitSigner, Images: []string{"registry.example.com/team/*"},
		Keyless: &types.ImageKeyless{Issuer: "https://token.actions.githubusercontent.com", Identity: "https://ci.example/build@refs/heads/main"},
	}}}
	return types.SiteConfig{Images: &types.SiteImages{Admission: adm}}
}

func admissionServer(site types.SiteConfig, a ImageAdmitter) *Server {
	return &Server{cfg: Config{Store: &capStore{site: site}, ImageAdmissionConfig: ImageAdmissionConfig{ImageAdmitter: a}}}
}

type admissionRun struct {
	ok   bool
	f    runFold
	resp *httptest.ResponseRecorder
}

func (r admissionRun) body(t *testing.T) errorBody {
	t.Helper()
	var b errorBody
	if err := json.Unmarshal(r.resp.Body.Bytes(), &b); err != nil {
		t.Fatalf("refusal body %q: %v", r.resp.Body.String(), err)
	}
	return b
}

// runAdmission runs the step alone, as the fold reaches it, for a request naming image.
func runAdmission(s *Server, ctx context.Context, mode foldMode, image string, profile []string, edit func(*runFold)) admissionRun {
	resp := httptest.NewRecorder()
	f := runFold{mode: mode, w: resp, r: governReq(ctx), req: &createRunRequest{Image: image},
		ceiling: governanceCeiling{Limits: types.GovernanceLimits{RequiredImageSigners: profile}}}
	if edit != nil {
		edit(&f)
	}
	ok := s.stepImageAdmission(&f, &foldRecorder{})
	return admissionRun{ok: ok, f: f, resp: resp}
}

// admitSiteUnreadable is a store whose site config read fails.
type admitSiteUnreadable struct{ *capStore }

func (admitSiteUnreadable) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{}, errors.New("db down")
}

// TestImageAdmissionOffIsByteIdentical: with the mode off (block absent, or present and off with signers
// defined) and no profile signer, the step answers nothing, asks the admitter nothing, adds no warning and
// leaves no verdict, so the run row, the run.create row and the 201 are what they always were.
func TestImageAdmissionOffIsByteIdentical(t *testing.T) {
	member := memberCtx(nil)
	for name, site := range map[string]types.SiteConfig{
		"no images block":  {},
		"digest rule only": {Images: &types.SiteImages{RequireDigest: true}},
		"mode off":         admissionSite(types.ImageAdmissionOff),
		"mode absent":      admissionSite(""),
	} {
		a := &fakeAdmitter{}
		got := runAdmission(admissionServer(site, a), member, foldCreate, admitTagRef, nil, nil)
		if !got.ok || got.resp.Body.Len() != 0 {
			t.Errorf("%s: step answered %d %q", name, got.resp.Code, got.resp.Body.String())
		}
		if r, ad := a.calls(); r != 0 || ad != 0 {
			t.Errorf("%s: admitter asked %d requires, %d admits; want none", name, r, ad)
		}
		if got.f.admission != nil || len(got.f.policyWarns) != 0 {
			t.Errorf("%s: verdict %+v, warnings %q; want none", name, got.f.admission, got.f.policyWarns)
		}
		data := map[string]any{}
		stampImageAdmission(data, got.f.admission)
		if len(data) != 0 {
			t.Errorf("%s: run.create datum gained %v", name, data)
		}
	}
	raw, err := json.Marshal(types.AgentRun{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "image_admission") {
		t.Errorf("a run with no verdict serialises image_admission: %s", raw)
	}
}

// TestImageAdmissionNoMemberImageChecksNothing: the convention image is not a member's choice (scope all is
// checked where the convention image is resolved), so enforce asks nothing of a request naming no image.
func TestImageAdmissionNoMemberImageChecksNothing(t *testing.T) {
	a := &fakeAdmitter{}
	got := runAdmission(admissionServer(admissionSite(types.ImageAdmissionEnforce), a), memberCtx(nil), foldCreate, "", []string{admitProfile}, nil)
	if r, ad := a.calls(); !got.ok || r != 0 || ad != 0 || got.f.admission != nil {
		t.Errorf("ok=%v requires=%d admits=%d verdict=%+v; want ok, no calls, no verdict", got.ok, r, ad, got.f.admission)
	}
}

// TestImageAdmissionEnforceAdmitsAndPins: an admitted image leaves the verdict for the run row, with the ref
// pinned to the verified digest (name, the tag as typed, and the digest), which reference.ParseNamed accepts.
func TestImageAdmissionEnforceAdmitsAndPins(t *testing.T) {
	a := &fakeAdmitter{}
	got := runAdmission(admissionServer(admissionSite(types.ImageAdmissionEnforce), a), memberCtx(nil), foldCreate, admitTagRef, nil, nil)
	if !got.ok {
		t.Fatalf("admitted image refused: %d %s", got.resp.Code, got.resp.Body.String())
	}
	v := got.f.admission
	if v == nil || v.Mode != types.ImageAdmissionEnforce || v.Verdict != types.ImageAdmissionAdmitted || v.Digest != admitDigest ||
		v.PinnedRef != admitPinned || v.Signer != admitSigner || v.Issuer == "" || v.Identity == "" || len(v.Attestations) != 1 {
		t.Fatalf("verdict %+v", v)
	}
	if _, err := reference.ParseNamed(v.PinnedRef); err != nil {
		t.Errorf("pinned ref %q does not parse: %v", v.PinnedRef, err)
	}
	if r, ad := a.calls(); r != 1 || ad != 1 || !a.requires[0].site || a.requires[0].ref != admitTagRef {
		t.Errorf("requires=%d admits=%d first=%+v; want one site check of the ref as typed", r, ad, a.requires)
	}
	data := map[string]any{}
	stampImageAdmission(data, v)
	if data["image_admission"] != v {
		t.Errorf("run.create datum %v does not carry the verdict", data)
	}
	if len(got.f.policyWarns) != 0 {
		t.Errorf("an admitted image warned %q", got.f.policyWarns)
	}
}

// TestImageAdmissionPinnedRefForms: a digest ref keeps its digest, a familiar name is made canonical, and a
// ref already pinned to another digest is pinned to the verified one.
// The run row says whether the verdict was read from the registry for this create or served from the cache.
func TestAdmittedImageKeepsCached(t *testing.T) {
	for _, cached := range []bool{false, true} {
		if got := admittedImage(types.ImageAdmissionEnforce, imageadmit.Verdict{Digest: "sha256:x", Cached: cached}); got.Cached != cached {
			t.Errorf("Cached = %v for a verdict with Cached = %v", got.Cached, cached)
		}
	}
}

func TestImageAdmissionPinnedRefForms(t *testing.T) {
	other := "sha256:" + strings.Repeat("9", 64)
	for in, want := range map[string]string{
		admitTagRef:                                    admitPinned,
		"registry.example.com/team/app@" + other:       "registry.example.com/team/app@" + admitDigest,
		"registry.example.com/team/app:1@" + other:     admitPinned,
		"registry.example.com:5000/team/app":           "registry.example.com:5000/team/app@" + admitDigest,
		"app:1":                                        "docker.io/library/app:1@" + admitDigest,
		"registry.example.com/team/app@" + admitDigest: "registry.example.com/team/app@" + admitDigest,
	} {
		got, err := imageAdmissionPinnedRef(in, admitDigest)
		if err != nil || got != want {
			t.Errorf("imageAdmissionPinnedRef(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := imageAdmissionPinnedRef(admitTagRef, "sha256:short"); err == nil {
		t.Error("a malformed digest pinned")
	}
}

// TestImageAdmissionEnforceRefusesEachReason: each refusal is its reason, its status and a sentence naming
// the image as typed.
func TestImageAdmissionEnforceRefusesEachReason(t *testing.T) {
	registryDown := &imageadmit.Unavailable{Cause: imageadmit.CauseUnreachable, Host: "registry.example.com", Registry: "registry.example.com",
		Err: errors.New("dial tcp 10.0.0.9:443: connect: connection refused")}
	const downDetail = "registry.example.com could not be reached"
	for _, tc := range []struct {
		name    string
		require func(bool, []string) (*imageadmit.Verdict, error)
		admit   func(imageadmit.Requirements) (imageadmit.Verdict, error)
		status  int
		reason  string
		says    []string
		admits  int
	}{
		{"no policy", func(bool, []string) (*imageadmit.Verdict, error) {
			return &imageadmit.Verdict{Reason: imageadmit.NoPolicy}, nil
		}, nil,
			http.StatusForbidden, string(imageadmit.NoPolicy), []string{admitTagRef, "images.admission"}, 0},
		{"signature missing", nil, func(imageadmit.Requirements) (imageadmit.Verdict, error) {
			return imageadmit.Verdict{Reason: imageadmit.SignatureMissing, Digest: admitDigest, Signers: []string{admitSigner}}, nil
		}, http.StatusForbidden, string(imageadmit.SignatureMissing), []string{admitTagRef, "sha256:ab12cd34ef56", admitSigner}, 1},
		{"signature invalid", nil, func(imageadmit.Requirements) (imageadmit.Verdict, error) {
			return imageadmit.Verdict{Reason: imageadmit.SignatureInvalid, Digest: admitDigest, Signer: admitSigner,
				Found: "https://ci.example/other@refs/heads/x", FoundIssuer: "https://issuer.example", Want: "https://ci.example/build@refs/heads/main",
				WantIssuer: "https://token.actions.githubusercontent.com"}, nil
		}, http.StatusForbidden, string(imageadmit.SignatureInvalid), []string{admitTagRef, "https://ci.example/other@refs/heads/x", "https://ci.example/build@refs/heads/main", admitSigner}, 1},
		{"signature invalid by key", nil, func(imageadmit.Requirements) (imageadmit.Verdict, error) {
			return imageadmit.Verdict{Reason: imageadmit.SignatureInvalid, Digest: admitDigest, Signer: "vendor-key", Want: "key-b"}, nil
		}, http.StatusForbidden, string(imageadmit.SignatureInvalid), []string{"no signature that verifies with a key", "vendor-key requires key key-b"}, 1},
		{"attestation missing", nil, func(imageadmit.Requirements) (imageadmit.Verdict, error) {
			return imageadmit.Verdict{Reason: imageadmit.AttestationMissing, Digest: admitDigest, Signer: admitSigner, Want: "https://slsa.dev/provenance/v1"}, nil
		}, http.StatusForbidden, string(imageadmit.AttestationMissing), []string{admitTagRef, "https://slsa.dev/provenance/v1", admitSigner}, 1},
		{"registry unavailable", nil, func(imageadmit.Requirements) (imageadmit.Verdict, error) {
			return imageadmit.Verdict{Reason: imageadmit.AdmissionUnavailable, Detail: downDetail}, registryDown
		}, http.StatusServiceUnavailable, string(imageadmit.AdmissionUnavailable), []string{admitTagRef, downDetail}, 1},
		{"trust root unreadable", func(bool, []string) (*imageadmit.Verdict, error) {
			return nil, errors.New("image admission policy is invalid: trust root github-private: open /etc/wardyn/sigstore/root.json: permission denied")
		}, nil, http.StatusServiceUnavailable, string(imageadmit.AdmissionUnavailable), []string{admitTagRef, "trust root file"}, 0},
	} {
		for _, mode := range []foldMode{foldCreate, foldPreflight} {
			a := &fakeAdmitter{require: tc.require, admit: tc.admit}
			srv, rec := admissionServer(admissionSite(types.ImageAdmissionEnforce), a), &recRecorder{}
			srv.cfg.Audit, srv.cfg.Now = rec, time.Now
			got := runAdmission(srv, memberCtx(nil), mode, admitTagRef, nil, nil)
			wantImageAdmissionDeniedRow(t, tc.name, mode, rec.snapshot(), tc.reason)
			if got.ok {
				t.Errorf("%s (mode %d): admitted", tc.name, mode)
				continue
			}
			b := got.body(t)
			if got.resp.Code != tc.status || b.Reason != tc.reason {
				t.Errorf("%s (mode %d): %d %q, want %d %q", tc.name, mode, got.resp.Code, b.Reason, tc.status, tc.reason)
			}
			for _, s := range tc.says {
				if !strings.Contains(b.Error, s) {
					t.Errorf("%s (mode %d): sentence %q does not say %q", tc.name, mode, b.Error, s)
				}
			}
			if strings.Contains(b.Error, "10.0.0.9") || strings.Contains(b.Error, "/etc/wardyn") {
				t.Errorf("%s (mode %d): sentence %q carries the logged error", tc.name, mode, b.Error)
			}
			if _, ad := a.calls(); ad != tc.admits {
				t.Errorf("%s (mode %d): %d registry checks, want %d", tc.name, mode, ad, tc.admits)
			}
		}
	}
}

// wantImageAdmissionDeniedRow: a create refusal writes one run.create denied row naming the reason, the image
// and the check; preflight is a dry run and writes none.
func wantImageAdmissionDeniedRow(t *testing.T, name string, mode foldMode, rows []types.AuditEvent, reason string) {
	t.Helper()
	if mode != foldCreate {
		if len(rows) != 0 {
			t.Errorf("%s (preflight): %d audit rows, want none", name, len(rows))
		}
		return
	}
	if len(rows) != 1 || rows[0].Action != "run.create" || rows[0].Outcome != "denied" || rows[0].RunID != nil || rows[0].Actor == "" {
		t.Errorf("%s (create): audit rows %+v, want one run.create denied row", name, rows)
		return
	}
	var d struct {
		Subject        string `json:"subject"`
		Reason         string `json:"reason"`
		Image          string `json:"image"`
		ImageAdmission struct {
			Mode    string `json:"mode"`
			Verdict string `json:"verdict"`
			Reason  string `json:"reason"`
		} `json:"image_admission"`
	}
	if err := json.Unmarshal(rows[0].Data, &d); err != nil {
		t.Fatalf("%s (create): row data %s: %v", name, rows[0].Data, err)
	}
	if d.Subject != rows[0].Actor || d.Reason != reason || d.Image != admitTagRef || d.ImageAdmission.Mode != types.ImageAdmissionEnforce ||
		d.ImageAdmission.Verdict != imageAdmissionRefused || d.ImageAdmission.Reason != reason {
		t.Errorf("%s (create): row data %s", name, rows[0].Data)
	}
}

// TestImageAdmissionUncoveredMakesNoRegistryCall: a ref no signer covers is refused on coverage alone, before
// any registry read, so a member cannot make the control plane contact a host no admin named.
func TestImageAdmissionUncoveredMakesNoRegistryCall(t *testing.T) {
	for _, ref := range []string{"169.254.169.254/x:y", "internal.host/x"} {
		a := &fakeAdmitter{require: func(bool, []string) (*imageadmit.Verdict, error) {
			return &imageadmit.Verdict{Reason: imageadmit.NoPolicy}, nil
		}}
		got := runAdmission(admissionServer(admissionSite(types.ImageAdmissionEnforce), a), memberCtx(nil), foldCreate, ref, nil, nil)
		if r, ad := a.calls(); got.ok || r != 1 || ad != 0 {
			t.Errorf("%s: ok=%v requires=%d admits=%d; want refused with no registry check", ref, got.ok, r, ad)
		}
	}
}

// TestImageAdmissionAuditNeverRefuses: audit reports every would-refuse as a warning on the 201 and a
// would_refuse verdict, never refuses, never pins, and an unreachable registry is a warning too.
func TestImageAdmissionAuditNeverRefuses(t *testing.T) {
	for name, tc := range map[string]struct {
		require func(bool, []string) (*imageadmit.Verdict, error)
		admit   func(imageadmit.Requirements) (imageadmit.Verdict, error)
		reason  imageadmit.Reason
	}{
		"no policy": {func(bool, []string) (*imageadmit.Verdict, error) {
			return &imageadmit.Verdict{Reason: imageadmit.NoPolicy}, nil
		}, nil, imageadmit.NoPolicy},
		"unsigned": {nil, func(imageadmit.Requirements) (imageadmit.Verdict, error) {
			return imageadmit.Verdict{Reason: imageadmit.SignatureMissing, Digest: admitDigest}, nil
		}, imageadmit.SignatureMissing},
		"registry down": {nil, func(imageadmit.Requirements) (imageadmit.Verdict, error) {
			return imageadmit.Verdict{Reason: imageadmit.AdmissionUnavailable, Detail: "the check took longer than 20 seconds"},
				&imageadmit.Unavailable{Cause: imageadmit.CauseTimeout, Host: "registry.example.com"}
		}, imageadmit.AdmissionUnavailable},
	} {
		a := &fakeAdmitter{require: tc.require, admit: tc.admit}
		got := runAdmission(admissionServer(admissionSite(types.ImageAdmissionAudit), a), memberCtx(nil), foldCreate, admitTagRef, nil, nil)
		if !got.ok || got.resp.Body.Len() != 0 {
			t.Errorf("%s: audit refused: %d %s", name, got.resp.Code, got.resp.Body.String())
			continue
		}
		v := got.f.admission
		if v == nil || v.Mode != types.ImageAdmissionAudit || v.Verdict != types.ImageAdmissionWouldRefuse || v.Reason != string(tc.reason) || v.PinnedRef != "" {
			t.Errorf("%s: verdict %+v, want audit would_refuse %s with no pin", name, v, tc.reason)
		}
		if len(got.f.policyWarns) != 1 || !strings.HasPrefix(got.f.policyWarns[0], imageAdmissionAuditPrefix) || !strings.Contains(got.f.policyWarns[0], admitTagRef) {
			t.Errorf("%s: warnings %q, want one audit-only sentence naming the image", name, got.f.policyWarns)
		}
	}
	// An audit pass records the check, unpinned.
	a := &fakeAdmitter{}
	got := runAdmission(admissionServer(admissionSite(types.ImageAdmissionAudit), a), memberCtx(nil), foldCreate, admitTagRef, nil, nil)
	if v := got.f.admission; !got.ok || v == nil || v.Verdict != types.ImageAdmissionAdmitted || v.Digest != admitDigest || v.PinnedRef != "" || len(got.f.policyWarns) != 0 {
		t.Errorf("audit pass: ok=%v verdict=%+v warnings=%q", got.ok, v, got.f.policyWarns)
	}
}

// TestImageAdmissionOperatorNotRefused: an operator's image is not checked under enforce (the digest rule's
// precedent); the run.create row's exempt mark is written where the row is.
func TestImageAdmissionOperatorNotRefused(t *testing.T) {
	a := &fakeAdmitter{require: func(bool, []string) (*imageadmit.Verdict, error) {
		return &imageadmit.Verdict{Reason: imageadmit.NoPolicy}, nil
	}}
	admin := operatorCtx(capSub, capEmail, oidc.RoleAdmin)
	got := runAdmission(admissionServer(admissionSite(types.ImageAdmissionEnforce), a), admin, foldCreate, admitTagRef, nil, nil)
	if r, ad := a.calls(); !got.ok || r != 0 || ad != 0 || got.f.admission != nil {
		t.Errorf("operator: ok=%v requires=%d admits=%d verdict=%+v; want not checked", got.ok, r, ad, got.f.admission)
	}
}

// TestImageAdmissionProfileSignersEnforceUnderSiteOff: a profile's required signers are enforced for its
// members whatever the site mode, so a profile only tightens. Under audit the profile's check refuses and the
// site's is asked separately, only to report.
func TestImageAdmissionProfileSignersEnforceUnderSiteOff(t *testing.T) {
	unsigned := func(req imageadmit.Requirements) (imageadmit.Verdict, error) {
		return imageadmit.Verdict{Reason: imageadmit.SignatureMissing, Digest: admitDigest, Signer: admitProfile}, nil
	}
	for _, site := range []types.SiteConfig{{}, admissionSite(types.ImageAdmissionOff), admissionSite(types.ImageAdmissionAudit)} {
		a := &fakeAdmitter{admit: unsigned}
		got := runAdmission(admissionServer(site, a), memberCtx(nil), foldCreate, admitTagRef, []string{admitProfile}, nil)
		if got.ok || got.resp.Code != http.StatusForbidden || got.body(t).Reason != string(imageadmit.SignatureMissing) {
			t.Errorf("site %+v: ok=%v %d %s; want 403 signature missing", site.Images, got.ok, got.resp.Code, got.resp.Body.String())
		}
		if len(a.requires) == 0 || a.requires[0].site || !slices.Equal(a.requires[0].profile, []string{admitProfile}) {
			t.Errorf("site %+v: first require %+v; want the profile's signers alone", site.Images, a.requires)
		}
	}

	// audit + profile: the profile passes, the site would refuse -> admitted, pinned, warned.
	a := &fakeAdmitter{require: func(site bool, _ []string) (*imageadmit.Verdict, error) {
		if site {
			return &imageadmit.Verdict{Reason: imageadmit.NoPolicy}, nil
		}
		return nil, nil
	}}
	got := runAdmission(admissionServer(admissionSite(types.ImageAdmissionAudit), a), memberCtx(nil), foldCreate, admitTagRef, []string{admitProfile}, nil)
	v := got.f.admission
	if !got.ok || v == nil || v.Mode != types.ImageAdmissionEnforce || v.Verdict != types.ImageAdmissionAdmitted || v.PinnedRef != admitPinned ||
		v.SiteAudit != string(imageadmit.NoPolicy) || len(got.f.policyWarns) != 1 {
		t.Errorf("audit+profile: ok=%v verdict=%+v warnings=%q", got.ok, v, got.f.policyWarns)
	}
	if len(a.requires) != 2 || a.requires[0].site || !a.requires[1].site || len(a.requires[1].profile) != 0 {
		t.Errorf("audit+profile requires %+v; want the profile's, then the site's alone", a.requires)
	}

	// enforce + profile: one check holds the image to both.
	a = &fakeAdmitter{}
	got = runAdmission(admissionServer(admissionSite(types.ImageAdmissionEnforce), a), memberCtx(nil), foldCreate, admitTagRef, []string{admitProfile}, nil)
	if !got.ok || len(a.requires) != 1 || !a.requires[0].site || !slices.Equal(a.requires[0].profile, []string{admitProfile}) {
		t.Errorf("enforce+profile: ok=%v requires %+v; want one check of site and profile together", got.ok, a.requires)
	}
}

// TestImageAdmissionProfileSentences: a profile's no_policy names the signer, and says when the deployment no
// longer defines it.
func TestImageAdmissionProfileSentences(t *testing.T) {
	for undefined, want := range map[bool]string{false: "which does not cover it", true: "which this deployment no longer defines"} {
		a := &fakeAdmitter{require: func(bool, []string) (*imageadmit.Verdict, error) {
			return &imageadmit.Verdict{Reason: imageadmit.NoPolicy, Signer: admitProfile, Undefined: undefined}, nil
		}}
		got := runAdmission(admissionServer(types.SiteConfig{}, a), memberCtx(nil), foldCreate, admitTagRef, []string{admitProfile}, nil)
		if got.ok {
			t.Fatalf("undefined=%v: admitted", undefined)
		}
		if b := got.body(t); !strings.Contains(b.Error, admitProfile) || !strings.Contains(b.Error, want) || !strings.Contains(b.Error, "governance profile") {
			t.Errorf("undefined=%v: sentence %q", undefined, b.Error)
		}
	}
}

// TestImageAdmissionMacOSVMNotApplicable: a macOS VM never runs the member image, so enforce asks nothing and
// records not_applicable.
func TestImageAdmissionMacOSVMNotApplicable(t *testing.T) {
	a := &fakeAdmitter{require: func(bool, []string) (*imageadmit.Verdict, error) {
		return &imageadmit.Verdict{Reason: imageadmit.NoPolicy}, nil
	}}
	got := runAdmission(admissionServer(admissionSite(types.ImageAdmissionEnforce), a), memberCtx(nil), foldCreate, admitTagRef, nil,
		func(f *runFold) { f.req.MachineKind = string(types.MachineMacOSVM) })
	if r, ad := a.calls(); !got.ok || r != 0 || ad != 0 {
		t.Errorf("macOS VM: ok=%v requires=%d admits=%d", got.ok, r, ad)
	}
	if v := got.f.admission; v == nil || v.Verdict != types.ImageAdmissionNotApplicable || v.Mode != types.ImageAdmissionEnforce || v.Digest != "" {
		t.Errorf("macOS VM verdict %+v, want enforce not_applicable", v)
	}
}

// TestImageAdmissionQueueStartRechecks: the queue's start replays the fold for a promoted run, and admission
// does not stand down for it: a run that waited is checked again when it starts.
func TestImageAdmissionQueueStartRechecks(t *testing.T) {
	a := &fakeAdmitter{}
	got := runAdmission(admissionServer(admissionSite(types.ImageAdmissionEnforce), a), memberCtx(nil), foldCreate, admitTagRef, nil,
		func(f *runFold) { f.promoted = true })
	if r, ad := a.calls(); !got.ok || r != 1 || ad != 1 {
		t.Errorf("promoted start: ok=%v requires=%d admits=%d; want one check", got.ok, r, ad)
	}
}

// TestImageAdmissionFailsClosed: an unreadable site config refuses, and with the mode on and no admitter
// built, enforce refuses with the unavailable reason while audit only warns.
func TestImageAdmissionFailsClosed(t *testing.T) {
	broken := &Server{cfg: Config{Store: admitSiteUnreadable{&capStore{}}, ImageAdmissionConfig: ImageAdmissionConfig{ImageAdmitter: &fakeAdmitter{}}}}
	if got := runAdmission(broken, memberCtx(nil), foldCreate, admitTagRef, nil, nil); got.ok || got.resp.Code != http.StatusInternalServerError {
		t.Errorf("unreadable site config: ok=%v %d", got.ok, got.resp.Code)
	}
	got := runAdmission(admissionServer(admissionSite(types.ImageAdmissionEnforce), nil), memberCtx(nil), foldCreate, admitTagRef, nil, nil)
	if got.ok || got.resp.Code != http.StatusServiceUnavailable || got.body(t).Reason != string(imageadmit.AdmissionUnavailable) {
		t.Errorf("enforce with no admitter: ok=%v %d %s", got.ok, got.resp.Code, got.resp.Body.String())
	}
	got = runAdmission(admissionServer(admissionSite(types.ImageAdmissionAudit), nil), memberCtx(nil), foldCreate, admitTagRef, nil, nil)
	if !got.ok || len(got.f.policyWarns) != 1 {
		t.Errorf("audit with no admitter: ok=%v warnings=%q", got.ok, got.f.policyWarns)
	}
}

// TestImageAdmissionStepIsInTheFold: the step runs at create and preflight (not the preview, which makes no
// probes), after the run's end and before host capacity, so a refusal leaves no run row and no identity.
func TestImageAdmissionStepIsInTheFold(t *testing.T) {
	idx := map[string]int{}
	for i, st := range runFoldSteps {
		full := runtime.FuncForPC(reflect.ValueOf(st.run).Pointer()).Name()
		name := full[strings.LastIndex(full, ".")+1:]
		idx[name] = i
		if name == "stepImageAdmission" && st.modes != foldCreate|foldPreflight {
			t.Errorf("stepImageAdmission modes %d, want create|preflight", st.modes)
		}
	}
	at, ok := idx["stepImageAdmission"]
	if !ok {
		t.Fatal("stepImageAdmission is not in runFoldSteps")
	}
	if !(idx["stepRunEnd"] < at && at < idx["stepHostCapacity"]) {
		t.Errorf("stepImageAdmission at %d; want between stepRunEnd (%d) and stepHostCapacity (%d)", at, idx["stepRunEnd"], idx["stepHostCapacity"])
	}
}

// TestImageAdmissionReasonsMatchImageadmit: the wire reasons are imageadmit's closed set, value for value.
func TestImageAdmissionReasonsMatchImageadmit(t *testing.T) {
	for api, ia := range map[string]imageadmit.Reason{
		reasonImageAdmissionNoPolicy:    imageadmit.NoPolicy,
		reasonImageSignatureMissing:     imageadmit.SignatureMissing,
		reasonImageSignatureInvalid:     imageadmit.SignatureInvalid,
		reasonImageAttestationMissing:   imageadmit.AttestationMissing,
		reasonImageAdmissionUnavailable: imageadmit.AdmissionUnavailable,
	} {
		if api != string(ia) {
			t.Errorf("reason %q != imageadmit %q", api, ia)
		}
	}
}

// TestImageAdmissionGovernedAdminIsChecked: with GovernAdminRuns on, an admin's own run is governed, so its
// image is checked as a member's is; with it off the admin is an operator and is not checked.
func TestImageAdmissionGovernedAdminIsChecked(t *testing.T) {
	admin := operatorCtx(capSub, capEmail, oidc.RoleAdmin)
	for _, on := range []bool{false, true} {
		a := &fakeAdmitter{require: func(bool, []string) (*imageadmit.Verdict, error) {
			return &imageadmit.Verdict{Reason: imageadmit.NoPolicy}, nil
		}}
		s := admissionServer(admissionSite(types.ImageAdmissionEnforce), a)
		s.cfg.GovernAdminRuns = on
		got := runAdmission(s, admin, foldCreate, admitTagRef, nil, nil)
		if r, _ := a.calls(); got.ok == on || (r == 1) != on {
			t.Errorf("GovernAdminRuns=%v: ok=%v requires=%d; want checked and refused only when on", on, got.ok, r)
		}
	}
}
