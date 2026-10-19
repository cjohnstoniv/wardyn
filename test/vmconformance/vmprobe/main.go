// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

// Command vmprobe is the VM conformance suite's guest probe. It runs inside
// the QEMU guest (the initramfs's /init starts it once the network is up),
// tries each way out of the guest, and prints one "VMPROBE {json}" line per
// row on the serial console. A row passes when the guest's networking keeps
// the relay contract; against an unpinned network the escape rows fail.
//
// Parameters come from the kernel command line: vmprobe.dns (the resolver),
// vmprobe.origin (the origin's literal address) and vmprobe.proxy (the
// proxy's literal address on the run network).
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

type row struct {
	Row    string `json:"row"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
}

var synthetic = netip.MustParsePrefix("198.18.0.0/15")

func main() {
	args := cmdline()
	p := &probe{dns: args["vmprobe.dns"], origin: args["vmprobe.origin"], proxy: args["vmprobe.proxy"]}
	p.res = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "udp", net.JoinHostPort(p.dns, "53"))
	}}
	for _, r := range []struct {
		name string
		fn   func() (bool, string)
	}{
		{"dns-synthetic", p.dnsSynthetic},
		{"dns-crlf-refused", p.dnsCRLF},
		{"https-allowed", func() (bool, string) { return p.tlsGET("origin.test", 443, "origin.test", false, true) }},
		{"http-allowed", func() (bool, string) { return p.httpReq("origin.test", "GET", "origin.test", 200) }},
		{"https-not-allowlisted", func() (bool, string) { return p.tlsGET("other.test", 443, "other.test", false, false) }},
		{"literal-ip-443", func() (bool, string) { return p.noTCP(p.origin, 443) }},
		{"literal-ip-proxy", func() (bool, string) { return p.noTCP(p.proxy, 3128) }},
		{"sni-fronting", func() (bool, string) { return p.tlsGET("origin.test", 443, "other.test", false, false) }},
		{"sni-split-match", func() (bool, string) { return p.tlsGET("origin.test", 443, "origin.test", true, true) }},
		{"sni-split-mismatch", func() (bool, string) { return p.tlsGET("origin.test", 443, "other.test", true, false) }},
		{"sni-delayed", p.sniDelayed},
		{"tls-granted-8443", func() (bool, string) { return p.tlsGET("origin.test", 8443, "origin.test", false, true) }},
		{"tls-granted-8443-mismatch", func() (bool, string) { return p.tlsGET("origin.test", 8443, "other.test", false, false) }},
		{"bare-host-22", func() (bool, string) { ok, d := p.banner("origin.test", 22); return !ok, d }},
		{"granted-2222-banner", func() (bool, string) { return p.banner("origin.test", 2222) }},
		{"http-host-mismatch", func() (bool, string) { return p.httpReq("origin.test", "GET", "other.test", 403) }},
		{"http-method-rule", func() (bool, string) { return p.httpReq("origin.test", "POST", "origin.test", 403) }},
		{"udp-dns-external", func() (bool, string) { return p.noUDP(p.origin, 53) }},
		{"udp-quic", func() (bool, string) { return p.noUDP(p.resolve("origin.test"), 443) }},
		{"udp-ntp", func() (bool, string) { return p.noUDP(p.origin, 123) }},
		{"icmp-gateway", p.noPing},
		{"ipv6", p.noIPv6},
	} {
		ok, detail := r.fn()
		b, _ := json.Marshal(row{Row: r.name, Pass: ok, Detail: detail})
		fmt.Printf("VMPROBE %s\n", b)
	}
	fmt.Println("VMPROBE-DONE")
}

func cmdline() map[string]string {
	b, _ := os.ReadFile("/proc/cmdline")
	out := map[string]string{}
	for _, f := range strings.Fields(string(b)) {
		if k, v, ok := strings.Cut(f, "="); ok {
			out[k] = v
		}
	}
	return out
}

type probe struct {
	dns, origin, proxy string
	res                *net.Resolver
}

func (p *probe) resolve(name string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addrs, err := p.res.LookupNetIP(ctx, "ip4", name)
	if err != nil || len(addrs) == 0 {
		return ""
	}
	return addrs[0].String()
}

// dnsSynthetic: names resolve only to the relay's synthetic range, never to
// a real address.
func (p *probe) dnsSynthetic() (bool, string) {
	a := p.resolve("origin.test")
	ip, err := netip.ParseAddr(a)
	return err == nil && synthetic.Contains(ip), "origin.test -> " + a
}

// dnsCRLF: a label carrying CR/LF is answered REFUSED.
func (p *probe) dnsCRLF() (bool, string) {
	q := []byte{0x12, 0x34, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	for _, l := range []string{"evil\r\nx", "test"} {
		q = append(q, byte(len(l)))
		q = append(q, l...)
	}
	q = append(q, 0, 0, 1, 0, 1)
	c, err := net.DialTimeout("udp", net.JoinHostPort(p.dns, "53"), 3*time.Second)
	if err != nil {
		return false, err.Error()
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write(q); err != nil {
		return false, err.Error()
	}
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	if err != nil || n < 4 {
		return false, fmt.Sprintf("no reply: %v", err)
	}
	rcode := buf[3] & 0x0f
	return rcode == 5, fmt.Sprintf("rcode %d", rcode)
}

// splitConn writes the first write (the ClientHello) in two segments 300 ms
// apart, so the relay must reassemble the server name across reads.
type splitConn struct {
	net.Conn
	done bool
}

func (s *splitConn) Write(b []byte) (int, error) {
	if s.done || len(b) < 20 {
		return s.Conn.Write(b)
	}
	s.done = true
	n, err := s.Conn.Write(b[:11])
	if err != nil {
		return n, err
	}
	time.Sleep(300 * time.Millisecond)
	m, err := s.Conn.Write(b[11:])
	return n + m, err
}

// tlsGET dials name:port, speaks TLS with sni and sends a GET. It reports
// pass when getting an answer matches want.
func (p *probe) tlsGET(name string, port int, sni string, split, want bool) (bool, string) {
	addr := p.resolve(name)
	if addr == "" {
		return !want, "no address for " + name
	}
	c, err := net.DialTimeout("tcp", net.JoinHostPort(addr, fmt.Sprint(port)), 5*time.Second)
	if err != nil {
		return !want, err.Error()
	}
	defer c.Close()
	if split {
		c = &splitConn{Conn: c}
	}
	tc := tls.Client(c, &tls.Config{ServerName: sni, InsecureSkipVerify: true})
	_ = tc.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := io.WriteString(tc, "GET /tls HTTP/1.1\r\nHost: "+sni+"\r\nConnection: close\r\n\r\n"); err != nil {
		return !want, err.Error()
	}
	resp, err := http.ReadResponse(bufio.NewReader(tc), nil)
	if err != nil {
		return !want, err.Error()
	}
	body, _ := io.ReadAll(resp.Body)
	got := resp.StatusCode == 200 && strings.HasPrefix(string(body), "origin")
	return got == want, fmt.Sprintf("%d %q", resp.StatusCode, body)
}

// sniDelayed sends the first bytes of a TLS record and then nothing: the
// relay must close the flow at its budget, not wait on the guest.
func (p *probe) sniDelayed() (bool, string) {
	c, err := net.DialTimeout("tcp", net.JoinHostPort(p.resolve("origin.test"), "443"), 5*time.Second)
	if err != nil {
		return false, err.Error()
	}
	defer c.Close()
	if _, err := c.Write([]byte{0x16, 0x03, 0x01}); err != nil {
		return false, err.Error()
	}
	_ = c.SetReadDeadline(time.Now().Add(20 * time.Second))
	start := time.Now()
	_, err = c.Read(make([]byte, 1))
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false, "still open after 20s"
	}
	return err != nil, fmt.Sprintf("closed after %s: %v", time.Since(start).Round(time.Second), err)
}

// httpReq sends one plain HTTP request to name:80 with the given Host and
// passes when the status is want.
func (p *probe) httpReq(name, method, host string, want int) (bool, string) {
	c, err := net.DialTimeout("tcp", net.JoinHostPort(p.resolve(name), "80"), 5*time.Second)
	if err != nil {
		return false, err.Error()
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	_, _ = fmt.Fprintf(c, "%s /plain HTTP/1.1\r\nHost: %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", method, host)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return false, err.Error()
	}
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode == want, fmt.Sprintf("%d %q %s", resp.StatusCode, body, resp.Header.Get("X-Wardyn-Egress-Reason"))
}

// banner reads the first line a server-speaks-first service sends.
func (p *probe) banner(name string, port int) (bool, string) {
	c, err := net.DialTimeout("tcp", net.JoinHostPort(p.resolve(name), fmt.Sprint(port)), 5*time.Second)
	if err != nil {
		return false, err.Error()
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(20 * time.Second))
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return false, err.Error()
	}
	return line == "SSH-2.0-origin\r\n", fmt.Sprintf("%q", line)
}

// noTCP passes when a TCP connection to addr:port cannot be made.
func (p *probe) noTCP(addr string, port int) (bool, string) {
	c, err := net.DialTimeout("tcp", net.JoinHostPort(addr, fmt.Sprint(port)), 5*time.Second)
	if err != nil {
		return true, err.Error()
	}
	_ = c.Close()
	return false, "connected"
}

// noUDP passes when a datagram to addr:port gets no reply.
func (p *probe) noUDP(addr string, port int) (bool, string) {
	if addr == "" {
		return true, "no address"
	}
	c, err := net.DialTimeout("udp", net.JoinHostPort(addr, fmt.Sprint(port)), 3*time.Second)
	if err != nil {
		return true, err.Error()
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = c.Write([]byte("vmprobe"))
	if _, err := c.Read(make([]byte, 64)); err != nil {
		return true, err.Error()
	}
	return false, "answered"
}

// noPing passes when the gateway does not answer an ICMP echo.
func (p *probe) noPing() (bool, string) {
	c, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return false, err.Error()
	}
	defer c.Close()
	msg, _ := (&icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{ID: 7, Seq: 1, Data: []byte("vmprobe")}}).Marshal(nil)
	if _, err := c.WriteTo(msg, &net.IPAddr{IP: net.IPv4(10, 0, 2, 2)}); err != nil {
		return true, err.Error()
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1500)
	for {
		n, _, err := c.ReadFrom(buf)
		if err != nil {
			return true, err.Error()
		}
		if m, err := icmp.ParseMessage(1, buf[:n]); err == nil && m.Type == ipv4.ICMPTypeEchoReply {
			return false, "echo reply"
		}
	}
}

// noIPv6 passes when a DNS query to the slirp-style IPv6 resolver gets no
// reply (the relay drops every IPv6 frame).
func (p *probe) noIPv6() (bool, string) {
	c, err := net.DialTimeout("udp6", "[fec0::3]:53", 3*time.Second)
	if err != nil {
		return true, err.Error()
	}
	defer c.Close()
	q := make([]byte, 12)
	binary.BigEndian.PutUint16(q, 0x4242)
	q[2] = 1
	q[5] = 1
	q = append(q, 6, 'o', 'r', 'i', 'g', 'i', 'n', 4, 't', 'e', 's', 't', 0, 0, 1, 0, 1)
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = c.Write(q)
	if _, err := c.Read(make([]byte, 512)); err != nil {
		return true, err.Error()
	}
	return false, "answered over IPv6"
}
