// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"
)

// Failure classes. These, and nothing a server or the URL stack wrote, are what reaches last_error,
// the audit row, the log and the metrics: a Go *url.Error prints the whole URL, which for a chat
// webhook carries its credential.
const (
	classTimeout         = "timeout"
	classTLSVerify       = "tls_verify"
	classDial            = "dial"
	classRedirectRefused = "redirect_refused"
	classExpired         = "expired"
	classUnknownChannel  = "unknown_channel"
	classStore           = "store_error"
)

func classHTTPStatus(code int) string { return "http_status:" + strconv.Itoa(code) }

// result is the outcome of one send: an empty class is success.
type result struct {
	class     string
	retryable bool
}

// send delivers one rendered body. The body is built by the caller; the signature covers its exact
// bytes. Only the type this build implements is reachable: Parse refuses the rest at boot.
func (ch Channel) send(ctx context.Context, client *http.Client, deliveryID string, body []byte, now time.Time) result {
	if ch.Type == TypeSMTP {
		return ch.sendMail(ctx, client, body, now)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ch.URL, bytes.NewReader(body))
	if err != nil {
		return result{class: classDial}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "wardyn-approval-notify")
	req.Header.Set("X-Wardyn-Delivery", deliveryID)
	if ch.HMACSecret != "" {
		req.Header.Set("X-Wardyn-Signature", Sign(ch.HMACSecret, now.Unix(), body))
	}
	if ch.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+ch.BearerToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return classify(err)
	}
	_ = resp.Body.Close() // the body is never read: it is never stored or logged
	code := resp.StatusCode
	switch {
	case code >= 200 && code < 300:
		return result{}
	case code >= 300 && code < 400:
		return result{class: classRedirectRefused}
	case code == http.StatusRequestTimeout, code == http.StatusTooManyRequests, code >= 500:
		return result{class: classHTTPStatus(code), retryable: true}
	}
	return result{class: classHTTPStatus(code)}
}

// classify reduces a transport error to a class without ever formatting it.
func classify(err error) result {
	var (
		unknownAuth x509.UnknownAuthorityError
		hostname    x509.HostnameError
		invalid     x509.CertificateInvalidError
		verify      *tls.CertificateVerificationError
		netErr      net.Error
	)
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return result{class: classTimeout, retryable: true}
	case errors.As(err, &unknownAuth), errors.As(err, &hostname), errors.As(err, &invalid), errors.As(err, &verify):
		return result{class: classTLSVerify, retryable: true}
	}
	return result{class: classDial, retryable: true}
}

// newClient builds the delivery client from a clone of http.DefaultTransport, which boot has already
// given the trusted CA and any daemon proxy, so nothing here mutates the shared TLS config. A 3xx is
// returned rather than followed: the body, the signature and a URL-embedded credential must never be
// handed to a host the operator did not name.
func newClient() *http.Client {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{}
	}
	return &http.Client{
		Transport:     base.Clone(),
		Timeout:       sendTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
