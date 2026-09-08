// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
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
// structural sibling of handleUploadScanResult: run-token auth with the
// cross-run-pollution guard (claimsForRunUpload), then a check against
// TRUSTED server state — never sandbox input — that the run is actually the
// aws-sso harness-login run before a credential can land.
//
// That covers WHICH run may write. WHAT it writes is bound to trusted server
// state too (F006): the blob's region and start_url must equal the operator's
// own boot config and the access-portal URL this run was launched with, and a
// run may capture only once. See the block below.
//
// Unlike scan, this run has no WorkspaceID (it is a login run, not a
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
	// Defense in depth (W15-d): this blob is persisted once and then baked
	// VERBATIM, unescaped, into every later Bedrock run's ~/.aws/config INI
	// (awsSSOConfigFileContents, runs_bedrock.go) — a single bad capture would
	// poison every subsequent run that credential mode serves, not just this
	// one. StartURL gets the same https-URL-no-whitespace guard the operator's
	// own pre-login input takes (validateSSOStartURL, harnesscred.go); the INI
	// template's other three interpolated fields (sso_account_id/sso_role_name/
	// sso_region) get the same control-character guard run.Repo already takes for
	// the identical reason (repoFieldSafe, runs_scm.go) — a newline in any could
	// otherwise smuggle extra keys/sections into the generated file. Region is
	// especially load-bearing: sso_region is written AFTER sso_start_url in the
	// [sso-session] block, so an injected duplicate sso_start_url via Region would
	// win under last-key-wins parsing and defeat the StartURL guard above.
	if verr := validateSSOStartURL(blob.StartURL); verr != nil {
		writeError(w, http.StatusBadRequest, "invalid sso token: "+verr.Error())
		return
	}
	if !repoFieldSafe(blob.AccountID) {
		writeError(w, http.StatusBadRequest, "invalid sso token: account_id contains control characters or whitespace")
		return
	}
	if !repoFieldSafe(blob.RoleName) {
		writeError(w, http.StatusBadRequest, "invalid sso token: role_name contains control characters or whitespace")
		return
	}
	if !repoFieldSafe(blob.Region) {
		writeError(w, http.StatusBadRequest, "invalid sso token: region contains control characters or whitespace")
		return
	}
	// F006 — bind WHAT is uploaded to WHAT THE OPERATOR ASKED FOR, not merely
	// to WHICH run may upload. Everything above authenticates the WRITER
	// (claimsForRunUpload + run.Task/run.Agent) and shape-checks the CONTENT;
	// none of it stops code running INSIDE the login sandbox (the vendor CLI
	// image, a fetched dependency) from PUTting a structurally perfect blob that
	// names an ATTACKER's IdP, region, account and role. That blob would land
	// under the OPERATOR-WIDE reserved harness name (storeAWSSSOBlob) and
	// resolveBedrockAuth selects it AHEAD of the host ~/.aws mount and the
	// static-key lanes for every LATER Bedrock run — baking the attacker's
	// start_url/account/role into that run's ~/.aws/config and appending the
	// attacker region's oidc./portal.sso. hosts to its egress allowlist
	// (ssoEgressHosts, runs_bedrock.go). The server already HOLDS both operator
	// values that pin this down, so the binding check needs no new trust source:
	//   region    — the same cmp.Or(BedrockAWSSSORegion, BedrockRegion) boot
	//               config launchHarnessLoginRun seeded this sandbox with, which
	//               handleHarnessLogin REQUIRES to be non-empty before an aws-sso
	//               login run may launch at all;
	//   start_url — the operator's own request value, read back from THIS run's
	//               harness.login.started audit event (loginRunSSOStartURL).
	// Both comparisons are exact and fail closed: an empty expectation (region
	// unset after a restart, audit row unreadable) can never equal a blob field
	// that valid() has already proven non-empty, so the upload is refused rather
	// than silently unbound.
	wantRegion := cmp.Or(s.cfg.BedrockAWSSSORegion, s.cfg.BedrockRegion)
	if blob.Region != wantRegion {
		writeError(w, http.StatusBadRequest,
			"sso token region does not match the AWS SSO region this login run was launched with")
		return
	}
	wantStartURL, aerr := s.loginRunSSOStartURL(r.Context(), claims.RunID)
	if aerr != nil {
		writeError(w, http.StatusInternalServerError, "verify sso token against login run: "+aerr.Error())
		return
	}
	if blob.StartURL != wantStartURL {
		writeError(w, http.StatusBadRequest,
			"sso token start_url does not match the AWS access portal URL this login run was launched with")
		return
	}
	// Once only. Even a correctly-bound blob must not be replaceable: the login
	// run stays alive until its idle auto-stop, so without this the sandbox can
	// overwrite the operator's genuine capture (same start_url/region, the
	// attacker's access_token/account/role) at any point afterwards. The stored
	// blob carries the SERVER-stamped SourceRunID below, so "this run already
	// captured" is answerable from the credential itself.
	if prev, found, rerr := s.readAWSSSOBlob(r.Context()); rerr != nil {
		writeError(w, http.StatusInternalServerError, "read existing aws sso credential: "+rerr.Error())
		return
	} else if found && prev.SourceRunID == claims.RunID.String() {
		writeError(w, http.StatusConflict, "this login run has already captured an aws sso credential")
		return
	}

	// Provenance is SERVER-set, never trusted from the client.
	blob.CapturedAt = s.cfg.Now().UTC()
	blob.SourceRunID = claims.RunID.String()

	if err := s.storeAWSSSOBlob(r.Context(), blob); err != nil {
		writeError(w, http.StatusInternalServerError, "store aws sso credential: "+err.Error())
		return
	}

	// Mask the captured token values out of THIS run's PTY capture/decision log.
	// PER-RUN, not AddGlobal (F007): these bytes are sandbox input — the guards
	// above bind WHOSE IdP and WHICH run, never what the token itself contains —
	// and secretmask's global corpus is applied to every run's output AND, via
	// cmd/wardynd's maskingRecorder, to the Data/Target of every persisted audit
	// event including run-less rows (Snapshot(uuid.Nil)), while Evict clears
	// per-run entries only. Registering sandbox-chosen bytes there let a process
	// inside the vendor login image pick strings that redact every operator's
	// logs — "connection refused", an IP:port — for the life of the daemon.
	// The credential itself loses NOTHING: resolveBedrockAuth AddGlobals the
	// stored blob's access/refresh/client-secret on every dispatch that actually
	// selects this credential (runs_bedrock.go), so the global registration
	// follows the SERVER's decision to use the credential rather than the
	// sandbox's decision to name a string. Add is nil-safe.
	s.cfg.MaskRegistry.Add(claims.RunID, []byte(blob.AccessToken))
	if blob.RefreshToken != "" {
		s.cfg.MaskRegistry.Add(claims.RunID, []byte(blob.RefreshToken))
	}

	s.recordAudit(r.Context(), s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
		"harness.credential.captured", harnessCredSecretName(awsSSOProvider), "success",
		mustJSON(map[string]any{"provider": awsSSOProvider, "source": "helper"})))
	w.WriteHeader(http.StatusNoContent)
}
