// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package subscription yields the operator's LIVE Anthropic subscription OAuth
// access token from the resident ~/.claude credentials, so the egress proxy can
// inject a fresh token per request instead of the sandbox holding a COPY that
// goes stale (access-token expiry + refresh-token rotation lock the copy out).
//
// Single-owner discipline: only the resident `claude` binary ever refreshes and
// rotates the token (it owns the atomic write-back and coordinates with the
// operator's other claude sessions). This provider only ever READS the file, and
// on the rare near-expiry path DELEGATES the refresh to `claude` — it never
// reimplements Anthropic's undocumented OAuth refresh_token flow.
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
	"time"
)

const (
	// defaultRefreshMargin: treat a token expiring within this window as needing
	// a refresh. Kept slightly wider than the proxy injector's re-resolve margin
	// so that when the injector asks at expiresAt-margin, this provider still
	// sees "within margin" and returns a freshly-refreshed token (no thrash).
	defaultRefreshMargin = 10 * time.Minute
	// defaultRefreshTimeout bounds the delegated `claude` refresh invocation.
	defaultRefreshTimeout = 120 * time.Second
)

// Token is a live subscription access token and its expiry.
type Token struct {
	Value     string
	ExpiresAt time.Time
}

// Provider yields the operator's current Anthropic subscription access token.
type Provider interface {
	Current(ctx context.Context) (Token, error)
	// Peek returns the resident token WITHOUT refreshing or delegating to `claude`
	// (read-only). It is for status/provenance surfaces that must not trigger a
	// refresh side effect; unlike Current it does NOT reject an expired token — the
	// expiry is returned for the caller to interpret against its own clock.
	Peek() (Token, error)
}

// Config configures the resident-credentials provider.
type Config struct {
	// CredPath is the path to the resident credentials file. Empty defaults to
	// ~/.claude/.credentials.json.
	CredPath string
	// ClaudeBin is the resident CLI used to delegate a refresh. Empty defaults
	// to "claude" (resolved against PATH).
	ClaudeBin string
	// RefreshMargin / RefreshTimeout override the defaults above (0 = default).
	RefreshMargin  time.Duration
	RefreshTimeout time.Duration
	// Now is overridable in tests; defaults to time.Now.
	Now func() time.Time
}

type provider struct {
	credPath  string
	claudeBin string
	margin    time.Duration
	refreshTO time.Duration
	now       func() time.Time
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
	margin := cfg.RefreshMargin
	if margin <= 0 {
		margin = defaultRefreshMargin
	}
	to := cfg.RefreshTimeout
	if to <= 0 {
		to = defaultRefreshTimeout
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &provider{credPath: credPath, claudeBin: bin, margin: margin, refreshTO: to, now: now}, nil
}

// Current returns the live subscription access token. It piggybacks on the
// resident token when it is comfortably unexpired (the common case — the
// operator's own `claude` keeps it fresh); otherwise it delegates a refresh to
// the resident `claude` and re-reads. Fails closed if no valid token can be
// obtained (never returns an expired token).
func (p *provider) Current(ctx context.Context) (Token, error) {
	tok, err := p.read()
	if err == nil && tok.Value != "" && tok.ExpiresAt.After(p.now().Add(p.margin)) {
		return tok, nil // piggyback: fresh enough
	}

	// Near/at expiry (or unreadable): delegate the refresh to the resident
	// claude, which rotates + writes back the token, then re-read.
	if rerr := p.delegateRefresh(); rerr != nil {
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

// delegateRefresh runs a minimal read-only `claude` turn to force an
// authenticated request, which refreshes + rotates the resident token as a side
// effect (claude owns the write-back). ANTHROPIC_API_KEY is scrubbed so claude
// uses the subscription session, never an API key.
func (p *provider) delegateRefresh() error {
	// Bind the refresh subprocess to a FRESH background context, NOT the caller's
	// request ctx. The caller here is the /internal/injection HTTP handler, whose
	// client (wardyn-proxy) times out at seconds; deriving the subprocess deadline
	// from that request ctx meant a client give-up SIGKILLed `claude` mid
	// credential-write (corrupting the resident token) and made the full
	// refreshTO budget unreachable. Detached, the refresh runs to completion up to
	// refreshTO regardless of whether the original caller is still waiting.
	ctx, cancel := context.WithTimeout(context.Background(), p.refreshTO)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.claudeBin, //nolint:gosec // operator-configured CLI path
		"-p", "ok", "--permission-mode", "plan", "--max-turns", "1", "--output-format", "json")
	cmd.Env = scrubAPIKey(os.Environ())
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

// scrubAPIKey returns env with ANTHROPIC_API_KEY removed so the resident claude
// authenticates with the subscription session (mirrors the composer backend).
func scrubAPIKey(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
