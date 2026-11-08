// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package approval

import (
	"encoding/json"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestTallyKeyFileDownload: a reviewed download's row is counted under its bare kind when a run's end cancels it.
func TestTallyKeyFileDownload(t *testing.T) {
	ap := types.ApprovalRequest{Kind: types.ApprovalFileDownload,
		RequestedScope: json.RawMessage(`{"name":"report.csv","bytes":17,"sha256":"","requested_by":"sub-a"}`)}
	if got := TallyKey(ap); got != "file_download" {
		t.Fatalf("TallyKey(file_download) = %q, want file_download", got)
	}
}
