// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

// The forwarder is how the person's browser reaches the proxy's sign-in
// listener. The browser can only speak to a TCP proxy, and loopback is shared
// with the agent, so the forwarder serves a connection only when the kernel
// says its other end belongs to the person's uid (/proc/net/tcp), and only
// then adds the sign-in credential the agent cannot read. It relays CONNECT
// and nothing else, so no request target beyond host:port ever leaves it.

// signinProxyUser matches internal/egress/proxy's Basic user name.
const signinProxyUser = "wardyn-signin"

type forwarder struct {
	ln       net.Listener
	upstream string // the proxy's sign-in listener, host:port
	auth     string // the Proxy-Authorization value
	peerUID  func(peer, local netip.AddrPort) (int, error)
}

func newForwarder(upstream, cred string) (*forwarder, error) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	return &forwarder{
		ln:       ln,
		upstream: upstream,
		auth:     "Basic " + base64.StdEncoding.EncodeToString([]byte(signinProxyUser+":"+cred)),
		peerUID:  procPeerUID,
	}, nil
}

func (f *forwarder) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *forwarder) serve(want int) {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(c, want)
	}
}

func (f *forwarder) handle(c net.Conn, want int) {
	defer c.Close()
	peer, perr := netip.ParseAddrPort(c.RemoteAddr().String())
	local, lerr := netip.ParseAddrPort(c.LocalAddr().String())
	if perr != nil || lerr != nil {
		return
	}
	if uid, err := f.peerUID(peer, local); err != nil || uid != want {
		return
	}
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if req.Method != http.MethodConnect {
		_, _ = io.WriteString(c, "HTTP/1.1 405 Method Not Allowed\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return
	}
	target := req.Host
	if _, _, err := net.SplitHostPort(target); err != nil {
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	up, err := net.DialTimeout("tcp", f.upstream, 10*time.Second)
	if err != nil {
		_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return
	}
	defer up.Close()
	if _, err := fmt.Fprintf(up, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: %s\r\n\r\n", target, target, f.auth); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, br); closeWrite(up); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, up); closeWrite(c); done <- struct{}{} }()
	<-done
	<-done
}

func closeWrite(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
}

// procPeerUID reads the uid owning the socket at the far end of a loopback
// connection: the /proc/net/tcp row whose local address is peer and whose
// remote address is local.
func procPeerUID(peer, local netip.AddrPort) (int, error) {
	b, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		return -1, err
	}
	return peerUIDFrom(string(b), peer, local)
}

func peerUIDFrom(table string, peer, local netip.AddrPort) (int, error) {
	want := procAddr(peer) + " " + procAddr(local)
	for _, line := range strings.Split(table, "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 8 || f[1]+" "+f[2] != want {
			continue
		}
		return strconv.Atoi(f[7])
	}
	return -1, errors.New("no socket for that peer")
}

// procAddr spells an IPv4 address and port the way /proc/net/tcp does on a
// little-endian host: the address's four bytes read as a native word, in hex.
func procAddr(ap netip.AddrPort) string {
	a := ap.Addr().Unmap().As4()
	return fmt.Sprintf("%08X:%04X", binary.LittleEndian.Uint32(a[:]), ap.Port())
}
