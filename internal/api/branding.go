// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/hostrules"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Console branding (#1125, packet approved 2026-09-27). One org-wide record: a
// name and its format, a primary colour and its text colour, an optional dark
// pair, an optional logo and an optional https Support link. Only those are
// brandable — danger/warning/success/info and the admin-view cue stay Wardyn's
// own, so no brand can disguise a security cue. Nothing here touches the CSP:
// the logo is served from 'self', and the colours are applied by the console
// as validated #rrggbb values on two CSS custom properties.

const (
	brandLogoMax     = 512 * 1024
	brandPNGMaxSide  = 4096
	brandOrgNameMax  = 64
	brandSupportMax  = 2048
	brandingBodyMax  = 1 << 20 // a 512 KB logo is ~700 KB of base64
	brandLogoPathFmt = "/api/v1/branding/logo?v=%s"
)

// The named reasons a refused write carries (errorBody.Reason), one per rule,
// are declared in reasons_routes.go (brandReason* — #656 review round: the
// docs guard only reads the two reasons files).

// mountBrandingRoutes: the two reads the sign-in page needs are anonymous (the
// reader has not signed in), the Support link is for signed-in people, and
// only a SUPER admin writes — the same tier as the site-config PUT.
func (s *Server) mountBrandingRoutes(r chi.Router) {
	r.Get("/branding", s.handleGetBranding)
	r.Get("/branding/logo", s.handleGetBrandingLogo)
	r.Group(func(r chi.Router) {
		r.Use(s.humanOrAdminAuth)
		r.Get("/branding/settings", s.handleGetBrandingSettings)
		operatorOnly := r.With(s.requireOperator)
		operatorOnly.Put("/branding/settings", s.handlePutBranding)
		operatorOnly.Delete("/branding/settings", s.handleDeleteBranding)
	})
}

// brandingPublic is everything the anonymous read may say: what the sign-in
// page draws. The dark pair is the one IN EFFECT (derived when none is set).
type brandingPublic struct {
	OrgName         string `json:"org_name,omitempty"`
	NameFormat      string `json:"name_format,omitempty"`
	Primary         string `json:"primary,omitempty"`
	PrimaryText     string `json:"primary_text,omitempty"`
	DarkPrimary     string `json:"dark_primary,omitempty"`
	DarkPrimaryText string `json:"dark_primary_text,omitempty"`
	LogoURL         string `json:"logo_url,omitempty"`
	// IconURL is the browser tab icon: the logo, or a monogram tile in the
	// primary colour when there is none. Present on every branded answer.
	IconURL string `json:"icon_url,omitempty"`
}

// brandingSettings adds what only a signed-in reader gets: the Support link,
// whether the dark pair was set rather than derived (the card's checkbox), and
// whether the logo is the site config's file (#1215, read-only — the card offers
// no Remove logo for one).
type brandingSettings struct {
	brandingPublic
	SupportURL   string `json:"support_url,omitempty"`
	DarkCustom   bool   `json:"dark_custom,omitempty"`
	LogoFromFile bool   `json:"logo_from_file,omitempty"`
}

func (s *Server) brandingSettingsView(b types.Branding) brandingSettings {
	return brandingSettings{
		brandingPublic: s.publicBranding(b), SupportURL: b.SupportURL, DarkCustom: b.DarkPrimary != "",
		LogoFromFile: b.LogoFromFile && len(b.Logo) > 0,
	}
}

func (s *Server) publicBranding(b types.Branding) brandingPublic {
	v := brandingPublic{
		OrgName: b.OrgName, NameFormat: b.NameFormat, Primary: b.Primary, PrimaryText: b.PrimaryText,
		DarkPrimary: b.DarkPrimary, DarkPrimaryText: b.DarkPrimaryText,
	}
	if v.DarkPrimary == "" {
		if c, _, ok := parseHexColour(b.Primary); ok {
			v.DarkPrimary, v.DarkPrimaryText = deriveDarkPrimary(c)
		}
	}
	icon := brandIcon(b)
	sum := sha256.Sum256(icon)
	v.IconURL = s.cfg.BasePath + fmt.Sprintf(brandLogoPathFmt, hex.EncodeToString(sum[:6]))
	if len(b.Logo) > 0 {
		v.LogoURL = v.IconURL
	}
	return v
}

// brandIcon is what GET /branding/logo serves for a branded console: the
// uploaded logo, else a monogram tile. The tile is generated here, from the
// validated colours and the escaped initials, so a brand with no logo still
// gets a tab icon from 'self' (a data: icon would need the CSP widened).
func brandIcon(b types.Branding) []byte {
	if len(b.Logo) > 0 {
		return b.Logo
	}
	var initials []rune
	for _, word := range strings.Fields(b.OrgName) {
		if len(initials) == 2 {
			break
		}
		r, _ := utf8.DecodeRuneInString(word)
		initials = append(initials, unicode.ToUpper(r))
	}
	var text bytes.Buffer
	_ = xml.EscapeText(&text, []byte(string(initials)))
	return []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><rect width="32" height="32" rx="7" fill="` +
		b.Primary + `"></rect><text x="16" y="21" font-size="13" font-weight="700" text-anchor="middle" font-family="sans-serif" fill="` +
		b.PrimaryText + `">` + text.String() + `</text></svg>`)
}

// loadBranding reads the record. ok=false means the answer is already written;
// found=false is the unbranded console, which a store without the capability
// also is.
func (s *Server) loadBranding(w http.ResponseWriter, r *http.Request) (b types.Branding, found, ok bool) {
	bs, has := s.cfg.Store.(store.BrandingStore)
	if !has {
		return types.Branding{}, false, true
	}
	b, err := bs.GetBranding(r.Context())
	if errors.Is(err, store.ErrNotFound) {
		return types.Branding{}, false, true
	}
	if err != nil {
		writeServerError(w, r, "get branding", err)
		return types.Branding{}, false, false
	}
	return b, true, true
}

// handleGetBranding is the ANONYMOUS read (sign-in page): the public subset
// only, and `{}` for an unbranded console.
func (s *Server) handleGetBranding(w http.ResponseWriter, r *http.Request) {
	b, found, ok := s.loadBranding(w, r)
	if !ok {
		return
	}
	if !found {
		writeJSON(w, http.StatusOK, brandingPublic{})
		return
	}
	writeJSON(w, http.StatusOK, s.publicBranding(b))
}

// handleGetBrandingSettings is the signed-in read (any tier): the public
// subset plus the Support link the header shows everyone.
func (s *Server) handleGetBrandingSettings(w http.ResponseWriter, r *http.Request) {
	b, found, ok := s.loadBranding(w, r)
	if !ok {
		return
	}
	if !found {
		writeJSON(w, http.StatusOK, brandingSettings{})
		return
	}
	writeJSON(w, http.StatusOK, s.brandingSettingsView(b))
}

// handleGetBrandingLogo serves the stored logo (or the monogram tile, see
// brandIcon), ANONYMOUS like the read that names it. The type is the one validated at write time, never sniffed, and
// nosniff is set here as well as by securityHeaders so the pairing cannot
// come apart.
func (s *Server) handleGetBrandingLogo(w http.ResponseWriter, r *http.Request) {
	b, found, ok := s.loadBranding(w, r)
	if !ok {
		return
	}
	if !found {
		writeErrorReason(w, http.StatusNotFound, reasonBrandingNotBranded, "this console is not branded")
		return
	}
	contentType := b.LogoType
	if len(b.Logo) == 0 {
		contentType = "image/svg+xml"
	}
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(brandIcon(b))
}

// brandingRequest is the PUT body. Logo nil keeps the stored logo;
// remove_logo drops it. Data is base64 on the wire (encoding/json's []byte).
type brandingRequest struct {
	OrgName         string        `json:"org_name"`
	NameFormat      string        `json:"name_format"`
	Primary         string        `json:"primary"`
	PrimaryText     string        `json:"primary_text"`
	DarkPrimary     string        `json:"dark_primary,omitempty"`
	DarkPrimaryText string        `json:"dark_primary_text,omitempty"`
	SupportURL      string        `json:"support_url,omitempty"`
	Logo            *brandingLogo `json:"logo,omitempty"`
	RemoveLogo      bool          `json:"remove_logo,omitempty"`
}

type brandingLogo struct {
	ContentType string `json:"content_type"`
	Data        []byte `json:"data"`
}

// brandingRefusal is a validation failure: its named reason and the sentence.
type brandingRefusal struct{ reason, msg string }

func (e *brandingRefusal) Error() string { return e.msg }

func refuse(reason, format string, a ...any) *brandingRefusal {
	return &brandingRefusal{reason: reason, msg: fmt.Sprintf(format, a...)}
}

// handlePutBranding validates and replaces the record (SUPER admin), audited
// as branding.write.
func (s *Server) handlePutBranding(w http.ResponseWriter, r *http.Request) {
	bs, has := s.cfg.Store.(store.BrandingStore)
	if !has {
		writeErrorReason(w, http.StatusNotImplemented, reasonBrandingStoreUnavailable, "branding requires the Postgres store backend")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, brandingBodyMax))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeErrorReason(w, http.StatusRequestEntityTooLarge, brandReasonLogoSize,
				"logo: Upload an image under 512 KB (SVG or PNG).")
			return
		}
		writeErrorReason(w, http.StatusBadRequest, reasonBrandingBodyUnreadable, "read request body: "+err.Error())
		return
	}
	var req brandingRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, "invalid JSON body: "+err.Error())
		return
	}
	b, refusal := validateBranding(req)
	if refusal != nil {
		writeErrorReason(w, http.StatusBadRequest, refusal.reason, refusal.msg)
		return
	}
	// A logo the site config delivers would be put back by its next apply, so a
	// removal is refused rather than quietly undone (#1215). The console never
	// offers it; this is the API's answer to a caller that tries.
	if req.RemoveLogo {
		cur, err := bs.GetBranding(r.Context())
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			writeServerError(w, r, "get branding", err)
			return
		}
		if err == nil && cur.LogoFromFile && len(cur.Logo) > 0 {
			writeErrorReason(w, http.StatusBadRequest, brandReasonLogoFromFile,
				"remove_logo: this logo comes from the site configuration's branding.logo_path; take that key out of the site configuration instead")
			return
		}
	}
	b.UpdatedBy = principalFromRequest(r)
	saved, err := bs.PutBranding(r.Context(), b, req.Logo == nil && !req.RemoveLogo)
	if err != nil {
		writeServerError(w, r, "put branding", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"branding.write", "branding", "success", mustJSON(brandingAuditData(saved))))
	writeJSON(w, http.StatusOK, s.brandingSettingsView(saved))
}

// brandingAuditData is the whole record in the clear — every field is shown to
// anyone who opens the console — with the logo as its digest and size.
func brandingAuditData(b types.Branding) map[string]any {
	logo := ""
	if len(b.Logo) > 0 {
		sum := sha256.Sum256(b.Logo)
		logo = hex.EncodeToString(sum[:])
	}
	return map[string]any{
		"org_name": b.OrgName, "name_format": b.NameFormat, "primary": b.Primary,
		"primary_text": b.PrimaryText, "dark_primary": b.DarkPrimary, "dark_primary_text": b.DarkPrimaryText,
		"support_url": b.SupportURL, "logo_sha256": logo, "logo_type": b.LogoType, "logo_bytes": len(b.Logo),
	}
}

// handleDeleteBranding returns the console to unbranded (SUPER admin), audited
// as branding.delete when there was a record to remove.
func (s *Server) handleDeleteBranding(w http.ResponseWriter, r *http.Request) {
	bs, has := s.cfg.Store.(store.BrandingStore)
	if !has {
		writeErrorReason(w, http.StatusNotImplemented, reasonBrandingStoreUnavailable, "branding requires the Postgres store backend")
		return
	}
	removed, err := bs.DeleteBranding(r.Context())
	if err != nil {
		writeServerError(w, r, "delete branding", err)
		return
	}
	if removed {
		s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
			"branding.delete", "branding", "success", nil))
	}
	w.WriteHeader(http.StatusNoContent)
}

// validateBranding checks a PUT body and returns the record to store, with
// every colour in canonical #rrggbb form.
func validateBranding(req brandingRequest) (types.Branding, *brandingRefusal) {
	b := types.Branding{
		OrgName: strings.TrimSpace(req.OrgName), NameFormat: req.NameFormat,
		SupportURL: strings.TrimSpace(req.SupportURL),
	}
	if b.OrgName == "" || utf8.RuneCountInString(b.OrgName) > brandOrgNameMax ||
		strings.ContainsFunc(b.OrgName, unsafeHelpRune) {
		return b, refuse(brandReasonOrgName, "org_name: enter the organisation's name — up to %d characters, on one line.", brandOrgNameMax)
	}
	if b.NameFormat != types.BrandNamePrefix && b.NameFormat != types.BrandNameSuffix {
		return b, refuse(brandReasonNameFormat, `name_format: must be "prefix" (<Company> Wardyn) or "suffix" (Wardyn for <Company>).`)
	}
	var refusal *brandingRefusal
	if b.Primary, b.PrimaryText, refusal = validateBrandPair("primary", "primary_text", req.Primary, req.PrimaryText); refusal != nil {
		return b, refusal
	}
	if req.DarkPrimary != "" || req.DarkPrimaryText != "" {
		if b.DarkPrimary, b.DarkPrimaryText, refusal = validateBrandPair("dark_primary", "dark_primary_text",
			req.DarkPrimary, req.DarkPrimaryText); refusal != nil {
			return b, refusal
		}
	}
	if refusal := validateSupportURL(b.SupportURL); refusal != nil {
		return b, refusal
	}
	if req.Logo != nil {
		logo, refusal := validateLogo(req.Logo.ContentType, req.Logo.Data)
		if refusal != nil {
			return b, refusal
		}
		b.Logo, b.LogoType = logo, req.Logo.ContentType
	}
	return b, nil
}

// validateBrandPair checks a primary colour and the text drawn on it: both
// hex colours, and at least 4.5:1 apart.
func validateBrandPair(fillField, textField, fill, text string) (string, string, *brandingRefusal) {
	fc, fillHex, ok := parseHexColour(fill)
	if !ok {
		return "", "", refuse(brandReasonColour, "%s: Enter a valid hex colour, like #0f766e.", fillField)
	}
	tc, textHex, ok := parseHexColour(text)
	if !ok {
		return "", "", refuse(brandReasonColour, "%s: Enter a valid hex colour, like #0f766e.", textField)
	}
	if ratio := contrastRatio(fc, tc); ratio < minBrandContrast {
		return "", "", refuse(brandReasonContrast, "%s: This text colour has a contrast ratio of %s:1 against the "+
			"button background. Wardyn requires at least 4.5:1 for body text (WCAG AA).", textField, ratioText(ratio))
	}
	return fillHex, textHex, nil
}

// validateSupportURL: optional, https only, a real host, no sign-in details.
func validateSupportURL(link string) *brandingRefusal {
	if link == "" {
		return nil
	}
	u, err := url.Parse(link)
	if err != nil || !strings.EqualFold(u.Scheme, "https") {
		return refuse(brandReasonLink, "support_url: This link must use https. http:// links, and links with no scheme, aren't allowed.")
	}
	if len(link) > brandSupportMax || u.User != nil ||
		strings.ContainsFunc(link, func(r rune) bool { return unsafeHelpRune(r) || unicode.IsSpace(r) }) ||
		!hostrules.ValidApprovedHost(strings.ToLower(u.Hostname())) {
		return refuse(brandReasonLinkShape, "support_url: must be a plain web address with a real host name — no spaces or sign-in details.")
	}
	return nil
}

// validateLogo checks an uploaded logo and returns the bytes to store: a PNG
// exactly as sent, or an SVG rebuilt by sanitizeSVG.
func validateLogo(contentType string, data []byte) ([]byte, *brandingRefusal) {
	if len(data) > brandLogoMax {
		return nil, refuse(brandReasonLogoSize, "logo: This logo is %s. Upload an image under 512 KB (SVG or PNG).", logoSizeText(len(data)))
	}
	switch contentType {
	case "image/png":
		cfg, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > brandPNGMaxSide || cfg.Height > brandPNGMaxSide {
			return nil, refuse(brandReasonLogo, "logo: this is not a PNG Wardyn can use (at most %d pixels on a side).", brandPNGMaxSide)
		}
		return data, nil
	case "image/svg+xml":
		clean, err := sanitizeSVG(data)
		if err != nil {
			return nil, refuse(brandReasonLogo, "%s", err.Error())
		}
		return clean, nil
	}
	return nil, refuse(brandReasonLogo, "logo: must be an SVG or PNG image.")
}

// logoSizeText matches the Branding card's rendering of a file size.
func logoSizeText(n int) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1f MB", float64(n)/1_000_000)
	}
	return fmt.Sprintf("%d KB", (n+1023)/1024) // rounded UP: 512 KB + 1 byte is not "512 KB"
}
