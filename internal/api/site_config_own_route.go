// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// carryOwnRouteBlocks refuses, on w, a PUT /site-config that changes a block with a writer of its own,
// and carries those blocks forward: false means it answered. Carved out of handlePutSiteConfig for funlen.
func carryOwnRouteBlocks(w http.ResponseWriter, cfg *types.SiteConfig, existing types.SiteConfig) bool {
	// The egress baseline lowers grades, so it has a stricter writer than this door: the egress block
	// and the set of baseline-marked internal hosts must come back as stored, whoever is asking.
	carryForwardInternalHostMarks(cfg, existing)
	if msg := inlineBaselineRefusal(*cfg, existing); msg != "" {
		writeErrorReason(w, http.StatusBadRequest, reasonSiteConfigEgressViaOwnRoute, msg)
		return false
	}
	cfg.Egress = existing.Egress
	// The recording level is the security tier's governance write as well (governance_recording.go): the
	// block must come back as stored, so a round trip of GET passes and a change is refused.
	if recordingState(*cfg) != recordingState(existing) {
		writeErrorReason(w, http.StatusBadRequest, reasonSiteConfigRecordingViaOwnRoute,
			"recording is a governance write: change it with PUT /governance/recording, not PUT /site-config")
		return false
	}
	return true
}
