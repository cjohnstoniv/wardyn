// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// siteBrandStore is the site-config fake plus the in-memory branding record.
type siteBrandStore struct {
	*fakeSiteConfigStore
	br *brandingMemStore
}

func (s siteBrandStore) GetBranding(ctx context.Context) (types.Branding, error) {
	return s.br.GetBranding(ctx)
}

func (s siteBrandStore) PutBranding(ctx context.Context, b types.Branding, keep bool) (types.Branding, error) {
	return s.br.PutBranding(ctx, b, keep)
}

func (s siteBrandStore) DeleteBranding(ctx context.Context) (bool, error) {
	return s.br.DeleteBranding(ctx)
}

func (s siteBrandStore) SetBrandingLogo(ctx context.Context, logo []byte, typ string, fromFile bool, by string) (bool, error) {
	return s.br.SetBrandingLogo(ctx, logo, typ, fromFile, by)
}

func siteBrandingServer(t *testing.T, branded bool) (*Server, *siteBrandStore, *recRecorder) {
	t.Helper()
	h := newHarness(t)
	st := &siteBrandStore{fakeSiteConfigStore: &fakeSiteConfigStore{}, br: &brandingMemStore{}}
	if branded {
		st.br.rec = &types.Branding{OrgName: "Example Corp", NameFormat: types.BrandNamePrefix, Primary: "#7c3aed", PrimaryText: "#ffffff"}
	}
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Secrets = getErrStore{getErr: secretstore.ErrNotFound}
	cfg.Approvals = h.approvals
	return New(cfg), st, h.audit
}

func putSiteLogoPath(t *testing.T, srv *Server, path string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"branding": map[string]any{"logo_path": path}})
	w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, string(body))
	return w.Code, w.Body.String()
}

func writeFile(t *testing.T, path string, data []byte) string {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A site config naming logo_path stores the file as the logo, marks it
// file-delivered, and the signed-in read says so.
func TestSiteConfigBrandingLogoApplies(t *testing.T) {
	srv, st, audit := siteBrandingServer(t, true)
	png := testPNG(t, 8)
	path := writeFile(t, filepath.Join(t.TempDir(), "logo.png"), png)

	if code, body := putSiteLogoPath(t, srv, path); code != http.StatusOK {
		t.Fatalf("PUT = %d %s", code, body)
	}
	if st.br.rec == nil || string(st.br.rec.Logo) != string(png) || st.br.rec.LogoType != "image/png" || !st.br.rec.LogoFromFile {
		t.Fatalf("logo not applied and marked: %+v", st.br.rec)
	}
	w := do(t, srv, http.MethodGet, "/api/v1/branding/settings", adminToken, "")
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got["logo_from_file"] != true {
		t.Fatalf("GET /branding/settings = %s, want logo_from_file true", w.Body.String())
	}
	if anon := do(t, srv, http.MethodGet, "/api/v1/branding", "", ""); strings.Contains(anon.Body.String(), "logo_from_file") {
		t.Errorf("the anonymous read carries the file-delivered flag: %s", anon.Body.String())
	}
	var datum map[string]any
	for _, ev := range audit.snapshot() {
		if ev.Action == "site_config.write" {
			_ = json.Unmarshal(ev.Data, &datum)
		}
	}
	if datum["branding_logo_path"] != path || datum["branding_logo_sha256"] == nil {
		t.Errorf("site_config.write datum = %v, want the path and the file's digest", datum)
	}
}

// An SVG goes through the same rebuild as an upload, and a Kubernetes-style
// symlink chain inside the directory is followed.
func TestSiteConfigBrandingLogoSVGAndMountLinks(t *testing.T) {
	srv, st, _ := siteBrandingServer(t, true)
	dir := t.TempDir()
	ts := filepath.Join(dir, "..2026_09_30")
	if err := os.Mkdir(ts, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(ts, "logo.svg"), []byte(
		`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 8 8"><rect width="8" height="8" fill="#7c3aed"/></svg>`))
	for link, target := range map[string]string{"..data": "..2026_09_30", "logo.svg": "..data/logo.svg"} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	if code, body := putSiteLogoPath(t, srv, filepath.Join(dir, "logo.svg")); code != http.StatusOK {
		t.Fatalf("PUT = %d %s", code, body)
	}
	if st.br.rec.LogoType != "image/svg+xml" || !strings.Contains(string(st.br.rec.Logo), "<svg") || !st.br.rec.LogoFromFile {
		t.Fatalf("svg not applied: %+v", st.br.rec)
	}
}

// A bad file refuses the whole apply: nothing reaches the store.
func TestSiteConfigBrandingLogoBadFileRefused(t *testing.T) {
	dir := t.TempDir()
	outside := writeFile(t, filepath.Join(t.TempDir(), "secret.png"), testPNG(t, 8))
	if err := os.Symlink(outside, filepath.Join(dir, "escape.png")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "dir.png"), 0o700); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, path, want string }{
		{"not a PNG", writeFile(t, filepath.Join(dir, "fake.png"), []byte("<svg/>")), "not a PNG Wardyn can use"},
		{"script in an SVG", writeFile(t, filepath.Join(dir, "evil.svg"), []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>x</script></svg>`)), "contains <script>"},
		{"over 512 KB", writeFile(t, filepath.Join(dir, "big.png"), make([]byte, brandLogoMax+1)), "over the 512 KB limit"},
		{"missing", filepath.Join(dir, "missing.png"), "cannot read"},
		{"a link out of its directory", filepath.Join(dir, "escape.png"), "leaves its own directory"},
		{"a directory", filepath.Join(dir, "dir.png"), "not a regular file"},
		{"relative", "logo.png", "absolute path"},
		{"dot-dot", dir + "/../x/logo.png", "absolute path"},
		{"a gif", filepath.Join(dir, "logo.gif"), "must name a .svg or .png file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, st, audit := siteBrandingServer(t, true)
			code, body := putSiteLogoPath(t, srv, tc.path)
			if code != http.StatusBadRequest {
				t.Fatalf("PUT = %d %s, want 400", code, body)
			}
			if reason, msg := errReason(t, []byte(body)); reason != reasonSiteConfigInvalid || !strings.Contains(msg, tc.want) {
				t.Errorf("reason %q msg %q, want %q carrying %q", reason, msg, reasonSiteConfigInvalid, tc.want)
			}
			if st.fakeSiteConfigStore.putSeen != nil || len(st.br.rec.Logo) != 0 {
				t.Error("a refused apply wrote something")
			}
			for _, ev := range audit.snapshot() {
				if ev.Action == "site_config.write" {
					t.Error("a refused apply was audited as a write")
				}
			}
		})
	}
}

// With no branding record there is nothing to hold the logo: the apply
// succeeds and says so, and the file is attached once the record exists.
func TestSiteConfigBrandingLogoPendingWithoutRecord(t *testing.T) {
	srv, st, _ := siteBrandingServer(t, false)
	path := writeFile(t, filepath.Join(t.TempDir(), "logo.png"), testPNG(t, 8))
	code, body := putSiteLogoPath(t, srv, path)
	if code != http.StatusOK || !strings.Contains(body, `"branding_logo_pending":true`) {
		t.Fatalf("PUT = %d %s, want 200 with branding_logo_pending", code, body)
	}
	if st.br.rec != nil {
		t.Fatalf("a record appeared: %+v", st.br.rec)
	}
	if w := do(t, srv, http.MethodPut, "/api/v1/branding/settings", adminToken, brandingBody(t, nil)); w.Code != http.StatusOK {
		t.Fatalf("save branding = %d %s", w.Code, w.Body.String())
	}
	if code, body := putSiteLogoPath(t, srv, path); code != http.StatusOK || strings.Contains(body, "branding_logo_pending") {
		t.Fatalf("re-apply = %d %s", code, body)
	}
	if !st.br.rec.LogoFromFile || len(st.br.rec.Logo) == 0 {
		t.Fatalf("logo not attached at the next apply: %+v", st.br.rec)
	}
}

// remove_logo on a file-delivered logo is refused; every other save keeps it,
// and an upload replaces it (the file's next apply puts it back).
func TestBrandingRemoveLogoRefusedForFileLogo(t *testing.T) {
	srv, st, _ := siteBrandingServer(t, true)
	path := writeFile(t, filepath.Join(t.TempDir(), "logo.png"), testPNG(t, 8))
	if code, body := putSiteLogoPath(t, srv, path); code != http.StatusOK {
		t.Fatalf("PUT = %d %s", code, body)
	}
	w := do(t, srv, http.MethodPut, "/api/v1/branding/settings", adminToken,
		brandingBody(t, func(b map[string]any) { b["remove_logo"] = true }))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("remove_logo = %d %s, want 400", w.Code, w.Body.String())
	}
	if reason, msg := errReason(t, w.Body.Bytes()); reason != brandReasonLogoFromFile || !strings.Contains(msg, "branding.logo_path") {
		t.Errorf("reason %q msg %q", reason, msg)
	}
	if len(st.br.rec.Logo) == 0 || !st.br.rec.LogoFromFile {
		t.Fatalf("a refused removal changed the record: %+v", st.br.rec)
	}
	if w := do(t, srv, http.MethodPut, "/api/v1/branding/settings", adminToken, brandingBody(t, nil)); w.Code != http.StatusOK ||
		!st.br.rec.LogoFromFile {
		t.Fatalf("a plain save = %d, flag %v; want it kept", w.Code, st.br.rec.LogoFromFile)
	}
	if w := do(t, srv, http.MethodPut, "/api/v1/branding/settings", adminToken,
		brandingBody(t, func(b map[string]any) { b["logo"] = logoField("image/png", testPNG(t, 9)) })); w.Code != http.StatusOK ||
		st.br.rec.LogoFromFile {
		t.Fatalf("an upload = %d, flag %v; want it cleared", w.Code, st.br.rec.LogoFromFile)
	}
	// An uploaded logo is removable.
	if w := do(t, srv, http.MethodPut, "/api/v1/branding/settings", adminToken,
		brandingBody(t, func(b map[string]any) { b["remove_logo"] = true })); w.Code != http.StatusOK || len(st.br.rec.Logo) != 0 {
		t.Fatalf("remove an uploaded logo = %d", w.Code)
	}
}

// Taking logo_path out of the document removes the logo it delivered, never one
// a person uploaded; a body that does not name branding leaves everything.
func TestSiteConfigBrandingLogoPathRemovedAndCarriedForward(t *testing.T) {
	srv, st, _ := siteBrandingServer(t, true)
	path := writeFile(t, filepath.Join(t.TempDir(), "logo.png"), testPNG(t, 8))
	if code, body := putSiteLogoPath(t, srv, path); code != http.StatusOK {
		t.Fatalf("PUT = %d %s", code, body)
	}
	if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{"scm_hosts":["github.example.com"]}`); w.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", w.Code, w.Body.String())
	}
	if st.fakeSiteConfigStore.putSeen.Branding == nil || st.fakeSiteConfigStore.putSeen.Branding.LogoPath != path ||
		len(st.br.rec.Logo) == 0 || !st.br.rec.LogoFromFile {
		t.Fatalf("silence dropped the path or the logo: %+v / %+v", st.fakeSiteConfigStore.putSeen.Branding, st.br.rec)
	}
	if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{"branding":{}}`); w.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", w.Code, w.Body.String())
	}
	if st.fakeSiteConfigStore.putSeen.Branding != nil || len(st.br.rec.Logo) != 0 || st.br.rec.LogoFromFile {
		t.Fatalf("naming an empty block left the file's logo: %+v / %+v", st.fakeSiteConfigStore.putSeen.Branding, st.br.rec)
	}
	// An uploaded logo survives the same apply.
	st.br.rec.Logo, st.br.rec.LogoType = testPNG(t, 9), "image/png"
	if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{"branding":{}}`); w.Code != http.StatusOK || len(st.br.rec.Logo) == 0 {
		t.Fatalf("an uploaded logo was removed by an apply: %d", w.Code)
	}
}

// A store with no branding capability refuses a logo_path rather than
// pretending to have applied it.
func TestSiteConfigBrandingLogoNeedsTheBrandingStore(t *testing.T) {
	fake := &fakeSiteConfigStore{}
	srv, _ := newSiteConfigHarness(t, fake)
	path := writeFile(t, filepath.Join(t.TempDir(), "logo.png"), testPNG(t, 8))
	if code, body := putSiteLogoPath(t, srv, path); code != http.StatusNotImplemented || fake.putSeen != nil {
		t.Fatalf("PUT = %d %s, want 501 and no write", code, body)
	}
}

var _ store.BrandingStore = siteBrandStore{}
