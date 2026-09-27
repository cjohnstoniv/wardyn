// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "time"

// Branding name formats: "<Company> Wardyn" and "Wardyn for <Company>".
const (
	BrandNamePrefix = "prefix"
	BrandNameSuffix = "suffix"
)

// Branding is the org-wide console branding record (migration 0091_branding).
// Only the primary colour, its text colour, the logo and the name are
// brandable: danger/warning/success/info and the admin-view cue are fixed
// Wardyn tokens a brand can never recolour. Empty DarkPrimary means the dark
// pair is derived (DeriveDarkPrimary in internal/api).
type Branding struct {
	OrgName         string
	NameFormat      string
	Primary         string
	PrimaryText     string
	DarkPrimary     string
	DarkPrimaryText string
	SupportURL      string
	Logo            []byte
	LogoType        string
	UpdatedAt       time.Time
	UpdatedBy       string
}
