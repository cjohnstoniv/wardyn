// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for the console branding record (migration 0091).
// Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPG_Branding_RoundTripKeepAndClear(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	// One row per database: start from none, whatever an earlier run left.
	if _, err := st.DeleteBranding(ctx); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if _, err := st.GetBranding(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unbranded get: err = %v, want ErrNotFound", err)
	}

	logo := []byte("\x89PNG fixture")
	b := types.Branding{
		OrgName: "Example Corp", NameFormat: types.BrandNamePrefix, Primary: "#7c3aed", PrimaryText: "#ffffff",
		SupportURL: "https://status.example.com", Logo: logo, LogoType: "image/png", UpdatedBy: "admin@example.com",
	}
	saved, err := st.PutBranding(ctx, b, false)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if saved.OrgName != b.OrgName || !bytes.Equal(saved.Logo, logo) || saved.LogoType != "image/png" || saved.UpdatedAt.IsZero() {
		t.Fatalf("saved = %+v", saved)
	}

	// keepLogo: the second save changes the name and format and keeps the logo.
	b.OrgName, b.NameFormat, b.Logo, b.LogoType = "Example Two", types.BrandNameSuffix, nil, ""
	b.DarkPrimary, b.DarkPrimaryText = "#a78bfa", "#171717"
	if _, err := st.PutBranding(ctx, b, true); err != nil {
		t.Fatalf("put keep-logo: %v", err)
	}
	got, err := st.GetBranding(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.OrgName != "Example Two" || got.NameFormat != types.BrandNameSuffix || got.DarkPrimary != "#a78bfa" ||
		!bytes.Equal(got.Logo, logo) || got.LogoType != "image/png" {
		t.Fatalf("after keep-logo save: %+v", got)
	}

	// keepLogo=false with no logo removes it.
	if _, err := st.PutBranding(ctx, b, false); err != nil {
		t.Fatalf("put remove-logo: %v", err)
	}
	if got, _ := st.GetBranding(ctx); len(got.Logo) != 0 || got.LogoType != "" {
		t.Fatalf("logo not removed: %d bytes, type %q", len(got.Logo), got.LogoType)
	}

	// #1215: SetBrandingLogo marks a file-delivered logo; a keep-logo save keeps
	// the mark, an upload or a removal clears it, and an empty logo clears it.
	if ok, err := st.SetBrandingLogo(ctx, logo, "image/png", true, "site-config"); err != nil || !ok {
		t.Fatalf("set logo: ok=%v err=%v", ok, err)
	}
	if _, err := st.PutBranding(ctx, b, true); err != nil {
		t.Fatalf("put keep-logo: %v", err)
	}
	if got, _ := st.GetBranding(ctx); !got.LogoFromFile || !bytes.Equal(got.Logo, logo) {
		t.Fatalf("keep-logo save lost the file-delivered mark: %+v", got)
	}
	b.Logo, b.LogoType = logo, "image/png"
	if saved, err := st.PutBranding(ctx, b, false); err != nil || saved.LogoFromFile {
		t.Fatalf("an upload kept the file-delivered mark: %+v err=%v", saved, err)
	}
	if _, err := st.SetBrandingLogo(ctx, logo, "image/png", true, "site-config"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetBrandingLogo(ctx, nil, "", true, "site-config"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetBranding(ctx); got.LogoFromFile || len(got.Logo) != 0 {
		t.Fatalf("an empty logo left the mark or the bytes: %+v", got)
	}
	b.Logo, b.LogoType = nil, ""

	// The schema refuses a format or logo type the API never writes.
	b.NameFormat = "both"
	if _, err := st.PutBranding(ctx, b, false); err == nil {
		t.Fatal("name_format CHECK did not refuse \"both\"")
	}

	removed, err := st.DeleteBranding(ctx)
	if err != nil || !removed {
		t.Fatalf("delete: removed=%v err=%v", removed, err)
	}
	if removed, _ := st.DeleteBranding(ctx); removed {
		t.Fatal("second delete reported a row")
	}
	if ok, err := st.SetBrandingLogo(ctx, logo, "image/png", true, "x"); err != nil || ok {
		t.Fatalf("set logo with no record: ok=%v err=%v, want false", ok, err)
	}
}
