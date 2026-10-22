// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// validateSiteRecording refuses a recording block whose level is not one (keystrokes are none).
func validateSiteRecording(r *types.SiteRecording) error {
	if r != nil && !r.Level.Valid() {
		return fmt.Errorf("recording.level: %q is not a recording level (off, terminal or screen_actions)", r.Level)
	}
	return nil
}

// validateRecordingLevel refuses a policy's recording value that is not a level.
func validateRecordingLevel(l types.RecordingLevel) error {
	if !l.Valid() {
		return fmt.Errorf("recording: %q is not a recording level (off, terminal or screen_actions)", l)
	}
	return nil
}

// carryForwardSiteRecording keeps the stored recording block when the body does not name it, on the
// provider blocks' MDM re-apply terms, and stores an all-default block as none so a GET stays
// byte-identical to an install that never set one.
func carryForwardSiteRecording(cfg *types.SiteConfig, existing types.SiteConfig, present map[string]bool) {
	if !present["recording"] {
		cfg.Recording = existing.Recording
	} else if cfg.Recording != nil && *cfg.Recording == (types.SiteRecording{}) {
		cfg.Recording = nil
	}
}

// refuseSiteRecordingWithoutStore answers 422 recording_store_off to a save that locks a recording
// minimum above off while this organisation keeps no session recordings, and reports whether it did.
func (s *Server) refuseSiteRecordingWithoutStore(w http.ResponseWriter, r *types.SiteRecording) bool {
	if r == nil || !r.Locked || r.OrgLevel() == types.RecordingOff || s.cfg.RecordingStore != nil {
		return false
	}
	writeErrorReason(w, placement.ReasonRecordingStoreOff.Status(), string(placement.ReasonRecordingStoreOff), runnerpool.RecordingSaveStoreOffMsg())
	return true
}

// auditSiteRecording puts the organisation's effective recording level on the site_config.write row.
func auditSiteRecording(datum map[string]any, r *types.SiteRecording, named bool) {
	if r == nil && !named {
		return
	}
	datum["recording_level"] = string(r.OrgLevel())
	datum["recording_locked"] = r != nil && r.Locked
}
