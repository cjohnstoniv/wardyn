// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/imageadmit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The image's origin, not its ref, decides whether a run is on the convention image. A member who chooses an
// image whose ref is the convention image's (a ready prewrapped row named in the request, or a workspace's
// prewrapped base image) keeps the member's admission: the launch neither checks nor records it as the
// convention image, and a revive holds it to the member's profile signers as they are now.

// originRef is an operator's convention image written as a canonical digest ref, so its pin is itself.
const originRef = "registry.example.com/team/agent@" + admitDigest

// originSite enforces admission with scope, its site signers both covering originRef; a member's profile may
// require release-ci, whose signature originRef does not carry.
func originSite(scope string) types.SiteConfig {
	sc := admissionSite(types.ImageAdmissionEnforce)
	sc.Images.Admission.Scope = scope
	sc.Images.Admission.Signers = append(sc.Images.Admission.Signers, types.ImageSigner{
		Name: "release-ci", Images: []string{"registry.example.com/team/*"},
		Keyless: &types.ImageKeyless{Issuer: "https://token.actions.githubusercontent.com", Identity: "https://ci.example/release@refs/heads/main"},
	})
	return sc
}

// originAdmitter has the real coverage, and a registry where originRef is signed by the site signer only: a
// check that requires a profile signer refuses it.
func originAdmitter() *compiledAdmitter {
	return &compiledAdmitter{fakeAdmitter{admit: func(req imageadmit.Requirements) (imageadmit.Verdict, error) {
		if len(req.AllOf) > 0 {
			return imageadmit.Verdict{Reason: imageadmit.SignatureMissing, Digest: admitDigest, Signers: []string{"release-ci"}}, nil
		}
		return imageadmit.Verdict{Digest: admitDigest, Signer: admitSigner}, nil
	}}}
}

// originReviveFixture is a lost member run that named originRef and holds kept, on a site with scope, whose
// owner's profile now requires release-ci.
func originReviveFixture(t *testing.T, kept *types.ImageAdmissionVerdict, scope string) (*reviveFixture, *compiledAdmitter) {
	t.Helper()
	a := originAdmitter()
	f := reviveAdmissionFixture(t, types.ImageAdmissionEnforce, kept, a)
	f.st.site.Images = originSite(scope).Images
	in := originRef
	f.st.run.ImageInput, f.st.run.Image = &in, originRef
	f.rs.profiles[0].Limits.RequiredImageSigners = []string{"release-ci"}
	return f, a
}

// TestImageOriginMemberImageEqualToTheConvention: a member's prewrapped image whose ref equals the convention
// image's is launched under the member's verdict, unchecked as the convention image and with nothing recorded
// over that verdict. Once the member's profile requires a signer the image lacks, its revive and the admin bulk
// restart both refuse it, under scope all and scope member alike.
func TestImageOriginMemberImageEqualToTheConvention(t *testing.T) {
	if pinned, err := imageAdmissionPinnedRef(originRef, admitDigest); err != nil || pinned != originRef {
		t.Fatalf("pin of %q = %q, %v; want the ref itself", originRef, pinned, err)
	}
	row := types.BaseImageEntry{Kind: types.BaseImageKindPrewrapped, Image: originRef,
		Check: &types.BaseImageCheck{Status: types.BaseImageCheckReady, AcceptedArches: []string{"amd64"}}}
	ws := types.Workspace{
		ID:        uuid.New(),
		Sources:   []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
		BaseImage: &types.WorkspaceBaseImage{Kind: types.BaseImageKindPrewrapped, Image: originRef},
	}
	member := pinnedVerdict(originRef) // the member's image, checked and pinned at create
	for _, tc := range []struct {
		name   string
		req    createRunRequest
		wsRefs []types.Workspace
	}{
		{"prewrapped image named in the request", createRunRequest{Agent: "claude-code", Image: originRef}, nil},
		{"workspace's prewrapped base image", createRunRequest{Agent: "claude-code"}, []types.Workspace{ws}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, st, _, run := pinFixture(t, member)
			a := &fakeAdmitter{}
			st.prewrapped, st.site = row, originSite(types.ImageAdmissionScopeAll)
			srv.cfg.AgentImages, srv.cfg.ImageAdmitter = map[string]string{"claude-code": originRef}, a
			if got := agentImage("claude-code", srv.cfg.AgentImages); got != originRef {
				t.Fatalf("convention image = %q, want %q", got, originRef)
			}
			image, _, failed := srv.resolveCreateRunImage(t.Context(), tc.req, run, tc.wsRefs)
			if failed || image != originRef {
				t.Fatalf("dispatched %q (failed %v), want the member's pin %q", image, failed, originRef)
			}
			if r, n := a.calls(); r != 0 || n != 0 {
				t.Errorf("launch asked %d Require / %d Admit; the member's image is not checked as the convention image", r, n)
			}
			kept := member
			if v := st.recorded(); v != nil {
				t.Errorf("launch recorded %+v over the member's verdict", v)
				kept = v
			}
			for _, scope := range []string{types.ImageAdmissionScopeAll, types.ImageAdmissionScopeMember} {
				t.Run("revive, scope "+scope, func(t *testing.T) {
					f, a := originReviveFixture(t, kept, scope)
					code, body := f.reviveAs(t, true)
					if code != http.StatusForbidden || !strings.Contains(body, reasonImageSignatureMissing) ||
						!strings.Contains(body, "Start a new run to use an allowed image") {
						t.Fatalf("revive = %d %s; want 403 %s: the profile now requires release-ci", code, body, reasonImageSignatureMissing)
					}
					if len(a.requires) != 1 || !slices.Equal(a.requires[0].profile, []string{"release-ci"}) || a.requires[0].ref != originRef {
						t.Errorf("Require %+v, want the member's image under the profile's release-ci", a.requires)
					}
					f.assertReviveRefused(t, reasonImageSignatureMissing)
				})
				t.Run("admin bulk restart, scope "+scope, func(t *testing.T) {
					f, _ := originReviveFixture(t, kept, scope)
					if res := f.restart(t); res.OK || res.Reason != reasonImageSignatureMissing {
						t.Fatalf("restart result = %+v, want refused with %s", res, reasonImageSignatureMissing)
					}
				})
			}
		})
	}
}

// TestImageOriginReviveKeepsTheMemberImage: a run whose create named a member image is never revived as the
// convention image, even when its verdict records a convention check: it has no admitted pin of the member's
// image, so it is refused without a check, never re-checked against the site's signers alone.
func TestImageOriginReviveKeepsTheMemberImage(t *testing.T) {
	stamped := pinnedVerdict(originRef)
	stamped.Ref = originRef
	for _, scope := range []string{types.ImageAdmissionScopeAll, types.ImageAdmissionScopeMember} {
		t.Run("scope "+scope, func(t *testing.T) {
			f, a := originReviveFixture(t, stamped, scope)
			code, body := f.reviveAs(t, true)
			if code != http.StatusForbidden || !strings.Contains(body, reviveNotAdmitted) || !strings.Contains(body, originRef) {
				t.Fatalf("revive = %d %s; want 403 %s naming the image", code, body, reviveNotAdmitted)
			}
			if r, n := a.calls(); r != 0 || n != 0 {
				t.Errorf("admitter asked %d Require / %d Admit, want none", r, n)
			}
			f.assertReviveRefused(t, reviveNotAdmitted)
		})
	}
}
