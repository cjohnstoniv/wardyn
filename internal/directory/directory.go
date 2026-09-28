// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package directory turns what an admin TYPES into what Wardyn STORES.
//
// Every "who" field in the product is a claim value, rarely the thing a
// human knows (an Entra group's `groups` claim carries its OBJECT GUID, not
// its name). Hand-typing those is a silent-mismatch landmine: a
// plausible-looking wrong string binds nothing and fails open.
//
// So: Entry.ClaimValue IS the contract — the exact string the claim will
// carry, and what a UI inserts on selection. Entry.DisplayName is what a UI
// RENDERS. The two are deliberately different fields, since for groups they
// are deliberately different strings.
//
// Provider-abstracted on purpose: one interface, Entra ships first,
// Okta/Google are later connectors behind the same contract. The whole
// capability is opt-in — unconfigured, every field stays free text.
package directory

import (
	"context"
	"errors"
	"fmt"
)

// Kind selects which class of directory object a search covers. KindAny is
// IN the v1 contract, not a convenience: the People-step Value field is one
// kind-LESS input that accepts an App Role, a group, or an email.
type Kind string

const (
	KindUser    Kind = "user"
	KindGroup   Kind = "group"
	KindAppRole Kind = "approle"
	KindAny     Kind = "any"
)

// Valid reports whether k is one of the four contract kinds.
func (k Kind) Valid() bool {
	switch k {
	case KindUser, KindGroup, KindAppRole, KindAny:
		return true
	}
	return false
}

const (
	// MaxResults is ONE GLOBAL cap across every kind in a single search, not a
	// per-kind quota: in KindAny the three per-kind result sets are
	// concatenated in anyKindOrder and the first MaxResults survive, so the
	// outcome is deterministic ("App Roles first, then groups, then users").
	// Every search re-trims to this cap on the way out: $top is a request to
	// the upstream, not a guarantee from it.
	MaxResults = 20

	// MinQueryLen is the shortest query a connector will send upstream. Below
	// it a search returns empty WITHOUT a round trip, since one or two
	// characters match most of a directory. The connector-side backstop for
	// the same floor the HTTP layer enforces.
	MinQueryLen = 2
)

// Entry is one suggestion. DisplayName is rendered; ClaimValue is stored.
type Entry struct {
	// DisplayName is the human-readable label — a user's display name, a
	// group's name, an App Role's display name. Never store this.
	DisplayName string `json:"display_name"`
	// ClaimValue is the exact string the claim carries and the field must
	// hold: email (user), object GUID (group), manifest value (App Role).
	ClaimValue string `json:"claim_value"`
	// Kind is which of the three sources produced this entry.
	Kind Kind `json:"kind"`
	// Detail is a secondary disambiguator for the row. Presentation only —
	// never the stored value.
	Detail string `json:"detail,omitempty"`
}

// Directory searches a configured identity provider. Implementations are
// expected to be safe for concurrent use.
type Directory interface {
	// Search returns at most MaxResults entries matching q, in the fixed
	// anyKindOrder when kind is KindAny. A query shorter than MinQueryLen
	// returns (nil, nil) — empty is not an error.
	Search(ctx context.Context, q string, kind Kind) ([]Entry, error)
}

// ErrUnconfigured means no directory provider is configured. The HTTP layer
// answers it with a distinct 503 code, the UI's ABSENT-MODE signal: the
// combobox degrades to a plain free-text input. Deliberately a sentinel, not
// a ProviderError — nothing upstream failed, the feature is simply off.
var ErrUnconfigured = errors.New("directory: no provider configured")

// ProviderError wraps a failure talking to the upstream directory. Op names
// the call and Status carries the upstream HTTP status when there was one (0
// otherwise), so the HTTP layer can decide what to tell the admin.
type ProviderError struct {
	Provider string // "entra"
	Op       string
	Status   int
	Err      error
}

func (e *ProviderError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("directory: %s %s: upstream status %d: %v", e.Provider, e.Op, e.Status, e.Err)
	}
	return fmt.Sprintf("directory: %s %s: %v", e.Provider, e.Op, e.Err)
}

func (e *ProviderError) Unwrap() error { return e.Err }

// anyKindOrder is the FIXED order KindAny concatenates in. App Roles lead
// since they are the smallest, most-intentional set; users trail since they
// are the largest and would otherwise crowd out the other two under the cap.
var anyKindOrder = [...]Kind{KindAppRole, KindGroup, KindUser}

// searchAny implements the KindAny merge ONCE, here, so every connector gets
// the same order and the same single cap. per is the connector's
// single-kind query. An error from any per-kind query aborts the whole
// search (a partial result silently missing a whole class of subjects is
// worse than an honest failure) — except Entra's App Role lookup, which
// degrades to (nil, nil) when the tenant lacks the permission for it.
func searchAny(ctx context.Context, q string, per func(context.Context, string, Kind) ([]Entry, error)) ([]Entry, error) {
	out := make([]Entry, 0, MaxResults)
	for _, k := range anyKindOrder {
		if len(out) >= MaxResults {
			break
		}
		got, err := per(ctx, q, k)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	return out[:min(len(out), MaxResults)], nil
}
