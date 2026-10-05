// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package notify_test

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeSMTP is an in-process relay that records every command verb it receives. Its certificate is the
// one httptest mints for 127.0.0.1, so trust is decided entirely by the client's root pool.
type fakeSMTP struct {
	ln       net.Listener
	cert     tls.Certificate
	trust    *http.Client // an http.Client whose transport trusts cert
	starttls bool         // advertise STARTTLS
	mailCode string       // reply to MAIL, default 250; a text-bearing reply tests redaction
	authList string       // the AUTH extension's mechanisms, default "PLAIN"; "" omits the line

	mu    sync.Mutex
	verbs []string // every command verb, in order, e.g. EHLO STARTTLS AUTH DATA
	auth  []bool   // for each AUTH seen, whether it arrived over TLS
	creds []string // for each accepted AUTH, "mechanism user password" as the relay decoded it
	data  string   // the DATA payload, dot-unstuffed, CRLF-normalised to LF
}

const secretRelayText = "relay-says-BOGUS-SECRET-TEXT-9183"

func newFakeSMTP(t *testing.T, starttls bool) *fakeSMTP {
	t.Helper()
	ts := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(ts.Close)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, cert: ts.TLS.Certificates[0], trust: ts.Client(), starttls: starttls, mailCode: "250 ok", authList: "PLAIN"}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeSMTP) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeSMTP) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.verbs...)
}

func (f *fakeSMTP) saw(verb string) bool {
	for _, v := range f.seen() {
		if v == verb {
			return true
		}
	}
	return false
}

// answerAuth answers one AUTH command like a relay: a mechanism it did not advertise is a 504, PLAIN
// takes its initial response, and LOGIN runs the two base64 334 challenges (RFC 4954).
func (f *fakeSMTP) answerAuth(tp *textproto.Conn, line string) {
	parts := strings.Fields(line)
	mech := ""
	if len(parts) > 1 {
		mech = strings.ToUpper(parts[1])
	}
	if !slices.Contains(strings.Fields(strings.ToUpper(f.authList)), mech) {
		_ = tp.PrintfLine("504 5.7.4 Unrecognized authentication type")
		return
	}
	dec := func(s string) string { b, _ := base64.StdEncoding.DecodeString(s); return string(b) }
	var user, pass string
	switch mech {
	case "PLAIN":
		if len(parts) < 3 {
			_ = tp.PrintfLine("501 PLAIN needs an initial response")
			return
		}
		fields := strings.Split(dec(parts[2]), "\x00")
		if len(fields) != 3 {
			_ = tp.PrintfLine("501 bad PLAIN response")
			return
		}
		user, pass = fields[1], fields[2]
	case "LOGIN":
		_ = tp.PrintfLine("334 %s", base64.StdEncoding.EncodeToString([]byte("Username:")))
		u, err := tp.ReadLine()
		if err != nil {
			return
		}
		_ = tp.PrintfLine("334 %s", base64.StdEncoding.EncodeToString([]byte("Password:")))
		p, err := tp.ReadLine()
		if err != nil {
			return
		}
		user, pass = dec(u), dec(p)
	default:
		_ = tp.PrintfLine("504 unsupported")
		return
	}
	f.mu.Lock()
	f.creds = append(f.creds, mech+" "+user+" "+pass)
	f.mu.Unlock()
	_ = tp.PrintfLine("235 ok")
}

func (f *fakeSMTP) serve(raw net.Conn) {
	defer raw.Close()
	var conn net.Conn = raw
	tp := textproto.NewConn(conn)
	secure := false
	_ = tp.PrintfLine("220 fake ESMTP ready")
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		verb := strings.ToUpper(strings.Fields(line + " ")[0])
		f.mu.Lock()
		f.verbs = append(f.verbs, verb)
		if verb == "AUTH" {
			f.auth = append(f.auth, secure)
		}
		f.mu.Unlock()
		switch verb {
		case "EHLO", "HELO":
			_ = tp.PrintfLine("250-fake")
			if f.starttls && !secure {
				_ = tp.PrintfLine("250-STARTTLS")
			}
			if f.authList == "" {
				_ = tp.PrintfLine("250 OK")
			} else {
				_ = tp.PrintfLine("250 AUTH %s", f.authList)
			}
		case "STARTTLS":
			_ = tp.PrintfLine("220 go ahead")
			srv := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{f.cert}})
			if err := srv.Handshake(); err != nil {
				return
			}
			conn, secure = srv, true
			tp = textproto.NewConn(conn)
		case "AUTH":
			f.answerAuth(tp, line)
		case "MAIL":
			_ = tp.PrintfLine("%s", f.mailCode)
		case "RCPT":
			_ = tp.PrintfLine("250 ok")
		case "DATA":
			_ = tp.PrintfLine("354 go")
			var b strings.Builder
			r := bufio.NewReader(tp.DotReader())
			for {
				l, err := r.ReadString('\n')
				b.WriteString(l)
				if err != nil {
					break
				}
			}
			f.mu.Lock()
			f.data = strings.ReplaceAll(b.String(), "\r\n", "\n")
			f.mu.Unlock()
			_ = tp.PrintfLine("250 queued")
		case "QUIT":
			_ = tp.PrintfLine("221 bye")
			return
		default:
			_ = tp.PrintfLine("500 unknown")
		}
	}
}
