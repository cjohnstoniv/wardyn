// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestErasedDownloadAskerCountsAsEveryone: a download whose asker was erased reads as asked for by every principal,
// so a second-human rule lets no person decide it; a named asker is only themselves. The name is reserved, so no
// person signs in under it.
func TestErasedDownloadAskerCountsAsEveryone(t *testing.T) {
	download := func(by string) types.ApprovalRequest {
		return types.ApprovalRequest{Kind: types.ApprovalFileDownload, RequestedScope: json.RawMessage(
			`{"name":"build.log","bytes":12,"sha256":"` + strings.Repeat("ab", 32) + `","requested_by":"` + by + `"}`)}
	}
	if !askedForDownload(download("dana@example.test"), "dana@example.test") || askedForDownload(download("dana@example.test"), "lee@example.test") {
		t.Fatal("a named asker: want dana alone to count as the asker")
	}
	if !askedForDownload(download(types.FileDownloadAskerErased), "lee@example.test") {
		t.Fatal("an erased asker: want every principal to count as the asker, so no person decides it under a second-human rule")
	}
	if s := (&Server{}); !s.isReservedPrincipal(types.FileDownloadAskerErased) || !s.isReservedPrincipal(" Erased ") {
		t.Fatal("erased: want it reserved, so no person signs in under the erased asker's name")
	}
}
