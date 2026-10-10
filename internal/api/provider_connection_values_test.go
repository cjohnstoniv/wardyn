// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestProviderCause_UnusableStoredValuesAllowReconnect(t *testing.T) {
	for _, tc := range []struct {
		name   string
		kind   types.ModelProviderKind
		value  string
		action string
	}{
		{"empty key", types.ModelProviderAnthropicAPIKey, "", providerAccessAddKeyAction},
		{"malformed subscription", types.ModelProviderAnthropicSubscription, "invalid-json", providerAccessSignInClaude},
		{"empty subscription", types.ModelProviderAnthropicSubscription, `{}`, providerAccessSignInClaude},
		{"malformed SSO", types.ModelProviderBedrockSSO, "invalid-json", modelAccessSignInAction},
		{"wrong SSO type", types.ModelProviderBedrockSSO, `{"access_token":42}`, modelAccessSignInAction},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, sec := newSecretsHarness(t)
			p := paKeyProvider("provider", tc.kind)
			if err := sec.For(paOwner).Put(t.Context(), providerCredentialName(p), []byte(tc.value)); err != nil {
				t.Fatal(err)
			}
			got := h.srv.providerAccessFor(t.Context(), p, paOwner)
			if got.State != modelAccessNotConfigured || got.Cause == "store_unreadable" || got.Action != tc.action {
				t.Fatalf("stored unusable value: %+v", got)
			}
		})
	}
}
