// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

type pinPostureStore struct {
	sc  types.SiteConfig
	err error
}

func (s pinPostureStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return s.sc, s.err
}

func perUserRoster(account string) types.SiteConfig {
	row := types.AgentProvider{
		ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO,
		CredentialSource: types.CredentialSourcePerUser,
		SSOStartURL:      "https://my-sso.awsapps.com/start",
	}
	if account != "" {
		row.SSOAccountID, row.SSORoleName = account, "BedrockRunner"
	}
	return types.SiteConfig{AgentProviders: &types.AgentProviders{Agents: []types.AgentProvider{row}}}
}

// TestWarnBedrockSSOPinPosture: the deployment where a sandbox-chosen account and
// role are stored unchecked must warn at boot — the only other warning there
// (BedrockModelARNNamesNoAccount) fires exclusively for a model that looks like an
// ARN.
func TestWarnBedrockSSOPinPosture(t *testing.T) {
	const bare = "us.anthropic.claude-sonnet-4-5-20250929-v1:0"
	const arn = "arn:aws:bedrock:us-east-1:111111111111:inference-profile/us.anthropic.claude-v1:0"
	for name, c := range map[string]struct {
		st       pinPostureStore
		model    string
		wantWarn bool
	}{
		"unpinned row, bare model":    {st: pinPostureStore{sc: perUserRoster("")}, model: bare, wantWarn: true},
		"unpinned row, account ARN":   {st: pinPostureStore{sc: perUserRoster("")}, model: arn},
		"pinned row, bare model":      {st: pinPostureStore{sc: perUserRoster("111111111111")}, model: bare},
		"no roster":                   {st: pinPostureStore{}, model: bare},
		"roster read failure is mute": {st: pinPostureStore{err: errors.New("pg down")}, model: bare},
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			warnBedrockSSOPinPosture(context.Background(), c.st, c.model)

			warned := strings.Contains(buf.String(), "pins no account/role")
			if warned != c.wantWarn {
				t.Errorf("warned = %v, want %v; log = %s", warned, c.wantWarn, buf.String())
			}
		})
	}
}
