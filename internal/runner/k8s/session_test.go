// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"sync"
	"testing"
)

// TestShQuote covers the POSIX single-quote escaping recipe directly: wrap
// in single quotes, and an embedded single quote is closed, backslash-quote,
// reopened.
func TestShQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"simple", "'simple'"},
		{"it's", `'it'\''s'`},
		{"has spaces", "'has spaces'"},
		{"", "''"},
		{"a'b'c", `'a'\''b'\''c'`},
	}
	for _, tc := range cases {
		if got := shQuote(tc.in); got != tc.want {
			t.Errorf("shQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestEnvWrapScript is M5's table test: embedded single-quote, spaces, and
// an entry with no '=' (a malformed KEY=VALUE pair, passed through quoted
// rather than dropped or panicking).
func TestEnvWrapScript(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		argv []string
		want string
	}{
		{
			name: "simple",
			env:  []string{"FOO=bar"},
			argv: []string{"echo", "hi"},
			want: "export FOO='bar'; exec 'echo' 'hi'",
		},
		{
			name: "embedded single quote in value",
			env:  []string{"FOO=it's"},
			argv: []string{"echo"},
			want: `export FOO='it'\''s'; exec 'echo'`,
		},
		{
			name: "spaces in value and argv",
			env:  []string{"MSG=hello world"},
			argv: []string{"echo", "a b"},
			want: "export MSG='hello world'; exec 'echo' 'a b'",
		},
		{
			name: "entry with no =",
			env:  []string{"MALFORMED"},
			argv: []string{"true"},
			want: "export 'MALFORMED'; exec 'true'",
		},
		{
			name: "multiple env vars",
			env:  []string{"A=1", "B=2"},
			argv: []string{"cmd"},
			want: "export A='1' B='2'; exec 'cmd'",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := envWrapScript(tc.env, tc.argv); got != tc.want {
				t.Errorf("envWrapScript(%v, %v) = %q, want %q", tc.env, tc.argv, got, tc.want)
			}
		})
	}
}

// TestTermSizeQueue_ConcurrentPushStop is M5's race test: concurrent
// pushers racing a stop must never panic — push must stay panic-safe even
// after (or during) a concurrent Close, which is exactly why stop signals
// via a separate done channel instead of closing the data channel outright.
// Run under -race.
func TestTermSizeQueue_ConcurrentPushStop(t *testing.T) {
	q := newTermSizeQueue()
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := uint16(1); i < 100; i++ {
			q.push(i, i)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			q.Next()
		}
	}()
	go func() {
		defer wg.Done()
		q.stop()
	}()
	wg.Wait()

	// A push (and a redundant stop) after the race above settled must still
	// never panic.
	q.push(1, 1)
	q.stop()
}

// TestTermSizeQueue_NextNilAfterStop is the deterministic (non-concurrent)
// half of the stop contract: with nothing racing it, Next() reliably
// observes "stopped" and returns nil — the TerminalSizeQueue contract for
// "monitoring has stopped".
func TestTermSizeQueue_NextNilAfterStop(t *testing.T) {
	q := newTermSizeQueue()
	q.stop()
	if got := q.Next(); got != nil {
		t.Errorf("Next() after stop with nothing pending = %v, want nil", got)
	}
}
