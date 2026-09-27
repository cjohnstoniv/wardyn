// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// testAgentImages is the boot image map every case here validates against: one
// custom agent the catalog does not know, which is exactly the shape
// harnessByID documents as supported and a roster therefore has to be able to
// name.
var testAgentImages = map[string]string{"acme-agent": "ghcr.io/acme/agent:1"}

// agentNotEnabledTail is agent422NotEnabled's half after the agent name — what a
// body assertion can match without re-spelling the sentence. Derived from the
// constant, so the M2 canon swap moves both.
var agentNotEnabledTail = strings.SplitN(agent422NotEnabled, "%q", 2)[1]

func agentRow(id string, m types.AgentMechanism) types.AgentProvider {
	return types.AgentProvider{ID: id, Mechanism: m}
}

func agentBlock(rows ...types.AgentProvider) *types.AgentProviders {
	return &types.AgentProviders{Agents: rows}
}

// TestValidateAgentProviders is the write-boundary table: every rule §5c.4
// names, each as its own case, because these are the only thing standing
// between an admin's choice and a roster that renders a live credential chip
// over a lane nothing can honour.
func TestValidateAgentProviders(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block *types.AgentProviders
		model string // the boot Bedrock model this door validates against
		want  string // "" = accepted; otherwise a substring the refusal must carry
	}{
		{name: "a nil block is legacy open mode", block: nil},
		{name: "an empty block is valid (the clear form normalizes it away first)",
			block: &types.AgentProviders{}},
		{name: "claude-code on bedrock_sso", block: agentBlock(agentRow("claude-code", types.AgentMechanismBedrockSSO))},
		{name: "claude-code on the subscription lane",
			block: agentBlock(agentRow("claude-code", types.AgentMechanismAnthropicSubscription))},
		{name: "codex-cli on its own api key", block: agentBlock(agentRow("codex-cli", types.AgentMechanismOpenAIAPIKey))},
		{name: "the BYOA catalog row takes none", block: agentBlock(agentRow("none", types.AgentMechanismNone))},
		{name: "a custom image-map agent takes none", block: agentBlock(agentRow("acme-agent", types.AgentMechanismNone))},

		{name: "an agent this deployment cannot run", block: agentBlock(agentRow("ghost", types.AgentMechanismNone)),
			want: "names no agent"},
		{name: "a custom image with a model mechanism",
			block: agentBlock(agentRow("acme-agent", types.AgentMechanismBedrockSSO)),
			want:  "its mechanism must be none"},
		// THE FOLD: bedrock_bearer is one sub-lane of the coarse "bedrock" the
		// catalog is keyed by, and the refusal is the catalog's OWN reviewed
		// sentence — never a second opinion about what Codex CLI speaks.
		{name: "an impossible pair is refused with the catalog's verbatim reason",
			block: agentBlock(agentRow("codex-cli", types.AgentMechanismBedrockBearer)),
			want:  reasonXBedrockCodex},
		{name: "the other direction, folded the same way",
			block: agentBlock(agentRow("claude-code", types.AgentMechanismOpenAIAPIKey)),
			want:  reasonXOpenAIClaude},
		{name: "none on a catalog agent Wardyn wires a credential for",
			block: agentBlock(agentRow("claude-code", types.AgentMechanismNone)),
			want:  "name the lane you configured"},
		{name: "a lane on the BYOA row",
			block: agentBlock(agentRow("none", types.AgentMechanismBedrockSSO)),
			want:  "mechanism must be none"},
		{name: "an unknown mechanism", block: agentBlock(agentRow("claude-code", "bedrock_magic")),
			want: "is not a model-access mechanism"},
		{name: "an unknown credential source", block: agentBlock(types.AgentProvider{
			ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO, CredentialSource: "everyone",
		}), want: "is not a credential source"},
		{name: "duplicate ids", block: agentBlock(
			agentRow("claude-code", types.AgentMechanismBedrockSSO),
			agentRow("claude-code", types.AgentMechanismAnthropicAPIKey)),
			want: "is not unique"},

		// per_user names the two lanes whose credential the member actually
		// HOLDS: the AWS SSO session they sign in for, and the bedrock-api-key
		// they store under their own principal. bedrock_env is the daemon's own
		// environment and bedrock_static the operator's resident SigV4 keys —
		// operator-namespace reads both, so declaring either per_user would
		// promise one credential per person and serve the admin's.
		{name: "per_user off the two per-principal lanes", block: agentBlock(types.AgentProvider{
			ID: "claude-code", Mechanism: types.AgentMechanismBedrockEnv,
			CredentialSource: types.CredentialSourcePerUser,
		}), want: "per_user is available for bedrock_bearer and bedrock_sso only, not bedrock_env"},
		{name: "per_user bedrock_bearer is the lane #153 opened", block: agentBlock(types.AgentProvider{
			ID: "claude-code", Mechanism: types.AgentMechanismBedrockBearer,
			CredentialSource: types.CredentialSourcePerUser,
		})},
		// The portal and the pin stay bedrock_sso's ALONE. A per-user BEARER row
		// has no sign-in, so a start URL accepted there is the same defect the
		// shared-row case below names: an admin believing they pinned a portal
		// nothing reads.
		{name: "a start URL on a per_user bearer row is refused, not ignored", block: agentBlock(types.AgentProvider{
			ID: "claude-code", Mechanism: types.AgentMechanismBedrockBearer,
			CredentialSource: types.CredentialSourcePerUser, SSOStartURL: "https://acme.awsapps.com/start",
		}), want: "sso_start_url applies only when"},
		{name: "per_user with no start URL", block: agentBlock(types.AgentProvider{
			ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO,
			CredentialSource: types.CredentialSourcePerUser,
		}), want: "sso_start_url is required"},
		{name: "per_user with a start URL that is not one", block: agentBlock(types.AgentProvider{
			ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO,
			CredentialSource: types.CredentialSourcePerUser, SSOStartURL: "my-org.awsapps.com/start",
		}), want: "AWS access portal URL"},
		{name: "per_user, fully stated", block: agentBlock(types.AgentProvider{
			ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO,
			CredentialSource: types.CredentialSourcePerUser, SSOStartURL: "https://acme.awsapps.com/start",
		})},
		// Accepted-and-never-read is how an admin comes to believe they pinned a
		// portal they did not.
		{name: "a start URL on a shared row is refused, not ignored", block: agentBlock(types.AgentProvider{
			ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO,
			SSOStartURL: "https://acme.awsapps.com/start",
		}), want: "sso_start_url applies only when"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAgentProviders(tc.block, testAgentImages, tc.model)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("validateAgentProviders() = %v, want accepted", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateAgentProviders() = nil, want a refusal naming %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal = %q, want it to name %q", err.Error(), tc.want)
			}
		})
	}
}

// TestAgentMechanismProviderType pins the FOLD itself: harnessDef.ProviderTypes
// is keyed by four COARSE values, so every bedrock_* sub-lane has to collapse
// onto "bedrock" before it is looked up. A mechanism that folded to itself would
// read "" (possible) out of that map and admit every impossible pair silently.
func TestAgentMechanismProviderType(t *testing.T) {
	for m, want := range map[types.AgentMechanism]string{
		types.AgentMechanismBedrockBearer:         types.AgentProviderTypeBedrock,
		types.AgentMechanismBedrockSSO:            types.AgentProviderTypeBedrock,
		types.AgentMechanismBedrockEnv:            types.AgentProviderTypeBedrock,
		types.AgentMechanismBedrockAWSDir:         types.AgentProviderTypeBedrock,
		types.AgentMechanismAnthropicAPIKey:       "anthropic_api_key",
		types.AgentMechanismAnthropicSubscription: "anthropic_subscription",
		types.AgentMechanismOpenAIAPIKey:          "openai_api_key",
		types.AgentMechanismNone:                  "none",
	} {
		if got := m.ProviderType(); got != want {
			t.Errorf("%s.ProviderType() = %q, want %q", m, got, want)
		}
	}
	// …and EVERY folded value except "none" must be a key EVERY credential-wiring
	// catalog row actually answers for. validateAgentMechanism reads an ABSENT key
	// as "" — possible — the default harnessProviderReason documents as "never
	// load-bearing, every type this harness's family is ever asked about is listed
	// explicitly". This loop is what keeps that true: a row that stops enumerating
	// a coarse type would otherwise silently ADMIT an impossible pair, and the
	// admin would get a live credential chip over a lane nothing can honour.
	//
	// Every row, not just claude-code: pinning one row is how the next harness
	// added to the catalog ships with a half-filled map and nothing says so.
	for _, def := range harnessCatalog {
		if def.NoManagedAuth {
			// The BYOA row wires nothing, so its nil map is the answer:
			// validateAgentMechanism decides it on the flag, never on a lookup.
			continue
		}
		for _, m := range types.ClosedAgentMechanismList() {
			pt := types.AgentMechanism(m).ProviderType()
			if pt == string(types.AgentMechanismNone) {
				continue
			}
			if _, named := def.ProviderTypes[pt]; !named {
				t.Errorf("mechanism %s folds to %q, which %s's ProviderTypes does not name — "+
					"an unnamed key reads as POSSIBLE, so an impossible pair would be admitted",
					m, pt, def.ID)
			}
		}
	}
}

func newAgentProvidersHarness(t *testing.T, fake *fakeSiteConfigStore) (*Server, *recRecorder) {
	t.Helper()
	h := newHarness(t)
	cfg := baseTestConfig(h, fake)
	cfg.AgentImages = testAgentImages
	return New(cfg), h.audit
}

// TestAgentProvidersGet pins the read: a never-configured install gets the
// zero-value document with 200 and an ETag it can send back.
func TestAgentProvidersGet(t *testing.T) {
	srv, _ := newAgentProvidersHarness(t, &fakeSiteConfigStore{})
	w := do(t, srv, http.MethodGet, "/api/v1/agent-providers", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := strings.TrimSpace(w.Body.String()); got != "{}" {
		t.Errorf("unconfigured GET body = %s, want {}", got)
	}
	if w.Header().Get("ETag") == "" {
		t.Error("GET carries no ETag — the console's If-Match round trip has nothing to send")
	}
}

// TestAgentProvidersPut is the write: it persists, it audits WITHOUT the start
// URL, it hands back the new ETag, {} clears, and a stale If-Match is 412.
func TestAgentProvidersPut(t *testing.T) {
	fake := &fakeSiteConfigStore{cfg: types.SiteConfig{ScmHosts: []string{"github.com"}}}
	srv, audit := newAgentProvidersHarness(t, fake)

	const startURL = "https://acme.awsapps.com/start"
	body := `{"agents":[{"id":"claude-code","mechanism":"bedrock_sso","credential_source":"per_user",` +
		`"sso_start_url":"` + startURL + `"},{"id":"codex-cli","mechanism":"openai_api_key","disabled":true}]}`
	w := do(t, srv, http.MethodPut, "/api/v1/agent-providers", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if fake.putSeen == nil || fake.putSeen.AgentProviders == nil {
		t.Fatal("the agent roster was not written")
	}
	if len(fake.putSeen.AgentProviders.Agents) != 2 {
		t.Fatalf("stored %d rows, want 2", len(fake.putSeen.AgentProviders.Agents))
	}
	// The rest of the site config is untouched: this door writes one sub-object.
	if len(fake.putSeen.ScmHosts) != 1 {
		t.Errorf("scm_hosts = %v, want the rest of the document untouched", fake.putSeen.ScmHosts)
	}
	if w.Header().Get("ETag") == "" {
		t.Error("PUT echoes no ETag")
	}

	// The audit datum, and the one field that must never be in it.
	var writes []types.AuditEvent
	for _, ev := range audit.events {
		if ev.Action == "agent_provider.write" {
			writes = append(writes, ev)
		}
	}
	if len(writes) != 1 {
		t.Fatalf("agent_provider.write events = %d, want 1", len(writes))
	}
	if strings.Contains(string(writes[0].Data), startURL) {
		t.Errorf("the AWS access portal URL reached the audit log: %s", writes[0].Data)
	}
	var datum struct {
		AgentCount        int      `json:"agent_count"`
		IDs               []string `json:"ids"`
		Mechanisms        []string `json:"mechanisms"`
		CredentialSources []string `json:"credential_sources"`
		Disabled          []string `json:"disabled"`
	}
	if err := json.Unmarshal(writes[0].Data, &datum); err != nil {
		t.Fatal(err)
	}
	if datum.AgentCount != 2 || len(datum.IDs) != 2 {
		t.Errorf("datum = %+v, want both rows counted and named", datum)
	}
	if strings.Join(datum.Mechanisms, ",") != "bedrock_sso,openai_api_key" {
		t.Errorf("mechanisms = %v, want both lanes, sorted", datum.Mechanisms)
	}
	// An unset source renders as its EFFECTIVE value, not as "".
	if strings.Join(datum.CredentialSources, ",") != "per_user,shared" {
		t.Errorf("credential_sources = %v, want per_user and shared", datum.CredentialSources)
	}
	if strings.Join(datum.Disabled, ",") != "codex-cli" {
		t.Errorf("disabled = %v, want the one row that is off", datum.Disabled)
	}

	t.Run("a stale If-Match is refused before the write", func(t *testing.T) {
		w := doWithHeaders(t, srv, http.MethodPut, "/api/v1/agent-providers", adminToken,
			`{"agents":[{"id":"claude-code","mechanism":"anthropic_api_key"}]}`,
			map[string]string{"If-Match": `"stale"`})
		if w.Code != http.StatusPreconditionFailed {
			t.Fatalf("stale If-Match = %d, want 412; body=%s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), agent412Stale) {
			t.Errorf("412 body = %s, want the DRAFT sentence", w.Body.String())
		}
	})

	t.Run("the GET's own ETag satisfies the PUT", func(t *testing.T) {
		etag := do(t, srv, http.MethodGet, "/api/v1/agent-providers", adminToken, "").Header().Get("ETag")
		w := doWithHeaders(t, srv, http.MethodPut, "/api/v1/agent-providers", adminToken,
			`{"agents":[{"id":"claude-code","mechanism":"anthropic_api_key"}]}`,
			map[string]string{"If-Match": etag})
		if w.Code != http.StatusOK {
			t.Fatalf("fresh If-Match = %d, want 200; body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("{} is the clear form and normalizes back to an absent key", func(t *testing.T) {
		w := do(t, srv, http.MethodPut, "/api/v1/agent-providers", adminToken, `{}`)
		if w.Code != http.StatusOK {
			t.Fatalf("clear = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		if fake.putSeen.AgentProviders != nil {
			t.Errorf("an empty block was stored as %+v, want nil — {} must clear, not persist an empty object",
				fake.putSeen.AgentProviders)
		}
		if agentProvidersConfigured(fake.cfg) {
			t.Error("a cleared roster still reads as configured — the install is back in legacy open mode")
		}
	})

	t.Run("an unwritable row is refused with the DRAFT constant", func(t *testing.T) {
		w := do(t, srv, http.MethodPut, "/api/v1/agent-providers", adminToken,
			`{"agents":[{"id":"acme-agent","mechanism":"bedrock_sso"}]}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("PUT = %d, want 400; body=%s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "its mechanism must be none") {
			t.Errorf("400 body = %s, want the custom-image sentence", w.Body.String())
		}
	})

	t.Run("an unknown field is refused", func(t *testing.T) {
		if w := do(t, srv, http.MethodPut, "/api/v1/agent-providers", adminToken, `{"bogus":1}`); w.Code != http.StatusBadRequest {
			t.Fatalf("unknown field = %d, want 400", w.Code)
		}
	})
}

// TestSiteConfigDoorCarriesTheAgentRoster is the MDM half: /etc/wardyn/site-config.json
// is re-applied on every boot and predates this key, so silence must carry the
// roster forward — and an explicit {} must still clear it.
func TestSiteConfigDoorCarriesTheAgentRoster(t *testing.T) {
	stored := types.SiteConfig{
		AgentProviders: agentBlock(agentRow("claude-code", types.AgentMechanismBedrockSSO)),
	}

	t.Run("a body that never names the key carries it forward", func(t *testing.T) {
		fake := &fakeSiteConfigStore{cfg: stored}
		srv, _ := newAgentProvidersHarness(t, fake)
		w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{"scm_hosts":["github.com"]}`)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		if fake.putSeen.AgentProviders == nil {
			t.Fatal("an older client's silence DELETED the org's agent roster — every desktop boot would")
		}
	})

	t.Run("an explicit {} clears it", func(t *testing.T) {
		fake := &fakeSiteConfigStore{cfg: stored}
		srv, _ := newAgentProvidersHarness(t, fake)
		w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{"agent_providers":{}}`)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		if fake.putSeen.AgentProviders != nil {
			t.Errorf("agent_providers = %+v, want nil — {} is the clear form on BOTH doors", fake.putSeen.AgentProviders)
		}
	})

	t.Run("the same validator guards this door", func(t *testing.T) {
		fake := &fakeSiteConfigStore{}
		srv, _ := newAgentProvidersHarness(t, fake)
		w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken,
			`{"agent_providers":{"agents":[{"id":"ghost","mechanism":"none"}]}}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("PUT = %d, want 400 — the MDM door must not store what /agent-providers refuses; body=%s",
				w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "names no agent") {
			t.Errorf("400 body = %s, want the unknown-agent sentence", w.Body.String())
		}
	})

	t.Run("site_config.write records the enabled count", func(t *testing.T) {
		fake := &fakeSiteConfigStore{}
		srv, audit := newAgentProvidersHarness(t, fake)
		w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken,
			`{"agent_providers":{"agents":[{"id":"claude-code","mechanism":"bedrock_sso"},`+
				`{"id":"codex-cli","mechanism":"openai_api_key","disabled":true}]}}`)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		for _, ev := range audit.events {
			if ev.Action != "site_config.write" {
				continue
			}
			var d struct {
				AgentProviders int `json:"agent_providers"`
			}
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				t.Fatal(err)
			}
			if d.AgentProviders != 1 {
				t.Errorf("site_config.write agent_providers = %d, want 1 (a disabled row narrows nothing)", d.AgentProviders)
			}
			return
		}
		t.Fatal("no site_config.write event")
	})
}

// agentRosterFixture is one create-and-dispatch server whose site config carries
// (or does not carry) a roster — integStore is already the shape a member create
// drives, with a settable SiteConfig.
func agentRosterFixture(t *testing.T, block *types.AgentProviders) *Server {
	t.Helper()
	h := newHarness(t)
	st := &integStore{
		govEscapeStore: newGovEscapeStore(&capStore{}),
		site:           types.SiteConfig{AgentProviders: block},
	}
	cfg := baseTestConfig(h, st)
	cfg.Broker = h.broker
	cfg.Runner = &fakeRunner{}
	cfg.AgentImages = testAgentImages
	// OIDC + a secret store + the deployment ceiling are what govEscapeFixture
	// wires for the same store: without them an SSO member's request is refused
	// before the roster is ever consulted, and the member arm below would pass
	// for the wrong reason.
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Secrets = &memSecrets{m: map[string][]byte{
		govCorpSecret: []byte("v"),
		// C2's declared-mechanism gate refuses a MODEL run at create when the
		// lane the row declares is not the one that would carry it, so a roster
		// fixture has to configure the lane its rows name — otherwise every
		// "an enabled row admits its agent" arm below would pass or fail on the
		// model credential rather than on the roster.
		bedrockAPIKeySecret: []byte("bedrock-bearer-test"),
	}}
	cfg.BedrockRegion, cfg.BedrockModel = "us-east-1", "us.anthropic.claude-sonnet-4-5-20250929-v1:0"
	cfg.MaskRegistry = secretmask.NewRegistry()
	cfg.DefaultPolicy = govDeployment()
	return New(cfg)
}

// TestAgentRosterRefusesRunCreate is the enforcement half, and its FIRST half is
// the one that matters most: with no block, nothing changes.
func TestAgentRosterRefusesRunCreate(t *testing.T) {
	t.Run("legacy open mode admits every agent, as it did in 0.7.1", func(t *testing.T) {
		srv := agentRosterFixture(t, nil)
		for _, agent := range []string{"claude-code", "acme-agent"} {
			w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken,
				`{"task":"echo hi","agent":"`+agent+`"}`)
			if w.Code == http.StatusUnprocessableEntity && strings.Contains(w.Body.String(), "not an enabled agent") {
				t.Errorf("--agent %s was refused with NO roster configured: %s", agent, w.Body.String())
			}
		}
	})

	t.Run("an enabled row admits its agent", func(t *testing.T) {
		srv := agentRosterFixture(t, agentBlock(agentRow("claude-code", types.AgentMechanismBedrockSSO)))
		w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, `{"task":"echo hi","agent":"claude-code"}`)
		if w.Code == http.StatusUnprocessableEntity {
			t.Fatalf("an enabled agent was refused: %s", w.Body.String())
		}
	})

	t.Run("an agent with no row is refused with the member sentence", func(t *testing.T) {
		srv := agentRosterFixture(t, agentBlock(agentRow("codex-cli", types.AgentMechanismOpenAIAPIKey)))
		w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, `{"task":"echo hi","agent":"claude-code"}`)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("create = %d, want 422; body=%s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), agentNotEnabledTail) {
			t.Errorf("422 body = %s, want the DRAFT sentence", w.Body.String())
		}
		// A member-facing refusal names the agent and NOTHING else.
		if strings.Contains(w.Body.String(), "codex-cli") || strings.Contains(w.Body.String(), "openai") {
			t.Errorf("the refusal discloses the roster: %s", w.Body.String())
		}
	})

	// "Operators and members alike": the roster is the ORG's statement of what
	// this install runs, not a per-principal ceiling, so the same door answers the
	// same way to a member — and every other case here drives it as an operator.
	t.Run("a member is refused the same way, with the same sentence", func(t *testing.T) {
		srv := agentRosterFixture(t, agentBlock(agentRow("codex-cli", types.AgentMechanismOpenAIAPIKey)))
		w := doSSO(t, srv, http.MethodPost, "/api/v1/runs",
			govSession(t, govMemberSub, []string{"eng"}, false), `{"task":"echo hi","agent":"claude-code"}`)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("member create = %d, want 422; body=%s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), agentNotEnabledTail) {
			t.Errorf("422 body = %s, want the DRAFT sentence", w.Body.String())
		}
	})

	t.Run("a member is admitted by an enabled row", func(t *testing.T) {
		srv := agentRosterFixture(t, agentBlock(agentRow("claude-code", types.AgentMechanismBedrockSSO)))
		w := doSSO(t, srv, http.MethodPost, "/api/v1/runs",
			govSession(t, govMemberSub, []string{"eng"}, false), `{"task":"echo hi","agent":"claude-code"}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("member create = %d, want 201; body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("a disabled row is refused too", func(t *testing.T) {
		srv := agentRosterFixture(t, agentBlock(types.AgentProvider{
			ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO, Disabled: true,
		}))
		w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, `{"task":"echo hi","agent":"claude-code"}`)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("create = %d, want 422; body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("an exec run naming an image and no agent is not touched", func(t *testing.T) {
		// agentRequirementError deliberately admits this shape, and a roster of
		// agents has nothing to say about a run that names none.
		srv := agentRosterFixture(t, agentBlock(agentRow("codex-cli", types.AgentMechanismOpenAIAPIKey)))
		w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken,
			`{"task":"echo hi","task_mode":"exec","image":"ubuntu:24.04"}`)
		if w.Code == http.StatusUnprocessableEntity && strings.Contains(w.Body.String(), "not an enabled agent") {
			t.Fatalf("an agentless exec run was refused by the agent roster: %s", w.Body.String())
		}
	})
}

// TestAgentRosterRefusesRecordLaunch is the STEP-RUN half. newStepRun hardcodes
// claude-code and calls Store.CreateRun directly, so every server-launched lane
// bypasses run create's identical check — and a record session is an interactive
// MODEL run, so a codex-only org must not be able to record through claude-code.
func TestAgentRosterRefusesRecordLaunch(t *testing.T) {
	newSrv := func(t *testing.T, block *types.AgentProviders) (*Server, *fakeRunner) {
		t.Helper()
		h := newHarness(t)
		ws := types.Workspace{ID: uuid.New(), Status: types.WorkspaceScanned}
		st := newRecordLLMModeStore(ws)
		st.sc = types.SiteConfig{AgentProviders: block}
		fr := &fakeRunner{}
		cfg := baseTestConfig(h, ceilingRecordStore{recordLLMModeStore: st})
		cfg.Runner = fr
		cfg.Broker = h.broker
		// A record session is an INTERACTIVE MODEL run, so C2's declared-mechanism
		// gate applies to it: a row naming a Bedrock lane with no Bedrock
		// credential anywhere would refuse the launch on THAT ground and never
		// exercise the roster check this test is about. Configure the lane the rows
		// below declare, so the only refusal left to observe is the roster's.
		cfg.BedrockRegion, cfg.BedrockModel = "us-east-1", "us.anthropic.claude-sonnet-4-5-20250929-v1:0"
		cfg.Secrets = &memSecrets{m: map[string][]byte{bedrockAPIKeySecret: []byte("bedrock-bearer-test")}}
		cfg.MaskRegistry = secretmask.NewRegistry()
		return New(cfg), fr
	}

	t.Run("a codex-only roster refuses the record launch", func(t *testing.T) {
		srv, _ := newSrv(t, agentBlock(agentRow("codex-cli", types.AgentMechanismOpenAIAPIKey)))
		_, _, err := srv.launchRecordRun(t.Context(), "admin@corp.example",
			types.Workspace{ID: uuid.New(), Status: types.WorkspaceScanned}, "build", "build", false)
		if !errors.Is(err, errAgentNotEnabled) {
			t.Fatalf("launchRecordRun err = %v, want the agent-roster refusal", err)
		}
		if !strings.Contains(err.Error(), "not an enabled agent") {
			t.Errorf("err = %v, want the same sentence run create answers with", err)
		}
	})

	t.Run("no roster launches exactly as it did", func(t *testing.T) {
		srv, _ := newSrv(t, nil)
		_, _, err := srv.launchRecordRun(t.Context(), "admin@corp.example",
			types.Workspace{ID: uuid.New(), Status: types.WorkspaceScanned}, "build", "build", false)
		if errors.Is(err, errAgentNotEnabled) {
			t.Fatalf("a record launch was refused with NO roster configured: %v", err)
		}
	})

	t.Run("an enabled claude-code row launches", func(t *testing.T) {
		srv, _ := newSrv(t, agentBlock(agentRow("claude-code", types.AgentMechanismBedrockSSO)))
		_, _, err := srv.launchRecordRun(t.Context(), "admin@corp.example",
			types.Workspace{ID: uuid.New(), Status: types.WorkspaceScanned}, "build", "build", false)
		if errors.Is(err, errAgentNotEnabled) {
			t.Fatalf("an enabled agent was refused: %v", err)
		}
	})
}

// rosterRecordStore is recordStore with a site config — the record HTTP door's
// fixture needs the roster the launch path reads, and recordStore's own
// GetSiteConfig deliberately answers the zero value (legacy open mode).
type rosterRecordStore struct {
	*recordStore
	sc types.SiteConfig
}

func (s *rosterRecordStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return s.sc, nil
}

// TestRecordDoorAnswersTheRosterRefusal is the HTTP half of the step-run check,
// and it pins the two things a unit test on launchRecordRun cannot see:
//
//  1. the STATUS. handleRecordWorkspace maps errAgentNotEnabled to 422 by
//     errors.Is + TrimPrefix; one refactor of that chain and an org-policy
//     refusal reads as a 500 with the sentinel prefix still on it.
//  2. that the refusal COSTS NO STATE. It sits above the import-step CAS claim,
//     so a refused record leaves the workspace's active-run slot free and writes
//     no failed record result — a member could otherwise wedge a workspace by
//     asking for an agent their org does not offer.
func TestRecordDoorAnswersTheRosterRefusal(t *testing.T) {
	wsID := uuid.New()
	ws := types.Workspace{
		ID: wsID, Status: types.WorkspaceScanned,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/w", Target: "/home/agent/work"}},
	}
	st := &rosterRecordStore{
		recordStore: &recordStore{importStateFake: importStateFake{ws: ws}},
		sc:          types.SiteConfig{AgentProviders: agentBlock(agentRow("codex-cli", types.AgentMechanismOpenAIAPIKey))},
	}
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.Runner = &fakeRunner{} // past the no-runner 503, into the launch
	srv := New(cfg)

	w := do(t, srv, http.MethodPost, "/api/v1/workspaces/"+wsID.String()+"/record", adminToken, `{"name":"capture"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("record = %d, want 422 — the status run create answers for the identical cause; body=%s",
			w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), agentNotEnabledTail) {
		t.Errorf("422 body = %s, want the DRAFT sentence", w.Body.String())
	}
	// The internal sentinel is a Go-side marker, never a member-facing word.
	if strings.Contains(w.Body.String(), errAgentNotEnabled.Error()) {
		t.Errorf("the sentinel prefix reached the caller: %s", w.Body.String())
	}
	if st.claimedRun != nil {
		t.Errorf("a refused record claimed the workspace's import-step slot (run %s) — an org-policy "+
			"refusal must cost no state, like the two refusals beside it", st.claimedRun)
	}
	if st.saved != nil {
		t.Errorf("a refused record wrote a record result (%s) — nothing was launched to report on", st.saved)
	}
}

// TestSetupHarnessToolsCarryTheRoster is the member-safe projection: the console's
// only roster reader. The ROW COUNT never changes — a disabled agent is published
// as disabled, never hidden — and the start URL is never in it.
func TestSetupHarnessToolsCarryTheRoster(t *testing.T) {
	sc := types.SiteConfig{AgentProviders: agentBlock(types.AgentProvider{
		ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO,
		CredentialSource: types.CredentialSourcePerUser, SSOStartURL: "https://acme.awsapps.com/start",
	})}
	tools := setupHarnessTools(sc, nil)
	if len(tools) != len(harnessCatalog) {
		t.Fatalf("len = %d, want %d — the server never filters this list by the roster",
			len(tools), len(harnessCatalog))
	}
	byID := map[string]SetupHarnessTool{}
	for _, tool := range tools {
		byID[tool.ID] = tool
	}
	claude := byID["claude-code"]
	if !claude.Enabled || claude.Mechanism != "bedrock_sso" || claude.CredentialSource != "per_user" {
		t.Errorf("claude-code = %+v, want enabled on the per-user SSO lane", claude)
	}
	codex := byID["codex-cli"]
	if codex.Enabled || codex.Mechanism != "" {
		t.Errorf("codex-cli = %+v, want disabled with nothing claimed — the roster does not name it", codex)
	}
	raw, err := json.Marshal(tools)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "awsapps.com") {
		t.Errorf("the AWS access portal URL reached the member-safe projection: %s", raw)
	}
	// enabled:false must be on the wire. omitempty here would make a disabled row
	// indistinguishable from an older daemon's silence, and the console renders
	// those two states differently on purpose.
	if !strings.Contains(string(raw), `"enabled":false`) {
		t.Errorf("a disabled row serialized without enabled=false: %s", raw)
	}
}

// TestRedactSetupStatusKeepsTheRoster: the three fields survive the member
// reduction. A member deciding whether to sign in has to see that their org
// captures credentials per person, and which agents are on offer.
func TestRedactSetupStatusKeepsTheRoster(t *testing.T) {
	st := SetupStatus{Harnesses: setupHarnessTools(types.SiteConfig{
		AgentProviders: agentBlock(types.AgentProvider{
			ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO,
			CredentialSource: types.CredentialSourcePerUser, SSOStartURL: "https://acme.awsapps.com/start",
		}),
	}, nil)}
	got := redactSetupStatusForUser(st, false, false)
	if len(got.Harnesses) != len(harnessCatalog) {
		t.Fatalf("a member sees %d harness rows, want all %d", len(got.Harnesses), len(harnessCatalog))
	}
	for _, tool := range got.Harnesses {
		if tool.ID != "claude-code" {
			continue
		}
		if !tool.Enabled || tool.Mechanism != "bedrock_sso" || tool.CredentialSource != "per_user" {
			t.Errorf("the member reduction dropped the roster fields: %+v", tool)
		}
	}
}

// finding 1: the roster's account/role pin
//
// Ask 1 of the finding, and the one the operator asked for first: "let the
// admin pin account + role on the roster row, beside sso_start_url, owned the
// same way — a new entitlement cannot move it." These are the SAVE-time rules,
// which is the earliest door a wrong identity can be refused at: the roster
// refuses a pin that disagrees with the model's account, so the sign-in that
// would have earned the IAM 403 never launches.

func pinnedRow(account, role string) types.AgentProvider {
	return types.AgentProvider{
		ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO,
		CredentialSource: types.CredentialSourcePerUser,
		SSOStartURL:      "https://acme.awsapps.com/start",
		SSOAccountID:     account, SSORoleName: role,
	}
}

// TestAgentProviders_PinOverridesTheModelsAccount (S2-09): an EXPLICIT pin is
// the admin's deliberate answer to "which account signs in", so the save door
// takes it even when the configured model lives in another account — a
// resource-shared application inference profile legitimately does. It is not
// silent: the save WARNs, naming both accounts.
//
// The check itself stays for the UNPINNED case, where there is no deliberate
// answer to defer to: with no pin there is nothing for the save door to take,
// so the rule lives on at capture — bindCaptureToPin, pinned by
// TestUploadSSOToken_AccountIDNotModelARNAccountRejected (ssotoken_binding_test.go).
func TestAgentProviders_PinOverridesTheModelsAccount(t *testing.T) {
	const model = "arn:aws:bedrock:us-west-2:111111111111:inference-profile/us.anthropic.claude-sonnet-4-20250514-v1:0"
	// The WARN is the only thing the save door has left to say, so it is asserted
	// rather than described (R-05): a refactor that drops it would otherwise stay
	// green in every gate.
	var logged bytes.Buffer
	restore := slog.Default()
	t.Cleanup(func() { slog.SetDefault(restore) }) // restored even if the call below panics or fails
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	err := validateAgentProviders(agentBlock(pinnedRow("222222222222", "BedrockRunner")), testAgentImages, model)
	slog.SetDefault(restore)
	if err != nil {
		t.Fatalf("an explicit cross-account pin was refused: %v — a resource-shared inference profile has no other way to be configured", err)
	}
	for _, want := range []string{"222222222222", "111111111111"} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("the save logged %q, want it to name account %s — the disagreement must be spoken, not silent", logged.String(), want)
		}
	}
	// The agreeing pin saves, unchanged.
	if err := validateAgentProviders(agentBlock(pinnedRow("111111111111", "BedrockRunner")), testAgentImages, model); err != nil {
		t.Errorf("an agreeing pin was refused: %v", err)
	}
}

// TestAgentProviders_PinFieldsRefusedOnSharedRow: accepted-and-never-read is
// how an admin comes to believe they pinned an account they did not — the same
// reasoning that already forbids sso_start_url on a non-per_user row.
func TestAgentProviders_PinFieldsRefusedOnSharedRow(t *testing.T) {
	for name, row := range map[string]types.AgentProvider{
		"a shared bedrock_sso row": {
			ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO,
			SSOAccountID: "111111111111", SSORoleName: "BedrockRunner",
		},
		"a per_user row on another mechanism": {
			ID: "claude-code", Mechanism: types.AgentMechanismBedrockBearer,
			SSOAccountID: "111111111111", SSORoleName: "BedrockRunner",
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateAgentProviders(agentBlock(row), testAgentImages, "")
			if err == nil {
				t.Fatal("pin fields on a row that can never read them were accepted")
			}
		})
	}
}

// TestAgentProviders_PinFieldsControlCharsRejected: both halves are baked
// VERBATIM into every later run's generated ~/.aws/config INI
// (awsSSOConfigFileContents), so the write boundary holds them to their real
// grammars — 12 digits, and the IAM role-name character set — rather than
// merely to "non-empty". A newline here would smuggle extra keys into that file.
func TestAgentProviders_PinFieldsControlCharsRejected(t *testing.T) {
	for name, row := range map[string]types.AgentProvider{
		"a newline in the account":         pinnedRow("111111111111\nsso_role_name = Admin", "BedrockRunner"),
		"a newline in the role":            pinnedRow("111111111111", "BedrockRunner\nsso_account_id = 999999999999"),
		"an account that is not 12 digits": pinnedRow("12345", "BedrockRunner"),
		"an account with letters":          pinnedRow("11111111111a", "BedrockRunner"),
		"a role with a space":              pinnedRow("111111111111", "Bedrock Runner"),
		"a role with a slash":              pinnedRow("111111111111", "path/BedrockRunner"),
		"a role over 64 characters":        pinnedRow("111111111111", strings.Repeat("a", 65)),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateAgentProviders(agentBlock(row), testAgentImages, ""); err == nil {
				t.Fatal("an unsafe pin value was accepted — it is baked verbatim into the generated ~/.aws/config")
			}
		})
	}
	// The legal shapes all save. `+=,.@_-` is the IAM role-name set.
	for _, role := range []string{"BedrockRunner", "Wardyn+Bedrock=Role,v1.0@corp_x-y", strings.Repeat("a", 64)} {
		if err := validateAgentProviders(agentBlock(pinnedRow("111111111111", role)), testAgentImages, ""); err != nil {
			t.Errorf("a legal IAM role name %q was refused: %v", role, err)
		}
	}
}

// TestAgentProviders_PinOptionalWhenSingleAccount: the pin is OPTIONAL, because
// a single-account deployment never had this problem and must not be made to
// answer a question it does not have. But it is optional as a PAIR: pinning the
// account alone still leaves the role picked for whoever signs in, which is the
// same defect one level down.
func TestAgentProviders_PinOptionalWhenSingleAccount(t *testing.T) {
	unpinned := types.AgentProvider{
		ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO,
		CredentialSource: types.CredentialSourcePerUser,
		SSOStartURL:      "https://acme.awsapps.com/start",
	}
	if err := validateAgentProviders(agentBlock(unpinned), testAgentImages, ""); err != nil {
		t.Errorf("an unpinned per_user row was refused: %v", err)
	}
	for name, row := range map[string]types.AgentProvider{
		"account without role": pinnedRow("111111111111", ""),
		"role without account": pinnedRow("", "BedrockRunner"),
	} {
		t.Run(name, func(t *testing.T) {
			err := validateAgentProviders(agentBlock(row), testAgentImages, "")
			if err == nil {
				t.Fatal("half a pin was accepted")
			}
			if !strings.Contains(err.Error(), "together") {
				t.Errorf("refusal = %q, want it to say the two are set together or not at all", err)
			}
		})
	}
}

// TestAgentProviderAuditData_CarriesPins: unlike sso_start_url (which names an
// organisation's identity provider and is deliberately absent), an account id
// and a role name are exactly what a refused-capture review needs — "which
// identity was this deployment pinned to when that capture was refused" has no
// other answer in the trail.
func TestAgentProviderAuditData_CarriesPins(t *testing.T) {
	data := agentProviderAuditData(types.AgentProviders{Agents: []types.AgentProvider{
		pinnedRow("111111111111", "BedrockRunner"),
		{ID: "codex-cli", Mechanism: types.AgentMechanismOpenAIAPIKey},
	}})
	pins, ok := data["pins"].([]string)
	if !ok {
		t.Fatalf("audit data has no pins: %v", data)
	}
	if len(pins) != 1 || pins[0] != "111111111111/BedrockRunner" {
		t.Errorf("pins = %v, want the one pinned row's account/role", pins)
	}
	if strings.Contains(mustJSONString(data), "acme.awsapps.com") {
		t.Error("the audit datum carries the start URL — it names an organisation's IdP and no review question needs it")
	}
}

func mustJSONString(v any) string { return string(mustJSON(v)) }
