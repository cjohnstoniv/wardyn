// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Console branding (migration 0091_branding). One row or none; the request is
// validated at the API boundary (internal/api/branding.go).
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// BrandingStore is the OPTIONAL store capability behind console branding,
// optional for DeviceStore's reason: a fake without it answers the public read
// as unbranded and refuses a write, never a silent no-op.
type BrandingStore interface {
	// GetBranding returns the record, or ErrNotFound for an unbranded console.
	GetBranding(ctx context.Context) (types.Branding, error)
	// PutBranding replaces the record. keepLogo leaves the stored logo as it
	// is and ignores b.Logo/b.LogoType.
	PutBranding(ctx context.Context, b types.Branding, keepLogo bool) (types.Branding, error)
	// DeleteBranding removes the record; false when there was none.
	DeleteBranding(ctx context.Context) (bool, error)
	// SetBrandingLogo replaces only the logo and its file-delivered mark; an
	// empty logo removes it. false when there is no record to attach it to.
	SetBrandingLogo(ctx context.Context, logo []byte, logoType string, fromFile bool, updatedBy string) (bool, error)
}

var _ BrandingStore = PG{}

const brandingCols = `org_name, name_format, primary_color, primary_text, dark_primary, dark_primary_text,
	support_url, logo, logo_type, logo_from_file, updated_at, updated_by`

func scanBranding(row pgx.Row) (types.Branding, error) {
	var b types.Branding
	err := row.Scan(&b.OrgName, &b.NameFormat, &b.Primary, &b.PrimaryText, &b.DarkPrimary,
		&b.DarkPrimaryText, &b.SupportURL, &b.Logo, &b.LogoType, &b.LogoFromFile, &b.UpdatedAt, &b.UpdatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.Branding{}, ErrNotFound
	}
	if err != nil {
		return types.Branding{}, fmt.Errorf("store: branding: %w", err)
	}
	return b, nil
}

// GetBranding implements BrandingStore.
func (s PG) GetBranding(ctx context.Context) (types.Branding, error) {
	return scanBranding(s.Pool.QueryRow(ctx, `SELECT `+brandingCols+` FROM branding WHERE singleton`))
}

// PutBranding implements BrandingStore.
func (s PG) PutBranding(ctx context.Context, b types.Branding, keepLogo bool) (types.Branding, error) {
	const q = `
		INSERT INTO branding (singleton, org_name, name_format, primary_color, primary_text,
			dark_primary, dark_primary_text, support_url, logo, logo_type, updated_by)
		VALUES (true, $1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (singleton) DO UPDATE SET
			org_name = EXCLUDED.org_name,
			name_format = EXCLUDED.name_format,
			primary_color = EXCLUDED.primary_color,
			primary_text = EXCLUDED.primary_text,
			dark_primary = EXCLUDED.dark_primary,
			dark_primary_text = EXCLUDED.dark_primary_text,
			support_url = EXCLUDED.support_url,
			logo = CASE WHEN $11 THEN branding.logo ELSE EXCLUDED.logo END,
			logo_type = CASE WHEN $11 THEN branding.logo_type ELSE EXCLUDED.logo_type END,
			logo_from_file = CASE WHEN $11 THEN branding.logo_from_file ELSE false END,
			updated_by = EXCLUDED.updated_by,
			updated_at = now()
		RETURNING ` + brandingCols
	var logo []byte
	logoType := ""
	if !keepLogo && len(b.Logo) > 0 {
		logo, logoType = b.Logo, b.LogoType
	}
	return scanBranding(s.Pool.QueryRow(ctx, q, b.OrgName, b.NameFormat, b.Primary, b.PrimaryText,
		b.DarkPrimary, b.DarkPrimaryText, b.SupportURL, logo, logoType, b.UpdatedBy, keepLogo))
}

// DeleteBranding implements BrandingStore.
func (s PG) DeleteBranding(ctx context.Context) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM branding WHERE singleton`)
	if err != nil {
		return false, fmt.Errorf("store: delete branding: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// SetBrandingLogo implements BrandingStore.
func (s PG) SetBrandingLogo(ctx context.Context, logo []byte, logoType string, fromFile bool, updatedBy string) (bool, error) {
	if len(logo) == 0 {
		logo, logoType, fromFile = nil, "", false
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE branding SET logo = $1, logo_type = $2, logo_from_file = $3,
		updated_by = $4, updated_at = now() WHERE singleton`, logo, logoType, fromFile, updatedBy)
	if err != nil {
		return false, fmt.Errorf("store: set branding logo: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
