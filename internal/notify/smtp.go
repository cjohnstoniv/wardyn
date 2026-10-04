// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// SMTP failure classes beyond the shared ones. A relay's reply text is never stored: only its code.
const (
	classNoRecipient     = "no_recipient"
	classSTARTTLSMissing = "starttls_missing"
)

// errNoRecipient is returned by render when no address survives the send-time checks.
var errNoRecipient = errors.New("no recipient")

// maxAddress is the longest mailbox RFC 5321 allows.
const maxAddress = 254

// validAddress reports whether s is one bare mailbox: net/mail must parse it and return it unchanged,
// which rules out a display name, a list, a group and a comment, and it carries no whitespace, control
// character, separator or angle bracket that could add a header or an envelope recipient.
func validAddress(s string) bool {
	if s == "" || len(s) > maxAddress || strings.ContainsAny(s, ",;<>") || strings.IndexFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) >= 0 {
		return false
	}
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s && a.Name == ""
}

// mailRecipients is the static list followed by the resolved notify targets, each re-checked, with
// duplicates dropped. An address that fails is skipped and never reaches a header or the envelope.
func mailRecipients(static []string, resolved []recipient) []string {
	var out []string
	add := func(a string) {
		if validAddress(a) && !containsFold(out, a) {
			out = append(out, a)
		}
	}
	for _, a := range static {
		add(a)
	}
	for _, r := range resolved {
		add(r.Email)
	}
	return out
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

// renderMail builds the message for one row: headers from validated fields, then a plain-text body
// from the same allowlisted lines as the chat bodies. It has no Date header; send adds that. The To
// header is the envelope: send reads its recipients back from it.
func (ch Channel) renderMail(deliveryID uuid.UUID, msg message, resolved []recipient) ([]byte, error) {
	to := mailRecipients(ch.To, resolved)
	if len(to) == 0 {
		return nil, errNoRecipient
	}
	var body strings.Builder
	body.WriteString(msg.Title + "\n\n")
	for _, r := range msg.lines(func(s string) string { return s }) {
		body.WriteString(r[0] + ": " + r[1] + "\n")
	}
	if msg.ConsoleURL != "" {
		body.WriteString("\nOpen approvals: " + msg.ConsoleURL + "\n")
	}
	var b bytes.Buffer
	b.WriteString("From: " + ch.From + "\r\n")
	b.WriteString("To: " + strings.Join(to, ", ") + "\r\n")
	b.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", "[Wardyn] "+msg.Title) + "\r\n")
	b.WriteString("Message-ID: <" + deliveryID.String() + ch.From[strings.LastIndex(ch.From, "@"):] + ">\r\n")
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
	qp := quotedprintable.NewWriter(&b)
	_, _ = qp.Write([]byte(body.String()))
	if err := qp.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// sendMail delivers a rendered message over verified STARTTLS. Nothing is authenticated or sent until
// the relay has advertised STARTTLS and its certificate has verified against the delivery client's
// roots (the system roots plus WARDYN_TRUSTED_CA_FILE) for the configured host. net/smtp's PlainAuth is
// used only after the upgrade, so its own localhost exemption is never reached.
func (ch Channel) sendMail(ctx context.Context, client *http.Client, body []byte, now time.Time) result {
	m, err := mail.ReadMessage(bytes.NewReader(body))
	if err != nil {
		return result{class: classStore}
	}
	to, err := m.Header.AddressList("To")
	if err != nil || len(to) == 0 {
		return result{class: classNoRecipient}
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(ch.Host, strconv.Itoa(ch.Port)))
	if err != nil {
		return classify(err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	c, err := smtp.NewClient(conn, ch.Host)
	if err != nil {
		_ = conn.Close()
		return smtpResult(err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Hello("localhost"); err != nil {
		return smtpResult(err)
	}
	if ok, _ := c.Extension("STARTTLS"); !ok {
		return result{class: classSTARTTLSMissing, retryable: true}
	}
	if err := c.StartTLS(ch.tlsConfig(client)); err != nil {
		return smtpResult(err)
	}
	if ch.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", ch.Username, ch.Password, ch.Host)); err != nil {
			return smtpResult(err)
		}
	}
	if err := c.Mail(ch.From); err != nil {
		return smtpResult(err)
	}
	for _, a := range to {
		if err := c.Rcpt(a.Address); err != nil {
			return smtpResult(err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return smtpResult(err)
	}
	if _, err := w.Write(append([]byte("Date: "+now.UTC().Format(time.RFC1123Z)+"\r\n"), body...)); err != nil {
		return smtpResult(err)
	}
	if err := w.Close(); err != nil {
		return smtpResult(err)
	}
	_ = c.Quit() // the relay has accepted the message; a failed QUIT changes nothing
	return result{}
}

// tlsConfig is a copy of the delivery client's TLS config (which boot has given the trusted CA) with
// verification pinned on: the server name is the configured host, nothing is skippable and no ALPN is
// offered.
func (ch Channel) tlsConfig(client *http.Client) *tls.Config {
	cfg := &tls.Config{}
	if tr, ok := client.Transport.(*http.Transport); ok && tr.TLSClientConfig != nil {
		cfg = tr.TLSClientConfig.Clone()
	}
	cfg.ServerName = ch.Host
	cfg.InsecureSkipVerify = false
	cfg.VerifyConnection = nil
	cfg.VerifyPeerCertificate = nil
	cfg.NextProtos = nil
	cfg.MinVersion = tls.VersionTLS12
	return cfg
}

// smtpResult reduces an error to a class: a relay's reply to its code (4xx retries, 5xx does not),
// anything else through classify. The reply text and the error's own text are never formatted.
func smtpResult(err error) result {
	var te *textproto.Error
	if errors.As(err, &te) {
		return result{class: "smtp_reply:" + strconv.Itoa(te.Code), retryable: te.Code >= 400 && te.Code < 500}
	}
	return classify(err)
}
