// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
)

var errMaskSyncUnavailable = errors.New("no shared masking registry to ask")

// setup_checks_ha.go holds the three /setup/status rows that exist only while
// WARDYN_HA is on. Their labels and sentences are mock packet M10's canon, the
// way the kek_service row's are; setup_checks_ha_test.go pins them byte for byte.

// MaskSyncProbe is the part of the shared masking registry the
// mask_registry_shared row asks about: nil when this replica's LISTEN
// connection is up and its cursor has reached the committed generation, an
// error otherwise.
type MaskSyncProbe interface {
	Synced(ctx context.Context) error
}

// haChecks returns ha_mode, recording_store_shared and mask_registry_shared, in
// that order. Only the last can fail, and it fails when this replica cannot
// confirm it holds the latest masking list. It is never Blocking: the replica
// already refuses the doors that depend on it, and sending every admin into the
// setup funnel during a Postgres outage would hide the console that shows it.
func (s *Server) haChecks(ctx context.Context) []SetupCheck {
	return []SetupCheck{
		haModeCheck(),
		recordingStoreSharedCheck(s.cfg.RecordingStore != nil),
		s.maskRegistrySharedCheck(ctx),
	}
}

func haModeCheck() SetupCheck {
	return SetupCheck{
		ID: "ha_mode", Label: "High availability", Status: "ok",
		Detail: "High availability is on (`WARDYN_HA`). Several replicas serve this deployment. Per-replica limits apply to each replica, so they add up across replicas.",
	}
}

// recordingStoreSharedCheck is ok with the pg store, info with recording off.
// WARDYN_HA refuses to boot with any other store, so recording is on exactly
// when the Postgres store is.
func recordingStoreSharedCheck(recording bool) SetupCheck {
	if !recording {
		return SetupCheck{
			ID: "recording_store_shared", Label: "Shared recording store", Status: "info",
			Detail: "Recording is off, so no replica stores recordings.",
		}
	}
	return SetupCheck{
		ID: "recording_store_shared", Label: "Shared recording store", Status: "ok",
		Detail: "Recordings are stored in Postgres, so every replica reads the same ones.",
	}
}

// maskRegistrySharedCheck asks this replica, with the status request's own
// context (bounded by the caller's timeout), whether its masking list is
// current. A probe that is missing counts as not confirmed: HA without a
// registry to ask has nothing to vouch for.
func (s *Server) maskRegistrySharedCheck(ctx context.Context) SetupCheck {
	ctx, cancel := context.WithTimeout(ctx, storePingTimeout)
	defer cancel()
	var err error
	if s.cfg.MaskSync == nil {
		err = errMaskSyncUnavailable
	} else {
		err = s.cfg.MaskSync.Synced(ctx)
	}
	if err == nil {
		return SetupCheck{
			ID: "mask_registry_shared", Label: "Shared secret masking", Status: "ok",
			Detail: "This replica has the latest secret-masking list that every replica shares.",
		}
	}
	slog.WarnContext(ctx, "api: this replica cannot confirm its secret-masking list is current", slog.Any("err", err))
	return SetupCheck{
		ID: "mask_registry_shared", Label: "Shared secret masking", Status: "fail",
		Detail: "This replica can't confirm it has the latest secret-masking list. Until it can, it refuses recording uploads and new attaches, and shows a placeholder in place of live output.",
		Fix:    "Check this replica's connection to Postgres. It catches up on its own when the connection returns.",
	}
}
