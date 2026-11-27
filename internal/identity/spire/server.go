// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package spire

import (
	"context"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

// Selector is one SPIRE workload selector, "<type>:<value>" (k8s:ns:runs is type k8s, value ns:runs).
type Selector struct {
	Type  string
	Value string
}

// String is the selector in SPIRE's "<type>:<value>" form.
func (s Selector) String() string { return s.Type + ":" + s.Value }

// Entry is a SPIRE registration entry, as far as wardynd writes and reads one.
type Entry struct {
	ID          string
	SPIFFEID    spiffeid.ID
	ParentID    spiffeid.ID
	Selectors   []Selector
	X509SVIDTTL time.Duration
}

// Server is the narrow part of the SPIRE Server API the provider calls, as its admin. The gRPC adapter
// implements it against a real server; spiretest implements it in memory.
type Server interface {
	// MintJWTSVID has the server sign a JWT-SVID for id with one audience and ttl.
	MintJWTSVID(ctx context.Context, id spiffeid.ID, audience string, ttl time.Duration) (string, error)
	// FindEntries returns the IDs of the entries whose selectors include sel.
	FindEntries(ctx context.Context, sel Selector) ([]string, error)
	// CreateEntry registers e (its ID is ignored).
	CreateEntry(ctx context.Context, e Entry) error
	// DeleteEntries deletes the entries; one already gone counts as deleted.
	DeleteEntries(ctx context.Context, ids []string) error
	// ListEntriesByID returns every entry whose SPIFFE ID is id.
	ListEntriesByID(ctx context.Context, id spiffeid.ID) ([]Entry, error)
	Close() error
}
