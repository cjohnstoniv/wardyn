// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package subscription yields the operator's LIVE Anthropic subscription OAuth
// access token from the resident ~/.claude credentials, so the egress proxy can
// inject a fresh token per request instead of the sandbox holding a COPY that
// goes stale.
//
// Single-owner discipline: only the resident `claude` binary ever refreshes and
// rotates the token. This provider only ever READS the file, and on the rare
// near-expiry path DELEGATES the refresh to `claude` — it never reimplements
// Anthropic's undocumented OAuth refresh_token flow.
package subscription

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/cliutil"
)

const (
	// defaultRefreshMargin: treat a token expiring within this window as
	// needing a refresh. Kept slightly wider than the proxy injector's
	// re-resolve margin so the two never thrash against each other.
	defaultRefreshMargin = 10 * time.Minute
	// defaultRefreshTimeout bounds the delegated `claude` refresh invocation.
	defaultRefreshTimeout = 120 * time.Second
	// refreshNegativeTTL is how long a FAILED delegated refresh is remembered,
	// so callers piling up behind it get the same answer instead of each
	// spending its own `claude` turn. Bounds only the FAILURE arm — a success
	// needs no timer, since the re-read under the lock sees the fresh token.
	refreshNegativeTTL = 30 * time.Second
)

// Token is a live subscription access token and its expiry.
type Token struct {
	Value     string
	ExpiresAt time.Time
}

// Provider yields the operator's current Anthropic subscription access token.
type Provider interface {
	Current(ctx context.Context) (Token, error)
	// Peek returns the resident token WITHOUT refreshing or delegating to
	// `claude` — for status/provenance surfaces that must not trigger a
	// refresh side effect. Unlike Current, it does NOT reject an expired token.
	Peek() (Token, error)
}

// Config configures the resident-credentials provider.
type Config struct {
	// CredPath is the path to the resident credentials file. Empty defaults to
	// ~/.claude/.credentials.json.
	CredPath string
	// ClaudeBin is the resident CLI that delegates a refresh. Empty defaults
	// to "claude" (resolved against PATH).
	ClaudeBin string
	// Now is overridable in tests; defaults to time.Now.
	Now func() time.Time
}

type provider struct {
	credPath  string
	claudeBin string
	margin    time.Duration
	refreshTO time.Duration
	now       func() time.Time

	// refreshMu serializes delegateRefresh across concurrent Current() callers
	// (B11a-F7), and guards the two fields below. See refreshOnce.
	refreshMu      sync.Mutex
	lastRefreshAt  time.Time
	lastRefreshErr error
}

// New builds a resident-credentials Provider. It does NOT verify the file or the
// binary at construction time (either may appear later); a missing token surfaces
// as a clear, fail-closed error on Current.
func New(cfg Config) (Provider, error) {
	credPath := strings.TrimSpace(cfg.CredPath)
	if credPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("subscription: resolve home for credentials path: %w", err)
		}
		credPath = filepath.Join(home, ".claude", ".credentials.json")
	}
	bin := strings.TrimSpace(cfg.ClaudeBin)
	if bin == "" {
		bin = "claude"
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &provider{credPath: credPath, claudeBin: bin, margin: defaultRefreshMargin, refreshTO: defaultRefreshTimeout, now: now}, nil
}

// Current returns the live subscription access token. It piggybacks on the
// resident token when comfortably unexpired; otherwise it delegates a refresh
// to the resident `claude` and re-reads. Fails closed (never returns an
// expired token).
func (p *provider) Current(ctx context.Context) (Token, error) {
	tok, err := p.read()
	if err == nil && tok.Value != "" && tok.ExpiresAt.After(p.now().Add(p.margin)) {
		return tok, nil // piggyback: fresh enough
	}

	// Near/at expiry (or unreadable): delegate the refresh to the resident
	// claude, which rotates + writes back the token, then re-read.
	if rerr := p.refreshOnce(); rerr != nil {
		if err != nil {
			return Token{}, fmt.Errorf("subscription token unavailable and refresh failed: read: %v; refresh: %w", err, rerr)
		}
		return Token{}, fmt.Errorf("subscription token near expiry and refresh failed: %w", rerr)
	}
	tok, err = p.read()
	if err != nil {
		return Token{}, fmt.Errorf("subscription token: re-read after refresh: %w", err)
	}
	if tok.Value == "" || !tok.ExpiresAt.After(p.now()) {
		return Token{}, errors.New("subscription token still expired after refresh; run `claude` on the host to sign in")
	}
	return tok, nil
}

// Peek reads the resident token without refreshing. It reuses read() so there is
// exactly one credentials parser (no duplicate parse in the status handler).
func (p *provider) Peek() (Token, error) {
	return p.read()
}

// credFile is the subset of ~/.claude/.credentials.json we parse. The refresh
// token is deliberately NOT read here — this process never handles it.
type credFile struct {
	ClaudeAiOauth struct {
		AccessToken string `json:"accessToken"`
		ExpiresAt   int64  `json:"expiresAt"` // unix milliseconds
	} `json:"claudeAiOauth"`
}

func (p *provider) read() (Token, error) {
	b, err := os.ReadFile(p.credPath)
	if err != nil {
		return Token{}, fmt.Errorf("read %s: %w", p.credPath, err)
	}
	var cf credFile
	if err := json.Unmarshal(b, &cf); err != nil {
		return Token{}, fmt.Errorf("parse credentials: %w", err)
	}
	o := cf.ClaudeAiOauth
	if o.AccessToken == "" {
		return Token{}, errors.New("no claudeAiOauth.accessToken in credentials (not signed in to a subscription?)")
	}
	return Token{Value: o.AccessToken, ExpiresAt: time.UnixMilli(o.ExpiresAt)}, nil
}

// refreshOnce is the single-flight in front of delegateRefresh: N runs
// arriving here as N concurrent refreshes would each spawn its own `claude
// -p ok`, all writing the ONE resident ~/.claude/.credentials.json. The
// mutex serializes them, and the re-read UNDER the lock turns serialization
// into single-flight (a waiter sees the winner's fresh token instead of
// spending its own turn). The negative cache covers the failure arm, where
// nothing lands on disk for the re-read to see.
//
// ponytail: a mutex rather than golang.org/x/sync/singleflight — the waiters
// here WANT to re-read the file the winner wrote rather than share its return
// value, which is the part singleflight would not give us, and it is an
// indirect dependency this package would be promoting to a direct one.
func (p *provider) refreshOnce() error {
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()

	// Someone else may have refreshed while we waited for the lock.
	if tok, err := p.read(); err == nil && tok.Value != "" && tok.ExpiresAt.After(p.now().Add(p.margin)) {
		return nil
	}
	if p.lastRefreshErr != nil && !p.lastRefreshAt.IsZero() && p.now().Sub(p.lastRefreshAt) < refreshNegativeTTL {
		return p.lastRefreshErr
	}
	err := p.delegateRefresh()
	p.lastRefreshAt, p.lastRefreshErr = p.now(), err
	return err
}

// delegateRefresh runs a minimal read-only `claude` turn to force an
// authenticated request, which refreshes + rotates the resident token as a side
// effect (claude owns the write-back). ANTHROPIC_API_KEY is scrubbed so claude
// uses the subscription session, never an API key.
func (p *provider) delegateRefresh() error {
	// Bind to a FRESH background context, NOT the caller's request ctx: a
	// client give-up on the short-lived HTTP caller would otherwise SIGKILL
	// `claude` mid credential-write, corrupting the resident token.
	ctx, cancel := context.WithTimeout(context.Background(), p.refreshTO)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.claudeBin, //nolint:gosec // operator-configured CLI path
		"-p", "ok", "--permission-mode", "plan", "--max-turns", "1", "--output-format", "json")
	cmd.Env = cliutil.ScrubChildEnv(os.Environ())
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("delegated refresh via %q timed out after %s", p.claudeBin, p.refreshTO)
		}
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		return fmt.Errorf("delegated refresh via %q failed: %w (%s)", p.claudeBin, err, truncate(msg, 200))
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
