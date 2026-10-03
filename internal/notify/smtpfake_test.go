// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package notify_test

import (
	"bufio"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
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

	mu    sync.Mutex
	verbs []string // every command verb, in order, e.g. EHLO STARTTLS AUTH DATA
	auth  []bool   // for each AUTH seen, whether it arrived over TLS
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
	f := &fakeSMTP{ln: ln, cert: ts.TLS.Certificates[0], trust: ts.Client(), starttls: starttls, mailCode: "250 ok"}
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
			_ = tp.PrintfLine("250 AUTH PLAIN")
		case "STARTTLS":
			_ = tp.PrintfLine("220 go ahead")
			srv := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{f.cert}})
			if err := srv.Handshake(); err != nil {
				return
			}
			conn, secure = srv, true
			tp = textproto.NewConn(conn)
		case "AUTH":
			_ = tp.PrintfLine("235 ok")
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
