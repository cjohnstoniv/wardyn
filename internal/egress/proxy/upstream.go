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
	"time"
)

// upstreamConnectTimeout bounds the CONNECT handshake with the corporate
// parent proxy (dial + reply). Matches the 15s dialer in newProxy: an
// upstream that accepts TCP but never answers must surface as a dial
// failure, never as a hang. A var, not a const, so a test can shrink it (see
// TestUpstreamConnectTimeout_ProductionValueUnchanged).
var upstreamConnectTimeout = 15 * time.Second

// upstreamProxy is the OPTIONAL corporate parent proxy that wardyn-proxy
// chains its egress through, since the only way out of a locked-down
// corporate network may be the org's HTTP CONNECT proxy (frequently a private
// address). When set, every forward-egress dial is issued as CONNECT to this
// proxy instead of a direct dial. SECURITY: control-plane calls never
// traverse it (controlTransport in newProxy), so the run token is never sent
// toward the corp proxy. The embedded credential is held only here in proxy
// memory and registered in the process secret-mask registry (NewServer).
type upstreamProxy struct {
	// addr is the corp proxy's host:port. Resolved and pinned WITHOUT the
	// private-IP guard — the deliberate, audited exception.
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
		// Only plaintext-HTTP CONNECT-forwarding is implemented; an https://
		// proxy would need a TLS wrap first or the Basic cred goes cleartext to
		// the proxy (the tunneled payload to the real target stays end-to-end
		// TLS regardless — this is only the hop to the proxy).
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
		// user[:pass] -> "Basic base64(user:pass)". Use the decoded
		// username/password, not u.User.String() (percent-encodes them,
		// mangling a password like "p@ss/w0rd"). NewServer registers
		// maskValues() so the cleartext credential is never logged.
		cred := u.User.Username()
		if pw, ok := u.User.Password(); ok {
			cred += ":" + pw
		}
		up.authHeader = "Basic " + base64.StdEncoding.EncodeToString([]byte(cred))
	}
	return up, nil
}

// maskValues returns the secret byte-strings that must be masked from any
// decision-log / stdout output: the base64 credential as it appears on the
// wire, the decoded user:pass, and the password half alone. Empty when no
// credential is configured.
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

// dialThroughUpstream dials the corporate parent proxy and issues a CONNECT
// for the REAL destination host:port, returning the established tunnel.
//
// SECURITY RELAXATION (deliberate + audited): the corp proxy address is
// resolved+pinned without the private-IP/loopback/metadata guard — it's the
// operator-configured trusted egress hop, same trust boundary as the
// control-plane URL. This exception applies only to dialing the configured
// proxy; agent-chosen targets keep the full guard. The real host is sent by
// name — the corp proxy does the outbound DNS+dial, so the vetted-IP TOCTOU
// pin is relaxed for this hop only (documented in evaluate()).
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
	// Bound the read: a proxy that accepts the TCP connection and never
	// answers would otherwise hang forever — the MITM path has already told
	// the agent "200 Connection Established" before this dial, so an approved
	// request would hang instead of failing fast into the normal
	// dial-failed DENY + 502.
	if err := conn.SetReadDeadline(time.Now().Add(upstreamConnectTimeout)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("set upstream CONNECT deadline: %w", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read upstream CONNECT response: %w", err)
	}
	_ = resp.Body.Close()
	// Clear the deadline: the tunnel that follows is long-lived.
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("clear upstream CONNECT deadline: %w", err)
	}
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
