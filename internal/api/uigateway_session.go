// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The UI-sandbox relay SESSION: what the wardyn_ui_sess cookie carries, which
// URLs it is a credential for, and what is re-checked before it opens another
// connection. uigateway.go is the transport (enter, relay, dial, launcher);
// this file is the credential.
//
// Two invariants live here and nowhere else:
//
//   - A session names ONE APP, so it is scoped to one app. The relay path is
//     /r/<run-id>/<app>/… and the cookie's Path is /r/<run-id>/<app>/ — a run
//     may declare up to 8 ui_apps, and a single per-run cookie meant entering
//     the second app REPLACED the first app's session in the browser (one name,
//     one Path), after which the first tab's own requests dialed the second
//     app's port. Per-app Paths let the two coexist; the sess.App check in
//     handleUIRelay is the server-side half, since a browser's path match is
//     the only thing keeping them apart and a non-browser client has none.
//
//   - A session is BOUNDED-STALE, not a frozen bearer. It carries its own
//     issued-at, so the operator's WARDYN_UI_SANDBOX_SESSION_TTL applies to
//     cookies already in browsers (the relay's sibling of WARDYN_SSH_ROLE_TTL),
//     and uiDial re-asserts role/ownership against the freshly-loaded run and
//     asks SessionRevocations before every NEW connection. A cookie in the
//     pre-0.7.4 format has no issued-at to bound or compare, so it fails CLOSED.
package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// defaultUISessionTTL bounds a relay session when the operator sets no knob.
// Long enough for a working session in an editor (re-entering means minting
// another ticket from the console), short enough that a cookie captured from a
// browser profile is not a permanent key to a sandbox.
const defaultUISessionTTL = 8 * time.Hour

// uiSession is what the relay cookie carries. It is signed (HMAC-SHA256 under
// WARDYN's ui session key), never stored: the ONE authorization decision — is
// this human allowed to reach this run's declared app — was made when the
// single-use ticket was redeemed, and Port is captured there from the run's
// EFFECTIVE policy so no later request can name a different port.
//
// Principal/Role/IssuedAt are what make it re-checkable rather than final: see
// uiSessionStillAuthorized.
type uiSession struct {
	Run       uuid.UUID `json:"r"`
	App       string    `json:"a"`
	Port      int       `json:"p"`
	Principal string    `json:"s"`
	Role      string    `json:"o"`
	Expires   int64     `json:"e"`
	// IssuedAt is when the ticket was redeemed, in Unix seconds. It is the
	// staleness bound the TTL knob re-applies to an already-minted cookie, and
	// the timestamp SessionRevocations compares against a revoke cutoff — the
	// same role oidc.Session.IssuedAt plays for the console session. Absent
	// (a pre-0.7.4 cookie) is refused, not tolerated: neither of those checks
	// can be made without it.
	IssuedAt int64 `json:"i"`
}

type uiSessionCtxKey struct{}

func uiSessionFromContext(ctx context.Context) (uiSession, bool) {
	sess, ok := ctx.Value(uiSessionCtxKey{}).(uiSession)
	return sess, ok
}

// uiSessionTTL is WARDYN_UI_SANDBOX_SESSION_TTL, or the default. Read through
// this rather than off cfg so a Config built without cmd/wardynd's flags — every
// test harness — gets the shipped posture instead of a zero TTL that refuses
// every session, exactly as SSHRoleTTL does.
func (s *Server) uiSessionTTL() time.Duration {
	if s.cfg.UISessionTTL > 0 {
		return s.cfg.UISessionTTL
	}
	return defaultUISessionTTL
}

// uiCookiePath scopes the cookie to ONE app of ONE run. Cookies are not
// port-scoped, but they ARE path-scoped: this is what stops run A's page from
// making the browser send run B's session (the shared-origin residual's
// mitigation in path mode; host mode adds a second, stronger boundary) AND what
// lets two apps of the SAME run hold their own sessions at once.
func uiCookiePath(runID uuid.UUID, app string) string {
	return uiRelayPrefix(runID, app) + "/"
}

// uiRelayPrefix is the path every one of an app's relayed requests hangs off,
// and the prefix uiRewrite trims back off before the request reaches the app.
// One definition, so the cookie's Path, the enter redirect and the trim can
// never disagree.
func uiRelayPrefix(runID uuid.UUID, app string) string {
	return uiRunPrefix + runID.String() + "/" + app
}

// parseUIRunPath splits /r/<run-id>/<app>/... into the run and the app.
// Anything else — a missing or malformed id, a NON-CANONICAL spelling of one,
// or no app segment at all — is not a relay path.
//
// The canonical-spelling requirement is load-bearing, not tidiness: uuid.Parse
// accepts upper-case, braced and unhyphenated forms, while uiRewrite trims the
// canonical lower-case prefix, so a non-browser client naming the run in any
// other spelling made the gateway forward the /r/<id> prefix into the app.
func parseUIRunPath(p string) (uuid.UUID, string, bool) {
	rest := strings.TrimPrefix(p, uiRunPrefix)
	if rest == p {
		return uuid.Nil, "", false
	}
	seg, rest, _ := strings.Cut(rest, "/")
	id, err := uuid.Parse(seg)
	if err != nil || seg != id.String() {
		return uuid.Nil, "", false
	}
	app, _, _ := strings.Cut(rest, "/")
	if app == "" {
		return uuid.Nil, "", false
	}
	return id, app, true
}

func (s *Server) encodeUISession(sess uiSession) string {
	payload, _ := json.Marshal(sess)
	mac := hmac.New(sha256.New, s.cfg.UISessionKey)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// decodeUISession verifies and decodes the relay cookie. Every failure —
// absent, malformed, forged, expired, too old for the configured TTL, or in the
// pre-issued-at format — returns false with no distinction: this listener has
// ONE auth mechanism and no fallback, so there is nothing to negotiate and no
// oracle to offer.
func (s *Server) decodeUISession(r *http.Request, now time.Time) (uiSession, bool) {
	c, err := r.Cookie(uiCookieName)
	if err != nil {
		return uiSession{}, false
	}
	rawPayload, rawSig, found := strings.Cut(c.Value, ".")
	if !found {
		return uiSession{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(rawPayload)
	if err != nil {
		return uiSession{}, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(rawSig)
	if err != nil {
		return uiSession{}, false
	}
	mac := hmac.New(sha256.New, s.cfg.UISessionKey)
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return uiSession{}, false
	}
	var sess uiSession
	if err := json.Unmarshal(payload, &sess); err != nil {
		return uiSession{}, false
	}
	if sess.Run == uuid.Nil || sess.App == "" || sess.Port <= 0 || now.Unix() >= sess.Expires {
		return uiSession{}, false
	}
	// Issued-at, then the CONFIGURED TTL over it. Checking the TTL here and not
	// only at mint is the whole point of the knob: shortening it has to bind
	// the cookies already sitting in browsers, whose signed Expires was
	// computed under the old, longer bound.
	if sess.IssuedAt <= 0 || now.Sub(time.Unix(sess.IssuedAt, 0)) >= s.uiSessionTTL() {
		return uiSession{}, false
	}
	return sess, true
}

// uiSessionStillAuthorized is the per-CONNECTION re-check uiDial runs against
// freshly-loaded state, and the reason the cookie carries a principal, a role
// and an issued-at at all.
//
// The cookie is valid for up to the session TTL, which is long enough for a
// human to be demoted, to lose the run, or to be revoked outright inside one
// session. The console session stops on its very next request when an admin
// revokes it (D16), attach re-checks per connect behind a 30s ticket, and SSH
// bounds a stale admin override with WARDYN_SSH_ROLE_TTL — the relay was the
// one lane where "authorized at enter" meant "authorized until the cookie
// expires", while holding an editor with an in-sandbox terminal.
//
// Both halves are needed: the ownership re-assert catches a demotion or a
// handed-over run but NOT an off-boarded owner (they still equal
// run.CreatedBy), and the revoke cutoff is what catches that one.
//
// The bound, stated plainly: this runs per NEW connection, so a relayed
// WebSocket a human already holds keeps working until it closes. Killing the
// run is what ends an in-flight session — the same bound attach and both SSH
// lanes publish.
func (s *Server) uiSessionStillAuthorized(ctx context.Context, sess uiSession, run types.AgentRun) error {
	if sess.Role != oidc.RoleAdmin && run.CreatedBy != sess.Principal {
		return uiFail(ctx, http.StatusForbidden,
			"this UI session is no longer authorized for this run — open the app again from its run page")
	}
	if s.cfg.SessionRevocations == nil {
		return nil
	}
	// The relay knows the minting principal and nothing else, so it offers that
	// one string as BOTH identities. IsSessionRevoked matches either the sub or
	// the email, which is exactly the "don't make an admin guess which the IdP
	// made authoritative" rule handleRevokeSessions writes cutoffs under — and
	// the principal is whichever of the two this deployment's auth published.
	revoked, err := s.cfg.SessionRevocations.IsSessionRevoked(ctx, sess.Principal, sess.Principal,
		time.Unix(sess.IssuedAt, 0).UTC())
	if err != nil {
		// Fail CLOSED. A revocation lookup that cannot answer is the one case
		// where continuing would serve the credential the admin just cancelled,
		// and a relay connection is cheap to retry.
		slog.ErrorContext(ctx, "wardynd: ui gateway revocation lookup failed", "run_id", sess.Run, "err", err)
		return uiFail(ctx, http.StatusServiceUnavailable, "could not verify this UI session; try again")
	}
	if revoked {
		return uiFail(ctx, http.StatusForbidden,
			"this UI session is no longer authorized for this run — open the app again from its run page")
	}
	return nil
}
