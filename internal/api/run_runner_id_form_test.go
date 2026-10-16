// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// A runner id sent in braces, urn:uuid: or 32-hex form is carried in the canonical form, so the queue's
// revoke pass and sweep match it; one that does not parse is left for the placement gate.
func TestDecodeRunRequestCanonicalisesTheRunnerID(t *testing.T) {
	id := uuid.New()
	for body, want := range map[string]string{
		`{"runner_id":"` + id.String() + `"}`:                              id.String(),
		`{"runner_id":"{` + id.String() + `}"}`:                            id.String(),
		`{"runner_id":"urn:uuid:` + id.String() + `"}`:                     id.String(),
		`{"runner_id":"` + strings.ToUpper(id.String()) + `"}`:             id.String(),
		`{"runner_id":"` + strings.ReplaceAll(id.String(), "-", "") + `"}`: id.String(),
		`{"runner_id":"not-a-uuid"}`:                                       "not-a-uuid",
		`{}`:                                                               "",
	} {
		var req createRunRequest
		rec := httptest.NewRecorder()
		if !(&Server{}).decodeRunRequest(rec, httptest.NewRequest("POST", "/api/v1/runs", strings.NewReader(body)), &req) {
			t.Fatalf("%s refused: %d %s", body, rec.Code, rec.Body)
		}
		if req.RunnerID != want {
			t.Errorf("%s: runner_id = %q, want %q", body, req.RunnerID, want)
		}
	}
}
