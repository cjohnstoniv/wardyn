// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestMarkImageAdmissionExempt: while image admission is on (enforce or audit), an operator's or the
// break-glass admin's run.create row says image_admission_exempt, so the org can see the runs admission did
// not check. The key is absent with admission off (block absent, or off with signers defined), for a run with
// no member image, and for a governed caller, whose image is checked and carries image_admission instead.
// Marking reads no registry.
func TestMarkImageAdmissionExempt(t *testing.T) {
	member := memberCtx(nil)
	admin := operatorCtx(capSub, capEmail, oidc.RoleAdmin)
	for _, tc := range []struct {
		name   string
		site   types.SiteConfig
		ctx    context.Context
		govern bool
		image  string
		want   bool
	}{
		{"operator, enforce, tag", admissionSite(types.ImageAdmissionEnforce), admin, false, admitTagRef, true},
		{"operator, audit, tag", admissionSite(types.ImageAdmissionAudit), admin, false, admitTagRef, true},
		{"operator, enforce, pinned", admissionSite(types.ImageAdmissionEnforce), admin, false, admitPinned, true},
		{"operator, block absent", types.SiteConfig{}, admin, false, admitTagRef, false},
		{"operator, off with signers", admissionSite(types.ImageAdmissionOff), admin, false, admitTagRef, false},
		{"operator, enforce, no image", admissionSite(types.ImageAdmissionEnforce), admin, false, "", false},
		{"member, enforce", admissionSite(types.ImageAdmissionEnforce), member, false, admitTagRef, false},
		{"governed admin, enforce", admissionSite(types.ImageAdmissionEnforce), admin, true, admitTagRef, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &fakeAdmitter{}
			s := admissionServer(tc.site, a)
			s.cfg.GovernAdminRuns = tc.govern
			data := map[string]any{}
			s.markRunCreateExempt(tc.ctx, tc.image, data)
			got, has := data["image_admission_exempt"]
			if has != tc.want || (has && got != true) {
				t.Errorf("data = %v, want image_admission_exempt: true present = %v", data, tc.want)
			}
			if r, ad := a.calls(); r != 0 || ad != 0 {
				t.Errorf("marking asked the admitter: requires=%d admits=%d", r, ad)
			}
		})
	}
	// An unreadable site config leaves the row unmarked, as the digest rule's mark does; the create itself
	// already failed closed in the fold for a governed caller.
	s := &Server{cfg: Config{Store: admitSiteUnreadable{&capStore{}}}}
	data := map[string]any{}
	s.markRunCreateExempt(admin, admitTagRef, data)
	if _, has := data["image_admission_exempt"]; has {
		t.Errorf("site config unreadable: data = %v, want no image_admission_exempt", data)
	}
}
