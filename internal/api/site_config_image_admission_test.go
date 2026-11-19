// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Image admission's site settings (images.admission): written through PUT /site-config like images.require_digest,
// validated at write by validateSiteImageAdmission, carried forward when a body omits the block, and audited by name.

const publicGoodRootFile = "../releasesig/trusted_root.json"

func admissionSigner(name string) types.ImageSigner {
	return types.ImageSigner{
		Name:   name,
		Images: []string{"registry.corp.example/team/*", "registry.corp.example/tools/app"},
		Keyless: &types.ImageKeyless{
			Issuer:   "https://token.actions.githubusercontent.com",
			Identity: "https://github.com/corp/images/.github/workflows/build.yml@refs/heads/main",
		},
	}
}

func TestValidateSiteImageAdmission(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	was := imageTrustedRootDir
	imageTrustedRootDir = dir
	t.Cleanup(func() { imageTrustedRootDir = was })
	pg, err := os.ReadFile(mustAbs(t, publicGoodRootFile))
	if err != nil {
		t.Fatal(err)
	}
	inDir := writeFile(t, filepath.Join(dir, "private.json"), pg)
	outsideRoot := writeFile(t, filepath.Join(outside, "root.json"), pg)
	garbage := writeFile(t, filepath.Join(dir, "garbage.json"), []byte("this is not a trusted root"))
	// A symbolic link that stays in the directory (a mounted Secret's files are links), and one that leaves it.
	for link, target := range map[string]string{"linked.json": inDir, "escape.json": outsideRoot} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	root := func(path string) func(a *types.SiteImageAdmission) {
		return func(a *types.SiteImageAdmission) {
			a.TrustedRoots = []types.ImageTrustedRoot{{Name: "private", Path: path}}
			a.Signers[0].Keyless.TrustedRoot = "private"
		}
	}
	with := func(mut func(a *types.SiteImageAdmission)) *types.SiteImages {
		a := &types.SiteImageAdmission{Mode: "audit", Signers: []types.ImageSigner{admissionSigner("corp-ci")}}
		mut(a)
		return &types.SiteImages{Admission: a}
	}
	for _, tc := range []struct {
		name string
		im   *types.SiteImages
	}{
		{"no images block", nil},
		{"a block without admission", &types.SiteImages{RequireDigest: true}},
		{"prefix and exact image entries", with(func(*types.SiteImageAdmission) {})},
		{"a readable trusted root in the directory", with(root(inDir))},
		{"a link to a root in the directory", with(root(filepath.Join(dir, "linked.json")))},
	} {
		if err := validateSiteImageAdmission(tc.im); err != nil {
			t.Errorf("%s: refused: %v", tc.name, err)
		}
	}
	for _, tc := range []struct {
		name string
		im   *types.SiteImages
		want string
	}{
		{"an unknown mode", with(func(a *types.SiteImageAdmission) { a.Mode = "block" }), "mode"},
		{"an image entry that is not a ref or a prefix", with(func(a *types.SiteImageAdmission) { a.Signers[0].Images = []string{"Not An Image"} }), "images[0]"},
		{"a wildcard that is not a registry prefix", with(func(a *types.SiteImageAdmission) { a.Signers[0].Images = []string{"team/*"} }), "images[0]"},
		{"a trusted root that is not readable", with(func(a *types.SiteImageAdmission) {
			a.TrustedRoots = []types.ImageTrustedRoot{{Name: "private", Path: filepath.Join(dir, "missing.json")}}
		}), "private"},
		{"a trusted root that does not parse", with(func(a *types.SiteImageAdmission) {
			a.TrustedRoots = []types.ImageTrustedRoot{{Name: "private", Path: garbage}}
		}), "private"},
		{"a root outside the directory", with(root(outsideRoot)), "must be in " + dir},
		{"a ../ escape from the directory", with(root(dir + "/../" + filepath.Base(outside) + "/root.json")), "must be in " + dir},
		{"a link that leaves the directory", with(root(filepath.Join(dir, "escape.json"))), "must be in " + dir},
		{"enforce with no signer", &types.SiteImages{Admission: &types.SiteImageAdmission{Mode: "enforce"}}, "audit"},
		{"a keyless signer with neither identity form", with(func(a *types.SiteImageAdmission) { a.Signers[0].Keyless.Identity = "" }), "identity"},
	} {
		err := validateSiteImageAdmission(tc.im)
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
		} else if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "images.admission") {
			t.Errorf("%s: error %q should name images.admission and %q", tc.name, err, tc.want)
		} else if strings.Contains(err.Error(), "this is not a trusted root") || strings.Contains(err.Error(), "mediaType") {
			t.Errorf("%s: error %q quotes the file", tc.name, err)
		}
	}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatal(err)
	}
	return abs
}

// validateSiteConfigBody is the door: a bad block is a 400, a good one is stored and read back.
func TestPutSiteConfigImageAdmission(t *testing.T) {
	fake := &fakeSiteConfigStore{}
	srv, audit := newSiteConfigHarness(t, fake)
	put := func(images any) (int, string) {
		body, _ := json.Marshal(map[string]any{"images": images})
		w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, string(body))
		return w.Code, w.Body.String()
	}
	if code, body := put(map[string]any{"admission": map[string]any{"mode": "block"}}); code != http.StatusBadRequest || !strings.Contains(body, "images.admission") {
		t.Fatalf("PUT unknown mode = %d %s, want 400 naming images.admission", code, body)
	}
	if fake.putSeen != nil {
		t.Fatalf("a refused block was stored: %+v", fake.putSeen.Images)
	}
	if code, body := put(map[string]any{"admission": map[string]any{
		"mode": "audit", "signers": []any{admissionSigner("corp-ci")},
	}}); code != http.StatusOK {
		t.Fatalf("PUT = %d %s", code, body)
	}
	got := fake.putSeen.Images
	if got == nil || got.Admission == nil || got.Admission.Mode != "audit" || !slices.Equal(got.Admission.SignerNames(), []string{"corp-ci"}) {
		t.Fatalf("stored %+v", got)
	}
	var datum map[string]any
	for _, ev := range audit.snapshot() {
		if ev.Action == "site_config.write" {
			_ = json.Unmarshal(ev.Data, &datum)
		}
	}
	if datum["images_admission_mode"] != "audit" {
		t.Errorf("site_config.write datum = %v, want images_admission_mode audit", datum)
	}
}

func TestSiteImagesCarryForwardKeepsAdmission(t *testing.T) {
	stored := &types.SiteImages{RequireDigest: true, Admission: &types.SiteImageAdmission{
		Mode: "enforce", Signers: []types.ImageSigner{admissionSigner("corp-ci")}}}
	for _, tc := range []struct {
		name      string
		body      *types.SiteImages
		named     bool
		wantNil   bool
		wantAdmit bool
	}{
		{"a body that omits images keeps the admission block", nil, false, false, true},
		{"a body naming only require_digest replaces the block", &types.SiteImages{RequireDigest: true}, true, false, false},
		{"an all-default admission block is stored as none", &types.SiteImages{Admission: &types.SiteImageAdmission{Mode: "off", Scope: "member"}}, true, true, false},
		{"an empty admission block beside require_digest drops only the admission", &types.SiteImages{RequireDigest: true, Admission: &types.SiteImageAdmission{}}, true, false, false},
		{"off with signers defined is kept", &types.SiteImages{Admission: &types.SiteImageAdmission{Signers: []types.ImageSigner{admissionSigner("corp-ci")}}}, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &types.SiteConfig{Images: tc.body}
			carryForwardSiteImages(cfg, types.SiteConfig{Images: stored}, map[string]bool{"images": tc.named})
			if (cfg.Images == nil) != tc.wantNil {
				t.Fatalf("Images = %+v, want nil = %v", cfg.Images, tc.wantNil)
			}
			if got := cfg.Images != nil && cfg.Images.Admission != nil; got != tc.wantAdmit {
				t.Errorf("admission kept = %v, want %v (%+v)", got, tc.wantAdmit, cfg.Images)
			}
		})
	}
}

func TestAuditSiteImagesAdmissionKeys(t *testing.T) {
	signers := func(names ...string) *types.SiteImages {
		a := &types.SiteImageAdmission{Mode: "audit", Scope: "all"}
		for _, n := range names {
			a.Signers = append(a.Signers, admissionSigner(n))
		}
		return &types.SiteImages{Admission: a}
	}
	datum := func(before, after *types.SiteImages, named bool) map[string]any {
		d := map[string]any{}
		auditSiteImages(d, before, after, named)
		return d
	}
	for _, key := range []string{"images_admission_mode", "images_admission_scope", "images_admission_signers"} {
		if _, has := datum(nil, nil, false)[key]; has {
			t.Errorf("a deployment with no block wrote %s", key)
		}
		if _, has := datum(nil, &types.SiteImages{RequireDigest: true}, true)[key]; has {
			t.Errorf("a block that names only require_digest wrote %s", key)
		}
	}
	d := datum(nil, signers("corp-ci", "vendor"), true)
	if d["images_admission_mode"] != "audit" || d["images_admission_scope"] != "all" {
		t.Errorf("datum = %v, want mode audit and scope all", d)
	}
	if got, _ := d["images_admission_signers"].([]string); !slices.Equal(got, []string{"corp-ci", "vendor"}) {
		t.Errorf("images_admission_signers = %v, want names only", d["images_admission_signers"])
	}
	if raw, _ := json.Marshal(d); strings.Contains(string(raw), "token.actions.githubusercontent.com") {
		t.Errorf("the row carries a signer's identity or issuer, not just its name: %s", raw)
	}
	if _, has := d["images_admission_signers_before"]; !has {
		t.Errorf("the signer set went from none to two: want before and after sets, got %v", d)
	}
	same := datum(signers("corp-ci"), signers("corp-ci"), true)
	for _, key := range []string{"images_admission_signers_before", "images_admission_signers_after", "images_admission_mode_before"} {
		if _, has := same[key]; has {
			t.Errorf("an unchanged admission block wrote %s", key)
		}
	}
	changed := datum(signers("corp-ci"), signers("vendor"), true)
	before, _ := changed["images_admission_signers_before"].([]string)
	after, _ := changed["images_admission_signers_after"].([]string)
	if !slices.Equal(before, []string{"corp-ci"}) || !slices.Equal(after, []string{"vendor"}) {
		t.Errorf("before %v after %v, want [corp-ci] and [vendor]", before, after)
	}
	removed := datum(signers("corp-ci"), nil, true)
	if removed["images_admission_mode"] != "off" || removed["images_admission_mode_before"] != "audit" {
		t.Errorf("removing the block wrote %v, want mode off, was audit", removed)
	}
}

// site_config.go sits at the 1000-line gate: the validation is one call there.
func TestSiteConfigBodyCallsImageAdmissionValidation(t *testing.T) {
	src, err := os.ReadFile("site_config.go")
	if err != nil || !strings.Contains(string(src), "validateSiteImageAdmission(cfg.Images)") {
		t.Errorf("validateSiteConfigBody does not call validateSiteImageAdmission(cfg.Images): %v", err)
	}
	if n := strings.Count(string(src), "\n"); n > 1000 {
		t.Errorf("site_config.go is %d lines; the gate is 1000", n)
	}
}
