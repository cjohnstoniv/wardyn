// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build live

package testlive

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveBedrock (LL3): one Claude Haiku 4.5 call on Identity Center role
// credentials for the capped member account, then the reply is checked.
func TestLiveBedrock(t *testing.T) {
	Require(t, EnvBedrock, EnvBedrockAccount, EnvBedrockRole, EnvBedrockRegion, EnvSSORegion, EnvSSOTokenFile)
	cfg, err := LoadBedrockConfig(os.Getenv)
	if err != nil {
		Fatalf(t, "%v", err)
	}
	token, err := LoadSSOToken(time.Now())
	if errors.Is(err, ErrTokenExpired) {
		// A stale credential is a broken invocation once WARDYN_LIVE_BEDROCK=1
		// has been opted into, not an absence of intent (#463): Fatalf, not
		// Skipf, so this can never print `ok` on a run asked to prove something.
		Fatalf(t, "live: %v. Sign in again with `aws sso login` on the member-account profile, then re-run (docs/LIVE-TESTS.md, LL3)", err)
	}
	if err != nil {
		Fatalf(t, "%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	hc := &http.Client{Timeout: time.Minute}
	b, err := NewMemberBedrock(ctx, cfg, token, DefaultEndpoints(cfg), hc)
	if err != nil {
		Fatalf(t, "%v", err)
	}
	reply, err := b.Converse(ctx, "Reply with the single word pong and nothing else.", 16)
	if err != nil {
		Fatalf(t, "%v", err)
	}
	Logf(t, "reply: %q", reply)
	if !strings.Contains(strings.ToLower(reply), "pong") {
		Errorf(t, "the reply does not contain pong: %q", reply)
	}
}
