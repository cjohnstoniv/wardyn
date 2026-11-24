// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
)

// The UI gateway's one shared reverse proxy: its request rewrite and its response strip. Split out of
// uigateway.go for scripts/check-file-size.sh; uigateway.go's header states the security model.

// uiReverseProxy builds the ONE shared reverse proxy (and its exec-lane
// transport) on first use. Per-request state — which run, which port — rides
// the request context, so nothing here is per-run and the idle-connection pool
// is shared and bounded.
func (s *Server) uiReverseProxy() *httputil.ReverseProxy {
	s.uiProxyOnce.Do(func() {
		s.uiProxy = &httputil.ReverseProxy{
			Rewrite:        s.uiRewrite,
			ModifyResponse: s.uiStripOutbound,
			ErrorHandler:   uiErrorHandler,
			// Flush immediately: the relayed apps are interactive (an editor's
			// long-poll, a dev server's HMR stream), and a buffered write on a
			// pipe-backed conn shows up as a frozen UI.
			FlushInterval: -1,
			Transport: &http.Transport{
				DialContext:  s.uiDial,
				MaxIdleConns: maxUIConnsPerRun * 4,
				// A host is one run's app (run:port); a request past the cap waits for a pooled conn (serveUIRelay).
				MaxConnsPerHost: maxUIConnsPerRun, MaxIdleConnsPerHost: maxUIConnsPerRun,
				IdleConnTimeout: uiIdleConnTimeout,
				// No proxy, no TLS: the destination is a sandbox's own loopback
				// reached over an exec, never a network address.
				Proxy: nil,
			},
		}
	})
	return s.uiProxy
}

// uiRewrite builds the outbound request: the /r/<run-id>/<app> prefix comes off
// (the app is served at its own root, as it was before the app segment existed
// — an app that assumes it is mounted at / is no more and no less broken than
// under /r/<run-id>), the
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
	prefix := uiRelayPrefix(sess.Run, sess.App)
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
	// A request from the app's own page carries the gateway's origin, which is not that Host: an app that
	// checks Origin against Host (code-server refuses its workbench's WebSocket so) would refuse its own
	// page. That origin becomes the app's own; any other goes on as it came, for the app's check to judge.
	if s.uiOwnOrigin(pr.In) {
		pr.Out.Header.Set("Origin", "http://"+pr.Out.Host)
	}
	uiStripInbound(pr.Out, s.cfg.UICookiePolicy)
	uiCallerHeaders(pr.Out.Header, sess)
	if uiDesktopApp(sess.App) {
		pr.Out.Header.Del("Sec-WebSocket-Extensions") // compression would hide the RFB stream (uiFilterUpgrade)
		pr.Out = desktopConnTrace(pr.Out)
	}
}

// uiStripOutbound is the browser-ward half: an app in the sandbox must not be able to set, overwrite or
// delete a wardyn_* cookie in the operator's browser (cookie tossing — a sandbox-set wardyn_ui_sess or
// console session cookie would be an authentication attack, not a rendering quirk), nor set one that
// escapes its own host (uiSetCookieAllowed), nor register a service worker that controls more than its
// own app (uiConfineServiceWorker), nor clear site data (a browser applies it to the registrable domain the console shares).
func (s *Server) uiStripOutbound(resp *http.Response) error {
	uiFilterSetCookie(resp.Header)
	resp.Header.Del("Clear-Site-Data")
	if sess, ok := uiSessionFromContext(resp.Request.Context()); ok {
		uiConfineServiceWorker(resp.Request, resp.Header, s.uiBasePath()+uiRelayPrefix(sess.Run, sess.App)+"/")
		if resp.StatusCode == http.StatusSwitchingProtocols {
			if err := s.uiFilterUpgrade(resp, sess); err != nil {
				return err
			}
			s.watchInboundStream(resp)
		}
	} else {
		resp.Header.Del("Service-Worker-Allowed")
	}
	return nil
}

// uiConfineServiceWorker caps the scope of any service worker an app registers
// at the app's own relay prefix. In path mode every run shares one origin, and
// a worker registered at "/" would see every later request to it: other runs'
// apps and the enter hand-off itself.
//
// The browser caps a registration's scope at the script's own directory, which
// through this relay is always inside the app's prefix, unless the script's
// response carries Service-Worker-Allowed. So the header is the one way out,
// and the relay owns it: it is removed from every response, and on the
// worker-script fetch itself (Service-Worker: script, sent by the browser) it
// is set to the app's own prefix. That keeps an app that registers its worker
// at its own root working (editors do), and nothing it sends can widen it. The
// browser enforces the cap on the script response, so it holds whatever the
// app's pages do.
//
// Rejected: refusing worker scripts outright, which breaks editors whose
// webviews need one; and a CSP worker-src on the app's pages, which rides a
// document the app writes, also governs the dedicated workers editors depend
// on, and does not see the scope a registration asks for.
func uiConfineServiceWorker(req *http.Request, h http.Header, prefix string) {
	h.Del("Service-Worker-Allowed")
	if req.Header.Get("Service-Worker") == "script" {
		h.Set("Service-Worker-Allowed", prefix)
	}
}

// uiFilterSetCookie keeps only the Set-Cookie values uiSetCookieAllowed passes.
func uiFilterSetCookie(h http.Header) {
	raw := h.Values("Set-Cookie")
	if len(raw) == 0 {
		return
	}
	h.Del("Set-Cookie")
	for _, sc := range raw {
		if uiSetCookieAllowed(sc) {
			h.Add("Set-Cookie", sc)
		}
	}
}

// uiIsWardynCookie matches the reserved cookie namespace case-insensitively.
// Browsers treat cookie names case-sensitively, so a "WARDYN_" cookie is a
// different cookie and harmless — stripping it anyway costs nothing and
// removes a class of near-miss confusion. The __Host- and __Secure- spellings
// are the same namespace: under secure cookies the console's own session is
// __Host-wardyn_session (oidc.CookieName), and on a shared hostname the
// browser sends it here too.
func uiIsWardynCookie(name string) bool {
	n := strings.ToLower(name)
	n = strings.TrimPrefix(n, "__host-")
	n = strings.TrimPrefix(n, "__secure-")
	return strings.HasPrefix(n, uiCookiePrefix)
}
