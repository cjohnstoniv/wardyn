// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// upstreamProxy is the OPTIONAL corporate parent proxy that wardyn-proxy chains
// its egress through. In a locked-down corporate network the sandbox host has NO
// direct route to the internet — the ONLY way out is the org's HTTP CONNECT
// proxy, which is FREQUENTLY a PRIVATE address (e.g. proxy.corp:8080 @ 10.x).
// When set, every FORWARD-EGRESS dial (HTTPS CONNECT tunnels, plain-HTTP
// forwards, and the MITM LLM path) is issued as CONNECT <real-host>:<port> to
// this proxy instead of a direct dial. Control-plane calls to wardynd NEVER
// traverse it — that split is enforced by a dedicated controlTransport (see
// newProxy), so the run token is never sent toward the corp proxy.
//
// SECURITY: the embedded credential (if the operator's URL carried user:pass@)
// is held ONLY here in proxy memory — the same posture as the run token — and is
// registered in the process secret-mask registry (see NewServer) so it can never
// leak into a decision log or stdout.
type upstreamProxy struct {
	// addr is the corp proxy's host:port, dialed directly. It is resolved and
	// pinned WITHOUT the private-IP guard — the deliberate, audited exception.
	addr string
	// authHeader is the "Basic <base64>" Proxy-Authorization value, or "" when
	// the operator configured no credential.
	authHeader string
	// host / port are the parsed authority, kept for the startup audit record.
	host string
	port int
}

// parseUpstreamProxy parses an operator-configured upstream-proxy URL of the
// form http[s]://[user:pass@]host[:port]. Scheme must be http or https and host
// is required. Returns (nil, nil) for the empty string (upstream disabled), so
// callers can use it both to validate config and to build the live proxy.
func parseUpstreamProxy(raw string) (*upstreamProxy, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		// Do NOT echo the raw URL — it may carry user:pass credentials.
		return nil, fmt.Errorf("upstream proxy url: malformed (redacted)")
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		// Only plaintext-HTTP CONNECT-forwarding is implemented. dialThroughUpstream
		// does a plaintext TCP CONNECT + Proxy-Authorization; an https:// proxy
		// would need a TLS wrap first (else the Basic cred goes cleartext to the
		// proxy). Reject https until that's implemented rather than silently
		// leaking the credential. (The tunneled payload to the real target is
		// still end-to-end TLS regardless — this is only the hop to the proxy.)
	default:
		return nil, fmt.Errorf("upstream proxy url: unsupported scheme %q (only http is supported; https-to-proxy is not yet implemented)", u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return nil, fmt.Errorf("upstream proxy url: missing host")
	}
	port := 80
	if ps := u.Port(); ps != "" {
		n, perr := strconv.Atoi(ps)
		if perr != nil || n <= 0 || n > 65535 {
			return nil, fmt.Errorf("upstream proxy url: bad port %q", ps)
		}
		port = n
	}
	up := &upstreamProxy{
		addr: net.JoinHostPort(host, strconv.Itoa(port)),
		host: host,
		port: port,
	}
	if u.User != nil {
		// user[:pass] -> "Basic base64(user:pass)". Use the DECODED username/password,
		// not u.User.String(), which percent-encodes them: a password like "p@ss/w0rd"
		// would otherwise be sent as "p%40ss%2Fw0rd" — the wrong credential, so the
		// upstream proxy rejects the CONNECT. The cleartext credential is NEVER logged;
		// NewServer registers maskValues() in the mask registry.
		cred := u.User.Username()
		if pw, ok := u.User.Password(); ok {
			cred += ":" + pw
		}
		up.authHeader = "Basic " + base64.StdEncoding.EncodeToString([]byte(cred))
	}
	return up, nil
}

// maskValues returns the secret byte-strings that must be masked from any
// decision-log / stdout output: the base64 credential as it appears on the wire
// (Proxy-Authorization), the decoded user:pass, and the password half alone.
// Empty when no credential is configured. (secretmask ignores values shorter
// than its MinLen, so trivially short creds are simply not registered.)
func (u *upstreamProxy) maskValues() [][]byte {
	if u == nil || u.authHeader == "" {
		return nil
	}
	b64 := strings.TrimPrefix(u.authHeader, "Basic ")
	vals := [][]byte{[]byte(b64)}
	if dec, err := base64.StdEncoding.DecodeString(b64); err == nil && len(dec) > 0 {
		vals = append(vals, dec)
		if i := bytes.IndexByte(dec, ':'); i >= 0 && i+1 < len(dec) {
			vals = append(vals, dec[i+1:]) // the password half, the most sensitive part
		}
	}
	return vals
}

// dialThroughUpstream dials the corporate parent proxy and issues a CONNECT for
// the REAL destination host:port, returning the established tunnel.
//
// SECURITY RELAXATION (deliberate + audited): the corp proxy address is
// resolved+pinned WITHOUT the private-IP/loopback/metadata guard — a corp proxy
// is frequently a private IP (10.x), and this is the OPERATOR-CONFIGURED trusted
// egress hop, the same trust boundary as the control-plane URL
// (resolveTrustedURL). This exception applies ONLY to dialing the configured
// proxy; agent-chosen targets keep the full guard (evaluate() still runs
// policy/approval/method, and the literal-IP guard still denies an agent naming
// a private IP directly). The real host is sent by NAME — the corp proxy does
// the outbound DNS+dial, so the sandbox never needs to resolve it (the vetted-IP
// TOCTOU pin is relaxed for this hop only, documented in evaluate()).
func (p *Proxy) dialThroughUpstream(ctx context.Context, realHost string, realPort int) (net.Conn, error) {
	up := p.upstream
	if up == nil {
		return nil, fmt.Errorf("upstream proxy not configured")
	}
	// Resolve+pin the corp proxy address (TOCTOU guard) but SKIP the private-IP
	// denial — trusted operator config (same as the control-plane endpoint).
	pinned, err := p.resolveTrustedURL("http://" + up.addr)
	if err != nil {
		return nil, fmt.Errorf("resolve upstream proxy: %w", err)
	}
	conn, err := p.dial(ctx, "tcp", pinned)
	if err != nil {
		return nil, fmt.Errorf("dial upstream proxy: %w", err)
	}
	authority := net.JoinHostPort(realHost, strconv.Itoa(realPort))
	var req strings.Builder
	fmt.Fprintf(&req, "CONNECT %s HTTP/1.1\r\n", authority)
	fmt.Fprintf(&req, "Host: %s\r\n", authority)
	if up.authHeader != "" {
		fmt.Fprintf(&req, "Proxy-Authorization: %s\r\n", up.authHeader)
	}
	req.WriteString("\r\n")
	if _, err := conn.Write([]byte(req.String())); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write upstream CONNECT: %w", err)
	}
	// Read the proxy's reply to our CONNECT; a 2xx means the tunnel is open.
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read upstream CONNECT response: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		_ = conn.Close()
		return nil, fmt.Errorf("upstream proxy refused CONNECT: %s", resp.Status)
	}
	// If the proxy pipelined bytes past the response headers (rare for CONNECT),
	// replay them before the raw conn so the caller's first read (e.g. a TLS
	// ServerHello) isn't lost.
	if n := br.Buffered(); n > 0 {
		pre := make([]byte, n)
		if _, err := io.ReadFull(br, pre); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("drain upstream buffer: %w", err)
		}
		return &prefixConn{Conn: conn, prefix: pre}, nil
	}
	return conn, nil
}

// prefixConn serves a few bytes already buffered off the wire before delegating
// to the underlying conn, so no data read while parsing the CONNECT response is
// lost.
type prefixConn struct {
	net.Conn
	prefix []byte
}

func (c *prefixConn) Read(b []byte) (int, error) {
	if len(c.prefix) > 0 {
		n := copy(b, c.prefix)
		c.prefix = c.prefix[n:]
		return n, nil
	}
	return c.Conn.Read(b)
}
