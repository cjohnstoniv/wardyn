// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretstore

import (
	"context"
	"time"
)

// ProviderChange contains only the destination metadata approved for disclosure.
type ProviderChange struct {
	Owner          string    `json:"owner"`
	ProviderID     string    `json:"provider_id"`
	ProviderUID    string    `json:"provider_uid"`
	Reason         string    `json:"reason"`
	ChangedAt      time.Time `json:"changed_at"`
	NewDestination string    `json:"new_destination"`
}

// ProviderChangeStore reads and erases history in this view's own namespace only.
type ProviderChangeStore interface {
	ProviderChange(context.Context, string) (ProviderChange, bool, error)
	DeleteProviderChanges(context.Context) error
}

// ProviderInvalidation describes a purge; an empty successor UID means deletion.
type ProviderInvalidation struct {
	OldUID string
	Change ProviderChange
}

type providerInvalidationsKey struct{}

// WithProviderInvalidations binds the change records to the credential purge transaction.
func WithProviderInvalidations(ctx context.Context, changes []ProviderInvalidation) context.Context {
	return context.WithValue(ctx, providerInvalidationsKey{}, changes)
}

// ProviderInvalidationsFrom returns the provider changes accompanying a purge.
func ProviderInvalidationsFrom(ctx context.Context) []ProviderInvalidation {
	changes, _ := ctx.Value(providerInvalidationsKey{}).([]ProviderInvalidation)
	return changes
}
