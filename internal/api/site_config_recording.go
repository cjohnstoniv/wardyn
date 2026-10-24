// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"

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

// auditSiteRecording puts the organisation's effective recording level on the site_config.write row.
func auditSiteRecording(datum map[string]any, r *types.SiteRecording, named bool) {
	if r == nil && !named {
		return
	}
	datum["recording_level"] = string(r.OrgLevel())
	datum["recording_locked"] = r != nil && r.Locked
}
