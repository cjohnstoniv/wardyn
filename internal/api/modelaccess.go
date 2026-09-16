// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Model-access lifecycle — WHOSE credential, and what state it is in.
//
// Two things live here because they are the same question asked twice. An
// AgentProviders row says whether a deployment's model credential is ONE
// credential for everyone (`shared`, today) or ONE PER PERSON (`per_user`,
// captured by that person's own AWS SSO sign-in), and every lane that reads or
// writes a captured AWS SSO session has to resolve that before it touches the
// secret store — a member served the admin's session is the exact failure
// per_user exists to prevent. That resolution is awsSSOScope.
//
// The other half is the probe. Before this, an expiring credential was
// reportable only as `Expired` (a boolean, after the fact) and the member's
// console read `llm_ready` — a DEPLOYMENT fact — for a per-principal question,
// so a member whose own session had lapsed read a green chip. One vocabulary of
// five states, computed once, is what stops the admin's checklist row and the
// member's chip from disagreeing about the same credential.

// modelAccessAgent is the agent whose model-access lane the per-principal probe
// reports on. bedrock_sso is claude-code's lane alone (resolveBedrockAuth is
// not-ready for any other agent, runs_bedrock.go), so there is exactly one row
// to read and no per-agent fan-out to design.
const modelAccessAgent = "claude-code"

// awsSSOScope names WHOSE captured AWS SSO credential a lane may read or write.
//
// THE ZERO VALUE IS THE OPERATOR NAMESPACE — today's behaviour, and what a
// `shared` row (or legacy open mode, where there is no roster at all) resolves
// to. Every call site that has nothing to say about per-principal credentials
// therefore keeps reading exactly the row it read before.
//
// Under perUser the ONLY admissible credential is owner's OWN row. That is
// stricter than it sounds, and both halves are load-bearing:
//
//   - Store.For(owner).Get FALLS BACK to the operator's ("", name) row
//     (internal/secretstore/pg), so the naive owner-scoped read would serve the
//     admin's session to a member with no capture of their own. The read here
//     uses For(owner).List() first and only Gets a name that list contains.
//   - resolveBedrockAuth's bearer / ~/.aws-mount / static-key arms are all bare
//     operator-namespace reads, so they are SKIPPED under perUser as well — a
//     credential must never silently change source.
//
// An EMPTY owner under perUser is a credential nobody owns: it resolves as
// not-configured, never as the operator's.
type awsSSOScope struct {
	perUser bool
	owner   string
}

// namespaced reports whether this scope names a per-principal namespace a read
// can actually be made in. False under perUser with no owner — the fail-closed
// direction, since the alternative is reading the operator's row.
func (sc awsSSOScope) namespaced() bool { return sc.perUser && sc.owner != "" }

// awsSSOScopeIsMechanism reports whether sc names the shared admin bearer
// token under a per_user row — a MECHANISM, not a person: no credential of
// its own to grade, no sign-in it could complete. LocalMode's own seat
// resolves to a real person-shaped namespace (runIdentitySubject prefers it
// over adminTokenPrincipal, e.g. "local:operator") and is deliberately NOT
// this — the whole reason per_user exists is that a real person signs in for
// themselves, and LocalMode's operator is exactly that person.
func awsSSOScopeIsMechanism(sc awsSSOScope) bool {
	return sc.perUser && sc.owner == adminTokenPrincipal
}

// AdminTokenPrincipal exports adminTokenPrincipal for cmd/wardynd's boot-time
// validation (S-06): a -local-operator value equal to it would collide with
// this mechanism principal, refusing the SAME operator seat at harness-login
// that boot just accepted.
func AdminTokenPrincipal() string { return adminTokenPrincipal }

// awsSSOCredentialSourceLabel is the scope as the audit trail names it — the
// same two wire values the admin wrote on the row, so a row and a log line are
// searchable by one string.
func awsSSOCredentialSourceLabel(sc awsSSOScope) string {
	if sc.perUser {
		return string(types.CredentialSourcePerUser)
	}
	return string(types.CredentialSourceShared)
}

// awsSSOScopeFor resolves the scope from the org's roster: the principal's own
// namespace under an enabled `per_user` row for this agent, the operator
// namespace otherwise (a `shared` row, a disabled row, no row, or no roster).
//
// "NO ROSTER" HERE MEANS A SITE CONFIG THAT WAS READ AND CARRIES NO ROW — never
// one that could not be read. The two are indistinguishable in the VALUE (both
// are the zero SiteConfig), which is exactly why the distinction is made by the
// CALLER and not here: dispatch retries the read once (siteConfigForDispatch)
// and then refuses outright rather than resolve a scope from a read that failed
// (enforceReadableRosterForCredential), and /setup/status fails closed to the
// caller's own namespace (setupStatusSSOScope below). This function is pure over
// the value it is handed and has no way to tell those apart, so it must never be
// handed a value nobody proved they read. Owner decision 3 / B2-F1.
//
// Pure over the site config, so the three call sites that already hold one
// (dispatch, the create-time mechanism gate) spend no extra read.
func awsSSOScopeFor(sc types.SiteConfig, agentID, subject string) awsSSOScope {
	row, ok := agentProviderFor(sc, agentID)
	if !ok || row.Disabled || row.CredentialSource != types.CredentialSourcePerUser {
		return awsSSOScope{}
	}
	return awsSSOScope{perUser: true, owner: subject}
}

// setupStatusSSOScope is the AWS SSO namespace a /setup/status READ answers
// for: the roster's, except that a roster read which FAILED resolves FAIL-
// CLOSED to the caller's own per-user namespace (R-4). awsSSOScopeFor's "no
// row" fallback is the OPERATOR namespace — right at a WRITE door, where an
// unreadable roster must not silently re-point a credential; on this read it
// handed every caller, a member included, the admin's harness row and
// model_access for the duration of a store blip, the one window in which a
// forged capture marker in a member's own login sandbox is corroborated by a
// session that is not theirs (S-13). readAWSSSOBlob answers a per-user scope
// it cannot name with "nothing". storeConfigured keeps a NIL store on the old
// path: that is an install with no roster, not a blip.
func setupStatusSSOScope(sc types.SiteConfig, scOK, storeConfigured bool, subject string) awsSSOScope {
	if storeConfigured && !scOK {
		return awsSSOScope{perUser: true, owner: subject}
	}
	return awsSSOScopeFor(sc, modelAccessAgent, subject)
}

// awsSSOScopeForAgent is awsSSOScopeFor for a caller that does NOT already hold
// a site config. ok=false means THE ROSTER COULD NOT BE READ, and the scope
// returned with it is the old legacy-open answer (the operator namespace) —
// never a scope a caller may write or delete under.
//
// The (scope, ok) pair, rather than the bare scope this used to return, because
// dropping ok is a fail-OPEN at every door that decides WHERE a credential
// lands: a store blip read as "no per_user row" sent a member's capture, and a
// Disconnect's Delete, to the deployment-wide row every run inherits (S2-01,
// S2-08). A read door may still ignore ok — dispatch does, deliberately — but
// it now has to say so.
func (s *Server) awsSSOScopeForAgent(ctx context.Context, agentID, subject string) (awsSSOScope, bool) {
	if s.cfg.Store == nil {
		return awsSSOScope{}, true // no store, no roster: an install with no per-user estate, not a blip
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return awsSSOScope{}, false
	}
	return awsSSOScopeFor(sc, agentID, subject), true
}

// The six model-access states — ONE vocabulary, ordered REGISTRATION-first.
//
// The ordering is the whole design. The SSO access token lives one hour on the
// estate this came from, so a probe keyed on it would make "expiring" the
// permanent resting state and "live" unreachable. What actually decides whether
// a person has to do something is the client REGISTRATION behind the refresh
// token: while that lives, dispatch renews the access token unattended.
const (
	// modelAccessLive: nothing for the person to do. A refresh token is present
	// and the registration is not near lapsing (or carries no expiry at all —
	// see registrationLapsed for why a zero timestamp counts as live).
	// expired_renewable FOLDS IN HERE deliberately: dispatch renews it, so
	// reporting it as a problem would ask for an hourly re-login the product no
	// longer needs.
	modelAccessLive = "live"
	// modelAccessExpiring: the registration lapses within a day, or a blob with
	// NO refresh token (a legacy sso_start_url profile) whose access token does.
	// Carries a deadline in its action line.
	modelAccessExpiring = "expiring"
	// modelAccessExpiredSignin: nothing can heal it — no refresh token, a spent
	// one, or a lapsed registration. The person signs in again.
	modelAccessExpiredSignin = "expired_signin"
	// modelAccessNotConfigured: a per_user deployment where this principal has
	// never captured a session. NOT an error — it is the first-run state.
	modelAccessNotConfigured = "not_configured"
	// modelAccessSharedExpired: a `shared` deployment whose ONE credential is
	// dead (or was never captured). There is nothing the member can do, so the
	// action names the admin rather than offering a button that would fail.
	modelAccessSharedExpired = "shared_expired"
	// modelAccessNotApplicable: the caller is a MECHANISM, not a person — the
	// shared admin bearer token under a per_user row; no credential to grade, no
	// sign-in it could complete, NO action.
	modelAccessNotApplicable = "not_applicable"
)

// modelAccessExpiringWindow is how far ahead of a lapse "expiring" starts. A day
// is the smallest window that still lets somebody act on it during a working
// day; anything shorter is a notice that arrives after the fact.
const modelAccessExpiringWindow = 24 * time.Hour

// The action lines the states ship. DRAFT (M2 canon pending) — the frozen copy
// is docs/design/workspace-providers-prompt.md §7.7
// (MODEL_ACCESS_EXPIRING_ACTION, SIGN_IN_AWS,
// MODEL_ACCESS_SHARED_EXPIRED_ACTION), reproduced BYTE-EXACT here because the
// console renders a server action line verbatim; the chip LABELS are the
// console's own copy module.
const (
	// DRAFT (M2 canon pending)
	modelAccessExpiringAction = "Sign in again before %s"
	// DRAFT (M2 canon pending)
	modelAccessSignInAction = "Sign in to AWS"
	// DRAFT (M2 canon pending)
	modelAccessSharedExpiredAction = "Your admin's model credential expired — ask them to reconnect it"
	// modelAccessPinContradictedAction replaces "Sign in to AWS" for the one
	// expired_signin the timestamp says nothing about: the session is live, and
	// it is the wrong IDENTITY. It names both pairs because the person reading
	// it has to choose an account and a role on the login terminal, and because
	// "sign in again" on its own reads like a transient glitch rather than a
	// deployment rule they are now on the wrong side of. %s = the stored
	// account, role; then the allowed account, role.
	//
	// DRAFT (M2 canon pending)
	modelAccessPinContradictedAction = "Your stored AWS session is for account %s / role %s; this row now allows %s / %s — sign in again."
	// harnessCredentialAWSPinMismatchDetail is the OPERATOR's half of the same
	// fact, on the harness_credential_aws checklist row (members never see the
	// checklist — redactSetupStatusForMember empties it). It exists because the
	// expired_signin arm it shares says "expired at <ts> and cannot be renewed",
	// which of this credential is simply false.
	//
	// DRAFT (M2 canon pending)
	harnessCredentialAWSPinMismatchDetail = "Your captured AWS SSO session names an AWS account and role this agent's roster row no longer allows, so Bedrock runs using it are refused before they start."
)

// harnessLoginMechanismPrincipalRefusal (M2 canon pending) — DRAFT
// (docs/design/workspace-providers-prompt.md §7 shape: a lowercase-opening
// clause naming what was refused, rendered verbatim by the console).
const harnessLoginMechanismPrincipalRefusal = "this deployment gives each person their own AWS sign-in, and the admin token is a shared credential rather than a person — every capture made with it would land in one namespace and overwrite the last. Sign in to the console, or use your own wdn_ API token, and start the sign-in from there"

// refuseHarnessLoginMechanismPrincipal writes the 422 refusal for a per_user
// row reached by the shared admin-bearer-token principal, audits it (S-07:
// the SIBLING refusals in this same function, denyMemberField/
// denyMemberCapability, both audit — this is the one refusal on the
// credential-capture route an operator's own CI job hits with no error
// budget, and a row is how they find out it stopped capturing), and returns
// false so authorizeHarnessLogin can `return types.AgentProvider{}, s.refuse...(w, r)`.
func (s *Server) refuseHarnessLoginMechanismPrincipal(w http.ResponseWriter, r *http.Request) bool {
	writeError(w, http.StatusUnprocessableEntity, harnessLoginMechanismPrincipalRefusal)
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"authz.denied", "setup.harness_login", "denied", mustJSON(map[string]any{"reason": "harness_login_mechanism_principal"})))
	return false
}

// SetupModelAccess is THIS PRINCIPAL's model-access answer: which state their
// own credential is in, the lane it is on, and the one thing to do about it.
//
// It is the field the member's chip reads INSTEAD of llm_ready, which stays a
// deployment fact (does SOME model path exist on this install) and cannot answer
// a per-person question. Redaction-safe by construction: a state name, a wire
// mechanism value and a member-facing sentence — no secret names, no topology,
// no AWS access portal URL. redactSetupStatusForMember therefore KEEPS it.
//
// Absent (a zero State) when there is nothing per-principal to say: no roster
// declares a lane for this agent AND no captured session exists. The console
// then renders today's chip, which is exactly what an install that never touched
// this feature should see.
type SetupModelAccess struct {
	State     string `json:"state"`
	Mechanism string `json:"mechanism,omitempty"`
	// Action is the one thing to do, already composed by the server ("" when
	// there is nothing).
	Action string `json:"action,omitempty"`
	// Deadline is the instant Action names, carried IN-PROCESS only (json:"-")
	// so the admin's checklist row prints the same one the member's action line
	// does. Not a second wire copy of a fact Action already states in the
	// member's own words — the console never re-composes that sentence.
	Deadline string `json:"-"`
	// PinMismatch says this answer's `expired_signin` is a CONTRADICTED identity
	// rather than a lapsed session. IN-PROCESS only (json:"-"): the console
	// renders Action verbatim and needs no second copy of what it says — this
	// exists so the admin-facing checklist row (awsSSOCredentialRow) does not
	// tell an operator their live session "expired at <ts> and cannot be
	// renewed", which of this one credential is simply false.
	PinMismatch bool `json:"-"`
	// PerUser says whether this answer is about a credential THIS PRINCIPAL
	// OWNS. IN-PROCESS only (json:"-"): it is not a fact the console renders, it
	// is what memberModelAccess needs to decide whether a member may be told a
	// deadline or offered a sign-in at all. False is `shared` AND legacy open
	// mode — in both, the graded blob is the OPERATOR's.
	PerUser bool `json:"-"`
}

// memberModelAccess is the member-facing projection of a model-access answer,
// applied by redactSetupStatusForMember.
//
// UNDER `shared` (AND LEGACY OPEN MODE) A MEMBER OWNS NOTHING HERE. The blob
// setupModelAccess graded is the OPERATOR's, so every state that asks the
// reader to act on their own credential — `expiring` with the admin's lapse
// timestamp, `expired_signin`/`not_configured` with "Sign in to AWS" — is both
// a disclosure of the operator's credential lifecycle and an instruction the
// server then refuses (harnessLoginNotPerUserRefusal). Redaction stripped
// SetupHarness.ExpiresAt and then republished an equivalent timestamp one field
// over, which is the exact contradiction the redaction comment calls out about
// secret names.
//
// So for a member under `shared` there are only two things that can be true:
// the admin's credential works (`live` — including while it is expiring, which
// is the admin's problem and is still on the admin's OWN setup row with its
// timestamp), or it does not (`shared_expired`, whose action names the admin
// and carries no timestamp). A deadline or a sign-in action is reserved for a
// principal who owns the credential: the operator, or a member under `per_user`
// (PerUser, untouched here).
func memberModelAccess(ma SetupModelAccess) SetupModelAccess {
	// Fail-safe, not a reachable path today (a real human is never the
	// mechanism principal): without this early return the default arm below
	// would rewrite an unknown state to `live`, which is a worse lie than
	// leaving it alone.
	if ma.State == "" || ma.State == modelAccessNotApplicable || ma.PerUser {
		return ma
	}
	out := SetupModelAccess{State: modelAccessLive, Mechanism: ma.Mechanism}
	switch ma.State {
	case modelAccessSharedExpired, modelAccessExpiredSignin, modelAccessNotConfigured:
		out.State = modelAccessSharedExpired
		// "" for ts: shared_expired's sentence interpolates nothing, and passing
		// the deadline here would be the leak this function exists to close.
		out.Action = modelAccessAction(modelAccessSharedExpired, "")
	}
	return out
}

// awsSSOCredentialState grades one captured AWS SSO session into the vocabulary
// above. perUser decides ONLY which of the two dead states applies: the person
// can repair their own session, nobody but the admin can repair the shared one.
//
// A blob with NO refresh token and MORE than a day of access token left reads
// `live` for the same reason expired_renewable does: there is nothing for the
// person to do YET. On the one-hour-token estate this design comes from, such a
// blob reaches `expiring` within the hour on its own.
func awsSSOCredentialState(blob awsSSOBlob, found, perUser bool, now time.Time) string {
	if !found {
		if perUser {
			return modelAccessNotConfigured
		}
		// Shared with nothing captured is, from a member's seat, the same fact as
		// shared-and-dead: the one credential everybody's runs use is not there.
		return modelAccessSharedExpired
	}
	dead := modelAccessSharedExpired
	if perUser {
		dead = modelAccessExpiredSignin
	}
	switch {
	case blob.renewable(now):
		if blob.registrationLapsed(now.Add(modelAccessExpiringWindow)) {
			return modelAccessExpiring
		}
		return modelAccessLive
	case blob.expired(now):
		return dead
	case !blob.ExpiresAt.After(now.Add(modelAccessExpiringWindow)):
		return modelAccessExpiring
	default:
		return modelAccessLive
	}
}

// modelAccessAction is the one thing to do about a state, "" when there is
// nothing. ts is the deadline the expiring line names (the registration's lapse,
// or the access token's expiry for a blob that cannot be renewed).
func modelAccessAction(state, ts string) string {
	switch state {
	case modelAccessExpiring:
		return fmt.Sprintf(modelAccessExpiringAction, ts)
	case modelAccessExpiredSignin, modelAccessNotConfigured:
		return modelAccessSignInAction
	case modelAccessSharedExpired:
		return modelAccessSharedExpiredAction
	default:
		return ""
	}
}

// modelAccessDeadline is the timestamp the expiring action names: the client
// registration's lapse when the blob can be renewed (that is what actually runs
// out), the access token's own expiry when it cannot.
func modelAccessDeadline(blob awsSSOBlob, found bool, now time.Time) string {
	switch {
	case !found:
		return ""
	case blob.renewable(now) && !blob.RegistrationExpiresAt.IsZero():
		return blob.RegistrationExpiresAt.UTC().Format(time.RFC3339)
	default:
		return blob.ExpiresAt.UTC().Format(time.RFC3339)
	}
}

// setupModelAccess grades the caller's own captured AWS SSO session against the
// org's declared lane. blob/found are the read setupHarnessCreds already made in
// the caller's own namespace — passed in rather than re-read, so the probe and
// the harness row can never be about two different credentials.
//
// oidcConfigured gates the mechanism arm below (S-01/S-02): with no OIDC, the
// admin token IS the only working per_user capture path (there is no console
// sign-in or wdn_ token to redirect to instead — see
// harnessLoginMechanismPrincipalRefusal), so a no-OIDC deployment must keep
// grading it exactly like any other per_user principal.
//
// Returns the zero value when there is nothing per-principal to report: no
// bedrock_sso row and no captured session (see SetupModelAccess).
func setupModelAccess(sc types.SiteConfig, blob awsSSOBlob, found bool, scope awsSSOScope, oidcConfigured bool, now time.Time) SetupModelAccess {
	row, declared := agentProviderFor(sc, modelAccessAgent)
	ssoLane := declared && !row.Disabled && row.Mechanism == types.AgentMechanismBedrockSSO
	// The caller is the shared admin bearer token, not a person: no sign-in it
	// could complete FOR A NEW SESSION. It is NOT an unreadable namespace —
	// "admin-token" is a non-empty owner, read and written like any other
	// (readAWSSSOBlob only refuses an EMPTY owner) — so a session already
	// captured there is real and dispatch still serves it to admin-token-
	// created runs; grade that like any other blob (!found below) rather than
	// hiding it behind a state that claims there is nothing to grade.
	if awsSSOScopeIsMechanism(scope) && !found && oidcConfigured {
		return SetupModelAccess{State: modelAccessNotApplicable, Mechanism: string(types.AgentMechanismBedrockSSO)}
	}
	if !ssoLane && !found {
		return SetupModelAccess{}
	}
	state := awsSSOCredentialState(blob, found, scope.perUser, now)
	deadline := modelAccessDeadline(blob, found, now)
	out := SetupModelAccess{State: state, Deadline: deadline, Action: modelAccessAction(state, deadline), PerUser: scope.perUser}
	// A STORED SESSION THE ROSTER NO LONGER ALLOWS grades expired_signin, which
	// is the one thing that matters here: MODEL_ACCESS_ACTIONABLE
	// (workspace-providers-copy.ts) is what decides whether the console offers
	// "Sign in to AWS" at all, and this session grades `live` on expiry alone —
	// so dispatch and create refuse the person's runs (P4) while the button that
	// would repair it is hidden and they are stranded. Signing in again IS the
	// repair: a new login run stamps the CURRENT pin and its capture overwrites
	// the blob, with no server-side invalidation anywhere.
	//
	// An EXISTING state, deliberately: it is already in the actionable set,
	// already renders SIGN_IN_AWS, and already drives the warn arm of
	// awsSSOCredentialRow, so the per-principal checklist row, the Getting
	// Started chip and the button all move with one switch arm — and nothing on
	// the wire or in the TS mirror changes.
	if found {
		if stored, pinned, mismatch := awsSSOPinContradiction(sc,
			awsSSOPin{AccountID: blob.AccountID, RoleName: blob.RoleName}); mismatch {
			out.State = modelAccessExpiredSignin
			out.Action = fmt.Sprintf(modelAccessPinContradictedAction,
				stored.AccountID, stored.RoleName, pinned.AccountID, pinned.RoleName)
			out.PinMismatch = true
		}
	}
	if ssoLane {
		out.Mechanism = string(row.Mechanism)
	} else {
		// A captured session with no declaration IS the bedrock_sso lane — legacy
		// open mode simply never wrote the row down.
		out.Mechanism = string(types.AgentMechanismBedrockSSO)
	}
	return out
}

// harnessCredentialCheck is the readiness row for a Wardyn-managed subscription
// token — the compose-mode analogue of claudeSubscriptionStagingCheck. It fires
// only when a credential is captured (no capture => the llm_provider check
// already says "connect a model"). Pure: the blob read is done by the caller.
//
// The AWS row reads its status off the SAME five-state grading the member's chip
// does (ma, computed from the same blob by setupModelAccess), so the two
// surfaces cannot disagree about one credential. Only the COPY differs, and it
// has to: an admin can reconnect a shared credential, which is exactly what the
// member's `shared_expired` action line says nobody but they can do.
func harnessCredentialCheck(h SetupHarness, ma SetupModelAccess) (SetupCheck, bool) {
	if !h.Captured {
		return SetupCheck{}, false
	}
	// AWS SSO carries a real expiry, so it gets a truthful row rather than the
	// age heuristic below (which exists only because setup-tokens expose none).
	if h.Provider == awsSSOProvider {
		return awsSSOCredentialRow(h, ma), true
	}
	if h.Aging {
		return SetupCheck{
			ID: "harness_credential", Label: "Managed Claude subscription", Status: "warn",
			Detail: "Your Wardyn-managed Claude subscription token was captured a long time ago (setup-token lives ~1 year). " +
				"It may be close to expiring; a run will fail if Anthropic has revoked it.",
			Fix: "Reconnect via container login on the provider step (Connect via container login → `claude setup-token` → paste).",
		}, true
	}
	return SetupCheck{
		ID: "harness_credential", Label: "Managed Claude subscription", Status: "ok",
		Detail: "A Wardyn-managed Claude subscription token is connected and injected proxy-side into every run — the " +
			"sandbox holds only an inert sentinel. Works in compose mode with no host ~/.claude.",
	}, true
}

// awsSSOCredentialRow is the captured-AWS-SSO checklist row, one arm per state.
//
// `live` covers expired-but-RENEWABLE: the credential renews itself at dispatch
// while its refresh token lives (awssso_refresh.go), so there is nothing for the
// operator to do and a warn row would ask for an hourly re-login the product no
// longer needs. Re-login is the answer only where renewal CANNOT happen.
func awsSSOCredentialRow(h SetupHarness, ma SetupModelAccess) SetupCheck {
	row := SetupCheck{ID: "harness_credential_aws", Label: "AWS SSO session", Status: "ok"}
	switch ma.State {
	case modelAccessExpiring:
		// ma.Deadline, NOT h.ExpiresAt. The access token lives about an hour and
		// the control plane renews it unattended; what actually runs out is the
		// OIDC client registration behind the refresh token (or, for a blob with
		// no refresh token, the access token — for which the two are the same
		// instant). Printing h.ExpiresAt here made this row disagree with the
		// member's own action line about one credential, and say something false:
		// the session keeps renewing past that timestamp.
		row.Status = "warn"
		row.Detail = "Your captured AWS SSO session stops being renewable at " + ma.Deadline +
			" — after that a Bedrock run using it fails until someone signs in again."
		row.Fix = "Re-run the containerized AWS SSO login on the provider step."
	case modelAccessExpiredSignin, modelAccessSharedExpired, modelAccessNotConfigured:
		row.Status = "warn"
		if ma.PinMismatch {
			// NOT "expired … and cannot be renewed": this session is live and
			// renewable, and the row would be saying something false about the one
			// credential it is describing. The member's own action line already
			// names both pairs; the row says the same fact in the operator's words.
			row.Detail = harnessCredentialAWSPinMismatchDetail
			row.Fix = ma.Action
			return row
		}
		row.Detail = "Your captured AWS SSO session expired at " + h.ExpiresAt +
			" and cannot be renewed (no refresh token, or its client registration lapsed), so Bedrock runs using it will fail."
		row.Fix = "Re-run the containerized AWS SSO login on the provider step."
	case modelAccessLive:
		if h.Expired {
			row.Detail = fmt.Sprintf(harnessCredentialAWSRenewingDetail, h.ExpiresAt)
			row.Fix = harnessCredentialAWSRenewingFix
			return row
		}
		row.Detail = "A captured AWS SSO session is connected (expires " + h.ExpiresAt +
			"). Bedrock runs exchange it for short-lived role credentials — no host ~/.aws mount and no static keys."
	default:
		// No grading reached this row (a blob the probe could not see). Say the
		// honest thing rather than claiming either direction.
		row.Detail = "A captured AWS SSO session is connected (expires " + h.ExpiresAt + ")."
	}
	return row
}
