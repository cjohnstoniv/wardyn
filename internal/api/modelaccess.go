// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Model-access lifecycle — WHOSE credential, and what state it is in.
//
// Two things live here because they are the same question asked twice. Every
// lane that reads or writes a captured AWS SSO session has to resolve whose it
// is before it touches the secret store — a person served somebody else's
// session is the failure a per-person credential exists to prevent. That
// resolution is awsSSOScope.
//
// The other half is the grading: one vocabulary of states, computed once, so
// the provider_access rows (provider_access.go) and the refusals dispatch gives
// for the same credential cannot disagree.

// awsSSOScope names WHOSE captured AWS SSO credential a lane may read or write:
// a model provider's (provider, its UID) for one person (owner), always
// perUser. The zero value is the operator namespace, which no model credential
// is served from any more; it survives only as the namespace the retired
// operator-wide sign-in blob lives in until it is deleted.
//
// Under perUser the ONLY admissible credential is owner's OWN row, and
// Store.For(owner).Get FALLS BACK to the operator's ("", name) row
// (internal/secretstore/pg), so the naive owner-scoped read would serve the
// admin's session to a person with no capture of their own. The read here uses
// For(owner).List() first and only Gets a name that list contains.
//
// bearer records which of the two lanes a Bedrock provider's kind names — the
// person's captured AWS SSO session or their own stored bearer — and a resolve
// reads only that lane.
//
// An EMPTY owner under perUser is a credential nobody owns: it resolves as
// not-configured, never as the operator's.
type awsSSOScope struct {
	perUser bool
	owner   string
	// bearer: the provider is bedrock_bearer, not bedrock_sso.
	bearer bool
	// provider is the UID of the model provider whose credential this is (always
	// perUser): its session lives under wardyn-provider-<uid>-sso.
	provider string
}

// ssoSecret is the name the captured AWS SSO session is stored under in this
// scope's namespace.
func (sc awsSSOScope) ssoSecret() string {
	if sc.provider != "" {
		return providerSecretName(sc.provider, providerSSOPart)
	}
	return harnessCredSecretName(awsSSOProvider)
}

// namespaced reports whether this scope names a per-principal namespace a read
// can actually be made in. False under perUser with no owner — the fail-closed
// direction, since the alternative is reading the operator's row.
func (sc awsSSOScope) namespaced() bool { return sc.perUser && sc.owner != "" }

// rowOwner is the owner of the row this scope reads: its principal when
// namespaced, else the operator's "".
func (sc awsSSOScope) rowOwner() string {
	if sc.namespaced() {
		return sc.owner
	}
	return ""
}

// AdminTokenPrincipal exports adminTokenPrincipal for cmd/wardynd's boot-time
// validation: a -local-operator value equal to it would collide with this
// mechanism principal, refusing the SAME operator seat at a provider sign-in
// that boot just accepted.
func AdminTokenPrincipal() string { return adminTokenPrincipal }

// awsSSOCredentialSourceLabel is the scope as the audit trail and the re-auth
// hold's scope name it: per_user for a person's own credential, shared for the
// operator namespace.
func awsSSOCredentialSourceLabel(sc awsSSOScope) string {
	if sc.perUser {
		return string(types.CredentialSourcePerUser)
	}
	return string(types.CredentialSourceShared)
}

// The model-access states — ONE vocabulary, ordered REGISTRATION-first.
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
	// expired_renewable folds in here deliberately: dispatch renews it, so
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
	// modelAccessNotConfigured: this principal has never captured a session.
	// NOT an error — it is the first-run state.
	modelAccessNotConfigured = "not_configured"
	// modelAccessNotApplicable: the caller is a MECHANISM, not a person — the
	// shared admin bearer token; no credential to grade, no sign-in it could
	// complete, NO action.
	modelAccessNotApplicable = "not_applicable"
)

// modelAccessExpiringWindow is how far ahead of a lapse "expiring" starts. A day
// is the smallest window that still lets somebody act on it during a working
// day; anything shorter is a notice that arrives after the fact.
const modelAccessExpiringWindow = 24 * time.Hour

// The action lines the states ship. DRAFT (M2 canon pending) — the frozen copy
// is docs/design/workspace-providers-prompt.md §7.7
// (MODEL_ACCESS_EXPIRING_ACTION, SIGN_IN_AWS), reproduced BYTE-EXACT here
// because the console renders a server action line verbatim.
const (
	modelAccessExpiringAction = "Sign in again before %s"
	// DRAFT (M2 canon pending)
	modelAccessSignInAction = "Sign in to AWS"
	// modelAccessPinContradictedAction replaces "Sign in to AWS" for the one
	// expired_signin the timestamp says nothing about: the session is live, and
	// it is the wrong IDENTITY. It names both pairs because the person reading
	// it has to choose an account and a role on the login terminal, and because
	// "sign in again" on its own reads like a transient glitch rather than a
	// deployment rule they are now on the wrong side of. %s = the stored
	// account, role; then the allowed account, role.
	modelAccessPinContradictedAction = "Your stored AWS session is for account %s / role %s; this row now allows %s / %s — sign in again."
)

// awsSSOCredentialState grades one person's captured AWS SSO session into the
// vocabulary above.
//
// A blob with NO refresh token and MORE than a day of access token left reads
// `live` for the same reason expired_renewable does: there is nothing for the
// person to do YET. On the one-hour-token estate this design comes from, such a
// blob reaches `expiring` within the hour on its own.
//
// spent SKIPS THE renewable ARM ENTIRELY: once AWS has retired the
// refresh token, whether the client registration ALSO lapsed is moot — no
// retry redeems it either way — so the only question left is the one dispatch
// itself asks, needsRefresh: inside the refresh skew a dispatch would already
// refuse this run, so grading anything but DEAD there would promise a launch
// the person cannot make; outside it the access token still signs
// requests, so `expiring` (not `live`) is what tells the person while there is
// still time to act.
func awsSSOCredentialState(blob awsSSOBlob, found, spent bool, now time.Time) string {
	if !found {
		return modelAccessNotConfigured
	}
	dead := modelAccessExpiredSignin
	if spent {
		if blob.needsRefresh(now) {
			return dead
		}
		return modelAccessExpiring
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
	default:
		return ""
	}
}

// modelAccessDeadline is the timestamp the expiring action names: the client
// registration's lapse when the blob can be renewed (that is what actually runs
// out), the access token's own expiry when it cannot.
//
// spent is checked EXPLICITLY, ahead of the renewable arm: a spent
// credential is never renewed again regardless of what registration timestamp
// it carries, so the deadline that matters is the moment dispatch itself stops
// serving the access token — ExpiresAt − awsSSORefreshSkew, the same instant
// needsRefresh uses to grade it dead.
func modelAccessDeadline(blob awsSSOBlob, found, spent bool, now time.Time) string {
	switch {
	case !found:
		return ""
	case spent:
		return blob.ExpiresAt.Add(-awsSSORefreshSkew).UTC().Format(time.RFC3339)
	case blob.renewable(now) && !blob.RegistrationExpiresAt.IsZero():
		return blob.RegistrationExpiresAt.UTC().Format(time.RFC3339)
	default:
		return blob.ExpiresAt.UTC().Format(time.RFC3339)
	}
}

// awsSSOTokenSpentFor is the "is this blob's refresh token already known dead"
// read the grading consults. Guarded on RefreshToken != "": the spent map is
// keyed by a fingerprint of the refresh token, and fingerprinting "" would mark
// every refresh-token-less blob spent the moment ANY one of them was.
func (s *Server) awsSSOTokenSpentFor(blob awsSSOBlob) bool {
	if blob.RefreshToken == "" {
		return false
	}
	return s.awsSSOTokenSpent(awsSSOTokenFingerprint(blob.RefreshToken))
}
