// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

type (
	// WallPreset is a named walls document that only narrows, picked at launch: built in, or kept for
	// one person, a group or the organisation (/api/v1/wall-presets).
	WallPreset = types.WallPreset
	// WallPresetScope is builtin, person, group or org.
	WallPresetScope = types.WallPresetScope
	// WallPresetChange is what a run's wall preset had become when it ended the run queued under it:
	// at Version, or Gone (deleted, or no longer open to the run's owner).
	WallPresetChange = types.WallPresetChange
)

// WallPresetList is GET /api/v1/wall-presets: the built-ins first, then every preset the caller may
// read, and where the caller may save one (the run templates' publish rules). Publish.GroupLabels
// labels those groups and the groups of the group presets listed.
type WallPresetList struct {
	Presets []WallPreset    `json:"presets"`
	Publish TemplatePublish `json:"publish"`
}

// WallPresetSaveRequest creates a preset (POST: scope, group_id for a group, name, spec) or replaces
// one (PUT: name, spec and expected_version, the version the caller read). The scope never changes;
// a preset reaches another audience by being saved there as a new one.
type WallPresetSaveRequest struct {
	Scope           WallPresetScope   `json:"scope,omitempty"`
	GroupID         string            `json:"group_id,omitempty"`
	Name            string            `json:"name"`
	Description     string            `json:"description,omitempty"`
	Spec            PersonalWallsSpec `json:"spec"`
	ExpectedVersion *int              `json:"expected_version,omitempty"`
}

// ListWallPresets returns the presets the caller may apply.
func (c *Client) ListWallPresets(ctx context.Context) (WallPresetList, error) {
	var out WallPresetList
	err := c.do(ctx, http.MethodGet, "/api/v1/wall-presets", nil, &out)
	return out, err
}

// GetWallPreset returns one preset by its id: a uuid, or a built-in's name.
func (c *Client) GetWallPreset(ctx context.Context, id string) (WallPreset, error) {
	var out WallPreset
	err := c.do(ctx, http.MethodGet, "/api/v1/wall-presets/"+url.PathEscape(id), nil, &out)
	return out, err
}

// SaveWallPreset creates a preset when id is empty, and replaces that preset otherwise
// (req.ExpectedVersion is then required by the server).
func (c *Client) SaveWallPreset(ctx context.Context, id string, req WallPresetSaveRequest) (WallPreset, error) {
	method, path := http.MethodPost, "/api/v1/wall-presets"
	if id != "" {
		method, path = http.MethodPut, path+"/"+url.PathEscape(id)
	}
	var out WallPreset
	err := c.do(ctx, method, path, req, &out)
	return out, err
}

// DeleteWallPreset removes a preset the caller may write. Runs already launched with it keep it.
func (c *Client) DeleteWallPreset(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/wall-presets/"+url.PathEscape(id), nil, nil)
}
