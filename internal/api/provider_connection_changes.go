// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const providerAccessRecheck = "Could not check your connection. Wait a moment, then re-check."

func (s *Server) providerInvalidations(before, after *types.ModelProviders) []secretstore.ProviderInvalidation {
	var out []secretstore.ProviderInvalidation
	for _, uid := range invalidatedProviderUIDs(before, after) {
		change := secretstore.ProviderInvalidation{OldUID: uid}
		for _, old := range before.Providers {
			if old.UID != uid {
				continue
			}
			change.Change.ProviderID = old.ID
			if next, ok := modelProviderByID(after, old.ID); ok {
				reason := "destination_changed"
				if old.Kind != next.Kind {
					reason = "kind_changed"
				}
				change.Change = secretstore.ProviderChange{ProviderID: next.ID, ProviderUID: next.UID,
					Reason: reason, ChangedAt: s.cfg.Now().UTC(), NewDestination: providerHost(next)}
			}
		}
		out = append(out, change)
	}
	return out
}

func unreadableProvider(row *SetupProviderAccess) {
	row.State, row.Cause, row.Action = modelAccessNotConfigured, "store_unreadable", providerAccessRecheck
}

func (s *Server) missingProviderCause(ctx context.Context, row *SetupProviderAccess, p types.ModelProvider, owner string) {
	if row.State != modelAccessNotConfigured || row.Cause != "" {
		return
	}
	row.Cause = "never_connected"
	if s.cfg.Secrets == nil || previewHidesOwnCredential(ctx) {
		return
	}
	history, ok := s.cfg.Secrets.For(owner).(secretstore.ProviderChangeStore)
	if !ok {
		return
	}
	change, found, err := history.ProviderChange(ctx, p.UID)
	if errors.Is(err, secretstore.ErrNoMetadata) {
		return
	}
	if err != nil {
		unreadableProvider(row)
		return
	}
	// A failed configuration save can leave the purge committed; do not disclose its uncommitted destination.
	if found && change.NewDestination != providerHost(p) {
		row.Cause = ""
		return
	}
	if found && change.ProviderID == p.ID {
		row.Cause, row.ChangedAt, row.NewDestination = change.Reason, &change.ChangedAt, change.NewDestination
	}
}
