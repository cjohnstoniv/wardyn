// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

// ComponentRequest is the body for POST /api/v1/me/components, PUT
// /api/v1/me/components/{id} and PUT /api/v1/components/{id}. The server
// validates the definition as the owner's kind of row: a person's saved component
// names DNS hosts only and cannot be shared or sent over plain HTTP, an
// organisation's can.
type ComponentRequest struct {
	Name       string              `json:"name"`
	Definition ComponentDefinition `json:"definition"`
}

// ComponentRequirement is one thing a saved component still needs before a run
// can use it: a secret to add, or a connection to make. The server returns the
// list on every save; it is empty until the component names something it needs.
type ComponentRequirement struct {
	Kind   string `json:"kind"`
	Name   string `json:"name,omitempty"`
	Status string `json:"status"`
	Fix    string `json:"fix,omitempty"`
}

// ComponentSaved answers a component save: the stored row, and what it still needs.
type ComponentSaved struct {
	Component
	Requirements []ComponentRequirement `json:"requirements"`
}

// ComponentSecretView is one secret an organisation component carries, as a
// person granted it sees it: how it is delivered and whether the value is the
// organisation's, never its name.
type ComponentSecretView struct {
	Delivery ComponentDelivery `json:"delivery"`
	Shared   bool              `json:"shared,omitempty"`
}

// OrgComponentView is an organisation component as a person granted it sees it:
// where it reaches and how, with no secret names and no config values.
type OrgComponentView struct {
	ID         uuid.UUID             `json:"id"`
	Name       string                `json:"name"`
	Hosts      []string              `json:"hosts"`
	Secrets    []ComponentSecretView `json:"secrets"`
	ConfigKeys []string              `json:"config_keys"`
}

// MyComponents answers GET /api/v1/me/components: what the caller may do with
// components, the ones they saved, and the organisation's they are granted.
type MyComponents struct {
	// MayDefine is whether the caller may define their own components.
	MayDefine bool `json:"may_define"`
	// ResidentDeliveryAllowed is false when the deployment refuses environment and
	// file delivery for every component.
	ResidentDeliveryAllowed bool `json:"resident_delivery_allowed"`
	// AutonomyCap is the level an unattended run carrying a component of one's own
	// is held to; empty means no cap.
	AutonomyCap AutonomyLevel `json:"autonomy_cap"`
	// Mine are the caller's saved components, by name.
	Mine []Component `json:"mine"`
	// Org are the organisation components the caller is granted, by name.
	Org []OrgComponentView `json:"org"`
}

// MyComponents lists the caller's component surface. GET /api/v1/me/components.
func (c *Client) MyComponents(ctx context.Context) (MyComponents, error) {
	var out MyComponents
	err := c.do(ctx, http.MethodGet, "/api/v1/me/components", nil, &out)
	return out, err
}

// SaveMyComponent stores a new component of the caller's own. 422 on an invalid
// definition or when the caller is at the saved-component limit; 409 on a name
// they already use; 403 when custom components are not turned on for them.
// POST /api/v1/me/components.
func (c *Client) SaveMyComponent(ctx context.Context, req ComponentRequest) (ComponentSaved, error) {
	var out ComponentSaved
	err := c.do(ctx, http.MethodPost, "/api/v1/me/components", req, &out)
	return out, err
}

// UpdateMyComponent replaces the name and definition of one of the caller's
// saved components and moves its version. 404 for an id that is not theirs.
// PUT /api/v1/me/components/{id}.
func (c *Client) UpdateMyComponent(ctx context.Context, id uuid.UUID, req ComponentRequest) (ComponentSaved, error) {
	var out ComponentSaved
	err := c.do(ctx, http.MethodPut, "/api/v1/me/components/"+id.String(), req, &out)
	return out, err
}

// DeleteMyComponent removes one of the caller's saved components. Runs already
// launched with it keep their record. 404 for an id that is not theirs.
// DELETE /api/v1/me/components/{id}.
func (c *Client) DeleteMyComponent(ctx context.Context, id uuid.UUID) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/me/components/"+id.String(), nil, nil)
}

// ListComponents lists the organisation's components, whole. Admin only.
// GET /api/v1/components.
func (c *Client) ListComponents(ctx context.Context) ([]Component, error) {
	var out []Component
	err := c.do(ctx, http.MethodGet, "/api/v1/components", nil, &out)
	return out, err
}

// PutComponent creates the organisation component with this id, or replaces it.
// A new component is attachable by nobody until an admin names who may use it
// under /api/v1/permissions. Admin only. PUT /api/v1/components/{id}.
func (c *Client) PutComponent(ctx context.Context, id uuid.UUID, req ComponentRequest) (ComponentSaved, error) {
	var out ComponentSaved
	err := c.do(ctx, http.MethodPut, "/api/v1/components/"+id.String(), req, &out)
	return out, err
}

// DeleteComponent removes an organisation component. Its restriction stays, so
// the id is never open to anyone, and runs launched with it keep their record.
// Admin only. DELETE /api/v1/components/{id}.
func (c *Client) DeleteComponent(ctx context.Context, id uuid.UUID) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/components/"+id.String(), nil, nil)
}
