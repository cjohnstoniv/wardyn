// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestPolicyPreviewImageAdmissionCheckedAtLaunch: the preview reads no registry, so where launch would check
// the member's image it reports image_admission "checked at launch", and where launch checks nothing the key
// is absent (a preview with admission off is byte-identical). For every case the preview says it exactly
// when the launch step (at Review) asks the admitter.
func TestPolicyPreviewImageAdmissionCheckedAtLaunch(t *testing.T) {
	member := memberCtx(nil)
	admin := operatorCtx(capSub, capEmail, oidc.RoleAdmin)
	for _, tc := range []struct {
		name    string
		site    types.SiteConfig
		ctx     context.Context
		profile []string
		image   string
		machine types.MachineKind
		govern  bool
		want    bool
	}{
		{"block absent", types.SiteConfig{}, member, nil, admitTagRef, "", false, false},
		{"off with signers", admissionSite(types.ImageAdmissionOff), member, nil, admitTagRef, "", false, false},
		{"enforce", admissionSite(types.ImageAdmissionEnforce), member, nil, admitTagRef, "", false, true},
		{"audit", admissionSite(types.ImageAdmissionAudit), member, nil, admitTagRef, "", false, true},
		{"profile signer, site off", types.SiteConfig{}, member, []string{admitProfile}, admitTagRef, "", false, true},
		{"enforce, pinned", admissionSite(types.ImageAdmissionEnforce), member, nil, admitPinned, "", false, true},
		{"no member image", admissionSite(types.ImageAdmissionEnforce), member, nil, "", "", false, false},
		{"operator", admissionSite(types.ImageAdmissionEnforce), admin, nil, admitTagRef, "", false, false},
		{"governed admin", admissionSite(types.ImageAdmissionEnforce), admin, nil, admitTagRef, "", true, true},
		{"macOS VM", admissionSite(types.ImageAdmissionEnforce), member, nil, admitTagRef, types.MachineMacOSVM, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &fakeAdmitter{}
			s := admissionServer(tc.site, a)
			s.cfg.GovernAdminRuns = tc.govern
			req := createRunRequest{Image: tc.image, MachineKind: string(tc.machine)}
			got := s.imageAdmissionPreview(governReq(tc.ctx).Context(), req, "", tc.profile)
			if r, ad := a.calls(); r != 0 || ad != 0 {
				t.Errorf("preview asked the admitter: requires=%d admits=%d", r, ad)
			}
			if (got == client.ImageAdmissionCheckedAtLaunch) != tc.want || (!tc.want && got != "") {
				t.Errorf("preview image_admission = %q, want checked at launch = %v", got, tc.want)
			}
			ls := admissionServer(tc.site, &fakeAdmitter{})
			ls.cfg.GovernAdminRuns = tc.govern
			launch := runAdmission(ls, tc.ctx, foldPreflight, tc.image, tc.profile,
				func(f *runFold) { f.req.MachineKind = string(tc.machine) })
			if checked := launch.f.admission != nil && launch.f.admission.Verdict != types.ImageAdmissionNotApplicable; checked != tc.want {
				t.Errorf("launch checked = %v (verdict %+v), preview says %v: they disagree", checked, launch.f.admission, tc.want)
			}
		})
	}
}

// TestPolicyPreviewImageAdmissionWire: the fact rides the preview reply as image_admission, absent when unset,
// and the preview handler sets it.
func TestPolicyPreviewImageAdmissionWire(t *testing.T) {
	off, _ := json.Marshal(policyPreviewResponse{})
	if strings.Contains(string(off), "image_admission") {
		t.Errorf("preview with no fact = %s, want no image_admission key", off)
	}
	on, _ := json.Marshal(policyPreviewResponse{ImageAdmission: client.ImageAdmissionCheckedAtLaunch})
	if !strings.Contains(string(on), `"image_admission":"checked at launch"`) {
		t.Errorf("preview with the fact = %s", on)
	}
	src, err := os.ReadFile("policy_preview.go")
	if err != nil || !strings.Contains(string(src), "response.ImageAdmission = s.imageAdmissionPreview(") {
		t.Error("handlePolicyPreview does not set image_admission from imageAdmissionPreview")
	}
}
