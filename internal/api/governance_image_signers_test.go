// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"os"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A profile, an overlay or a restriction that names required_image_signers may name only the signers the site
// defines, or the built-in wardyn-release: a profile can require trust the site grants, never add any.
func TestRequiredImageSignersNamesCheckedAgainstSite(t *testing.T) {
	fake := &fakeSiteConfigStore{cfg: types.SiteConfig{Images: &types.SiteImages{Admission: &types.SiteImageAdmission{
		Mode: "audit", Signers: []types.ImageSigner{admissionSigner("corp-ci")}}}}}
	srv, _ := newSiteConfigHarness(t, fake)
	ctx := t.Context()
	for _, tc := range []struct {
		name  string
		names []string
		bad   string
	}{
		{"none", nil, ""},
		{"a site signer", []string{"corp-ci"}, ""},
		{"the built-in signer", []string{"wardyn-release"}, ""},
		{"both", []string{"corp-ci", "wardyn-release"}, ""},
		{"an unknown name", []string{"corp-ci", "ghost"}, "ghost"},
		{"a malformed name", []string{"Corp CI"}, "Corp CI"},
	} {
		msg := srv.requiredImageSignersRefusal(ctx, "limits.required_image_signers", tc.names)
		switch {
		case tc.bad == "" && msg != "":
			t.Errorf("%s: refused: %s", tc.name, msg)
		case tc.bad != "" && (!strings.Contains(msg, tc.bad) || !strings.HasPrefix(msg, "limits.required_image_signers")):
			t.Errorf("%s: message %q should name the field and %q", tc.name, msg, tc.bad)
		}
	}
}

// With no signer defined at all only the built-in is nameable; an unreadable site config refuses rather than
// accept a name it cannot check.
func TestRequiredImageSignersNoSiteBlockAndUnreadable(t *testing.T) {
	srv, _ := newSiteConfigHarness(t, &fakeSiteConfigStore{})
	if msg := srv.requiredImageSignersRefusal(t.Context(), "limits.required_image_signers", []string{"wardyn-release"}); msg != "" {
		t.Errorf("the built-in with no site block: %s", msg)
	}
	if msg := srv.requiredImageSignersRefusal(t.Context(), "limits.required_image_signers", []string{"corp-ci"}); msg == "" {
		t.Error("a name with no site signers was accepted")
	}
	broken, _ := newSiteConfigHarness(t, &fakeSiteConfigStore{getErr: os.ErrClosed})
	if msg := broken.requiredImageSignersRefusal(t.Context(), "limits.required_image_signers", []string{"corp-ci"}); msg == "" {
		t.Error("a name was accepted while the site config could not be read")
	}
	if msg := broken.requiredImageSignersRefusal(t.Context(), "limits.required_image_signers", nil); msg != "" {
		t.Errorf("no names need no site read: %s", msg)
	}
}

// The three doors that write the field call the check: the profile's limits and overlay_limits, and the
// restriction's limits.
func TestRequiredImageSignersDoorsCallTheCheck(t *testing.T) {
	for file, want := range map[string][]string{
		"governance_unit_write.go":         {`requiredImageSignersRefusal(r.Context(), "limits.required_image_signers"`, `requiredImageSignersRefusal(r.Context(), "overlay_limits.required_image_signers"`},
		"governance_restriction_routes.go": {`requiredImageSignersRefusal(r.Context(), "limits.required_image_signers"`},
	} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range want {
			if !strings.Contains(string(src), w) {
				t.Errorf("%s never calls %s", file, w)
			}
		}
	}
}
