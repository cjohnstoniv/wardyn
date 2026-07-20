// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// maxSSOTokenUploadBytes caps a single sso-token PUT. awsSSOBlob is a handful
// of short structured fields (not a bounded-but-open-ended facts dump like
// ScanFacts), so this is a generous ceiling against a hostile/misbehaving
// in-sandbox upload rather than a sizing of the real payload.
const maxSSOTokenUploadBytes = 16 << 10 // 16 KiB

// handleUploadSSOToken accepts a PUT /api/v1/internal/sso-token/{runID} from
// wardyn-aws-sso running inside the AWS SSO container-login run (see
// cmd/wardyn-aws-sso and harnesscred.go's captureViaHelper doc). It is the
// structural sibling of handleUploadScanResult/handleUploadVerifyResult:
// run-token auth with the cross-run-pollution guard (claimsForRunUpload), then
// a check against TRUSTED server state — never sandbox input — that the run
// is actually the aws-sso harness-login run before a credential can land.
//
// Unlike scan/verify, this run has no WorkspaceID (it is a login run, not a
// workspace run), so authSandboxRunUpload doesn't fit; the run-kind check
// here is harnessLoginTask + awsSSOAgent instead of a non-nil WorkspaceID.
func (s *Server) handleUploadSSOToken(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Secrets == nil {
		writeError(w, http.StatusServiceUnavailable, "no secret store configured")
		return
	}
	claims, ok := claimsForRunUpload(w, r)
	if !ok {
		return
	}
	run, err := s.cfg.Store.GetRun(r.Context(), claims.RunID)
	if err != nil {
		writeError(w, http.StatusForbidden, "run not found for sso-token upload")
		return
	}
	if run.Task != harnessLoginTask || run.Agent != awsSSOAgent {
		writeError(w, http.StatusForbidden, "run is not an aws sso container-login run")
		return
	}

	raw, ok := readCappedBody(w, r, maxSSOTokenUploadBytes, "sso token")
	if !ok {
		return
	}
	var blob awsSSOBlob
	if jerr := json.Unmarshal(raw, &blob); jerr != nil {
		writeError(w, http.StatusBadRequest, "invalid sso token: "+jerr.Error())
		return
	}
	// Structural guard — the replacement for the Anthropic prefix check (see
	// awsSSOBlob.valid doc). Checked on the CLIENT-supplied fields only, before
	// the server stamps its own provenance below, so a client can never satisfy
	// this by omission.
	if !blob.valid() {
		writeError(w, http.StatusBadRequest, "sso token blob is missing required fields (access_token/start_url/region/expires_at)")
		return
	}
	// Provenance is SERVER-set, never trusted from the client.
	blob.CapturedAt = s.cfg.Now().UTC()
	blob.SourceRunID = claims.RunID.String()

	if err := s.storeAWSSSOBlob(r.Context(), blob); err != nil {
		writeError(w, http.StatusInternalServerError, "store aws sso credential: "+err.Error())
		return
	}

	// Mask the captured token values out of every run's PTY capture/asciicast/
	// decision log process-globally, exactly like the Anthropic paste path
	// (see handleHarnessCredentialPaste). AddGlobal is nil-safe.
	s.cfg.MaskRegistry.AddGlobal([]byte(blob.AccessToken))
	if blob.RefreshToken != "" {
		s.cfg.MaskRegistry.AddGlobal([]byte(blob.RefreshToken))
	}

	s.recordAudit(r.Context(), s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
		"harness.credential.captured", harnessCredSecretName(awsSSOProvider), "success",
		mustJSON(map[string]any{"provider": awsSSOProvider, "source": "helper"})))
	w.WriteHeader(http.StatusNoContent)
}
