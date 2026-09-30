// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package subscription is the seam between the injection sink and a person's
// captured Claude subscription token: the sink resolves a subscription
// sentinel through a Provider, and the token lives only in proxy memory.
package subscription

import (
	"context"
	"time"
)

// Token is a live subscription access token and its expiry.
type Token struct {
	Value     string
	ExpiresAt time.Time
}

// Provider yields a person's current Claude subscription access token.
type Provider interface {
	Current(ctx context.Context) (Token, error)
	// Peek returns the stored token for a status surface: no cache fill, and
	// unlike Current it does not reject an expired token.
	Peek() (Token, error)
}
