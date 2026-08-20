// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The UI-sandbox gateway (pillar 4): a governed, ticket-gated HTTP relay from
// an operator's browser to ONE declared loopback port inside a run's sandbox
// (a code editor, a dev server), riding the SAME exec lane the SSH gateway's
// `-L` forward already uses — `socat - TCP:127.0.0.1:<port>` over
// Runner.ExecStream, wrapped as a net.Conn by execConn. There is no pod-IP or
// container-IP dial, no NetworkPolicy delta and no new network path out of the
// sandbox (invariant 3): the sandbox's own network is unchanged, and the bytes
// travel control-plane → substrate exec → container, exactly like an attach.
//
// It lives on a SECOND LISTENER (WARDYN_UI_SANDBOX_LISTEN, empty = off = no
// listener, mirroring the SSH gateway), and that is a security control, not a
// deployment convenience: relayed content is SANDBOX-AUTHORED JavaScript. On
// the console origin it could read the console's own storage (threat model
// "Console auth token storage") and drive every admin action the operator can.
// A second origin is what keeps it out. Boot REFUSES a UI listen address equal
// to the console's (cmd/wardynd, validateUISandboxConfig).
//
// The listener has exactly ONE authentication mechanism and never falls
// through to the console's session cookie or admin bearer:
//
//	POST /runs/{id}/attach-ticket  (existing, owner-or-admin, single-use, 30s)
//	  → GET <ui-origin>/__wardyn/enter?run=&app=&ticket=
//	  → cookie wardyn_ui_sess (HttpOnly, SameSite=Lax, Path=/r/<run-id>/)
//	  → 302 /r/<run-id>/<app path>   … every later request rides the cookie
//
// Cookies are not port-scoped, so a shared hostname would let sandbox content
// see console cookies and vice versa: every forwarded request has ALL wardyn_*
// cookies plus Authorization and ?ticket stripped, and every response has
// Set-Cookie: wardyn_* dropped (cookie tossing). Both directions are pinned by
// tests in uigateway_test.go.
//
// NO CONTENT IS RECORDED. ui.auth/ui.start/ui.open/ui.close say that a human
// opened and closed an app; there is no keystroke, screen or page capture on
// this path, and these actions are deliberately distinct from session.attach so
// a relay session can never appear in the recording picker as if it were one.
package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// uiEnterPath is the ticket-handoff endpoint — the ONLY unauthenticated
	// path on this listener, and it authenticates by consuming a single-use
	// attach ticket.
	uiEnterPath = "/__wardyn/enter"
	// uiRunPrefix roots every relayed request: /r/<run-id>/<app path>. The run
	// id is IN THE PATH so the session cookie can be Path-scoped to it — one
	// run's page cannot make the browser attach another run's cookie.
	uiRunPrefix = "/r/"
	// uiCookieName is the relay session cookie. The wardyn_ prefix is what the
	// inbound strip keys on, so it MUST keep it.
	uiCookieName = "wardyn_ui_sess"
	// uiCookiePrefix is the strip rule: no cookie in this namespace is ever
	// forwarded into a sandbox, whichever wardyn surface set it.
	uiCookiePrefix = "wardyn_"
	// uiSessionTTL bounds a relay session. Long enough for a working session in
	// an editor (re-entering means minting another ticket from the console),
	// short enough that a cookie captured from a browser profile is not a
	// permanent key to a sandbox.
	uiSessionTTL = 8 * time.Hour
	// maxUIConnsPerRun bounds concurrent relay connections — and therefore
	// concurrent socat execs — per run. Browsers open ~6 connections per
	// origin, so this is that plus headroom; it is a resource-exhaustion bound
	// (each connection is a live exec in the sandbox), the sibling of
	// maxSSHSessionsPerRun.
	maxUIConnsPerRun = 8
	// uiIdleConnTimeout reaps a pooled relay connection — and with it the socat
	// exec behind it — after this long idle. Without it, a closed browser tab
	// would leave execs parked in the sandbox until the run stopped.
	uiIdleConnTimeout = 90 * time.Second
	// uiReadyTTL is how long a successful launcher probe is trusted before the
	// next connection re-probes. Short: an app that died must resurface as a
	// relaunch, not as a hung connection.
	uiReadyTTL = 30 * time.Second
	// uiEnsureTimeout bounds the whole probe-launch-poll cycle inside the
	// sandbox, and uiEnsureWaitSecs is the in-sandbox half of it (the poll
	// loop). The outer bound is deliberately larger so the exec's own timeout
	// is what fires, carrying an exit code we can explain.
	uiEnsureWaitSecs = 20
	uiEnsureTimeout  = 30 * time.Second
	// uiLauncherPrefix is the BYOI launcher convention: an app named "code" is
	// started by exec'ing /usr/local/bin/wardyn-ui-code. A convention, never a
	// command string in policy — the policy names an app, the IMAGE decides
	// what that means.
	uiLauncherPrefix = "/usr/local/bin/wardyn-ui-"
)

// uiGatewayEnabled reports whether the gateway is configured. Empty listen
// address = off = no listener at all (UIGatewayHandler returns nil), and
// without a session key no cookie could be signed — so all three of listen
// address, key and store are required. The store is not optional plumbing
// here: every request redeems a ticket, loads a run, or reads a policy
// envelope, so a gateway without one could only fail.
func (s *Server) uiGatewayEnabled() bool {
	return s.cfg.UIListenAddr != "" && len(s.cfg.UISessionKey) >= 32 && s.cfg.Store != nil
}

// uiSandboxHealthz is /healthz's "ui_sandbox" field: nil (disabled) when the
// gateway is off, so a deployment without it simply omits the block — the same
// honest shape sshGatewayHealthz uses. enter_url_template is the ONE field the
// console reads: it never composes the UI origin itself (the whole point is
// that it is a different origin), it substitutes the three placeholders.
func (s *Server) uiSandboxHealthz() map[string]any {
	if !s.uiGatewayEnabled() {
		return nil
	}
	return map[string]any{
		"enabled":            true,
		"enter_url_template": s.uiEnterURLTemplate(),
		// host_mode says whether each run gets its own origin
		// (WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE) or every run shares one — the
		// residual an operator has to know about, published rather than buried.
		"host_mode": s.cfg.UIOriginTemplate != "",
	}
}

// uiEnterURLTemplate builds the enter URL with {run}/{app}/{ticket}
// placeholders. In host mode the origin template already carries {run}; in
// path mode the advertised base is shared by every run.
func (s *Server) uiEnterURLTemplate() string {
	base := s.cfg.UIOriginTemplate
	if base == "" {
		base = s.cfg.UIAdvertiseURL
	}
	if base == "" {
		// Last resort so a local `-ui-sandbox-listen :8081` is usable without a
		// second flag; boot warns that this is almost never externally right.
		base = "http://" + s.cfg.UIListenAddr
	}
	return strings.TrimSuffix(base, "/") + uiEnterPath + "?run={run}&app={app}&ticket={ticket}"
}

// UIGatewayHandler returns the gateway's http.Handler, or nil when the gateway
// is disabled (cmd/wardynd then starts no listener). It is deliberately NOT
// mounted on the console router: these routes must exist ONLY on the second
// origin, and Server.Handler() must 404 them — pinned by test.
func (s *Server) UIGatewayHandler() http.Handler {
	if !s.uiGatewayEnabled() {
		return nil
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Referrer-Policy on EVERY response: the enter URL carries a ticket in
		// its query, and a referrer leak would hand it to whatever the app
		// links to. No CSP/X-Frame-Options here — this origin serves the
		// sandbox's own app, and the console's policy would break it; the
		// separation of origins is the control, not a header on this one.
		w.Header().Set("Referrer-Policy", "no-referrer")
		switch {
		case r.URL.Path == uiEnterPath:
			s.handleUIEnter(w, r)
		case strings.HasPrefix(r.URL.Path, uiRunPrefix):
			s.handleUIRelay(w, r)
		default:
			writeError(w, http.StatusNotFound, "not found on the Wardyn UI gateway (open an app from the run detail page)")
		}
	})
}

// ─── session cookie ──────────────────────────────────────────────────────────

// uiSession is what the relay cookie carries. It is signed (HMAC-SHA256 under
// WARDYN's ui session key), never stored: the ONE authorization decision — is
// this human allowed to reach this run's declared app — was made when the
// single-use ticket was redeemed, and Port is captured there from the run's
// EFFECTIVE policy so no later request can name a different port.
type uiSession struct {
	Run       uuid.UUID `json:"r"`
	App       string    `json:"a"`
	Port      int       `json:"p"`
	Principal string    `json:"s"`
	Role      string    `json:"o"`
	Expires   int64     `json:"e"`
}

type uiSessionCtxKey struct{}

func uiSessionFromContext(ctx context.Context) (uiSession, bool) {
	sess, ok := ctx.Value(uiSessionCtxKey{}).(uiSession)
	return sess, ok
}

// uiCookiePath scopes the cookie to ONE run's relay paths. Cookies are not
// port-scoped, but they ARE path-scoped: this is what stops run A's page from
// making the browser send run B's session (the shared-origin residual's
// mitigation in path mode; host mode adds a second, stronger boundary).
func uiCookiePath(runID uuid.UUID) string { return uiRunPrefix + runID.String() + "/" }

func (s *Server) encodeUISession(sess uiSession) string {
	payload, _ := json.Marshal(sess)
	mac := hmac.New(sha256.New, s.cfg.UISessionKey)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// decodeUISession verifies and decodes the relay cookie. Every failure —
// absent, malformed, forged, expired — returns false with no distinction: this
// listener has ONE auth mechanism and no fallback, so there is nothing to
// negotiate and no oracle to offer.
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
	if sess.Run == uuid.Nil || sess.Port <= 0 || now.Unix() >= sess.Expires {
		return uiSession{}, false
	}
	return sess, true
}

// ─── enter: redeem the ticket, re-check, set the cookie ──────────────────────

// handleUIEnter is the ticket handoff:
//
//	GET /__wardyn/enter?run=<uuid>&app=<name>&ticket=<token>
//
// It consumes the single-use attach ticket (the SAME one the web terminal
// uses — no second ticket type), then RE-CHECKS everything the ticket cannot
// prove on its own against freshly-loaded state: owner-or-admin for THIS run
// (the ticket's stamped role/principal, as handleAttachWS does), the run still
// RUNNING with a sandbox, and the app actually declared in the run's EFFECTIVE
// policy. Only then does a cookie exist.
func (s *Server) handleUIEnter(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	q := r.URL.Query()
	runID, err := uuid.Parse(q.Get("run"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid or missing run id")
		return
	}
	app := q.Get("app")
	// Host binding (host mode only): the cookie about to be set is scoped to
	// THIS origin, so an enter served on the wrong host would mint a session
	// the run's own origin never sees — and would put one run's cookie on
	// another run's origin. Refuse instead.
	if want := s.uiRunOrigin(runID); want != "" && !strings.EqualFold(r.Host, want) {
		s.auditUI(&runID, types.ActorHuman, "unknown", "ui.auth", app, "denied",
			map[string]any{"reason": "wrong host for run", "host": r.Host})
		writeError(w, http.StatusForbidden, "this run's UI apps are served on a different host")
		return
	}

	ta, ok, err := consumeAttachTicket(r.Context(), s.cfg.Store, q.Get("ticket"), runID, s.cfg.Now())
	if err != nil {
		// A store failure is not a bad ticket (attach_ticket.go's own rule):
		// say so, log it, and never leak the database error to a caller who has
		// not authenticated.
		slog.ErrorContext(r.Context(), "wardynd: ui gateway ticket lookup failed", "run_id", runID, "err", err)
		writeError(w, http.StatusInternalServerError, "attach ticket lookup failed")
		return
	}
	if !ok {
		s.auditUI(&runID, types.ActorHuman, "unknown", "ui.auth", app, "denied",
			map[string]any{"reason": "invalid, expired, or already-used ticket"})
		writeError(w, http.StatusForbidden, "invalid, expired, or already-used attach ticket")
		return
	}

	run, err := s.cfg.Store.GetRun(r.Context(), runID)
	if err != nil {
		writeError(w, http.StatusForbidden, "attach ticket does not authorize this run")
		return
	}
	// Owner-or-admin, re-checked against the just-loaded run: this lane never
	// runs humanOrAdminAuth, so the ticket's stamped role/principal is the only
	// authorization signal, exactly as in handleAttachWS.
	if ta.role != oidc.RoleAdmin && run.CreatedBy != ta.principal {
		s.auditUI(&runID, types.ActorHuman, ta.principal, "ui.auth", app, "denied",
			map[string]any{"reason": "not the run owner"})
		writeError(w, http.StatusForbidden, "attach ticket does not authorize this run")
		return
	}
	if run.State != types.RunRunning || run.SandboxRef == "" {
		writeError(w, http.StatusConflict, "run is not RUNNING; cannot open a UI app (state="+string(run.State)+")")
		return
	}

	apps, err := s.effectiveUIApps(r.Context(), runID)
	if err != nil {
		slog.ErrorContext(r.Context(), "wardynd: ui gateway policy lookup failed", "run_id", runID, "err", err)
		writeError(w, http.StatusInternalServerError, "run policy lookup failed")
		return
	}
	declared, found := types.UIApp{}, false
	for _, a := range apps {
		if a.Name == app {
			declared, found = a, true
			break
		}
	}
	if !found {
		s.auditUI(&runID, types.ActorHuman, ta.principal, "ui.auth", app, "denied",
			map[string]any{"reason": "app not declared in the run's policy ui_apps"})
		writeError(w, http.StatusForbidden, "no UI app named "+strconv.Quote(app)+" is declared in this run's policy ui_apps")
		return
	}

	now := s.cfg.Now()
	sess := uiSession{
		Run: runID, App: declared.Name, Port: declared.Port,
		Principal: ta.principal, Role: ta.role,
		Expires: now.Add(uiSessionTTL).Unix(),
	}
	http.SetCookie(w, &http.Cookie{
		Name:     uiCookieName,
		Value:    s.encodeUISession(sess),
		Path:     uiCookiePath(runID),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.cfg.OIDCSecureCookies,
		Expires:  now.Add(uiSessionTTL),
	})
	s.auditUI(&runID, types.ActorHuman, ta.principal, "ui.auth", declared.Name, "success",
		map[string]any{"app": declared.Name, "port": declared.Port})
	http.Redirect(w, r, uiRunPrefix+runID.String()+declared.PathOrRoot(), http.StatusFound)
}

// uiRunOrigin returns the host this run's apps must be served on in HOST mode
// (WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE), or "" in shared-origin path mode where
// any host that reaches the listener is acceptable.
func (s *Server) uiRunOrigin(runID uuid.UUID) string {
	if s.cfg.UIOriginTemplate == "" {
		return ""
	}
	origin := strings.ReplaceAll(s.cfg.UIOriginTemplate, "{run}", runID.String())
	if i := strings.Index(origin, "://"); i >= 0 {
		origin = origin[i+3:]
	}
	return strings.TrimSuffix(origin, "/")
}

// ─── relay ───────────────────────────────────────────────────────────────────

// handleUIRelay serves /r/<run-id>/... — every request after the handoff. The
// cookie is the ONLY credential accepted here: there is no fall-through to the
// console's session or admin bearer, because the caller on this origin may be
// sandbox-authored JavaScript.
func (s *Server) handleUIRelay(w http.ResponseWriter, r *http.Request) {
	runID, ok := parseUIRunPath(r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, "not found on the Wardyn UI gateway (open an app from the run detail page)")
		return
	}
	sess, ok := s.decodeUISession(r, s.cfg.Now())
	if !ok || sess.Run != runID {
		writeError(w, http.StatusForbidden, "no valid UI session for this run — open the app again from its run page")
		return
	}
	// Keep the run's idle clock alive for the life of the session, debounced
	// exactly like the decision-ingest and attach keepalives: a human reading
	// code in an editor is not idle, and the reaper must not stop the run under
	// them.
	if s.cfg.Store != nil && s.shouldTouch(runID) {
		_ = s.cfg.Store.TouchRun(r.Context(), runID)
	}
	ctx := context.WithValue(r.Context(), uiSessionCtxKey{}, sess)
	ctx = context.WithValue(ctx, uiDialErrCtxKey{}, &uiDialErrBox{})
	s.uiReverseProxy().ServeHTTP(w, r.WithContext(ctx))
}

// parseUIRunPath extracts the run id from /r/<run-id>/... Anything else (a
// missing or malformed id) is not a relay path at all.
func parseUIRunPath(p string) (uuid.UUID, bool) {
	rest := strings.TrimPrefix(p, uiRunPrefix)
	seg, _, _ := strings.Cut(rest, "/")
	id, err := uuid.Parse(seg)
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

// uiReverseProxy builds the ONE shared reverse proxy (and its exec-lane
// transport) on first use. Per-request state — which run, which port — rides
// the request context, so nothing here is per-run and the idle-connection pool
// is shared and bounded.
func (s *Server) uiReverseProxy() *httputil.ReverseProxy {
	s.uiProxyOnce.Do(func() {
		s.uiProxy = &httputil.ReverseProxy{
			Rewrite:        s.uiRewrite,
			ModifyResponse: uiStripOutbound,
			ErrorHandler:   uiErrorHandler,
			// Flush immediately: the relayed apps are interactive (an editor's
			// long-poll, a dev server's HMR stream), and a buffered write on a
			// pipe-backed conn shows up as a frozen UI.
			FlushInterval: -1,
			Transport: &http.Transport{
				DialContext:     s.uiDial,
				MaxIdleConns:    maxUIConnsPerRun * 4,
				IdleConnTimeout: uiIdleConnTimeout,
				// No proxy, no TLS: the destination is a sandbox's own loopback
				// reached over an exec, never a network address.
				Proxy: nil,
			},
		}
	})
	return s.uiProxy
}

// uiRewrite builds the outbound request: the /r/<run-id> prefix comes off, the
// dial target becomes "<run-id>:<port>" (parsed back apart in uiDial — there
// is no IP to name), and the inbound hygiene runs. httputil.ProxyRequest has
// already removed the client's X-Forwarded-* headers and we deliberately do
// NOT re-add them: the sandbox has no business learning the operator's IP.
func (s *Server) uiRewrite(pr *httputil.ProxyRequest) {
	sess, ok := uiSessionFromContext(pr.In.Context())
	if !ok {
		// Unreachable: handleUIRelay always stamps the session. Fail closed by
		// dialing an address uiDial refuses rather than a real one.
		pr.Out.URL.Scheme, pr.Out.URL.Host = "http", "invalid:0"
		return
	}
	prefix := uiRunPrefix + sess.Run.String()
	pr.Out.URL.Scheme = "http"
	pr.Out.URL.Host = sess.Run.String() + ":" + strconv.Itoa(sess.Port)
	pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, prefix)
	pr.Out.URL.RawPath = strings.TrimPrefix(pr.In.URL.EscapedPath(), prefix)
	if pr.Out.URL.Path == "" {
		pr.Out.URL.Path, pr.Out.URL.RawPath = "/", ""
	}
	// The app sees a plain loopback Host, which is what it is bound to and what
	// its own CSRF/host checks expect — never the run id we dial by.
	pr.Out.Host = "localhost:" + strconv.Itoa(sess.Port)
	uiStripInbound(pr.Out)
}

// uiStripInbound is the sandbox-ward half of the header hygiene. Cookies are
// not port-scoped, so a browser attaches EVERY wardyn_* cookie for this
// hostname — including the console's session on a shared-host deployment — to
// a request bound for sandbox-authored code. They come off here, along with
// Authorization (same reason) and any ?ticket (single-use, but a ticket in an
// app's access log is still a ticket in a log).
func uiStripInbound(out *http.Request) {
	out.Header.Del("Authorization")
	out.Header.Del("Proxy-Authorization")
	if cookies := out.Cookies(); len(cookies) > 0 {
		kept := make([]string, 0, len(cookies))
		for _, c := range cookies {
			if !uiIsWardynCookie(c.Name) {
				kept = append(kept, c.Name+"="+c.Value)
			}
		}
		if len(kept) == 0 {
			out.Header.Del("Cookie")
		} else {
			out.Header.Set("Cookie", strings.Join(kept, "; "))
		}
	}
	if q := out.URL.Query(); q.Has("ticket") {
		q.Del("ticket")
		out.URL.RawQuery = q.Encode()
	}
}

// uiStripOutbound is the browser-ward half: an app in the sandbox must not be
// able to set, overwrite or delete a wardyn_* cookie in the operator's browser
// (cookie tossing — a sandbox-set wardyn_ui_sess or console session cookie
// would be an authentication attack, not a rendering quirk).
func uiStripOutbound(resp *http.Response) error {
	raw := resp.Header.Values("Set-Cookie")
	if len(raw) == 0 {
		return nil
	}
	kept := make([]string, 0, len(raw))
	for _, sc := range raw {
		name, _, _ := strings.Cut(sc, "=")
		if !uiIsWardynCookie(strings.TrimSpace(name)) {
			kept = append(kept, sc)
		}
	}
	resp.Header.Del("Set-Cookie")
	for _, sc := range kept {
		resp.Header.Add("Set-Cookie", sc)
	}
	return nil
}

// uiIsWardynCookie matches the reserved cookie namespace case-insensitively.
// Browsers treat cookie names case-sensitively, so a "WARDYN_" cookie is a
// different cookie and harmless — stripping it anyway costs nothing and
// removes a class of near-miss confusion.
func uiIsWardynCookie(name string) bool {
	return strings.HasPrefix(strings.ToLower(name), uiCookiePrefix)
}

// ─── dial: one exec per connection ───────────────────────────────────────────

// uiDialError is a dial failure with the status and message the browser should
// see. It travels out of band (uiDialErrBox on the request context) rather than
// through the transport's error wrapping, so the honest reason — a missing
// launcher, a stopped run, the connection cap — survives to the error handler
// instead of collapsing into a generic "bad gateway".
type uiDialError struct {
	status int
	msg    string
}

func (e *uiDialError) Error() string { return e.msg }

type uiDialErrCtxKey struct{}

type uiDialErrBox struct {
	mu  sync.Mutex
	err *uiDialError
}

func (b *uiDialErrBox) set(err *uiDialError) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.err = err
}

func (b *uiDialErrBox) get() *uiDialError {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}

// uiFail records a typed dial failure on the request's box (when there is one)
// and returns it as the dial error.
func uiFail(ctx context.Context, status int, msg string) error {
	err := &uiDialError{status: status, msg: msg}
	if box, ok := ctx.Value(uiDialErrCtxKey{}).(*uiDialErrBox); ok {
		box.set(err)
	}
	return err
}

// uiErrorHandler turns a relay failure into an honest response. The
// missing-launcher case is the one an operator actually hits (a BYOI image
// without the convention binary), and its message is a frozen string the
// console prints verbatim — see docs/design/ui-sandboxes-prompt.md.
func uiErrorHandler(w http.ResponseWriter, r *http.Request, err error) {
	if box, ok := r.Context().Value(uiDialErrCtxKey{}).(*uiDialErrBox); ok {
		if de := box.get(); de != nil {
			writeError(w, de.status, de.msg)
			return
		}
	}
	slog.WarnContext(r.Context(), "wardynd: ui relay failed", "path", r.URL.Path, "err", err)
	writeError(w, http.StatusBadGateway, "the sandbox closed the UI connection: "+err.Error())
}

// uiDial opens ONE relay connection: it re-checks the run is still live,
// enforces the per-run connection cap, makes sure the app is actually
// listening (launching it on demand), and then dials the sandbox's own
// loopback through the exec lane. addr is "<run-id>:<port>" — there is no IP
// anywhere in this path.
func (s *Server) uiDial(ctx context.Context, _, addr string) (net.Conn, error) {
	sess, ok := uiSessionFromContext(ctx)
	if !ok {
		return nil, uiFail(ctx, http.StatusForbidden, "no valid UI session for this connection")
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil || host != sess.Run.String() || portStr != strconv.Itoa(sess.Port) {
		return nil, uiFail(ctx, http.StatusForbidden, "UI session does not authorize this destination")
	}
	if s.cfg.Runner == nil {
		return nil, uiFail(ctx, http.StatusServiceUnavailable, "no runner configured; UI apps unavailable")
	}

	// Fresh run state on EVERY connection (not just at enter): a killed or
	// stopped run must stop serving, and this is the cheapest place that sees
	// every new connection.
	run, err := s.cfg.Store.GetRun(ctx, sess.Run)
	if err != nil {
		return nil, uiFail(ctx, http.StatusNotFound, "run not found")
	}
	if run.State != types.RunRunning || run.SandboxRef == "" {
		return nil, uiFail(ctx, http.StatusConflict, "run is not RUNNING; the UI app is gone (state="+string(run.State)+")")
	}

	release, ok := s.acquireUIConn(sess.Run)
	if !ok {
		return nil, uiFail(ctx, http.StatusServiceUnavailable,
			fmt.Sprintf("too many open UI connections for this run (max %d) — close a tab and retry", maxUIConnsPerRun))
	}
	if err := s.uiEnsureApp(ctx, run, sess); err != nil {
		release()
		return nil, err
	}

	execSess, err := s.cfg.Runner.ExecStream(ctx, run.SandboxRef, runner.ExecSpec{
		Argv: []string{"socat", "-", fmt.Sprintf("TCP:127.0.0.1:%d", sess.Port)},
	})
	if err != nil {
		release()
		return nil, uiFail(ctx, http.StatusBadGateway, sshExecStreamErrorMessage(err))
	}

	opened := s.cfg.Now()
	s.auditUI(&sess.Run, types.ActorHuman, sess.Principal, "ui.open", fmt.Sprintf("127.0.0.1:%d", sess.Port), "success",
		map[string]any{"app": sess.App, "port": sess.Port})
	conn := newExecConn(execSess, run.SandboxRef+":"+strconv.Itoa(sess.Port))
	return &uiConn{execConn: conn, closeFn: func() {
		release()
		s.auditUI(&sess.Run, types.ActorHuman, sess.Principal, "ui.close", fmt.Sprintf("127.0.0.1:%d", sess.Port), "success",
			map[string]any{"app": sess.App, "port": sess.Port, "duration_sec": int(s.cfg.Now().Sub(opened).Seconds())})
	}}, nil
}

// uiConn is an execConn that releases its per-run slot and writes the ui.close
// audit exactly once, whichever of net/http's read or write loop closes first.
type uiConn struct {
	*execConn
	once    sync.Once
	closeFn func()
}

func (c *uiConn) Close() error {
	c.once.Do(c.closeFn)
	return c.execConn.Close()
}

// acquireUIConn takes one of the run's connection slots, returning a release
// func and whether a slot was available. Process-local, like every other
// in-memory bound here (the single-replica control plane is a documented
// constraint, docs/OPERATIONS.md).
func (s *Server) acquireUIConn(runID uuid.UUID) (func(), bool) {
	s.uiConnsMu.Lock()
	defer s.uiConnsMu.Unlock()
	if s.uiConns == nil {
		s.uiConns = map[uuid.UUID]int{}
	}
	if s.uiConns[runID] >= maxUIConnsPerRun {
		return nil, false
	}
	s.uiConns[runID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.uiConnsMu.Lock()
			defer s.uiConnsMu.Unlock()
			if s.uiConns[runID] <= 1 {
				delete(s.uiConns, runID)
				return
			}
			s.uiConns[runID]--
		})
	}, true
}

// ─── on-demand launcher ──────────────────────────────────────────────────────

// uiEnsureApp makes sure the declared port is actually listening inside the
// sandbox, starting the app if it is not. ONE exec does the whole cycle —
// probe, launch, poll — because every extra round trip here is a full exec
// into the sandbox; its exit code is the answer:
//
//	0   already listening — nothing was started
//	5   the launcher ran and the port came up (this is what ui.start records)
//	3   the image has no launcher at the convention path (the BYOI case)
//	4   the launcher ran but nothing opened the port in time
//
// A successful probe is cached briefly (uiReadyTTL) so a browser opening six
// connections at once does not re-probe six times.
//
// ponytail: two simultaneous cold connections can both launch, since each
// probes before either launches. Harmless — the port is exclusive, so exactly
// one process keeps it and the loser exits; a per-app lock would buy nothing
// but contention.
func (s *Server) uiEnsureApp(ctx context.Context, run types.AgentRun, sess uiSession) error {
	key := sess.Run.String() + "/" + sess.App
	if s.uiAppReady(key) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, uiEnsureTimeout)
	defer cancel()

	launcher := uiLauncherPrefix + sess.App
	exec, err := s.cfg.Runner.ExecStream(ctx, run.SandboxRef, runner.ExecSpec{
		Argv: []string{"sh", "-c", uiLauncherScript(launcher, sess.Port)},
	})
	if err != nil {
		return uiFail(ctx, http.StatusBadGateway, sshExecStreamErrorMessage(err))
	}
	// Both streams MUST be drained before Wait: they are unbuffered pipes off
	// one demux goroutine (runner.ExecSession's contract), so an undrained byte
	// deadlocks the wait below.
	if exec.Stdout != nil {
		go func() { _, _ = io.Copy(io.Discard, exec.Stdout) }()
	}
	if exec.Stderr != nil {
		go func() { _, _ = io.Copy(io.Discard, exec.Stderr) }()
	}
	code := -1
	if exec.Wait != nil {
		code, err = exec.Wait()
		if err != nil {
			code = -1
		}
	}
	if exec.Close != nil {
		_ = exec.Close()
	}

	switch code {
	case 0: // already listening; nothing was started, so nothing to record
		s.markUIAppReady(key)
		return nil
	case 5:
		s.markUIAppReady(key)
		s.auditUI(&sess.Run, types.ActorHuman, sess.Principal, "ui.start", sess.App, "success",
			map[string]any{"app": sess.App, "port": sess.Port, "launcher": launcher})
		return nil
	case 3:
		// FROZEN string: the console prints this verbatim under its
		// missing-launcher error (docs/design/ui-sandboxes-prompt.md §7).
		return uiFail(ctx, http.StatusBadGateway,
			"no UI launcher in this image: "+launcher+" not found")
	case 4:
		s.auditUI(&sess.Run, types.ActorHuman, sess.Principal, "ui.start", sess.App, "failure",
			map[string]any{"app": sess.App, "port": sess.Port, "reason": "did not listen in time"})
		return uiFail(ctx, http.StatusBadGateway,
			fmt.Sprintf("%s started but nothing was listening on 127.0.0.1:%d after %ds", launcher, sess.Port, uiEnsureWaitSecs))
	default:
		return uiFail(ctx, http.StatusBadGateway,
			fmt.Sprintf("could not start UI app %q inside the sandbox (probe exit %d; the image needs sh and socat)", sess.App, code))
	}
}

// uiLauncherScript is the in-sandbox probe-launch-poll cycle. Both
// interpolations are policy-validated (a lower-case slug and an integer port,
// validateUIApps), so neither can carry a shell metacharacter; they are quoted
// anyway.
//
// ponytail: no setsid/nohup. The exec has no TTY, so there is no SIGHUP to
// survive; the launched process is reparented to the sandbox's PID 1 and dies
// with the sandbox — which is exactly the lifetime a relayed app should have.
func uiLauncherScript(launcher string, port int) string {
	return fmt.Sprintf(`p=%d
b='%s'
socat -u /dev/null "TCP:127.0.0.1:$p" >/dev/null 2>&1 && exit 0
[ -x "$b" ] || exit 3
"$b" >/dev/null 2>&1 </dev/null &
i=0
while [ "$i" -lt %d ]; do
  sleep 0.5
  socat -u /dev/null "TCP:127.0.0.1:$p" >/dev/null 2>&1 && exit 5
  i=$((i+1))
done
exit 4
`, port, launcher, uiEnsureWaitSecs*2)
}

func (s *Server) uiAppReady(key string) bool {
	s.uiReadyMu.Lock()
	defer s.uiReadyMu.Unlock()
	at, ok := s.uiReady[key]
	return ok && s.cfg.Now().Sub(at) < uiReadyTTL
}

func (s *Server) markUIAppReady(key string) {
	s.uiReadyMu.Lock()
	defer s.uiReadyMu.Unlock()
	if s.uiReady == nil {
		s.uiReady = map[string]time.Time{}
	} else if len(s.uiReady) > 1024 {
		// Pruned wholesale past a bound, like shouldTouch's map: the worst case
		// is one extra probe per live app.
		clear(s.uiReady)
	}
	s.uiReady[key] = s.cfg.Now()
}

// ─── audit ───────────────────────────────────────────────────────────────────

// auditUI writes one ui.* event on the DAEMON-lifetime context, never the
// request's: ui.close in particular reports the end of the very connection
// whose context is already cancelled by the time it runs, which is the same
// trailing-write race the SSH bridges' audits hit (sshgateway_channels.go).
//
// These actions are deliberately NOT session.attach: there is no PTY, no
// holder, and no content capture on this path, and a relay session must never
// appear in the recording picker as though a recording of it existed.
func (s *Server) auditUI(runID *uuid.UUID, actorType types.ActorType, principal, action, target, outcome string, data map[string]any) {
	s.recordAudit(s.cfg.BaseCtx, s.auditEvent(runID, actorType, principal, action, target, outcome, mustJSON(data)))
}
