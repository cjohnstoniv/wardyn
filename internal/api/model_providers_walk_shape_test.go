// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The kind SSO walk (ui/e2e/walk/helpers.ts putProvider) PUTs the model
// providers block from TypeScript, so nothing in the Go build ever
// type-checks that body — and the walk's first roster body once sent `agents`
// as an object where the handler decodes a list, a 400 on the walk's very
// first assertion that left every test after it unreachable.
//
// This is the cheapest possible pin for a cross-language shape: decode the
// exact literal the walk sends into the exact type the handler decodes into,
// and run the handler's own validation over it. The SERVER side of the
// contract is covered by the /model-providers handler tests; this ties the
// walk's spelling to it.

// walkProviderBody is the body ui/e2e/walk/helpers.ts sends, with the walk's
// default inputs. Kept in sync by TestWalkProviderBodyMatchesTheWalk below,
// which greps the walk for the fields rather than trusting this copy.
const walkProviderBody = `{"providers":[{"id":"bedrock-sso","name":"Amazon Bedrock (AWS sign-in)","kind":"bedrock_sso",` +
	`"bedrock":{"region":"us-east-1","sso_start_url":"https://wardyn-dev.awsapps.com/start",` +
	`"sso_account_id":"222222222222","sso_role_name":"WardynDev"},` +
	`"harnesses":[{"harness":"claude-code","model":"us.anthropic.claude-sonnet-4-5-20250929-v1:0"}]}]}`

func TestWalkProviderBodyDecodes(t *testing.T) {
	var got types.ModelProviders
	dec := json.NewDecoder(strings.NewReader(walkProviderBody))
	dec.DisallowUnknownFields() // the handler decodes strictly; so does this
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("the walk's provider body does not decode into types.ModelProviders: %v\nbody: %s", err, walkProviderBody)
	}
	if _, err := validateModelProviders(&got, providerWriteEnv{}); err != nil {
		t.Fatalf("the walk's provider body is refused by validateModelProviders: %v", err)
	}
	if len(got.Providers) != 1 {
		t.Fatalf("decoded %d providers, want exactly 1: %+v", len(got.Providers), got.Providers)
	}
	p := got.Providers[0]
	if p.Kind != types.ModelProviderBedrockSSO || !p.Serves("claude-code") {
		t.Errorf("provider = %+v, want a bedrock_sso provider serving claude-code — the walk's whole subject", p)
	}
	if b := p.Bedrock; b == nil || b.SSOStartURL == "" || b.SSOAccountID == "" || b.SSORoleName == "" {
		t.Errorf("the pin is incomplete: %+v — start URL, account and role are what the walk's /_seen assertion measures against", p.Bedrock)
	}
}

// TestWalkProviderBodyMatchesTheWalk keeps the literal above honest: if the
// walk drops a field or goes back to the retired roster door, this reds instead
// of the walk dying at its first assertion on a cluster somebody spent ten
// minutes standing up.
func TestWalkProviderBodyMatchesTheWalk(t *testing.T) {
	raw, err := os.ReadFile("../../ui/e2e/walk/helpers.ts")
	if err != nil {
		t.Fatalf("read the live walk source: %v", err)
	}
	walk := string(raw)
	for _, want := range []string{
		`"/api/v1/model-providers"`,
		`providers: [`, // a LIST whose elements carry id
		`kind: "bedrock_sso"`,
		`sso_start_url:`,
		`sso_account_id:`,
		`sso_role_name:`,
		`harness: "claude-code"`,
	} {
		if !strings.Contains(walk, want) {
			t.Errorf("the walk no longer contains %q — the provider PUT it sends may not decode into types.ModelProviders (see walkProviderBody)", want)
		}
	}
	if strings.Contains(walk, "/api/v1/agent-providers") && strings.Contains(walk, "mechanism:") {
		t.Error("the walk still writes a roster mechanism; the roster carries no model credential since #548")
	}
}
