// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package notify_test

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/notify"
)

// gapCovRelay is a verified-STARTTLS relay whose reply to chosen commands is set
// by the test: replies["RCPT"] = "550 no such user" answers every RCPT with it,
// and the key "DATA." replaces the reply to the end of the message.
type gapCovRelay struct {
	ln      net.Listener
	cert    tls.Certificate
	trust   *http.Client
	replies map[string]string

	mu    sync.Mutex
	verbs []string
}

func gapCovNewRelay(t *testing.T, replies map[string]string) *gapCovRelay {
	t.Helper()
	ts := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(ts.Close)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	r := &gapCovRelay{ln: ln, cert: ts.TLS.Certificates[0], trust: ts.Client(), replies: replies}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go r.serve(c)
		}
	}()
	return r
}

func (r *gapCovRelay) channel() notify.Channel {
	return notify.Channel{
		ID: "mail", Type: notify.TypeSMTP, Host: "127.0.0.1", Port: r.ln.Addr().(*net.TCPAddr).Port,
		From: "wardyn@example.com", Username: "relay-user", Password: "relay-pass",
	}
}

func (r *gapCovRelay) saw(verb string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range r.verbs {
		if v == verb {
			return true
		}
	}
	return false
}

func (r *gapCovRelay) reply(tp *textproto.Conn, key, dflt string) {
	if s, ok := r.replies[key]; ok {
		_ = tp.PrintfLine("%s", s)
		return
	}
	_ = tp.PrintfLine("%s", dflt)
}

func (r *gapCovRelay) serve(raw net.Conn) {
	defer raw.Close()
	conn := raw
	tp := textproto.NewConn(conn)
	secure := false
	_ = tp.PrintfLine("220 relay ready")
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		verb := strings.ToUpper(strings.Fields(line + " ")[0])
		r.mu.Lock()
		r.verbs = append(r.verbs, verb)
		r.mu.Unlock()
		switch verb {
		case "EHLO", "HELO":
			if s, ok := r.replies["EHLO"]; ok {
				_ = tp.PrintfLine("%s", s)
				continue
			}
			_ = tp.PrintfLine("250-relay")
			if !secure {
				_ = tp.PrintfLine("250-STARTTLS")
			}
			_ = tp.PrintfLine("250 AUTH PLAIN")
		case "STARTTLS":
			_ = tp.PrintfLine("220 go ahead")
			srv := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{r.cert}})
			if err := srv.Handshake(); err != nil {
				return
			}
			conn, secure = srv, true
			tp = textproto.NewConn(conn)
		case "AUTH":
			r.reply(tp, "AUTH", "235 ok")
		case "MAIL":
			r.reply(tp, "MAIL", "250 ok")
		case "RCPT":
			r.reply(tp, "RCPT", "250 ok")
		case "DATA":
			if s, ok := r.replies["DATA"]; ok {
				_ = tp.PrintfLine("%s", s)
				continue
			}
			_ = tp.PrintfLine("354 go")
			_, _ = io.Copy(io.Discard, tp.DotReader())
			r.reply(tp, "DATA.", "250 queued")
		case "QUIT":
			_ = tp.PrintfLine("221 bye")
			return
		default:
			_ = tp.PrintfLine("500 unknown")
		}
	}
}

func gapCovSend(t *testing.T, r *gapCovRelay, body string) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return notify.SendMail(ctx, r.channel(), r.trust, []byte(body), time.Unix(1_700_000_000, 0))
}

// A relay's refusal at each step of the exchange is reduced to its code, retried
// only for 4xx, and stops the exchange: nothing later is sent.
func TestGapCovSMTPRelayRefusalsAreClassifiedAndStopTheExchange(t *testing.T) {
	cases := []struct {
		name      string
		replies   map[string]string
		class     string
		retryable bool
		notSent   []string
	}{
		{"EHLO refused", map[string]string{"EHLO": "554 no service"}, "smtp_reply:554", false, []string{"STARTTLS", "AUTH", "MAIL"}},
		{"AUTH refused", map[string]string{"AUTH": "535 credentials rejected"}, "smtp_reply:535", false, []string{"MAIL", "RCPT", "DATA"}},
		{"RCPT refused for good", map[string]string{"RCPT": "550 no such user"}, "smtp_reply:550", false, []string{"DATA"}},
		{"RCPT refused for now", map[string]string{"RCPT": "452 mailbox busy"}, "smtp_reply:452", true, []string{"DATA"}},
		{"DATA refused", map[string]string{"DATA": "554 transaction failed"}, "smtp_reply:554", false, nil},
		{"message not accepted at the end of DATA", map[string]string{"DATA.": "451 try again later"}, "smtp_reply:451", true, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := gapCovNewRelay(t, c.replies)
			class, retry := gapCovSend(t, r, tinyMessage)
			if class != c.class || retry != c.retryable {
				t.Fatalf("SendMail = %q, retry %v; want %q, retry %v", class, retry, c.class, c.retryable)
			}
			for _, v := range c.notSent {
				if r.saw(v) {
					t.Errorf("the client sent %s after the relay refused", v)
				}
			}
		})
	}
}

// A message the worker could not have rendered is a store error; one with no
// readable To header is a no-recipient failure. Neither reaches the relay.
func TestGapCovSMTPUnsendableBodies(t *testing.T) {
	cases := []struct{ name, body, class string }{
		{"not a message", "", "store_error"},
		{"no To header", "From: wardyn@example.com\r\nSubject: s\r\n\r\nbody\r\n", "no_recipient"},
		{"unparseable To header", "From: wardyn@example.com\r\nTo: <broken\r\n\r\nbody\r\n", "no_recipient"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := gapCovNewRelay(t, nil)
			class, retry := gapCovSend(t, r, c.body)
			if class != c.class || retry {
				t.Fatalf("SendMail = %q, retry %v; want %q, no retry", class, retry, c.class)
			}
			if r.saw("EHLO") {
				t.Error("the client dialled the relay for a body it could not send")
			}
		})
	}
}
