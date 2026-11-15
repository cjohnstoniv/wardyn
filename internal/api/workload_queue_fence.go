// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"

	"github.com/google/uuid"
)

// launchBindingKey carries the namespace binding a custom resource's launch was authorised under from
// launchClaimed to the create it makes, so a run that waits in its pool's queue keeps the binding in its
// stored authority (queueAuthority.Binding) and its start holds only while that binding exists.
type launchBindingKey struct{}

func withLaunchBinding(ctx context.Context, binding uuid.UUID) context.Context {
	return context.WithValue(ctx, launchBindingKey{}, binding)
}

func launchBindingFrom(ctx context.Context) (uuid.UUID, bool) {
	b, ok := ctx.Value(launchBindingKey{}).(uuid.UUID)
	return b, ok
}
