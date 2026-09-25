// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The joined dispatch (#551): every door routes every kind through the same
// liveness check (providerLiveness) and dispatch through one lane
// (resolveProviderLane).

// TestRecordProviderChoice_ChecksEveryKindLive: a record session under a
// provider block checks the launcher's own credential by its provider's kind —
// a Claude sign-in for a subscription provider, never the key a key provider
// would need — and labels its model access by kind.
func TestRecordProviderChoice_ChecksEveryKindLive(t *testing.T) {
	const owner = "rec-owner@example.com"
	sub := subProvider("claude")
	sso := brSSOProvider()
	for _, tc := range []struct {
		name     string
		p        types.ModelProvider
		own      func(*memSecrets, types.ModelProvider)
		refusal  string // "" = chosen
		llmLabel string
	}{
		{"subscription, not signed in", sub, nil, mpSubNotSignedIn, ""},
		{"subscription, a KEY stored instead of a sign-in", sub, func(sec *memSecrets, p types.ModelProvider) {
			_ = sec.For(owner).Put(context.Background(), providerSecretName(p.UID, providerKeyPart), []byte("sk-not-a-sign-in"))
		}, mpSubNotSignedIn, ""},
		{"subscription, signed in", sub, func(sec *memSecrets, p types.ModelProvider) {
			_ = sec.For(owner).Put(context.Background(), providerSecretName(p.UID, providerOAuthPart), subBlob("tok"))
		}, "", "subscription"},
		{"bedrock sso, not signed in", sso, nil, mpBRNotSignedIn, ""},
		{"bedrock sso, signed in", sso, func(sec *memSecrets, p types.ModelProvider) {
			_ = sec.For(owner).Put(context.Background(), providerSecretName(p.UID, providerSSOPart),
				brBlob("tok", "123456789012", "BedrockUser", time.Now().Add(time.Hour)))
		}, "", "bedrock"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := providerRunFixture(t, types.SiteConfig{ModelProviders: providerBlock(tc.p)}, &capStore{}, nil)
			srv.cfg.AgentImages = map[string]string{"claude-code": "wardyn/agent-claude-code:local"}
			srv.cfg.Runner = nil
			if tc.own != nil {
				tc.own(srv.cfg.Secrets.(*memSecrets), tc.p)
			}
			choice, err := srv.recordProviderChoice(context.Background(), owner, types.Workspace{ID: uuid.New()})
			if tc.refusal != "" {
				if !errors.Is(err, errModelProviderRefused) || !strings.Contains(err.Error(), tc.refusal) {
					t.Fatalf("err = %v, want the model-provider refusal carrying %q", err, tc.refusal)
				}
				return
			}
			if err != nil || !choice.chosen || !choice.governs || choice.provider.ID != tc.p.ID {
				t.Fatalf("choice = %+v, %v, want %s chosen", choice, err, tc.p.ID)
			}
			label, _, minted, err := srv.recordSessionModelAccess(context.Background(), uuid.New(), time.Now(), &types.RunPolicySpec{},
				types.Workspace{}, choice, false)
			if err != nil || label != tc.llmLabel || len(minted) != 0 {
				t.Errorf("model access = %q, %d minted, %v; want %q and nothing minted here", label, len(minted), err, tc.llmLabel)
			}
		})
	}
}
