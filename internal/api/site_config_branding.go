// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Console branding through the site config (#1215): `branding.logo_path` names
// a file on the wardynd host. The daemon reads it when the document is applied,
// checks it exactly as a console upload is checked (validateLogo: the same
// size cap, the PNG decode, the SVG rebuild), and stores it as the branding
// logo, marked file-delivered (types.Branding.LogoFromFile).
//
// A bad file REFUSES the apply (400, like every other invalid block). What is
// only REPORTED, never refused, is a logo with no branding record to attach it
// to yet: the name and colours live on the Branding card, and an MDM file is
// re-applied on every boot of a laptop whose admin may not have saved them, so
// refusing there would block the rest of the document (proxy, providers) until
// someone did. The response says branding_logo_pending; the next apply after
// the card is saved attaches the file.

// siteBrandingLogoPathMax bounds the stored path (PATH_MAX on Linux).
const siteBrandingLogoPathMax = 4096

// normalizeSiteBranding turns an empty block (`{}`, or a blank path) into an
// absent one, so what is stored is what validateSiteBranding saw.
func normalizeSiteBranding(b *types.SiteBranding) *types.SiteBranding {
	if b == nil {
		return nil
	}
	if b.LogoPath = strings.TrimSpace(b.LogoPath); b.LogoPath == "" {
		return nil
	}
	return b
}

// validateSiteBranding is the shape rule for branding.logo_path: an absolute,
// clean path to a .svg or .png. The file itself is read by loadSiteBrandingLogo.
func validateSiteBranding(b *types.SiteBranding) error {
	if b == nil {
		return nil
	}
	p := b.LogoPath
	switch {
	case len(p) > siteBrandingLogoPathMax || strings.ContainsFunc(p, unicode.IsControl):
		return errors.New("branding.logo_path: not a usable path")
	case !filepath.IsAbs(p) || filepath.Clean(p) != p:
		return fmt.Errorf("branding.logo_path: must be an absolute path on the wardynd host with no '..', '//' or trailing '/', not %q", p)
	case logoTypeOfPath(p) == "":
		return fmt.Errorf("branding.logo_path: must name a .svg or .png file, not %q", p)
	}
	return nil
}

// logoTypeOfPath is the content type the file's extension claims; validateLogo
// then checks the bytes really are that type.
func logoTypeOfPath(p string) string {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	}
	return ""
}

// siteBrandingLogo is a logo file read and validated, ready to store.
type siteBrandingLogo struct {
	data        []byte
	contentType string
}

// loadSiteBrandingLogo reads the file branding.logo_path names. The path is
// resolved, and what it resolves to must still lie inside the directory it was
// written in (itself resolved): a Kubernetes ConfigMap or Secret mount is a
// symlink chain inside its own directory and passes; a link that leaves the
// directory (to /etc/shadow, say) is refused. Only a regular file is opened, so
// a FIFO or a device can never stall the apply (checked on the opened descriptor).
func loadSiteBrandingLogo(path string) (*siteBrandingLogo, error) {
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("branding.logo_path: cannot read %q: %w", path, err)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("branding.logo_path: cannot read %q: %w", path, err)
	}
	if rel, rerr := filepath.Rel(dir, real); rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("branding.logo_path: %q is a link that leaves its own directory", path)
	}
	// Opened first and judged on the descriptor, never on the path: a path
	// swapped for a FIFO between a stat and the open would otherwise block the
	// open forever, and O_NONBLOCK makes that open return at once. O_NOFOLLOW
	// refuses a last component that became a link after it was resolved above.
	f, err := os.OpenFile(real, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("branding.logo_path: cannot read %q: %w", path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("branding.logo_path: cannot read %q: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("branding.logo_path: %q is not a regular file", path)
	}
	if fi.Size() > brandLogoMax {
		return nil, fmt.Errorf("branding.logo_path: %q is %s, over the 512 KB limit", path, logoSizeText(int(fi.Size())))
	}
	raw, err := io.ReadAll(io.LimitReader(f, brandLogoMax+1))
	if err != nil {
		return nil, fmt.Errorf("branding.logo_path: cannot read %q: %w", path, err)
	}
	ctype := logoTypeOfPath(path)
	data, refusal := validateLogo(ctype, raw)
	if refusal != nil {
		return nil, fmt.Errorf("branding.logo_path: %q is refused: %s", path, refusal.msg)
	}
	return &siteBrandingLogo{data: data, contentType: ctype}, nil
}

// applySiteBrandingLogo makes the stored logo follow the document that was just
// saved, when the body named `branding`. logo is what loadSiteBrandingLogo read
// (nil when the document names no logo_path). It returns pending=true when a
// logo is named but no branding record exists to hold it.
//
//   - logo_path named: the file's bytes become the logo, marked file-delivered.
//     Unchanged bytes are left alone, so the every-boot re-apply writes nothing.
//   - logo_path absent: a logo this mechanism delivered is removed, which is
//     what the card's note promises ("take branding.logo_path out of that
//     file"). A logo a person uploaded is never touched.
func applySiteBrandingLogo(ctx context.Context, bs store.BrandingStore, logo *siteBrandingLogo, by string) (pending bool, err error) {
	cur, err := bs.GetBranding(ctx)
	found := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return false, fmt.Errorf("get branding: %w", err)
	}
	switch {
	case logo == nil && found && cur.LogoFromFile:
		_, err = bs.SetBrandingLogo(ctx, nil, "", false, by)
	case logo != nil && !found:
		return true, nil
	case logo != nil && !(cur.LogoFromFile && cur.LogoType == logo.contentType && bytes.Equal(cur.Logo, logo.data)):
		_, err = bs.SetBrandingLogo(ctx, logo.data, logo.contentType, true, by)
	default:
		err = nil
	}
	return false, err
}

// readSiteBrandingLogo is handlePutSiteConfig's first branding step: when the
// body named a logo_path, read and check the file, answering the refusal itself
// (ok=false). The branding store is returned for followSiteBranding.
func (s *Server) readSiteBrandingLogo(w http.ResponseWriter, named bool, b *types.SiteBranding) (*siteBrandingLogo, store.BrandingStore, bool) {
	bs, has := s.cfg.Store.(store.BrandingStore)
	if !named || b == nil {
		return nil, bs, true
	}
	if !has {
		writeErrorReason(w, http.StatusNotImplemented, reasonBrandingStoreUnavailable, "branding requires the Postgres store backend")
		return nil, nil, false
	}
	logo, err := loadSiteBrandingLogo(b.LogoPath)
	if err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonSiteConfigInvalid, "invalid site config: "+err.Error())
		return nil, nil, false
	}
	return logo, bs, true
}

// followSiteBranding is the second step, after the document is stored: make the
// logo follow it (applySiteBrandingLogo). pending is the response's
// branding_logo_pending. It answers nothing itself: the caller records the
// site_config.write audit row first and only then reports a failure, so a
// committed document is never left without its audit event.
func (s *Server) followSiteBranding(r *http.Request, named bool, bs store.BrandingStore, logo *siteBrandingLogo) (pending bool, err error) {
	if !named || bs == nil {
		return false, nil
	}
	pending, err = applySiteBrandingLogo(r.Context(), bs, logo, principalFromRequest(r))
	if err != nil {
		return false, err
	}
	if pending {
		slog.Warn("site config names branding.logo_path but the console has no branding yet; the logo is attached at the next apply after the Branding card is saved")
	}
	return pending, nil
}

// auditSiteBranding adds the branding keys to the site_config.write datum, only
// when the body NAMED branding: the file is the logo's owner from then on, so
// who delivered which bytes is reviewable from the log alone. logoFailed is the
// logo write having errored after the document committed: the row says so
// (branding_logo_failed) and carries no digest, as no bytes were stored.
// logoPending is the console having no branding record yet to hold the logo:
// the row says so (branding_logo_pending) and likewise carries no digest.
func auditSiteBranding(datum map[string]any, named bool, saved *types.SiteBranding, logo *siteBrandingLogo, logoFailed, logoPending bool) {
	if !named {
		return
	}
	if logoFailed {
		datum["branding_logo_failed"] = true
		logo = nil
	}
	if logoPending {
		datum["branding_logo_pending"] = true
		logo = nil
	}
	datum["branding_logo_path"] = ""
	if saved != nil {
		datum["branding_logo_path"] = saved.LogoPath
	}
	if logo != nil {
		sum := sha256.Sum256(logo.data)
		datum["branding_logo_sha256"] = hex.EncodeToString(sum[:])
	}
}
