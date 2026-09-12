// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
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
// Pure over the site config, so the three call sites that already hold one
// (dispatch, the create-time mechanism gate) spend no extra read.
func awsSSOScopeFor(sc types.SiteConfig, agentID, subject string) awsSSOScope {
	row, ok := agentProviderFor(sc, agentID)
	if !ok || row.Disabled || row.CredentialSource != types.CredentialSourcePerUser {
		return awsSSOScope{}
	}
	return awsSSOScope{perUser: true, owner: subject}
}

// awsSSOScopeForAgent is awsSSOScopeFor for a caller that does NOT already hold
// a site config. A read failure resolves to the operator namespace — legacy open
// mode, the same direction every other roster consumer takes on an outage: an
// unreadable roster must not silently re-point a credential.
func (s *Server) awsSSOScopeForAgent(ctx context.Context, agentID, subject string) awsSSOScope {
	if s.cfg.Store == nil {
		return awsSSOScope{}
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return awsSSOScope{}
	}
	return awsSSOScopeFor(sc, agentID, subject)
}

// The five model-access states — ONE vocabulary, ordered REGISTRATION-first.
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
)

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
// Returns the zero value when there is nothing per-principal to report: no
// bedrock_sso row and no captured session (see SetupModelAccess).
func setupModelAccess(sc types.SiteConfig, blob awsSSOBlob, found bool, scope awsSSOScope, now time.Time) SetupModelAccess {
	row, declared := agentProviderFor(sc, modelAccessAgent)
	ssoLane := declared && !row.Disabled && row.Mechanism == types.AgentMechanismBedrockSSO
	if !ssoLane && !found {
		return SetupModelAccess{}
	}
	state := awsSSOCredentialState(blob, found, scope.perUser, now)
	deadline := modelAccessDeadline(blob, found, now)
	out := SetupModelAccess{State: state, Deadline: deadline, Action: modelAccessAction(state, deadline)}
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
