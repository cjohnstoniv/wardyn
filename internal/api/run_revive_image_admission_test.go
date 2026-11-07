// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/imageadmit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Image admission at revive: a kept sandbox's image is re-checked by the digest its create's admission
// recorded (the pin), never by its tag, under the policy as it is now. While the image is held (site enforce,
// or a profile that requires a signer), a run with no recorded admitted digest (created before admission was
// on, in audit mode, or before the run recorded its image) is refused: its tag may name another image by now.
// Operators stay exempt, and their revive row is marked.

const reviveNotAdmitted = "revive_image_not_admitted"

// reviveAdmissionFixture is a lost member run whose create asked admission about admitTagRef and recorded
// verdict, on a deployment whose site mode is mode, checked by a.
func reviveAdmissionFixture(t *testing.T, mode string, verdict *types.ImageAdmissionVerdict, a ImageAdmitter) *reviveFixture {
	t.Helper()
	f, _ := newOwnerFixture(t)
	f.st.site.Images = admissionSite(mode).Images
	tag := admitTagRef
	f.st.run.ImageInput, f.st.run.ImageAdmission = &tag, verdict
	f.srv.cfg.ImageAdmitter = a
	return f
}

// reviveMark is the run.revive success row's image_admission_exempt.
func reviveMark(t *testing.T, f *reviveFixture) bool {
	t.Helper()
	for _, ev := range f.audit.eventsFor(f.run.ID, "run.revive") {
		if ev.Outcome == "success" {
			return leaseAuditData(t, ev)["image_admission_exempt"] == true
		}
	}
	return false
}

func wantReviveNotAdmitted(t *testing.T, f *reviveFixture, a *fakeAdmitter, code int, body string) {
	t.Helper()
	if code != http.StatusForbidden || !strings.Contains(body, reviveNotAdmitted) || !strings.Contains(body, admitTagRef) ||
		!strings.Contains(body, "Start a new run to use an allowed image") {
		t.Fatalf("revive = %d %s; want 403 %s naming the image, with the revive sentence", code, body, reviveNotAdmitted)
	}
	if r, n := a.calls(); r != 0 || n != 0 {
		t.Errorf("admitter asked %d Require / %d Admit; a run with no recorded digest must not be checked by its tag", r, n)
	}
	f.assertReviveRefused(t, reviveNotAdmitted)
}

// TestReviveImageAdmission_RechecksThePinnedDigest: an enforced, pinned run is re-checked at revive. Coverage is
// asked about the image as its create asked it, and the registry about the pin, so the tag is never resolved.
func TestReviveImageAdmission_RechecksThePinnedDigest(t *testing.T) {
	a := &fakeAdmitter{}
	f := reviveAdmissionFixture(t, types.ImageAdmissionEnforce, pinnedVerdict(admitPinned), a)
	if code, body := f.reviveAs(t, true); code != http.StatusOK {
		t.Fatalf("revive = %d %s, want 200", code, body)
	}
	if len(a.requires) != 1 || a.requires[0].ref != admitTagRef || !a.requires[0].site {
		t.Errorf("Require calls = %+v, want one site check of %q", a.requires, admitTagRef)
	}
	if !slices.Equal(a.admits, []string{admitPinned}) {
		t.Errorf("Admit refs = %q, want only the pin %q", a.admits, admitPinned)
	}
	if reviveMark(t, f) {
		t.Error("a checked revive was marked exempt")
	}
}

// TestReviveImageAdmission_ATagMovedToASignedImageIsRefused: the run was created with unsigned image A in audit
// mode (would refuse, nothing pinned); since then admission is enforced and the tag names a signed image B,
// which a check by tag would admit. The revive is refused without asking the registry.
func TestReviveImageAdmission_ATagMovedToASignedImageIsRefused(t *testing.T) {
	a := &fakeAdmitter{} // admits whatever it is asked: the tag now names a signed image
	unsignedA := &types.ImageAdmissionVerdict{Mode: types.ImageAdmissionAudit, Verdict: types.ImageAdmissionWouldRefuse,
		Reason: reasonImageSignatureMissing, Digest: "sha256:" + strings.Repeat("aa", 32)}
	f := reviveAdmissionFixture(t, types.ImageAdmissionEnforce, unsignedA, a)
	code, body := f.reviveAs(t, true)
	wantReviveNotAdmitted(t, f, a, code, body)
}

// TestReviveImageAdmission_AuditToEnforce: audit admits without pinning, so after the switch to enforce an
// audit-admitted run is refused at revive, as are a run created while admission was off and one from before the
// run recorded its image. While the site is still in audit, the same runs revive and nothing is asked.
func TestReviveImageAdmission_AuditToEnforce(t *testing.T) {
	auditAdmitted := &types.ImageAdmissionVerdict{Mode: types.ImageAdmissionAudit, Verdict: types.ImageAdmissionAdmitted, Digest: admitDigest, Signer: admitSigner}
	for _, tc := range []struct {
		name     string
		verdict  *types.ImageAdmissionVerdict
		noInput  bool
		enforced bool
	}{
		{"audit admitted, now enforce", auditAdmitted, false, true},
		{"created while off, now enforce", nil, false, true},
		{"recorded no image input, now enforce", nil, true, true},
		{"audit admitted, still audit", auditAdmitted, false, false},
		{"created while off, now audit", nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &fakeAdmitter{}
			mode := types.ImageAdmissionAudit
			if tc.enforced {
				mode = types.ImageAdmissionEnforce
			}
			f := reviveAdmissionFixture(t, mode, tc.verdict, a)
			if tc.noInput {
				f.st.run.ImageInput = nil
			}
			code, body := f.reviveAs(t, true)
			if tc.enforced {
				if code != http.StatusForbidden || !strings.Contains(body, reviveNotAdmitted) {
					t.Fatalf("revive = %d %s; want 403 %s", code, body, reviveNotAdmitted)
				}
				f.assertReviveRefused(t, reviveNotAdmitted)
				return
			}
			if code != http.StatusOK {
				t.Fatalf("revive under audit = %d %s, want 200", code, body)
			}
			if r, n := a.calls(); r != 0 || n != 0 {
				t.Errorf("audit revive asked %d Require / %d Admit, want none", r, n)
			}
		})
	}
}

// TestReviveImageAdmission_RemovedSignerRefuses: the pinned digest no longer passes the policy as it is now (its
// signer gone, or no longer trusted), so the revive is refused with the check's reason and the revive sentence,
// and a denied row. A registry that cannot answer is a 503 that replaces nothing.
func TestReviveImageAdmission_RemovedSignerRefuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		a      *fakeAdmitter
		reason string
	}{
		{"signer removed from the site", &fakeAdmitter{require: func(bool, []string) (*imageadmit.Verdict, error) {
			return &imageadmit.Verdict{Reason: imageadmit.NoPolicy}, nil
		}}, reasonImageAdmissionNoPolicy},
		{"signature no longer from a trusted signer", &fakeAdmitter{admit: func(imageadmit.Requirements) (imageadmit.Verdict, error) {
			return imageadmit.Verdict{Reason: imageadmit.SignatureMissing, Digest: admitDigest, Signers: []string{"other-ci"}}, nil
		}}, reasonImageSignatureMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := reviveAdmissionFixture(t, types.ImageAdmissionEnforce, pinnedVerdict(admitPinned), tc.a)
			code, body := f.reviveAs(t, true)
			if code != http.StatusForbidden || !strings.Contains(body, tc.reason) || !strings.Contains(body, "Start a new run to use an allowed image") {
				t.Fatalf("revive = %d %s; want 403 %s with the revive sentence", code, body, tc.reason)
			}
			f.assertReviveRefused(t, tc.reason)
		})
	}
	t.Run("registry unavailable", func(t *testing.T) {
		a := &fakeAdmitter{admit: func(imageadmit.Requirements) (imageadmit.Verdict, error) {
			return imageadmit.Verdict{Reason: imageadmit.AdmissionUnavailable, Detail: "registry.example.com could not be reached"}, errors.New("dial: refused")
		}}
		f := reviveAdmissionFixture(t, types.ImageAdmissionEnforce, pinnedVerdict(admitPinned), a)
		code, body := f.reviveAs(t, true)
		if code != http.StatusServiceUnavailable || !strings.Contains(body, reasonImageAdmissionUnavailable) {
			t.Fatalf("revive = %d %s; want 503 %s", code, body, reasonImageAdmissionUnavailable)
		}
		if len(f.rr.replaced) != 0 {
			t.Error("a revive whose image could not be checked replaced the proxy")
		}
	})
}

// TestReviveImageAdmission_OperatorRevive: an operator-owned run (never checked at create, so never pinned)
// revives under enforce, unchecked, and the run.revive row says it was exempt.
func TestReviveImageAdmission_OperatorRevive(t *testing.T) {
	a := &fakeAdmitter{}
	f := reviveAdmissionFixture(t, types.ImageAdmissionEnforce, nil, a)
	f.st.run.OperatorOwned = true
	if code, body := f.reviveAs(t, false); code != http.StatusOK {
		t.Fatalf("operator revive = %d %s, want 200", code, body)
	}
	if r, n := a.calls(); r != 0 || n != 0 {
		t.Errorf("operator revive asked %d Require / %d Admit, want none", r, n)
	}
	if !reviveMark(t, f) {
		t.Error("operator revive's run.revive row has no image_admission_exempt")
	}
}

// TestReviveImageAdmission_GovernAdminRuns: an SSO admin reviving their own run is exempt (and marked) unless
// GovernAdminRuns governs admins' runs, when an unpinned run is refused as a member's would be.
func TestReviveImageAdmission_GovernAdminRuns(t *testing.T) {
	for _, govern := range []bool{false, true} {
		t.Run(map[bool]string{false: "admin exempt", true: "admin under GovernAdminRuns"}[govern], func(t *testing.T) {
			a := &fakeAdmitter{}
			f := reviveAdmissionFixture(t, types.ImageAdmissionEnforce, nil, a)
			f.srv.cfg.GovernAdminRuns = govern
			r := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/revive", nil)
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("id", f.run.ID.String())
			r = r.WithContext(context.WithValue(withOIDCRole(withOIDCHuman(r.Context(), f.run.CreatedBy), oidc.RoleAdmin), chi.RouteCtxKey, rctx))
			w := httptest.NewRecorder()
			f.srv.handleReviveRun(w, r)
			if govern {
				wantReviveNotAdmitted(t, f, a, w.Code, w.Body.String())
				return
			}
			if w.Code != http.StatusOK || !reviveMark(t, f) {
				t.Fatalf("admin revive = %d %s (marked %v), want 200 marked exempt", w.Code, w.Body.String(), reviveMark(t, f))
			}
		})
	}
}

// TestReviveImageAdmission_NothingToCheck: with admission off (and no profile signer), for the convention image,
// and for a macOS VM run (which never runs the member image), a revive asks nothing and marks nothing.
func TestReviveImageAdmission_NothingToCheck(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode string
		edit func(*types.AgentRun)
	}{
		{"off, unpinned", types.ImageAdmissionOff, func(*types.AgentRun) {}},
		{"enforce, convention image", types.ImageAdmissionEnforce, func(r *types.AgentRun) { conv := ""; r.ImageInput = &conv }},
		{"enforce, macOS VM", types.ImageAdmissionEnforce, func(r *types.AgentRun) {
			r.MachineKind = types.MachineMacOSVM
			r.ImageAdmission = &types.ImageAdmissionVerdict{Mode: types.ImageAdmissionEnforce, Verdict: types.ImageAdmissionNotApplicable}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &fakeAdmitter{}
			f := reviveAdmissionFixture(t, tc.mode, nil, a)
			tc.edit(&f.st.run)
			if code, body := f.reviveAs(t, true); code != http.StatusOK {
				t.Fatalf("revive = %d %s, want 200", code, body)
			}
			if r, n := a.calls(); r != 0 || n != 0 || reviveMark(t, f) {
				t.Errorf("asked %d Require / %d Admit, marked %v; want nothing", r, n, reviveMark(t, f))
			}
		})
	}
}

// TestReviveImageAdmission_ProfileSignersUnderSiteOff: the owner's profile requires a signer, so the image is held
// at revive though the site mode is off: a pinned run is re-checked against the profile's signer only, and an
// unpinned one is refused.
func TestReviveImageAdmission_ProfileSignersUnderSiteOff(t *testing.T) {
	for _, pinned := range []bool{true, false} {
		t.Run(map[bool]string{true: "pinned", false: "unpinned"}[pinned], func(t *testing.T) {
			a := &fakeAdmitter{}
			var v *types.ImageAdmissionVerdict
			if pinned {
				v = pinnedVerdict(admitPinned)
			}
			f := reviveAdmissionFixture(t, types.ImageAdmissionOff, v, a)
			f.rs.profiles[0].Limits.RequiredImageSigners = []string{admitSigner}
			code, body := f.reviveAs(t, true)
			if !pinned {
				wantReviveNotAdmitted(t, f, a, code, body)
				return
			}
			if code != http.StatusOK {
				t.Fatalf("revive = %d %s, want 200", code, body)
			}
			if len(a.requires) != 1 || a.requires[0].site || !slices.Equal(a.requires[0].profile, []string{admitSigner}) ||
				!slices.Equal(a.admits, []string{admitPinned}) {
				t.Errorf("Require %+v / Admit %q, want the profile's %s alone, checked at the pin", a.requires, a.admits, admitSigner)
			}
		})
	}
}

// compiledAdmitter is a fakeAdmitter whose coverage is the real one (imageadmit.Compile over the site block),
// so a test can see which signer entries cover which refs.
type compiledAdmitter struct{ fakeAdmitter }

func (c *compiledAdmitter) Require(adm *types.SiteImageAdmission, ref string, site bool, profile []string) (imageadmit.Requirements, *imageadmit.Verdict, error) {
	c.fakeAdmitter.Require(adm, ref, site, profile) // records the call
	p, err := imageadmit.Compile(imageadmit.SiteConfig(adm), imageadmit.Options{})
	if err != nil {
		return imageadmit.Requirements{}, nil, err
	}
	req, v := p.Require(ref, site, profile)
	return req, v, nil
}

// TestReviveImageAdmission_ShortNameSignerCoversTheCanonicalPin: the pin is the canonical name
// (docker.io/library/app:1@sha256:...), while signer entries match refs as typed. A signer written for the short
// name "app:1" covered the image at create, and still covers it at revive, where the registry is asked about the
// canonical pin.
func TestReviveImageAdmission_ShortNameSignerCoversTheCanonicalPin(t *testing.T) {
	typed := "app:1"
	pinned, err := imageAdmissionPinnedRef(typed, admitDigest)
	if err != nil || !strings.HasPrefix(pinned, "docker.io/library/app:1@") {
		t.Fatalf("pin of %q = %q, %v; want the canonical docker.io/library name", typed, pinned, err)
	}
	a := &compiledAdmitter{}
	f := reviveAdmissionFixture(t, types.ImageAdmissionEnforce, pinnedVerdict(pinned), a)
	f.st.site.Images.Admission.Signers[0].Images = []string{typed}
	f.st.run.ImageInput = &typed
	if code, body := f.reviveAs(t, true); code != http.StatusOK {
		t.Fatalf("revive = %d %s, want 200: the short-name signer covers the image", code, body)
	}
	if !slices.Equal(a.admits, []string{pinned}) {
		t.Errorf("Admit refs = %q, want the canonical pin %q", a.admits, pinned)
	}
}

// TestReviveImageAdmission_SiteUnreadableFailsClosed: a revive that cannot read the site config cannot know the
// mode, so it is refused (503) and replaces nothing.
func TestReviveImageAdmission_SiteUnreadableFailsClosed(t *testing.T) {
	a := &fakeAdmitter{}
	f := reviveAdmissionFixture(t, types.ImageAdmissionEnforce, pinnedVerdict(admitPinned), a)
	digestInput := admitPinned // pinned by digest: the digest rule reads no site config for it
	f.st.run.ImageInput = &digestInput
	f.st.siteErr = errors.New("db down")
	if code, body := f.reviveAs(t, true); code != http.StatusServiceUnavailable {
		t.Fatalf("revive with the site config unreadable = %d %s, want 503", code, body)
	}
	if len(f.rr.replaced) != 0 {
		t.Error("a revive that could not read image admission replaced the proxy")
	}
}

// convRef is an operator's convention image, covered by the fixture's signer; convPinned is it at admitDigest.
const (
	convRef    = "registry.example.com/team/agent:1"
	convPinned = convRef + "@" + admitDigest
)

// conventionVerdict is the verdict a launch records for a convention image checked under scope all, read from
// the JSON the run row holds: origin convention, ref the convention image as configured, pinned_ref set only
// when enforce admitted it.
func conventionVerdict(t *testing.T, mode, verdict, pinned string) *types.ImageAdmissionVerdict {
	t.Helper()
	raw := fmt.Sprintf(`{"mode":%q,"verdict":%q,"digest":%q,"pinned_ref":%q,"ref":%q,"signer":%q,"origin":"convention"}`,
		mode, verdict, admitDigest, pinned, convRef, admitSigner)
	var v types.ImageAdmissionVerdict
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatal(err)
	}
	return &v
}

// scopeAllConventionFixture is a lost member run that started from the convention image (no member image) on a
// deployment that now enforces admission with scope all, its create having recorded verdict.
func scopeAllConventionFixture(t *testing.T, verdict *types.ImageAdmissionVerdict, a ImageAdmitter) *reviveFixture {
	t.Helper()
	f := reviveAdmissionFixture(t, types.ImageAdmissionEnforce, verdict, a)
	f.st.site.Images.Admission.Scope = types.ImageAdmissionScopeAll
	conv := ""
	f.st.run.ImageInput, f.st.run.Image = &conv, convRef
	return f
}

func wantConventionNotAdmitted(t *testing.T, f *reviveFixture, a *fakeAdmitter, code int, body string) {
	t.Helper()
	if code != http.StatusForbidden || !strings.Contains(body, reviveNotAdmitted) || !strings.Contains(body, convRef) ||
		!strings.Contains(body, "Start a new run to use an allowed image") {
		t.Fatalf("revive = %d %s; want 403 %s naming the convention image, with the revive sentence", code, body, reviveNotAdmitted)
	}
	if r, n := a.calls(); r != 0 || n != 0 {
		t.Errorf("admitter asked %d Require / %d Admit; an unrecorded convention image must not be checked by its tag", r, n)
	}
	f.assertReviveRefused(t, reviveNotAdmitted)
}

// TestReviveImageAdmission_ScopeAllConventionImage: under enforce with scope all, a kept sandbox that started
// from the convention image is re-checked at revive by the digest its launch recorded, under the signers as they
// are now; a sandbox whose launch recorded no admitted digest (started while admission was off, in audit mode,
// or under scope member) is refused without resolving the tag. Scope member keeps the convention image
// unchecked, a profile's signers bind only a member's image, and operators stay exempt and marked.
func TestReviveImageAdmission_ScopeAllConventionImage(t *testing.T) {
	t.Run("admitted pin: re-checked at the recorded digest", func(t *testing.T) {
		a := &fakeAdmitter{}
		f := scopeAllConventionFixture(t, conventionVerdict(t, types.ImageAdmissionEnforce, types.ImageAdmissionAdmitted, convPinned), a)
		if code, body := f.reviveAs(t, true); code != http.StatusOK {
			t.Fatalf("revive = %d %s, want 200", code, body)
		}
		if len(a.requires) != 1 || a.requires[0].ref != convRef || !a.requires[0].site || len(a.requires[0].profile) != 0 ||
			!slices.Equal(a.admits, []string{convPinned}) {
			t.Errorf("Require %+v / Admit %q, want the site's coverage of %q and the registry at %q", a.requires, a.admits, convRef, convPinned)
		}
	})
	t.Run("signer removed since the launch: refused", func(t *testing.T) {
		a := &fakeAdmitter{require: func(bool, []string) (*imageadmit.Verdict, error) {
			return &imageadmit.Verdict{Reason: imageadmit.NoPolicy}, nil
		}}
		f := scopeAllConventionFixture(t, conventionVerdict(t, types.ImageAdmissionEnforce, types.ImageAdmissionAdmitted, convPinned), a)
		code, body := f.reviveAs(t, true)
		if code != http.StatusForbidden || !strings.Contains(body, reasonImageAdmissionNoPolicy) || !strings.Contains(body, "Start a new run to use an allowed image") {
			t.Fatalf("revive = %d %s; want 403 %s with the revive sentence", code, body, reasonImageAdmissionNoPolicy)
		}
		f.assertReviveRefused(t, reasonImageAdmissionNoPolicy)
	})
	for _, tc := range []struct {
		name    string
		verdict func(t *testing.T) *types.ImageAdmissionVerdict
	}{
		{"started while admission was off, now enforce", func(*testing.T) *types.ImageAdmissionVerdict { return nil }},
		{"started under audit (would refuse), now enforce", func(t *testing.T) *types.ImageAdmissionVerdict {
			return conventionVerdict(t, types.ImageAdmissionAudit, types.ImageAdmissionWouldRefuse, "")
		}},
		{"started under audit (admitted, never pinned), now enforce", func(t *testing.T) *types.ImageAdmissionVerdict {
			return conventionVerdict(t, types.ImageAdmissionAudit, types.ImageAdmissionAdmitted, "")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &fakeAdmitter{}
			f := scopeAllConventionFixture(t, tc.verdict(t), a)
			code, body := f.reviveAs(t, true)
			wantConventionNotAdmitted(t, f, a, code, body)
		})
	}
	t.Run("admin bulk restart takes the same path", func(t *testing.T) {
		a := &fakeAdmitter{}
		f := scopeAllConventionFixture(t, nil, a)
		res := f.restart(t)
		if res.OK || res.Reason != reviveNotAdmitted {
			t.Fatalf("restart result = %+v, want refused with %s", res, reviveNotAdmitted)
		}
		if r, n := a.calls(); r != 0 || n != 0 {
			t.Errorf("restart asked %d Require / %d Admit, want none", r, n)
		}
	})
	t.Run("admin bulk restart re-checks an admitted pin", func(t *testing.T) {
		a := &fakeAdmitter{}
		f := scopeAllConventionFixture(t, conventionVerdict(t, types.ImageAdmissionEnforce, types.ImageAdmissionAdmitted, convPinned), a)
		if res := f.restart(t); !res.OK {
			t.Fatalf("restart result = %+v, want ok", res)
		}
		if !slices.Equal(a.admits, []string{convPinned}) {
			t.Errorf("Admit refs = %q, want %q", a.admits, convPinned)
		}
	})
	t.Run("operator-owned run: exempt and marked", func(t *testing.T) {
		a := &fakeAdmitter{}
		f := scopeAllConventionFixture(t, nil, a)
		f.st.run.OperatorOwned = true
		if code, body := f.reviveAs(t, false); code != http.StatusOK || !reviveMark(t, f) {
			t.Fatalf("operator revive = %d %s (marked %v), want 200 marked", code, body, reviveMark(t, f))
		}
		if r, n := a.calls(); r != 0 || n != 0 {
			t.Errorf("operator revive asked %d Require / %d Admit, want none", r, n)
		}
	})
	for _, tc := range []struct {
		name string
		edit func(*reviveFixture)
	}{
		{"scope member: the convention image stays unchecked", func(f *reviveFixture) { f.st.site.Images.Admission.Scope = types.ImageAdmissionScopeMember }},
		{"site off, profile signer: binds only a member's image", func(f *reviveFixture) {
			f.st.site.Images.Admission.Mode = types.ImageAdmissionOff
			f.rs.profiles[0].Limits.RequiredImageSigners = []string{admitSigner}
		}},
		{"audit, scope all: a revive refuses nothing", func(f *reviveFixture) { f.st.site.Images.Admission.Mode = types.ImageAdmissionAudit }},
		{"macOS VM", func(f *reviveFixture) { f.st.run.MachineKind = types.MachineMacOSVM }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &fakeAdmitter{}
			f := scopeAllConventionFixture(t, nil, a)
			tc.edit(f)
			if code, body := f.reviveAs(t, true); code != http.StatusOK {
				t.Fatalf("revive = %d %s, want 200", code, body)
			}
			if r, n := a.calls(); r != 0 || n != 0 {
				t.Errorf("asked %d Require / %d Admit, want none", r, n)
			}
		})
	}
}
