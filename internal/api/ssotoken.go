// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// maxSSOTokenUploadBytes caps a single sso-token PUT. awsSSOBlob is a handful
// of short structured fields (not a bounded-but-open-ended facts dump like
// ScanFacts), so this is a generous ceiling against a hostile/misbehaving
// in-sandbox upload rather than a sizing of the real payload.
const maxSSOTokenUploadBytes = 16 << 10 // 16 KiB

// DRAFT (M2 canon pending)

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

// ssoTokenRunKilledRefusal answers a login run that has been KILLED: its own
// Cancel, or the person's NEXT sign-in superseding it (one live sign-in sandbox
// per person, harnesscred_supersede.go). The sentence is read off a terminal
// inside that sandbox by whoever is still looking at it, so it says which
// attempt won rather than blaming this one — CONDITIONALLY: after a
// Cancel, or the pane's own post-capture kill, there is no newer sandbox to be
// sent to, and a sentence that assumes one sends the reader looking for it.
//
// DRAFT (M2 canon pending)
const ssoTokenRunKilledRefusal = "this sign-in sandbox was closed — a newer sign-in for you replaced it, " +
	"or it was cancelled; if you started a newer sign-in, finish it there"

// handleUploadSSOToken accepts a PUT /api/v1/internal/sso-token/{runID} from
// wardyn-aws-sso running inside the AWS SSO container-login run (see
// cmd/wardyn-aws-sso and harnesscred.go's captureViaHelper doc). It is the
// structural sibling of handleUploadScanResult: run-token auth with the
// cross-run-pollution guard (claimsForRunUpload), then a check against
// TRUSTED server state — never sandbox input — that the run is actually the
// aws-sso harness-login run before a credential can land.
//
// That covers WHICH run may write. WHAT it writes is bound to trusted server
// state too: the blob's region and start_url must equal the operator's
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
		// 403, not 404, for a not-found run: same reason as refuseTerminalRun —
		// claims.RunID comes from the presented run token, not a path parameter,
		// so a run this store cannot find is that token's own authority gone.
		// This branch does not split out store.ErrNotFound, so any other store
		// failure currently answers the same 403.
		writeError(w, http.StatusForbidden, "run not found for sso-token upload")
		return
	}
	if run.Task != harnessLoginTask || run.Agent != awsSSOAgent {
		writeError(w, http.StatusForbidden, "run is not an aws sso container-login run")
		return
	}
	// A KILLED run may still reach this route, and that is the belt the supersede
	// needs. Killing a run revokes its identity and token verification fails
	// closed on a revoked run (internal/identity/embedded.go) — but RevokeRun is
	// best-effort (a failed revoke is reported, not retried forever), and
	// /internal/sso-token/ is one of the routes a TERMINAL run is deliberately
	// allowed to use for five minutes after it ends (internal_live_run.go), so
	// revocation would otherwise be the ONLY thing standing between a superseded
	// sandbox's late upload and the capture the person just made in the new one.
	//
	// Safe against the honest paths: confirmCapture kills only AFTER the upload
	// (harness-login-pane.tsx) and Cancel wants no upload at all. Only
	// KILLED — a COMPLETED/FAILED/STOPPED login run is not a run something else
	// deliberately ended, and refusing those would be a new rule about a state
	// this lane has never produced.
	if run.State == types.RunKilled {
		s.refuseCapture(w, r, claims, http.StatusConflict, refuseReasonRunKilled, ssoTokenRunKilledRefusal, nil)
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
	// operator's access-portal URL, the credential scope, and the
	// roster's account/role pin. Read back off this run's OWN audit row rather
	// than re-resolved from the live roster — see loginRunStamp.
	stamp, aerr := s.loginRunStamp(r.Context(), claims.RunID)
	if aerr != nil {
		s.refuseCapture(w, r, claims, http.StatusInternalServerError, refuseReasonStampUnreadable,
			loggedMsg(r.Context(), "verify sso token against login run", aerr), nil)
		return
	}
	if msg, reason := s.bindSSOBlob(blob, stamp); msg != "" {
		s.refuseCapture(w, r, claims, http.StatusBadRequest, reason, msg, nil)
		return
	}

	// WHOSE credential this is — Decided at launch, read back here, never
	// recomputed from the live roster. The roster says whether this deployment
	// keeps ONE model credential for everyone (`shared`, today) or one per person
	// (`per_user`), and the namespace is the login run's own identity subject
	// (runIdentitySubject at launch == claims.Sub here, minted from the principal
	// humanOrAdminAuth injected) — trusted server state, never anything the
	// sandbox said. Every read and write below goes through it.
	//
	// Why the stamp and not a fresh resolution. This handler's other two bindings
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

	// The SAME per-person lock a sign-in launch takes (lockLoginSupersede), keyed
	// on this login run's CREATOR off the GetRun at the top of this handler —
	// because the read-modify-write below is the other half of the race: this
	// capture and the person's next sign-in's supersede pass are two requests
	// minutes apart, and unserialized they interleave into the stored credential.
	// Taken FIRST, before the per-scope mutex below, and that order is fixed:
	// creator key, then scope key, everywhere both are held. Inverting it here
	// would be the only place in the tree that did, which is how a deadlock gets
	// written. Fails open exactly as the launch's does.
	releaseLoginLock := s.lockLoginSupersede(r.Context(), run.CreatedBy)
	defer releaseLoginLock()

	// Serialised per scope, because the once-only guard below is a read-then-put
	// (a read-then-put race): two concurrent PUTs from the same login sandbox both read
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
	// Read through the same scope as the write. The operator-wide read this used
	// to make is not the blob this run is about under per_user: prev would be the
	// ADMIN's capture, its SourceRunID would never equal this run's, and the
	// member's own login sandbox could PUT over its own genuine capture as often
	// as it liked — the exact overwrite this guard exists to refuse, reopened by
	// reading the wrong namespace.
	if prev, found, rerr := s.readAWSSSOBlob(secretstore.WithPurpose(r.Context(), secretstore.PurposeStatus), scope); rerr != nil {
		s.refuseCapture(w, r, claims, http.StatusInternalServerError, refuseReasonStoreError,
			loggedMsg(r.Context(), "read existing aws sso credential", rerr), &scope)
		return
	} else if found && prev.SourceRunID == claims.RunID.String() {
		s.refuseCapture(w, r, claims, http.StatusConflict, refuseReasonAlreadyCaptured,
			"this login run has already captured an aws sso credential", &scope)
		return
	}

	// The run state again, re-read inside the lock — the KILLED check at the top
	// of this handler is a check-then-use with everything between it and the
	// write in the window: the body read, the stamp read, the bind, the scope
	// resolve, the lock wait and the once-only read. That is easily long enough
	// for the person's OWN next sign-in to supersede this run, and this upload
	// would then store AFTER the new sandbox's — the late-capture ordering the
	// supersede exists to prevent, arriving through the one door that had already
	// been checked. Same reason as the once-only guard beside it: a read-then-put
	// is only a guard if the read is inside the critical section.
	//
	// One GetRun, and the last thing before the write. It cannot be exact — the
	// kill takes no lock of ours — but it narrows a window measured in reads and
	// a lock wait to the two statements below.
	//
	// Fail closed on an unreadable run, with store_error beside the other two
	// arms decided in here: a capture whose owning run cannot be read is a
	// capture nobody can say is still wanted, and the sandbox's remedy (sign in
	// again) is the same either way.
	if live, rerr := s.cfg.Store.GetRun(r.Context(), claims.RunID); rerr != nil {
		s.refuseCapture(w, r, claims, http.StatusInternalServerError, refuseReasonStoreError,
			loggedMsg(r.Context(), "re-read login run before storing aws sso credential", rerr), &scope)
		return
	} else if live.State == types.RunKilled {
		s.refuseCapture(w, r, claims, http.StatusConflict, refuseReasonRunKilled, ssoTokenRunKilledRefusal, &scope)
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
			loggedMsg(r.Context(), "store aws sso credential", err), &scope)
		return
	}

	// Mask the captured token values out of THIS run's PTY capture/decision log.
	// PER-RUN, not AddGlobal: these bytes are sandbox input — the guards
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

	// A sign-in answers any held run. Every PENDING credential_reauth
	// this capture satisfies moves to APPROVED, so the sidecar holding that run's
	// model call wakes on its next poll instead of running out its budget.
	//
	// After the captured emit above, never before: invariant I7's chain is
	// captured -> resolved -> retry, and a resolution recorded ahead of the
	// capture that justifies it is a credential-bearing retry with no auditable
	// predecessor. Best-effort and never fatal — the credential is already
	// stored, and an unresolved row is repaired by the idempotent
	// reconcile-on-read rather than by asking the person to sign in twice.
	if live, rerr := s.cfg.Store.GetRun(r.Context(), claims.RunID); rerr == nil {
		s.resolvePendingReauth(r.Context(), scope, claims.Sub, live)
	}
	w.WriteHeader(http.StatusNoContent)
}

// bindSSOBlob binds WHAT is uploaded to what the operator asked for, and is the
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
	// Defense in depth: this blob is persisted once and then baked
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
	// Shape, not merely safety. The ROSTER-SAVE door has always held the
	// admin's pin to `^\d{12}$` and IAM's own role grammar (validateAgentSSOPin);
	// this door took anything without a control character. On the unpinned/bare-id
	// shape nothing else looks at these two at all, so a wrong-shaped identity was
	// stored and baked into every later run's ~/.aws/config, to be discovered as
	// somebody's 403. One rule, both doors — the file's own argument.
	if !awsAccountID.MatchString(blob.AccountID) {
		return ssoTokenAccountShapeRefusal, refuseReasonFieldShape
	}
	if !iamRoleName.MatchString(blob.RoleName) {
		return ssoTokenRoleShapeRefusal, refuseReasonFieldShape
	}
	// The two operator values the server already HOLDS, so the binding
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
	// Which account and role, not merely which portal.
	return bindCaptureToPin(blob, stamp, s.cfg.BedrockModel)
}

// missingFields names the fields valid() requires and this blob does not carry.
//
// The refusal sentence names THEM rather than a fixed list, because the list
// drifted: valid() also requires account_id and role_name, and the
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
// The unstamped arm is the operator arm. authorizeHarnessLogin lets nobody but
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
		// refuses rather than falling through to the unscoped write (ok is the
		// fail-closed half of awsSSOScopeForAgent).
		scope, ok := s.awsSSOScopeForAgent(ctx, modelAccessAgent, subject)
		return awsSSOScope{}, ok && !scope.perUser
	}
}
