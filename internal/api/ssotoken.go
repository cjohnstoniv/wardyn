// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// maxSSOTokenUploadBytes caps a single sso-token PUT. awsSSOBlob is a handful
// of short structured fields (not a bounded-but-open-ended facts dump like
// ScanFacts), so this is a generous ceiling against a hostile/misbehaving
// in-sandbox upload rather than a sizing of the real payload.
const maxSSOTokenUploadBytes = 16 << 10 // 16 KiB

// ── DRAFT (M2 canon pending) ────────────────────────────────────────────────

// ssoTokenUnstampedScopeRefusal answers a login run that carries no launch-time
// credential-scope stamp on a deployment whose roster now reads `per_user`.
// Such a run was launched before the stamp existed, so the server cannot prove
// whose namespace it was authorized to write — and under `per_user` the wrong
// answer is the operator-wide credential every run inherits. Refused rather
// than guessed; the person signs in again and the new run carries a stamp.
//
// DRAFT (M2 canon pending)
const ssoTokenUnstampedScopeRefusal = "this sign-in started before Wardyn recorded whose model credential it was for, " +
	"and this deployment now gives each person their own — start the sign-in again"

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
		s.refuseCapture(w, r, claims, http.StatusBadRequest, refuseReasonBlobShape,
			"invalid sso token: "+jerr.Error(), nil)
		return
	}
	// The LAUNCH-TIME record of what this run was authorized to capture: the
	// operator's access-portal URL, the credential scope, and (0.7.3) the
	// roster's account/role pin. Read back off this run's OWN audit row rather
	// than re-resolved from the live roster — see loginRunStamp.
	stamp, aerr := s.loginRunStamp(r.Context(), claims.RunID)
	if aerr != nil {
		s.refuseCapture(w, r, claims, http.StatusInternalServerError, refuseReasonStampUnreadable,
			"verify sso token against login run: "+aerr.Error(), nil)
		return
	}
	if msg, reason := s.bindSSOBlob(blob, stamp); msg != "" {
		s.refuseCapture(w, r, claims, http.StatusBadRequest, reason, msg, nil)
		return
	}

	// WHOSE credential this is — DECIDED AT LAUNCH, read back here, never
	// recomputed from the live roster. The roster says whether this deployment
	// keeps ONE model credential for everyone (`shared`, today) or one per person
	// (`per_user`), and the namespace is the login run's own identity subject
	// (runIdentitySubject at launch == claims.Sub here, minted from the principal
	// humanOrAdminAuth injected) — trusted server state, never anything the
	// sandbox said. Every read and write below goes through it.
	//
	// WHY THE STAMP AND NOT A FRESH RESOLUTION. This handler's other two bindings
	// (region, start_url) are launch-time state; the scope was not, and a login
	// run stays alive to harnessLoginIdleCap. An admin flipping the row from
	// `per_user` to `shared` inside that window turned the member's still-running
	// sandbox's PUT into a write of the operator-wide reserved harness name — the
	// one credential every later Bedrock run inherits, with an account_id and
	// role_name the blob is free to name (repoFieldSafe only). The file's promise
	// that "a member's capture can never overwrite the operator's" did not hold
	// across a roster edit; reading the launch-time stamp is what makes it hold.
	scope, ok := s.loginRunScope(r.Context(), stamp, claims.Sub)
	if !ok {
		// No stamp, on a deployment whose row now reads `per_user`: unprovable, so
		// refused. See ssoTokenUnstampedScopeRefusal.
		s.refuseCapture(w, r, claims, http.StatusConflict, refuseReasonUnstampedScope, ssoTokenUnstampedScopeRefusal, nil)
		return
	}

	// SERIALISED PER SCOPE, because the once-only guard below is a read-then-put
	// (V1-r2-lensS #4): two concurrent PUTs from the same login sandbox both read
	// "not captured yet" and both stored, last write winning, so the guard held
	// only against a SEQUENTIAL second capture. This is the refresher's own
	// per-owner single-flight lock (awssso_refresh.go), keyed the same way — the
	// scope IS the secret-store namespace both paths read-modify-write, so an
	// upload racing a dispatch-time renewal of the same credential is serialised
	// too, and two different people's uploads still never wait on each other.
	// Held through the store so the check and the write are one critical section.
	unlock := s.lockAWSSSOOwner(scope.owner)
	defer unlock()

	// Once only. Even a correctly-bound blob must not be replaceable: the login
	// run stays alive until its idle auto-stop, so without this the sandbox can
	// overwrite the operator's genuine capture (same start_url/region, the
	// attacker's access_token/account/role) at any point afterwards. The stored
	// blob carries the SERVER-stamped SourceRunID below, so "this run already
	// captured" is answerable from the credential itself.
	//
	// READ THROUGH THE SAME SCOPE AS THE WRITE. The operator-wide read this used
	// to make is not the blob this run is about under per_user: prev would be the
	// ADMIN's capture, its SourceRunID would never equal this run's, and the
	// member's own login sandbox could PUT over its own genuine capture as often
	// as it liked — the exact overwrite this guard exists to refuse, reopened by
	// reading the wrong namespace.
	if prev, found, rerr := s.readAWSSSOBlob(r.Context(), scope); rerr != nil {
		s.refuseCapture(w, r, claims, http.StatusInternalServerError, refuseReasonStoreError,
			"read existing aws sso credential: "+rerr.Error(), &scope)
		return
	} else if found && prev.SourceRunID == claims.RunID.String() {
		s.refuseCapture(w, r, claims, http.StatusConflict, refuseReasonAlreadyCaptured,
			"this login run has already captured an aws sso credential", &scope)
		return
	}

	// Provenance is SERVER-set, never trusted from the client.
	blob.CapturedAt = s.cfg.Now().UTC()
	blob.SourceRunID = claims.RunID.String()

	if err := s.storeAWSSSOBlob(r.Context(), scope, blob); err != nil {
		// Audited like every other refusal on this route: the provenance is
		// already stamped but NOTHING is persisted, so "the capture did not
		// land" is the honest reading, and a failed persist is exactly the
		// event an operator wants beside the rest rather than only in a 500.
		s.refuseCapture(w, r, claims, http.StatusInternalServerError, refuseReasonStoreError,
			"store aws sso credential: "+err.Error(), &scope)
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
		mustJSON(map[string]any{
			"provider": awsSSOProvider, "source": "helper",
			// owner + credential_source say WHOSE credential landed: "" / "shared" is
			// the one every run uses, a subject / "per_user" is one person's. Without
			// the pair a per_user estate's capture rows are indistinguishable from
			// each other, and "who signed in" is the first question after an incident.
			"owner": scope.owner, "credential_source": awsSSOCredentialSourceLabel(scope),
		})))
	w.WriteHeader(http.StatusNoContent)
}

// bindSSOBlob binds WHAT is uploaded to WHAT THE OPERATOR ASKED FOR, and is the
// whole of this handler's content check. Extracted from the handler body so
// handleUploadSSOToken stays a readable sequence of authorize / bind / store
// (and under the cyclomatic cap as the bindings grew from three to five).
//
// Returns ("", "") to accept; (sentence, reason) to refuse, where reason is
// from the fixed vocabulary in awssso_pin.go. Every check fails CLOSED: an
// empty expectation (region unset after a restart, an unreadable audit row)
// can never equal a blob field valid() has already proven non-empty, so the
// upload is refused rather than silently unbound.
//
// Everything AHEAD of this call authenticates the WRITER (claimsForRunUpload +
// run.Task/run.Agent against trusted server state); none of it stops code
// running INSIDE the login sandbox — the vendor CLI image, a fetched
// dependency, or simply a portal that listed somebody else's account first —
// from PUTting a structurally perfect blob naming an IdP, region, account and
// role nobody asked for. That blob would land under the reserved harness name
// and resolveBedrockAuth would select it AHEAD of the host ~/.aws mount and the
// static-key lanes for every LATER Bedrock run (runs_bedrock.go), baking its
// start_url/account/role into that run's ~/.aws/config and appending its
// region's oidc./portal.sso. hosts to the egress allowlist.
func (s *Server) bindSSOBlob(blob awsSSOBlob, stamp loginRunStamp) (msg, reason string) {
	// Structural guard — the replacement for the Anthropic prefix check (see
	// awsSSOBlob.valid doc). Checked on the CLIENT-supplied fields only, before
	// the server stamps its own provenance, so a client can never satisfy this
	// by omission.
	if missing := blob.missingFields(); len(missing) > 0 {
		return "sso token blob is missing required fields (" + strings.Join(missing, ", ") + ")", refuseReasonBlobShape
	}
	// Defense in depth (W15-d): this blob is persisted once and then baked
	// VERBATIM, unescaped, into every later Bedrock run's ~/.aws/config INI
	// (awsSSOConfigFileContents). StartURL gets the same https-URL-no-whitespace
	// guard the operator's own pre-login input takes (validateSSOStartURL); the
	// INI template's other three interpolated fields get the same
	// control-character guard run.Repo already takes for the identical reason
	// (repoFieldSafe, runs_scm.go) — a newline in any could otherwise smuggle
	// extra keys/sections into the generated file. Region is especially
	// load-bearing: sso_region is written AFTER sso_start_url in the
	// [sso-session] block, so an injected duplicate sso_start_url via Region
	// would win under last-key-wins parsing and defeat the StartURL guard.
	if verr := validateSSOStartURL(blob.StartURL); verr != nil {
		return "invalid sso token: " + verr.Error(), refuseReasonFieldUnsafe
	}
	for field, value := range map[string]string{
		"account_id": blob.AccountID, "role_name": blob.RoleName, "region": blob.Region,
	} {
		if !repoFieldSafe(value) {
			return "invalid sso token: " + field + " contains control characters or whitespace", refuseReasonFieldUnsafe
		}
	}
	// F006 — the two operator values the server already HOLDS, so the binding
	// needs no new trust source: the region is the same
	// cmp.Or(BedrockAWSSSORegion, BedrockRegion) boot config this sandbox was
	// launched with, and the start URL is the operator's own request value read
	// back off THIS run's harness.login.started row.
	if blob.Region != cmp.Or(s.cfg.BedrockAWSSSORegion, s.cfg.BedrockRegion) {
		return "sso token region does not match the AWS SSO region this login run was launched with", refuseReasonRegionMismatch
	}
	if blob.StartURL != stamp.SSOStartURL {
		return "sso token start_url does not match the AWS access portal URL this login run was launched with", refuseReasonStartURLMismatch
	}
	// Finding 1: WHICH account and role, not merely which portal.
	return bindCaptureToPin(blob, stamp, s.cfg.BedrockModel)
}

// missingFields names the fields valid() requires and this blob does not carry.
//
// The refusal sentence names THEM rather than a fixed list, because the list
// drifted: valid() also requires account_id and role_name, and since 0.7.3 the
// commonest capture failure is pickAccountRole coming back blank on a portal
// miss — so the person reading their login terminal was told four field names,
// none of which was the missing one.
func (b awsSSOBlob) missingFields() []string {
	var missing []string
	for _, f := range []struct {
		name  string
		empty bool
	}{
		{"access_token", b.AccessToken == ""},
		{"start_url", b.StartURL == ""},
		{"region", b.Region == ""},
		{"expires_at", b.ExpiresAt.IsZero()},
		{"account_id", b.AccountID == ""},
		{"role_name", b.RoleName == ""},
	} {
		if f.empty {
			missing = append(missing, f.name)
		}
	}
	return missing
}

// loginRunScope turns a login run's launch-time stamp into the scope its upload
// may write under. ok=false means "refuse": there is no stamp AND the roster
// now reads `per_user`, so the launch-time answer is unknowable and the only
// fallback available (the operator namespace) is precisely the wrong one.
//
// THE UNSTAMPED ARM IS THE OPERATOR ARM. authorizeHarnessLogin lets nobody but
// an operator launch a login run unless the row is `per_user`, so an unstamped
// run on a roster that does not read `per_user` today is an operator's — the
// same For("") this handler has always used, unchanged. If the row DOES read
// `per_user`, that proof is gone and the run is refused.
//
// ponytail: the residual is a run launched under `per_user` BEFORE this commit
// whose row was flipped to `shared` before it uploaded — unprovable either way,
// and it needs a pre-upgrade run still alive across the wardynd restart that
// deployed this code. Every run launched from here on carries a stamp.
func (s *Server) loginRunScope(ctx context.Context, stamp loginRunStamp, subject string) (awsSSOScope, bool) {
	switch stamp.CredentialSource {
	case string(types.CredentialSourcePerUser):
		// The launch-time owner, not the live roster's answer. Empty is fail-closed
		// on its own: storeAWSSSOBlob refuses a per-user blob with no owner.
		return awsSSOScope{perUser: true, owner: cmp.Or(stamp.Owner, subject)}, true
	case string(types.CredentialSourceShared):
		return awsSSOScope{}, true
	default:
		// A roster read that FAILED cannot prove the operator arm either, so it
		// refuses rather than falling through to the unscoped write (S2-08's
		// family: ok is the fail-closed half of awsSSOScopeForAgent).
		scope, ok := s.awsSSOScopeForAgent(ctx, modelAccessAgent, subject)
		return awsSSOScope{}, ok && !scope.perUser
	}
}
