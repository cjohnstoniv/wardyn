// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretstore

import (
	"context"
	"errors"
	"time"
)

// Meta is what a stored row says about itself — never its value, its name's
// ciphertext or its key material (credential-storage design §2.2, CS-6).
type Meta struct {
	Owner, Name string
	// Store is where the value lives: "pg" when it is sealed in the row, or
	// the external store a pointer row names ("vaultkv", "azurekv").
	Store string
	// PrincipalKey is true when the value is sealed under its owner's
	// principal key (enc_version 3), which destroying that key erases.
	PrincipalKey bool
	// AddedAt is when the row was first written; replacing the value keeps it.
	AddedAt time.Time
	// LastUsedAt is when a run's credential sink last used the value (MarkUsed),
	// to within a minute; nil if never.
	LastUsedAt *time.Time
	// ExpiresAt is the latest time a stored sign-in can still be used or
	// renewed; nil for a key or token.
	ExpiresAt *time.Time
}

// MetaStore is the row metadata a store keeps beside its values (the pg store,
// in both modes). It is not part of Store, as DeleteExpired is not: fakes and
// wrappers need not carry it, and a caller asserts it.
type MetaStore interface {
	// MarkUsed stamps LastUsedAt on this view's own row of name, unless it
	// was stamped within the last minute. A missing row is not an error.
	MarkUsed(ctx context.Context, name string) error
	// Metadata returns this view's OWN rows of names — never the operator's
	// fallback, never another owner's.
	Metadata(ctx context.Context, names []string) ([]Meta, error)
	// MetadataEverywhere returns every person's rows of names, whatever view
	// it is called on; the operator's ("") namespace is left out.
	MetadataEverywhere(ctx context.Context, names []string) ([]Meta, error)
}

// ErrNoMetadata is a MetaStore method's answer from a wrapper whose store
// keeps no row metadata.
var ErrNoMetadata = errors.New("secretstore: this store keeps no credential metadata")
