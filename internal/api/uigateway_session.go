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
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ── DRAFT (M2 canon pending) ────────────────────────────────────────────────
//
// The two refusals a re-checked session can hit. Both are deliberately the
// SAME sentence a stale cookie already gets ("open the app again from its run
// page"), because the human's next action is identical and the difference —
// off-boarded, revoked, or simply expired — is the audit log's to record (the
// ui.auth/denied `reason`), not the refused browser's to learn.
const (
	// uiSessionNoLongerAuthorizedMsg is the 403 body when role/ownership or the
	// revoke cutoff turns a still-valid cookie down at connect time.
	uiSessionNoLongerAuthorizedMsg = "this UI session is no longer authorized for this run — open the app again from its run page"
	// uiSessionUnverifiableMsg is the 503 body when the store cannot answer —
	// the revocation cutoff, or the run the session is re-checked against. A
	// retryable condition, said as one: the relay fails CLOSED rather than
	// serving a credential an admin may have just cancelled.
	uiSessionUnverifiableMsg = "could not verify this UI session; try again"
)

// The ui.auth/denied `reason` values a refused re-check writes. Not DRAFT
// strings: they are audit DATA, read by a SIEM rule, never rendered to a human
// — so they are stable identifiers rather than copy.
const (
	uiDeniedReasonNotAuthorized         = "not_authorized"
	uiDeniedReasonRevoked               = "revoked"
	uiDeniedReasonRevocationUnavailable = "revocation_unavailable"
	uiDeniedReasonRunUnreadable         = "run_unreadable"
)

// uiReassertInterval is how often a relay session riding an ALREADY-OPEN
// connection is re-checked. uiDial re-checks per new connection, but net/http
// only dials when its pool has nothing reusable — and IdleConnTimeout resets on
// every reuse, so an editor polling faster than that rode one warm connection
// with no re-check at all until the session TTL. 30s mirrors the attach
// keepalive's own cadence: short enough that "revoked" means minutes at worst,
// long enough that the check is a handful of store reads per session rather
// than one per relayed request.
const uiReassertInterval = 30 * time.Second

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

// uiSessionStillAuthorized is the re-check run against freshly-loaded state,
// and the reason the cookie carries a principal, a role and an issued-at at
// all. It returns the typed refusal so both callers — uiDial, which needs it as
// a transport error, and handleUIRelay, which writes it straight to the
// browser — refuse identically.
//
// The cookie is valid for up to the session TTL, which is long enough for a
// human to lose the run or be revoked outright inside one session. The console
// session stops on its very next request when an admin revokes it (D16), attach
// re-checks per connect behind a 30s ticket, and SSH bounds a stale admin
// override with WARDYN_SSH_ROLE_TTL — the relay was the one lane where
// "authorized at enter" meant "authorized until the cookie expires", while
// holding an editor with an in-sandbox terminal.
//
// Both halves are needed: the ownership re-assert catches a handed-over run but
// NOT an off-boarded owner (they still equal run.CreatedBy), and the revoke
// cutoff is what catches that one.
//
// NOT covered, deliberately: a ROLE demotion. sess.Role is the cookie's
// login-time snapshot and is never re-derived, exactly as the console session's
// own role is not — WARDYN_UI_SANDBOX_SESSION_TTL is the bound on it, the same
// bound WARDYN_SSH_ROLE_TTL is for the SSH admin override.
//
// Every refusal writes one ui.auth/denied naming which arm refused: "someone is
// driving a revoked relay credential" is exactly the thing an operator must be
// able to see in the trail, and it is otherwise invisible between that
// session's last ui.open and its ui.close.
func (s *Server) uiSessionStillAuthorized(ctx context.Context, sess uiSession, run types.AgentRun) *uiDialError {
	if sess.Role != oidc.RoleAdmin && run.CreatedBy != sess.Principal {
		return s.uiDenyReassert(sess, uiDeniedReasonNotAuthorized,
			http.StatusForbidden, uiSessionNoLongerAuthorizedMsg)
	}
	if s.cfg.SessionRevocations == nil {
		return nil
	}
	// The principal a relay session carries is ALWAYS the OIDC sub: it comes
	// from the attach ticket, which stamps actorFromRequest's principal, which
	// on the OIDC lane is the sub — the email rides a separate context key the
	// ticket has no column for. IsSessionRevoked takes both identities, so the
	// sub goes in both slots and its two arms collapse into one. CONSEQUENCE,
	// stated because it is a real gap: a revoke that NAMES THE EMAIL does not
	// reach an open relay session, though it does stop the same human's console
	// session. Name the sub, or use all:true — the reserved global cutoff
	// always reaches this. Closing it properly means carrying the email on
	// store.AttachTicket, a schema change.
	revoked, err := s.cfg.SessionRevocations.IsSessionRevoked(ctx, sess.Principal, sess.Principal,
		time.Unix(sess.IssuedAt, 0).UTC())
	if err != nil {
		// Fail CLOSED. A revocation lookup that cannot answer is the one case
		// where continuing would serve the credential the admin just cancelled,
		// and a relay connection is cheap to retry.
		slog.ErrorContext(ctx, "wardynd: ui gateway revocation lookup failed", "run_id", sess.Run, "err", err)
		return s.uiDenyReassert(sess, uiDeniedReasonRevocationUnavailable,
			http.StatusServiceUnavailable, uiSessionUnverifiableMsg)
	}
	if revoked {
		return s.uiDenyReassert(sess, uiDeniedReasonRevoked,
			http.StatusForbidden, uiSessionNoLongerAuthorizedMsg)
	}
	return nil
}

// uiDenyReassert audits the refusal and returns it. One place, so an arm cannot
// be added without its audit row.
func (s *Server) uiDenyReassert(sess uiSession, reason string, status int, msg string) *uiDialError {
	s.auditUI(&sess.Run, types.ActorHuman, sess.Principal, "ui.auth", sess.App, "denied",
		map[string]any{"app": sess.App, "port": sess.Port, "reason": reason})
	return &uiDialError{status: status, msg: msg}
}

// uiReassertRelay is the REQUEST-path half of the re-check, and it exists
// because the connection-path half cannot see most requests: net/http dials
// only when its idle pool has nothing reusable, and IdleConnTimeout restarts on
// every reuse, so an editor polling faster than the idle window rode ONE warm
// connection for the life of the session with uiDial never called again.
//
// Debounced per session to uiReassertInterval so this stays a couple of store
// reads per session rather than a pair per relayed request — and so it never
// turns a pooled request into a dial, which would undo the per-run connection
// bound the pool exists to hold.
//
// ponytail: the debounce map is pruned wholesale past a bound, like
// shouldTouch's. Worst case is one extra re-check per live session.
func (s *Server) uiReassertRelay(ctx context.Context, sess uiSession) *uiDialError {
	now := s.cfg.Now()
	if !s.uiReassertDue(sess, now) {
		return nil
	}
	run, err := s.cfg.Store.GetRun(ctx, sess.Run)
	if err != nil {
		// Fail CLOSED, for the same reason the revocation arm does: "the check
		// could not run" must never resolve to "carry on". Retryable, so 503.
		slog.ErrorContext(ctx, "wardynd: ui gateway re-assert could not load the run", "run_id", sess.Run, "err", err)
		return s.uiDenyReassert(sess, uiDeniedReasonRunUnreadable,
			http.StatusServiceUnavailable, uiSessionUnverifiableMsg)
	}
	if de := s.uiSessionStillAuthorized(ctx, sess, run); de != nil {
		return de
	}
	s.markUIReasserted(sess, now)
	return nil
}

// uiReassertKey identifies ONE minted session: the same human, the same run and
// the same enter. A re-enter mints a new issued-at, so it re-checks at once
// instead of inheriting the old session's window.
func uiReassertKey(sess uiSession) string {
	return sess.Principal + "\x00" + sess.Run.String() + "\x00" + strconv.FormatInt(sess.IssuedAt, 10)
}

func (s *Server) uiReassertDue(sess uiSession, now time.Time) bool {
	s.uiReassertMu.Lock()
	defer s.uiReassertMu.Unlock()
	at, ok := s.uiReassert[uiReassertKey(sess)]
	return !ok || now.Sub(at) >= uiReassertInterval
}

// markUIReasserted records a CLEAN pass only. A refused or unverifiable check
// leaves the window open, so the next request re-checks instead of being waved
// through for the rest of the interval.
func (s *Server) markUIReasserted(sess uiSession, now time.Time) {
	s.uiReassertMu.Lock()
	defer s.uiReassertMu.Unlock()
	if s.uiReassert == nil {
		s.uiReassert = map[string]time.Time{}
	} else if len(s.uiReassert) > 1024 {
		clear(s.uiReassert)
	}
	s.uiReassert[uiReassertKey(sess)] = now
}
