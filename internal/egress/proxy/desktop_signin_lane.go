// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// The desktop sign-in lane is the second listener a run's proxy opens when
// Config.SigninListen is set. It carries the person's sign-in browser only
// (the uid-1001 stack in the desktop image), and it is told apart from the
// agent by a per-sign-in credential the agent cannot present: wardynd hands it
// to the person's stack on stdin, it lives in the person's 0700 store, and the
// control plane accepts it only while that sign-in is open. Traffic here is
// tunnel-only: no TLS termination, no credential injection, and no request
// target is read beyond CONNECT's host:port, so a sign-in's URLs (an OAuth
// code= in a callback) never reach a log, the audit or content inspection.
// Egress policy still decides each host exactly as on the agent's listener.

const (
	// signinProxyUser is the Basic user name the person's stack sends.
	signinProxyUser = "wardyn-signin"
	// signinCheckPath asks the control plane whether a credential belongs to
	// an open sign-in of this run.
	signinCheckPath = "/api/v1/internal/desktop-signin/check"
	// signinCheckFor is how long an accepted credential is trusted before
	// the control plane is asked again: the lane closes this long, at most,
	// after the sign-in ends.
	signinCheckFor = 15 * time.Second
	// signinCheckTimeout bounds one control-plane check.
	signinCheckTimeout = 5 * time.Second
	// ruleSourceSigninViaOrg refuses a sign-in tunnel to a via_org host: the
	// organisation terminates those tunnels, and the sign-in lane terminates
	// nothing.
	ruleSourceSigninViaOrg = "builtin:signin-via-org"
)

type signinLaneKey struct{}

// signinConn marks a connection accepted on the sign-in listener.
type signinConn struct{ net.Conn }

// signinListener tags every accepted connection as the sign-in lane's.
type signinListener struct{ net.Listener }

func (l signinListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &signinConn{c}, nil
}

// signinConnContext is the server's ConnContext: only a connection from the
// sign-in listener carries the lane mark.
func signinConnContext(ctx context.Context, c net.Conn) context.Context {
	if _, ok := c.(*signinConn); ok {
		return context.WithValue(ctx, signinLaneKey{}, true)
	}
	return ctx
}

func onSigninLane(ctx context.Context) bool {
	on, _ := ctx.Value(signinLaneKey{}).(bool)
	return on
}

// signinChecks caches the credentials the control plane accepted.
type signinChecks struct {
	mu sync.Mutex
	ok map[[sha256.Size]byte]time.Time
}

// serveSignin answers one request on the sign-in listener.
func (p *Proxy) serveSignin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		// Never read, log or forward the target: it is where a sign-in's codes ride.
		http.Error(w, "Wardyn's sign-in listener carries CONNECT tunnels only.", http.StatusMethodNotAllowed)
		return
	}
	if !p.signinAuthorized(r.Context(), r.Header.Get("Proxy-Authorization")) {
		w.Header().Set("Proxy-Authenticate", `Basic realm="wardyn-signin"`)
		http.Error(w, "Wardyn's sign-in listener serves an open sign-in's browser only.", http.StatusProxyAuthRequired)
		return
	}
	var release func()
	r, release = p.admitSpoolHTTP(w, r, "")
	if r == nil {
		return
	}
	defer release()
	host, port := splitHostPort(r.Host, 443)
	if host == "" {
		host, port = splitHostPort(r.RequestURI, 443)
	}
	if _, ok := p.viaOrgRouteForHost(host); ok {
		log := decisionLog(egress.Request{
			AuditReservation: spoolTicket(r.Context()), RunID: p.runID, Host: host, Port: port,
			Method: http.MethodConnect, Time: p.now(),
		}, egress.Deny, ruleSourceSigninViaOrg)
		p.sink.emit(log)
		p.writeEgressDeny(w, host, port, &log, false)
		return
	}
	decision, target, log := p.evaluate(r.Context(), host, port, http.MethodConnect, "")
	switch decision {
	case egress.Deny:
		if log != nil {
			p.sink.emit(*log)
		}
		p.writeEgressDeny(w, host, port, log, true)
		return
	case egress.Pending:
		if log != nil {
			p.sink.emit(*log)
		}
		setEgressRefusalHeaders(w, egressRefusalPending, host)
		writeApprovalPending(w, log)
		return
	}
	// The hosts the agent reaches only through a checked door stay refused here too.
	if p.refuseADOTunnel(w, r, host, port) || p.refuseAzureTunnel(w, r, host, port) || p.refusePATAPITunnel(w, r, host, port) ||
		p.refuseGitHubAPITunnel(w, r, host, port) || p.refuseAzureARMTunnel(w, r, host, port) {
		return
	}
	p.openTunnel(w, r, host, port, target, log, nil)
}

// signinAuthorized reports whether header carries a credential of an open
// sign-in of this run.
func (p *Proxy) signinAuthorized(ctx context.Context, header string) bool {
	cred, ok := signinCredential(header)
	if !ok {
		return false
	}
	sum := sha256.Sum256([]byte(cred))
	now := p.now()
	p.signin.mu.Lock()
	until, hit := p.signin.ok[sum]
	p.signin.mu.Unlock()
	if hit && now.Before(until) {
		return true
	}
	if !p.checkSigninCredential(ctx, cred) {
		return false
	}
	p.signin.mu.Lock()
	defer p.signin.mu.Unlock()
	if p.signin.ok == nil {
		p.signin.ok = map[[sha256.Size]byte]time.Time{}
	}
	for k, t := range p.signin.ok {
		if !now.Before(t) {
			delete(p.signin.ok, k)
		}
	}
	p.signin.ok[sum] = now.Add(signinCheckFor)
	return true
}

// signinCredential returns the credential of a well-formed
// "Basic base64(wardyn-signin:<43-char base64url of 32 bytes>)" header.
func signinCredential(header string) (string, bool) {
	scheme, enc, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Basic") {
		return "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(enc))
	if err != nil {
		return "", false
	}
	user, cred, ok := strings.Cut(string(raw), ":")
	if !ok || user != signinProxyUser {
		return "", false
	}
	if b, err := base64.RawURLEncoding.DecodeString(cred); err != nil || len(b) != 32 {
		return "", false
	}
	return cred, true
}

// checkSigninCredential asks the control plane; anything but 204 refuses.
func (p *Proxy) checkSigninCredential(ctx context.Context, cred string) bool {
	if p.runToken == nil {
		return false
	}
	body, err := json.Marshal(map[string]string{"credential": cred})
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, signinCheckTimeout)
	defer cancel()
	resp, err := p.forwardToControlPlane(ctx, http.MethodPost, signinCheckPath, body, "application/json")
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusNoContent
}
