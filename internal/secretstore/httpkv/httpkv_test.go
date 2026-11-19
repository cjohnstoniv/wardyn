// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package httpkv

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestTransient(t *testing.T) {
	for status, want := range map[int]bool{0: true, 200: false, 404: false, 401: false, 403: false, 429: true, 500: true, 503: true} {
		if got := Transient(status); got != want {
			t.Errorf("Transient(%d) = %v, want %v", status, got, want)
		}
	}
}

func TestLoopback(t *testing.T) {
	for host, want := range map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true, "10.0.0.1": false, "vault.example": false, "LOCALHOST": false, "": false} {
		if got := Loopback(host); got != want {
			t.Errorf("Loopback(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestSameHost(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"Vault.Example", "vault.example", true},
		{"vault.example", "vault.example.", false},
		{"vault.example", "vault.exampl", false},
		{"Key.example", "key.example", false}, // Kelvin sign is not a k
		{"", "", true},
	} {
		if got := SameHost(c.a, c.b); got != c.want {
			t.Errorf("SameHost(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestEffectivePort(t *testing.T) {
	for raw, want := range map[string]string{
		"https://v.example":      "443",
		"https://v.example:443":  "443",
		"https://v.example:8200": "8200",
		"http://v.example":       "80",
		"http://v.example:8080":  "8080",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := EffectivePort(u); got != want {
			t.Errorf("EffectivePort(%s) = %s, want %s", raw, got, want)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	for value, want := range map[string]time.Duration{
		"":                              0,
		"0":                             0,
		"-3":                            0,
		"abc":                           0,
		"2":                             2 * time.Second,
		"3600":                          10 * time.Second,
		"Wed, 21 Oct 2099 07:28:00 GMT": 0,
	} {
		h := http.Header{}
		if value != "" {
			h.Set("Retry-After", value)
		}
		if got := RetryAfter(h, 10*time.Second); got != want {
			t.Errorf("RetryAfter(%q) = %v, want %v", value, got, want)
		}
	}
	if got := RetryAfter(nil, time.Second); got != 0 {
		t.Errorf("RetryAfter(nil) = %v, want 0", got)
	}
}

func TestRetry(t *testing.T) {
	t.Run("done first try", func(t *testing.T) {
		n := 0
		retried := Retry(context.Background(), 3, time.Millisecond, func() (bool, time.Duration) { n++; return false, 0 })
		if n != 1 || retried {
			t.Fatalf("calls = %d, retried = %v; want 1, false", n, retried)
		}
	})
	t.Run("succeeds after retries", func(t *testing.T) {
		n := 0
		retried := Retry(context.Background(), 3, time.Millisecond, func() (bool, time.Duration) { n++; return n < 3, 0 })
		if n != 3 || !retried {
			t.Fatalf("calls = %d, retried = %v; want 3, true", n, retried)
		}
	})
	t.Run("spends retries then stops", func(t *testing.T) {
		n := 0
		retried := Retry(context.Background(), 3, time.Millisecond, func() (bool, time.Duration) { n++; return true, 0 })
		if n != 4 || !retried {
			t.Fatalf("calls = %d, retried = %v; want 4, true", n, retried)
		}
	})
	t.Run("waits the longer of backoff and after", func(t *testing.T) {
		start := time.Now()
		n := 0
		Retry(context.Background(), 1, time.Millisecond, func() (bool, time.Duration) { n++; return n == 1, 50 * time.Millisecond })
		if d := time.Since(start); d < 50*time.Millisecond {
			t.Fatalf("waited %v, want at least 50ms", d)
		}
	})
	t.Run("stops when the context ends", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		n := 0
		retried := Retry(ctx, 3, time.Hour, func() (bool, time.Duration) { n++; cancel(); return true, 0 })
		if n != 1 || retried {
			t.Fatalf("calls = %d, retried = %v; want 1, false", n, retried)
		}
	})
}
