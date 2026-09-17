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

// The kind SSO walk (ui/e2e/live/sso-member.spec.ts) PUTs the agent roster from
// TypeScript, so nothing in the Go build ever type-checked that body — and the
// first version sent `agents` as an OBJECT keyed by agent id. AgentProviders.Agents
// is a LIST whose elements carry `id`, and handlePutAgentProviders decodes
// strictly, so that body was a 400 on the walk's very first assertion: every test
// after it was unreachable.
//
// This is the cheapest possible pin for a cross-language shape: decode the exact
// literal the spec sends into the exact type the handler decodes into. The
// SERVER side of the contract is already covered (TestAgentProvidersPut); what
// was missing was anything at all tying the spec's spelling to it.

// walkRosterBody is the body ui/e2e/live/sso-member.spec.ts sends, verbatim.
// Kept in sync by TestWalkRosterBodyMatchesTheSpec below, which greps the spec
// for the fields rather than trusting this copy.
const walkRosterBody = `{"agents":[{"id":"claude-code","mechanism":"bedrock_sso",` +
	`"credential_source":"per_user","sso_start_url":"https://wardyn-dev.awsapps.com/start",` +
	`"sso_account_id":"222222222222","sso_role_name":"WardynDev"}]}`

func TestWalkRosterBodyDecodes(t *testing.T) {
	var got types.AgentProviders
	dec := json.NewDecoder(strings.NewReader(walkRosterBody))
	dec.DisallowUnknownFields() // the handler decodes strictly; so does this
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("the walk's roster body does not decode into types.AgentProviders: %v\nbody: %s", err, walkRosterBody)
	}
	if len(got.Agents) != 1 {
		t.Fatalf("decoded %d rows, want exactly 1: %+v", len(got.Agents), got.Agents)
	}
	row := got.Agents[0]
	if row.ID != "claude-code" {
		t.Errorf("row id = %q, want claude-code — a row with no id names no agent", row.ID)
	}
	if row.Mechanism != types.AgentMechanismBedrockSSO {
		t.Errorf("mechanism = %q, want %q", row.Mechanism, types.AgentMechanismBedrockSSO)
	}
	if row.CredentialSource != types.CredentialSourcePerUser {
		t.Errorf("credential_source = %q, want %q — the walk's whole subject is the per-user lane", row.CredentialSource, types.CredentialSourcePerUser)
	}
	if row.SSOStartURL == "" || row.SSOAccountID == "" || row.SSORoleName == "" {
		t.Errorf("the pin is incomplete: %+v — start URL, account and role are what the walk's /_seen assertion measures against", row)
	}
}

// TestWalkRosterBodyMatchesTheSpec keeps the literal above honest: if the spec
// ever goes back to an object, or drops `id`, this reds instead of the walk
// dying at its first assertion on a cluster somebody spent ten minutes standing up.
func TestWalkRosterBodyMatchesTheSpec(t *testing.T) {
	// The roster PUT lives in the walk's shared helpers since 0.7.5 (putRoster
	// moved out of sso-member.spec.ts when the recovery spec began sending the
	// same body), so BOTH live files are read: the body must exist in one of
	// them, and the object-shaped mistake must exist in neither.
	var spec string
	for _, f := range []string{"../../ui/e2e/live/helpers.ts", "../../ui/e2e/live/sso-member.spec.ts"} {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read the live walk source %s: %v", f, err)
		}
		spec += string(raw) + "\n"
	}
	for _, want := range []string{
		`agents: [`,         // a LIST, never an object keyed by agent id
		`id: "claude-code"`, // …whose element names its agent
		`mechanism: "bedrock_sso"`,
		`credential_source: "per_user"`,
		`sso_account_id:`,
		`sso_role_name:`,
	} {
		if !strings.Contains(spec, want) {
			t.Errorf("the walk spec no longer contains %q — the roster PUT it sends may not decode into types.AgentProviders (see walkRosterBody)", want)
		}
	}
	if strings.Contains(spec, `agents: { "claude-code"`) {
		t.Error(`the walk spec sends agents as an OBJECT keyed by agent id; AgentProviders.Agents is a LIST and the handler decodes strictly — that body is a 400`)
	}
}
