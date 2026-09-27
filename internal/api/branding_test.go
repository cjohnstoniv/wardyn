// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// brandingMemStore is rbacStore plus an in-memory store.BrandingStore.
type brandingMemStore struct {
	rbacStore
	mu  sync.Mutex
	rec *types.Branding
}

func (m *brandingMemStore) GetBranding(context.Context) (types.Branding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rec == nil {
		return types.Branding{}, store.ErrNotFound
	}
	return *m.rec, nil
}

func (m *brandingMemStore) PutBranding(_ context.Context, b types.Branding, keepLogo bool) (types.Branding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if keepLogo {
		b.Logo, b.LogoType = nil, ""
		if m.rec != nil {
			b.Logo, b.LogoType = m.rec.Logo, m.rec.LogoType
		}
	}
	m.rec = &b
	return b, nil
}

func (m *brandingMemStore) DeleteBranding(context.Context) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	had := m.rec != nil
	m.rec = nil
	return had, nil
}

func brandingServer(t *testing.T) (*Server, *harness, *brandingMemStore) {
	t.Helper()
	h := newHarness(t)
	st := &brandingMemStore{}
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Secrets = getErrStore{getErr: secretstore.ErrNotFound}
	cfg.Approvals = h.approvals
	return New(cfg), h, st
}

func brandingBody(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	body := map[string]any{
		"org_name": "Example Corp", "name_format": "prefix",
		"primary": "#7c3aed", "primary_text": "#ffffff",
		"support_url": "https://status.example.com",
	}
	if mutate != nil {
		mutate(body)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func logoField(contentType string, data []byte) map[string]any {
	return map[string]any{"content_type": contentType, "data": base64.StdEncoding.EncodeToString(data)}
}

func testPNG(t *testing.T, side int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, side, side))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func errReason(t *testing.T, body []byte) (reason, msg string) {
	t.Helper()
	var e errorBody
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("error body %q: %v", body, err)
	}
	return e.Reason, e.Error
}

// Every server-side rule refuses with its own named reason (#1125 done-when:
// "an invalid colour, a low-contrast pair, or a non-https link is refused with
// a named reason").
func TestBrandingPutRefusesWithNamedReason(t *testing.T) {
	big := make([]byte, brandLogoMax+1)
	cases := []struct {
		name       string
		mutate     func(map[string]any)
		status     int
		reason     string
		msgContain string
	}{
		{"invalid primary colour", func(b map[string]any) { b["primary"] = "#7Q3aeZ" }, 400, brandReasonColour, "primary: Enter a valid hex colour, like #0f766e."},
		{"invalid text colour", func(b map[string]any) { b["primary_text"] = "white" }, 400, brandReasonColour, "primary_text: Enter a valid hex colour"},
		{"low contrast names the ratio", func(b map[string]any) { b["primary"] = "#fef08a" }, 400, brandReasonContrast,
			"contrast ratio of 1.1:1 against the button background. Wardyn requires at least 4.5:1"},
		{"low contrast dark pair", func(b map[string]any) { b["dark_primary"] = "#a78bfa"; b["dark_primary_text"] = "#ffffff" }, 400, brandReasonContrast, "dark_primary_text: "},
		{"dark primary without its text", func(b map[string]any) { b["dark_primary"] = "#a78bfa" }, 400, brandReasonColour, "dark_primary_text: "},
		{"http link", func(b map[string]any) { b["support_url"] = "http://status.example.com" }, 400, brandReasonLink, "This link must use https."},
		{"link with no scheme", func(b map[string]any) { b["support_url"] = "status.example.com" }, 400, brandReasonLink, "This link must use https."},
		{"javascript link", func(b map[string]any) { b["support_url"] = "javascript:alert(1)" }, 400, brandReasonLink, ""},
		{"link with sign-in details", func(b map[string]any) { b["support_url"] = "https://u:p@status.example.com" }, 400, brandReasonLinkShape, ""},
		{"oversized logo names its size", func(b map[string]any) { b["logo"] = logoField("image/png", big) }, 400, brandReasonLogoSize,
			"This logo is 513 KB. Upload an image under 512 KB (SVG or PNG)."},
		{"body past the cap is the logo's size reason", func(b map[string]any) { b["logo"] = logoField("image/png", make([]byte, 900*1024)) }, 413, brandReasonLogoSize, ""},
		{"not a PNG", func(b map[string]any) { b["logo"] = logoField("image/png", []byte("<svg/>")) }, 400, brandReasonLogo, ""},
		{"not SVG or PNG", func(b map[string]any) { b["logo"] = logoField("image/gif", []byte("GIF89a")) }, 400, brandReasonLogo, ""},
		{"malicious SVG", func(b map[string]any) {
			b["logo"] = logoField("image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`))
		}, 400, brandReasonLogo, "contains <script>"},
		{"empty org name", func(b map[string]any) { b["org_name"] = "  " }, 400, brandReasonOrgName, ""},
		{"org name with a line break", func(b map[string]any) { b["org_name"] = "Example\nCorp" }, 400, brandReasonOrgName, ""},
		{"unknown name format", func(b map[string]any) { b["name_format"] = "both" }, 400, brandReasonNameFormat, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, h, st := brandingServer(t)
			w := do(t, srv, http.MethodPut, "/api/v1/branding/settings", adminToken, brandingBody(t, tc.mutate))
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tc.status, w.Body.String())
			}
			reason, msg := errReason(t, w.Body.Bytes())
			if reason != tc.reason {
				t.Errorf("reason = %q, want %q (msg %q)", reason, tc.reason, msg)
			}
			if !strings.Contains(msg, tc.msgContain) {
				t.Errorf("msg = %q, want it to contain %q", msg, tc.msgContain)
			}
			if st.rec != nil {
				t.Errorf("a refused write stored a record: %+v", st.rec)
			}
			for _, ev := range h.audit.snapshot() {
				if strings.HasPrefix(ev.Action, "branding.") {
					t.Errorf("a refused write audited %s", ev.Action)
				}
			}
		})
	}
}

// Only a SUPER admin writes; the security tier and a user are refused, and no
// credential at all is a 401.
func TestBrandingWriteIsSuperAdminOnly(t *testing.T) {
	srv, _, st := brandingServer(t)
	body := brandingBody(t, nil)
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		if w := doSSO(t, srv, method, "/api/v1/branding/settings", nil, body); w.Code != http.StatusUnauthorized {
			t.Errorf("%s anonymous: status = %d, want 401", method, w.Code)
		}
		for _, role := range []string{oidc.RoleUser, oidc.RoleSecurityAdmin} {
			c := ssoSession(t, "sub-"+role, role+"@corp.example", role)
			if w := doSSO(t, srv, method, "/api/v1/branding/settings", c, body); w.Code != http.StatusForbidden {
				t.Errorf("%s as %s: status = %d, want 403; body=%s", method, role, w.Code, w.Body.String())
			}
		}
	}
	if st.rec != nil {
		t.Fatalf("a refused write stored a record: %+v", st.rec)
	}
	if w := do(t, srv, http.MethodPut, "/api/v1/branding/settings", adminToken, body); w.Code != http.StatusOK {
		t.Fatalf("admin PUT: status = %d; body=%s", w.Code, w.Body.String())
	}
	c := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	if w := doSSO(t, srv, http.MethodPut, "/api/v1/branding/settings", c, body); w.Code != http.StatusOK {
		t.Fatalf("super admin session PUT: status = %d; body=%s", w.Code, w.Body.String())
	}
}

func jsonKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// The anonymous read is the sign-in page's: name, format, colours and logo
// URL, never the Support link, and `{}` when unbranded.
func TestBrandingAnonymousReadIsThePublicSubset(t *testing.T) {
	srv, _, _ := brandingServer(t)
	w := doSSO(t, srv, http.MethodGet, "/api/v1/branding", nil, "")
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "{}" {
		t.Fatalf("unbranded: %d %q, want 200 {}", w.Code, w.Body.String())
	}
	body := brandingBody(t, func(b map[string]any) { b["logo"] = logoField("image/png", testPNG(t, 8)) })
	if w := do(t, srv, http.MethodPut, "/api/v1/branding/settings", adminToken, body); w.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}
	w = doSSO(t, srv, http.MethodGet, "/api/v1/branding", nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("anonymous GET: %d %s", w.Code, w.Body.String())
	}
	want := []string{"dark_primary", "dark_primary_text", "icon_url", "logo_url", "name_format", "org_name", "primary", "primary_text"}
	if got := jsonKeys(t, w.Body.Bytes()); !slices.Equal(got, want) {
		t.Fatalf("anonymous keys = %v, want exactly %v", got, want)
	}
	var pub brandingPublic
	_ = json.Unmarshal(w.Body.Bytes(), &pub)
	if pub.DarkPrimary == "" || pub.DarkPrimaryText != darkBackground || !strings.HasPrefix(pub.LogoURL, "/api/v1/branding/logo?v=") {
		t.Errorf("derived dark pair / logo url missing: %+v", pub)
	}

	if w := doSSO(t, srv, http.MethodGet, "/api/v1/branding/settings", nil, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous settings read: %d, want 401", w.Code)
	}
	c := ssoSession(t, "sub-user", "user@corp.example", oidc.RoleUser)
	w = doSSO(t, srv, http.MethodGet, "/api/v1/branding/settings", c, "")
	var set brandingSettings
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &set) != nil || set.SupportURL != "https://status.example.com" {
		t.Fatalf("signed-in settings read: %d %s", w.Code, w.Body.String())
	}
}

// The logo goes out with the type validated at write time and nosniff; an SVG
// goes out rebuilt, not as uploaded.
func TestBrandingLogoServedWithStrictTypeAndNosniff(t *testing.T) {
	pngBytes := testPNG(t, 16)
	svgIn := `<?xml version="1.0"?><!-- editor --><svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" ` +
		`xmlns:inkscape="http://www.inkscape.org/namespaces/inkscape" viewBox="0 0 24 24" inkscape:version="1"><metadata>x</metadata>` +
		`<defs><linearGradient id="g"><stop offset="0" stop-color="#fff"/></linearGradient></defs>` +
		`<rect width="24" height="24" rx="5" fill="url(#g)" style="stroke: #000; stroke-width: 1"/><use xlink:href="#g"/>` +
		`<title>Example &amp; Co</title></svg>`
	svgWant := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><defs><linearGradient id="g"><stop offset="0" stop-color="#fff"></stop>` +
		`</linearGradient></defs><rect width="24" height="24" rx="5" fill="url(#g)" style="stroke: #000; stroke-width: 1"></rect>` +
		`<use href="#g"></use><title>Example &amp; Co</title></svg>`
	for _, tc := range []struct {
		ct, want string
		in       []byte
	}{
		{"image/png", string(pngBytes), pngBytes},
		{"image/svg+xml", svgWant, []byte(svgIn)},
	} {
		t.Run(tc.ct, func(t *testing.T) {
			srv, _, _ := brandingServer(t)
			if w := doSSO(t, srv, http.MethodGet, "/api/v1/branding/logo", nil, ""); w.Code != http.StatusNotFound {
				t.Fatalf("unbranded: %d, want 404", w.Code)
			}
			body := brandingBody(t, func(b map[string]any) { b["logo"] = logoField(tc.ct, tc.in) })
			if w := do(t, srv, http.MethodPut, "/api/v1/branding/settings", adminToken, body); w.Code != http.StatusOK {
				t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
			}
			w := doSSO(t, srv, http.MethodGet, "/api/v1/branding/logo?v=anything", nil, "")
			if w.Code != http.StatusOK {
				t.Fatalf("GET logo: %d", w.Code)
			}
			if got := w.Header().Get("Content-Type"); got != tc.ct {
				t.Errorf("Content-Type = %q, want %q", got, tc.ct)
			}
			if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
			if got := w.Body.String(); got != tc.want {
				t.Errorf("body =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

// A brand with no logo still gets a tab icon from 'self': a monogram tile in its
// own validated colours, the initials escaped.
func TestBrandingIconIsAMonogramWithoutALogo(t *testing.T) {
	srv, _, _ := brandingServer(t)
	body := brandingBody(t, func(b map[string]any) { b["org_name"] = "<b> & Co" })
	if w := do(t, srv, http.MethodPut, "/api/v1/branding/settings", adminToken, body); w.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}
	var pub brandingPublic
	w := doSSO(t, srv, http.MethodGet, "/api/v1/branding", nil, "")
	if json.Unmarshal(w.Body.Bytes(), &pub) != nil || pub.LogoURL != "" || pub.IconURL == "" {
		t.Fatalf("public read without a logo: %s", w.Body.String())
	}
	w = doSSO(t, srv, http.MethodGet, "/api/v1/branding/logo", nil, "")
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/svg+xml" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("monogram: %d %v", w.Code, w.Header())
	}
	want := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><rect width="32" height="32" rx="7" fill="#7c3aed"></rect>` +
		`<text x="16" y="21" font-size="13" font-weight="700" text-anchor="middle" font-family="sans-serif" fill="#ffffff">&lt;&amp;</text></svg>`
	if got := w.Body.String(); got != want {
		t.Fatalf("monogram =\n%s\nwant\n%s", got, want)
	}
	if _, err := sanitizeSVG(w.Body.Bytes()); err != nil {
		t.Fatalf("the generated monogram fails Wardyn's own SVG rules: %v", err)
	}
}

// Each fixture is a way an SVG can run script or fetch; every one is refused.
func TestSanitizeSVGRefusesActiveContent(t *testing.T) {
	const open = `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink">`
	fixtures := map[string]string{
		"script element":        open + `<script>alert(1)</script></svg>`,
		"prefixed script":       `<svg:svg xmlns:svg="http://www.w3.org/2000/svg"><svg:script>alert(1)</svg:script></svg:svg>`,
		"xhtml script":          open + `<g><h:script xmlns:h="http://www.w3.org/1999/xhtml">alert(1)</h:script></g></svg>`,
		"foreignObject":         open + `<foreignObject><div xmlns="http://www.w3.org/1999/xhtml">x</div></foreignObject></svg>`,
		"onload on root":        `<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"></svg>`,
		"onclick uppercase":     open + `<rect ONCLICK="alert(1)"/></svg>`,
		"external image":        open + `<image href="https://example.com/x.png"/></svg>`,
		"anchor":                open + `<a href="javascript:alert(1)"><rect/></a></svg>`,
		"style element":         open + `<style>@import url(https://example.com/x.css);</style></svg>`,
		"external use":          open + `<use xlink:href="https://example.com/s.svg#x"/></svg>`,
		"javascript href":       open + `<use href="javascript:alert(1)"/></svg>`,
		"external url fill":     open + `<rect fill="url(https://example.com/p.svg#p)"/></svg>`,
		"external url in style": open + `<rect style="fill:url(https://example.com/p.svg#p)"/></svg>`,
		"escaped url":           open + `<rect fill="u\72l(https://example.com/p)"/></svg>`,
		"css src function":      open + `<rect fill="src(x)"/></svg>`,
		"style with import":     open + `<rect style="@import 'x'"/></svg>`,
		"animate":               open + `<rect><animate attributeName="href" to="javascript:alert(1)"/></rect></svg>`,
		"set":                   open + `<rect><set attributeName="onmouseover" to="alert(1)"/></rect></svg>`,
		"doctype entity":        `<!DOCTYPE svg [<!ENTITY x SYSTEM "file:///etc/passwd">]>` + open + `<text>&x;</text></svg>`,
		"stylesheet PI":         `<?xml-stylesheet href="https://example.com/x.css"?>` + open + `</svg>`,
		"not svg root":          `<html xmlns="http://www.w3.org/1999/xhtml"><svg xmlns="http://www.w3.org/2000/svg"/></html>`,
		"svg without namespace": `<svg><rect/></svg>`,
		"two roots":             open + `</svg><svg xmlns="http://www.w3.org/2000/svg"></svg>`,
		"not XML":               `<svg xmlns="http://www.w3.org/2000/svg"><rect></svg>`,
		"script in dropped":     open + `<metadata><script>alert(1)</script></metadata></svg>`,
	}
	for name, in := range fixtures {
		t.Run(name, func(t *testing.T) {
			if out, err := sanitizeSVG([]byte(in)); err == nil {
				t.Fatalf("accepted; emitted %s", out)
			}
		})
	}
}

// The dark pair is derived when unset and always passes; the vectors are the
// same ones ui/src/app/lib/branding.test.ts pins, so the two walks cannot
// drift.
func TestDeriveDarkPrimaryVectors(t *testing.T) {
	bg, _, _ := parseHexColour(darkBackground)
	for in, want := range brandDarkVectors {
		c, _, ok := parseHexColour(in)
		if !ok {
			t.Fatalf("bad vector %q", in)
		}
		got, text := deriveDarkPrimary(c)
		if got != want || text != darkBackground {
			t.Errorf("deriveDarkPrimary(%s) = %s on %s, want %s", in, got, text, want)
		}
		d, _, _ := parseHexColour(got)
		if r := contrastRatio(d, bg); r < minBrandContrast {
			t.Errorf("%s -> %s is only %.2f:1", in, got, r)
		}
	}
	if got := ratioText(4.46); got != "4.4" {
		t.Errorf("ratioText(4.46) = %s, want 4.4 (never rounds a failure up to 4.5)", got)
	}
}

var brandDarkVectors = map[string]string{
	"#7c3aed": "#9058f0",
	"#0f766e": "#338b84",
	"#000000": "#808080",
	"#fef08a": "#fef08a",
	"#b91c1c": "#cb5555",
}

// A change is audited: branding.write with the record in the clear, and
// branding.delete when the record is removed.
func TestBrandingWritesAreAudited(t *testing.T) {
	srv, h, st := brandingServer(t)
	body := brandingBody(t, func(b map[string]any) { b["logo"] = logoField("image/png", testPNG(t, 4)) })
	if w := do(t, srv, http.MethodPut, "/api/v1/branding/settings", adminToken, body); w.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}
	// A save without a logo field keeps the stored logo.
	if w := do(t, srv, http.MethodPut, "/api/v1/branding/settings", adminToken, brandingBody(t, nil)); w.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}
	if len(st.rec.Logo) == 0 {
		t.Fatal("a save with no logo field dropped the stored logo")
	}
	if w := do(t, srv, http.MethodDelete, "/api/v1/branding/settings", adminToken, ""); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE: %d %s", w.Code, w.Body.String())
	}
	var actions []string
	var first map[string]any
	for _, ev := range h.audit.snapshot() {
		if strings.HasPrefix(ev.Action, "branding.") {
			actions = append(actions, ev.Action)
			if first == nil {
				_ = json.Unmarshal(ev.Data, &first)
			}
		}
	}
	if !slices.Equal(actions, []string{"branding.write", "branding.write", "branding.delete"}) {
		t.Fatalf("audit actions = %v", actions)
	}
	if first["org_name"] != "Example Corp" || first["support_url"] != "https://status.example.com" || first["logo_type"] != "image/png" {
		t.Errorf("branding.write data = %v", first)
	}
}
