// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package subscription

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// countingClaude writes a fake `claude` that appends one line to a counter file
// on every invocation and then writes body to credPath, mimicking the resident
// CLI refreshing and writing the token back. It returns the counter path.
func countingClaude(t *testing.T, dir, credPath, body string) (bin, counter string) {
	t.Helper()
	counter = filepath.Join(dir, "invocations")
	bin = filepath.Join(dir, "claude")
	// The counter append and the credentials write are separate shell commands
	// on purpose: the count is taken BEFORE the write, so a second invocation
	// racing the first is counted even if its write is a no-op.
	script := fmt.Sprintf("#!/bin/sh\necho x >> %q\n", counter)
	if body != "" {
		script += fmt.Sprintf("cat > %q <<'EOF'\n%s\nEOF\n", credPath, body)
	}
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, counter
}

func invocations(t *testing.T, counter string) int {
	t.Helper()
	b, err := os.ReadFile(counter)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(strings.Fields(string(b)))
}

// B11a-F7. delegateRefresh had no single-flight: the proxy single-flights per
// HOST (inject.go's reMu), so N runs each POSTing /internal/injection inside the
// 10-minute margin arrived here as N concurrent refreshes, each spawning its own
// `claude -p ok` — N processes writing the ONE resident ~/.claude credentials
// file, which is the sharper harm than the stampede itself. One refresh must
// serve them all.
func TestCurrent_ConcurrentRefresh_DelegatesOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fake-claude is POSIX")
	}
	now := time.Unix(1_700_000_000, 0)
	dir := t.TempDir()
	cred := filepath.Join(dir, ".credentials.json")
	writeCreds(t, cred, "stale-token", now.Add(time.Minute)) // inside the 10m margin

	fresh := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"refreshed-token","refreshToken":"rt2","expiresAt":%d,"subscriptionType":"max"}}`,
		now.Add(4*time.Hour).UnixMilli())
	bin, counter := countingClaude(t, dir, cred, fresh)

	p := newTestProvider(t, cred, bin, now)

	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, n)
	toks := make(chan string, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			tok, err := p.Current(context.Background())
			errs <- err
			toks <- tok.Value
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	close(toks)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Current: %v", err)
		}
	}
	for v := range toks {
		if v != "refreshed-token" {
			t.Fatalf("token = %q, want refreshed-token — every caller must get the ONE refresh's result", v)
		}
	}
	if got := invocations(t, counter); got != 1 {
		t.Fatalf("claude invocations = %d, want 1 — %d concurrent Current() calls must share one delegated refresh", got, n)
	}
}

// A refresh that FAILS must not be re-attempted by every caller piled up behind
// it: without a short negative cache the re-read under the lock still sees a
// stale token, so each waiter would spend its own (failing, and on the timeout
// arm 120-second) `claude` turn.
func TestCurrent_ConcurrentRefresh_FailureIsNegativeCached(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fake-claude is POSIX")
	}
	now := time.Unix(1_700_000_000, 0)
	dir := t.TempDir()
	cred := filepath.Join(dir, ".credentials.json")
	writeCreds(t, cred, "stale-token", now.Add(time.Minute))

	// Counts, writes nothing, fails: the resident claude that cannot sign in.
	counter := filepath.Join(dir, "invocations")
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(fmt.Sprintf("#!/bin/sh\necho x >> %q\nexit 1\n", counter)), 0o700); err != nil {
		t.Fatal(err)
	}

	p := newTestProvider(t, cred, bin, now)

	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _ = p.Current(context.Background())
		}()
	}
	close(start)
	wg.Wait()

	if got := invocations(t, counter); got != 1 {
		t.Fatalf("claude invocations = %d, want 1 — a just-failed refresh must be negative-cached, not retried per caller", got)
	}
}

// NEGATIVE CONTROL for B11a-F7: a SINGLE caller is unchanged — it still
// delegates, still gets the refreshed token, and the single-flight adds no
// extra invocation of its own.
func TestCurrent_SingleCaller_StillDelegatesExactlyOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fake-claude is POSIX")
	}
	now := time.Unix(1_700_000_000, 0)
	dir := t.TempDir()
	cred := filepath.Join(dir, ".credentials.json")
	writeCreds(t, cred, "stale-token", now.Add(time.Minute))

	fresh := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"refreshed-token","refreshToken":"rt2","expiresAt":%d,"subscriptionType":"max"}}`,
		now.Add(4*time.Hour).UnixMilli())
	bin, counter := countingClaude(t, dir, cred, fresh)

	p := newTestProvider(t, cred, bin, now)
	tok, err := p.Current(context.Background())
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if tok.Value != "refreshed-token" {
		t.Fatalf("token = %q, want refreshed-token", tok.Value)
	}
	if got := invocations(t, counter); got != 1 {
		t.Fatalf("claude invocations = %d, want 1 for a single caller", got)
	}
}
