// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package notify_test

import (
	"context"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/notify"
)

const smtpPassword = "smtp-PASSWORD-0451"

func mailChannel(f *fakeSMTP) notify.Channel {
	return notify.Channel{
		ID: "mail", Type: notify.TypeSMTP, Host: "127.0.0.1", Port: f.port(), From: "wardyn@example.com",
		Username: "relay-user", Password: smtpPassword,
	}
}

var sentAt = time.Now().UTC().Truncate(time.Second)

const tinyMessage = "From: wardyn@example.com\r\nTo: a@example.com, b@example.com\r\nSubject: s\r\n\r\n.leading dot line\r\nbody\r\n"

func sendTo(t *testing.T, ch notify.Channel, client *http.Client) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return notify.SendMail(ctx, ch, client, []byte(tinyMessage), sentAt)
}

// TestSMTP_RefusesAServerWithoutSTARTTLS: the relay is on 127.0.0.1, where net/smtp's PlainAuth would
// send the password in clear. The explicit STARTTLS check stops the exchange first: no AUTH, no DATA.
func TestSMTP_RefusesAServerWithoutSTARTTLS(t *testing.T) {
	f := newFakeSMTP(t, false)
	class, _ := sendTo(t, mailChannel(f), f.trust)
	if class != "starttls_missing" {
		t.Fatalf("class = %q, want starttls_missing", class)
	}
	if f.saw("AUTH") || f.saw("DATA") || f.saw("MAIL") {
		t.Fatalf("commands after the refusal: %v", f.seen())
	}
}

// TestSMTP_RefusesAnUnverifiableCertificate: the client trusts nothing the relay's certificate chains to,
// so the handshake fails and no AUTH is sent.
func TestSMTP_RefusesAnUnverifiableCertificate(t *testing.T) {
	f := newFakeSMTP(t, true)
	class, _ := sendTo(t, mailChannel(f), &http.Client{Transport: &http.Transport{}})
	if class != "tls_verify" {
		t.Fatalf("class = %q, want tls_verify", class)
	}
	if f.saw("AUTH") || f.saw("MAIL") || f.saw("DATA") {
		t.Fatalf("commands after the refusal: %v", f.seen())
	}
}

// TestSMTP_VerifiedServerReceivesTheMessageAfterTLS: a trusted certificate, the password sent only over
// TLS, every header recipient on the envelope, and a leading dot in the body survives.
func TestSMTP_VerifiedServerReceivesTheMessageAfterTLS(t *testing.T) {
	f := newFakeSMTP(t, true)
	class, _ := sendTo(t, mailChannel(f), f.trust)
	if class != "" {
		t.Fatalf("class = %q, want success", class)
	}
	if want := []string{"EHLO", "STARTTLS", "EHLO", "AUTH", "MAIL", "RCPT", "RCPT", "DATA", "QUIT"}; !slices.Equal(f.seen(), want) {
		t.Fatalf("commands = %v, want %v", f.seen(), want)
	}
	if len(f.auth) != 1 || !f.auth[0] {
		t.Fatalf("AUTH over TLS = %v, want exactly one, secure", f.auth)
	}
	if !strings.HasPrefix(f.data, "Date: "+sentAt.Format(time.RFC1123Z)+"\nFrom: ") || !strings.Contains(f.data, "\n.leading dot line\n") {
		t.Fatalf("data = %q", f.data)
	}
}

// TestSMTP_RelayReplyTextIsNeverReturned: a relay's refusal comes back as its code alone.
func TestSMTP_RelayReplyTextIsNeverReturned(t *testing.T) {
	f := newFakeSMTP(t, true)
	f.mailCode = "550 " + secretRelayText
	class, retry := sendTo(t, mailChannel(f), f.trust)
	if class != "smtp_reply:550" || retry {
		t.Fatalf("class = %q retry %v, want smtp_reply:550 and no retry", class, retry)
	}
	f.mailCode = "451 " + secretRelayText
	if class, retry = sendTo(t, mailChannel(f), f.trust); class != "smtp_reply:451" || !retry {
		t.Fatalf("class = %q retry %v, want smtp_reply:451 and a retry", class, retry)
	}
}

// TestSMTP_DialAndTimeoutClasses: nothing listening is "dial"; a relay that accepts and never greets
// runs into the send deadline and is "timeout".
func TestSMTP_DialAndTimeoutClasses(t *testing.T) {
	f := newFakeSMTP(t, true)
	ch := mailChannel(f)
	_ = f.ln.Close()
	if class, _ := sendTo(t, ch, f.trust); class != "dial" {
		t.Fatalf("class = %q, want dial", class)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	ch.Port = ln.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if class, _ := notify.SendMail(ctx, ch, f.trust, []byte(tinyMessage), time.Now()); class != "timeout" {
		t.Fatalf("class = %q, want timeout", class)
	}
}
