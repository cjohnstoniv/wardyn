// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Preset is one launch preset: an admin-managed, named bundle of existing
// CreateRunRequest fields a launcher names (CreateRunRequest.Preset) instead
// of sending the whole spec. The server expands it and runs the unchanged
// create path, so the caller's own ceiling, capabilities, secrets and drive
// apply exactly as they would to Request sent explicitly. Version, the
// timestamps and the author fields are read-only.
type Preset struct {
	Name        string `json:"name"`
	Version     int    `json:"version"`
	Description string `json:"description,omitempty"`
	// UserTypes narrows who may list and launch the preset to these user
	// types; empty is every type. Admins see and launch every preset.
	UserTypes []string `json:"user_types"`
	// Request is the stored create-run body. It may not carry the
	// per-launch fields (title, task) or a preset of its own.
	Request   CreateRunRequest `json:"request"`
	CreatedAt time.Time        `json:"created_at"`
	UpdatedAt time.Time        `json:"updated_at"`
	CreatedBy string           `json:"created_by,omitempty"`
	UpdatedBy string           `json:"updated_by,omitempty"`
}

// PresetRequest is the PUT /api/v1/presets/{name} body.
type PresetRequest struct {
	Description string           `json:"description,omitempty"`
	UserTypes   []string         `json:"user_types,omitempty"`
	Request     CreateRunRequest `json:"request"`
}

// PresetsDocument is GET /api/v1/presets's body and ApplyPresets's
// parameter: `wardyn preset get` prints it and `wardyn preset set` reads it
// back.
type PresetsDocument struct {
	Presets []Preset `json:"presets"`
}

// ListPresets returns every preset the caller may launch: all of them for an
// admin, and for anyone else the ones open to their user type.
func (c *Client) ListPresets(ctx context.Context) (PresetsDocument, error) {
	var out PresetsDocument
	err := c.do(ctx, http.MethodGet, "/api/v1/presets", nil, &out)
	return out, err
}

// GetPreset returns one preset by name (404 when unknown, or not open to the
// caller's user type).
func (c *Client) GetPreset(ctx context.Context, name string) (Preset, error) {
	var out Preset
	err := c.do(ctx, http.MethodGet, "/api/v1/presets/"+url.PathEscape(name), nil, &out)
	return out, err
}

// PutPreset creates the named preset at version 1, or replaces it and moves
// its version by one. A body identical to the stored preset changes nothing.
// Admin only.
func (c *Client) PutPreset(ctx context.Context, name string, req PresetRequest) (Preset, error) {
	var out Preset
	err := c.do(ctx, http.MethodPut, "/api/v1/presets/"+url.PathEscape(name), req, &out)
	return out, err
}

// DeletePreset removes a preset by name. Admin only.
func (c *Client) DeletePreset(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/presets/"+url.PathEscape(name), nil, nil)
}

// ApplyPresets upserts every preset doc names, by name, over PUT
// /api/v1/presets/{name}, and returns a fresh ListPresets. Nothing doc omits
// is touched and nothing is deleted, and an unchanged preset keeps its
// version, so `wardyn preset get > f && wardyn preset set f` is a no-op.
func (c *Client) ApplyPresets(ctx context.Context, doc PresetsDocument) (PresetsDocument, error) {
	for _, p := range doc.Presets {
		req := PresetRequest{Description: p.Description, UserTypes: p.UserTypes, Request: p.Request}
		if _, err := c.PutPreset(ctx, p.Name, req); err != nil {
			return PresetsDocument{}, fmt.Errorf("apply preset %q: %w", p.Name, err)
		}
	}
	return c.ListPresets(ctx)
}
