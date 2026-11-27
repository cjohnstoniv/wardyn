// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"time"

	"github.com/google/uuid"
)

// PreviewVisibility is who may open a run's preview URL: private (the run's entry rule), org (people of this
// deployment) or public (anyone holding the link). Each is wider than the one before it.
type PreviewVisibility string

const (
	PreviewPrivate PreviewVisibility = "private"
	PreviewOrg     PreviewVisibility = "org"
	PreviewPublic  PreviewVisibility = "public"
)

// PreviewAppPrefix starts the name of the app the UI gateway presents for a preview ("__preview-<port>"). A
// declared app name starts alphanumeric, so the prefix cannot collide with one.
const PreviewAppPrefix = "__preview-"

// PreviewLinkPrincipalPrefix starts the principal of a public link's visitor ("preview-link:<preview id>").
const PreviewLinkPrincipalPrefix = "preview-link:"

// The bounds and default of how long a preview lives before it expires.
const (
	PreviewTTLMin     = time.Hour
	PreviewTTLMax     = 7 * 24 * time.Hour
	PreviewTTLDefault = 24 * time.Hour
)

// rank orders the visibilities from narrowest (1) to widest; 0 for a value that is not one.
func (v PreviewVisibility) rank() int {
	switch v {
	case PreviewPrivate:
		return 1
	case PreviewOrg:
		return 2
	case PreviewPublic:
		return 3
	}
	return 0
}

// Valid reports whether v is one of the three visibilities.
func (v PreviewVisibility) Valid() bool { return v.rank() > 0 }

// AtMost reports whether v is a visibility no wider than ceiling. It is false when either is not a
// visibility, so an unknown ceiling admits nothing.
func (v PreviewVisibility) AtMost(ceiling PreviewVisibility) bool {
	return v.Valid() && ceiling.Valid() && v.rank() <= ceiling.rank()
}

// RunPreview is one row of run_previews: a preview URL on a port a run serves. It never carries the link
// hash, so the wire never does.
type RunPreview struct {
	ID         uuid.UUID         `json:"id"`
	RunID      uuid.UUID         `json:"run_id"`
	Port       int               `json:"port"`
	Visibility PreviewVisibility `json:"visibility"`
	CreatedBy  string            `json:"created_by"`
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
	ExpiresAt  time.Time         `json:"expires_at"`
	RevokedAt  *time.Time        `json:"revoked_at,omitempty"`
}
