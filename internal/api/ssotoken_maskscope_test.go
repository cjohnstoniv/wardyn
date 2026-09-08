// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
)

// TestUploadSSOToken_MaskPatternsAreRunScoped is the F007 regression. The
// upload registered the blob's access_token/refresh_token with
// MaskRegistry.AddGlobal, and those bytes are pure sandbox input — the handler
// binds WHOSE IdP and WHICH run may write, never what the token itself
// contains. secretmask's global corpus is unioned into EVERY run's masker and,
// via cmd/wardynd's maskingRecorder, applied to the Data/Target of every
// persisted audit event including run-less rows (Snapshot(uuid.Nil)); Evict
// clears per-run entries only. So a process inside the vendor login image could
// pick arbitrary >=MinLen strings and have them replaced with the placeholder
// in every operator's logs for the life of the daemon.
//
// The credential loses no coverage: resolveBedrockAuth AddGlobals the STORED
// blob on every dispatch that actually selects this credential
// (runs_bedrock.go), so the global registration follows the server's decision
// to use it rather than the sandbox's decision to name a string.
func TestUploadSSOToken_MaskPatternsAreRunScoped(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	st := ssoBindingStore{
		run:    loginRunFor(runID),
		events: loginStartedFor(runID),
	}
	sec := &memSecrets{m: map[string][]byte{}}
	reg := secretmask.NewRegistry()
	cfg := baseTestConfig(h, st)
	cfg.Secrets = sec
	cfg.BedrockRegion = operatorRegion
	cfg.MaskRegistry = reg
	srv := New(cfg)
	h.srv = srv
	tok := h.mintRunToken(t, runID)

	// An ordinary log phrase in the access_token slot: what a poisoning upload
	// looks like, and indistinguishable from a real token to every guard above.
	const poison = "connection refused on 203.0.113.7:4444"
	w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+runID.String(), tok,
		ssoBlobBody(operatorStartURL, operatorRegion, poison))
	if w.Code != http.StatusNoContent {
		t.Fatalf("upload: code = %d, want 204; body=%s", w.Code, w.Body.String())
	}

	if containsSecret(reg.Snapshot(uuid.Nil), poison) {
		t.Error("a sandbox-supplied string became a PROCESS-GLOBAL mask pattern — it redacts every run's output and every run-less audit row")
	}
	if !containsSecret(reg.Snapshot(runID), poison) {
		t.Error("the captured value is not masked for the login run that produced it")
	}
}

func containsSecret(set [][]byte, want string) bool {
	for _, v := range set {
		if bytes.Equal(v, []byte(want)) {
			return true
		}
	}
	return false
}
