// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package httpkv holds the HTTP rules the vaultkv and azurekv clients share, so
// a fix to the redirect origin rule or the transient-error test reaches both.
// It sees status codes and header values only, never a request or response
// body, so a secret cannot reach an error or a log line through it.
package httpkv

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Transient reports whether status is worth a retry: a transport failure
// (status 0), a 429 or a 5xx.
func Transient(status int) bool {
	return status == 0 || status == http.StatusTooManyRequests || status >= 500
}

// Loopback reports whether host is localhost or a loopback IP literal.
func Loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// SameHost compares two host names as DNS does: ASCII letters fold and nothing
// else does. strings.EqualFold would also take a Kelvin sign for a k.
func SameHost(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// EffectivePort is u's port, or its scheme's default when it names none.
func EffectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "http" {
		return "80"
	}
	return "443"
}

// RetryAfter is the wait h's Retry-After asks for, in whole seconds and capped
// at limit; zero when the header is absent, not a positive integer, or an HTTP
// date.
func RetryAfter(h http.Header, limit time.Duration) time.Duration {
	s, err := strconv.Atoi(h.Get("Retry-After"))
	if err != nil || s <= 0 {
		return 0
	}
	return min(time.Duration(s)*time.Second, limit)
}

// Retry calls try until it reports no further attempt is wanted, retries
// retries have been spent, or ctx ends. Between attempts it waits backoff
// doubled per attempt, or after when that is longer. It reports whether the
// final answer followed a retry: an earlier attempt may have landed unseen.
func Retry(ctx context.Context, retries int, backoff time.Duration, try func() (again bool, after time.Duration)) (retried bool) {
	for attempt := 0; ; attempt++ {
		again, after := try()
		if !again || attempt == retries {
			return attempt > 0
		}
		select {
		case <-ctx.Done():
			return attempt > 0
		case <-time.After(max(backoff<<attempt, after)):
		}
	}
}
