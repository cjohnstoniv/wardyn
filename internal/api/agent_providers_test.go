// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
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

func agentRow(id string) types.AgentProvider {
	return types.AgentProvider{ID: id}
}

func agentBlock(rows ...types.AgentProvider) *types.AgentProviders {
	return &types.AgentProviders{Agents: rows}
}

// TestValidateAgentProviders is the write-boundary table. A row names an agent
// this deployment can run and whether it is on; how the agent reaches its model
// is its model provider's, so no row carries a lane any more.
func TestValidateAgentProviders(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block *types.AgentProviders
		want  string // "" = accepted; otherwise a substring the refusal must carry
	}{
		{name: "a nil block is legacy open mode", block: nil},
		{name: "an empty block is valid (the clear form normalizes it away first)",
			block: &types.AgentProviders{}},
		{name: "catalog agents", block: agentBlock(agentRow("claude-code"), agentRow("codex-cli"), agentRow("none"))},
		{name: "a custom image-map agent", block: agentBlock(agentRow("acme-agent"))},
		{name: "a row that is off", block: agentBlock(types.AgentProvider{ID: "claude-code", Disabled: true})},
		{name: "an agent this deployment cannot run", block: agentBlock(agentRow("ghost")), want: "names no agent"},
		{name: "duplicate ids", block: agentBlock(agentRow("claude-code"), agentRow("claude-code")), want: "is not unique"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAgentProviders(tc.block, testAgentImages)
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

// TestModelProviderKindsAreNamedByEveryHarness: validateProviderHarnesses reads
// an ABSENT harnessDef.ProviderTypes key as "" — possible — so every
// credential-wiring catalog row must name every model-provider kind, or a
// provider of a kind the harness cannot drive would be admitted for it
// silently. Every row, not just claude-code: pinning one row is how the next
// harness added to the catalog ships with a half-filled map.
func TestModelProviderKindsAreNamedByEveryHarness(t *testing.T) {
	for _, def := range harnessCatalog {
		if def.NoManagedAuth {
			continue // the BYOA row takes no provider at all
		}
		for _, k := range types.ClosedModelProviderKindList() {
			if _, named := def.ProviderTypes[k]; !named {
				t.Errorf("kind %s is not named by %s's ProviderTypes — an unnamed key reads as POSSIBLE", k, def.ID)
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

// TestAgentProvidersPut is the write: it persists, it audits the narrowed
// datum, it hands back the new ETag, {} clears, and a stale If-Match is 412.
func TestAgentProvidersPut(t *testing.T) {
	fake := &fakeSiteConfigStore{cfg: types.SiteConfig{ScmHosts: []string{"github.com"}}}
	srv, audit := newAgentProvidersHarness(t, fake)

	body := `{"agents":[{"id":"claude-code"},{"id":"codex-cli","disabled":true}]}`
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
	var datum struct {
		AgentCount int      `json:"agent_count"`
		IDs        []string `json:"ids"`
		Disabled   []string `json:"disabled"`
	}
	if err := json.Unmarshal(writes[0].Data, &datum); err != nil {
		t.Fatal(err)
	}
	if datum.AgentCount != 2 || len(datum.IDs) != 2 {
		t.Errorf("datum = %+v, want both rows counted and named", datum)
	}
	if strings.Join(datum.Disabled, ",") != "codex-cli" {
		t.Errorf("disabled = %v, want the one row that is off", datum.Disabled)
	}

	t.Run("a stale If-Match is refused before the write", func(t *testing.T) {
		w := doWithHeaders(t, srv, http.MethodPut, "/api/v1/agent-providers", adminToken,
			`{"agents":[{"id":"claude-code"}]}`,
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
			`{"agents":[{"id":"claude-code"}]}`,
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
		w := do(t, srv, http.MethodPut, "/api/v1/agent-providers", adminToken, `{"agents":[{"id":"ghost"}]}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("PUT = %d, want 400; body=%s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "names no agent") {
			t.Errorf("400 body = %s, want the unknown-agent sentence", w.Body.String())
		}
	})

	t.Run("an unknown field is refused", func(t *testing.T) {
		if w := do(t, srv, http.MethodPut, "/api/v1/agent-providers", adminToken, `{"bogus":1}`); w.Code != http.StatusBadRequest {
			t.Fatalf("unknown field = %d, want 400", w.Code)
		}
	})

	// No alias window: the retired lane fields are unknown fields now, refused
	// rather than silently dropped.
	for _, field := range []string{`"mechanism":"bedrock_sso"`, `"credential_source":"per_user"`,
		`"sso_start_url":"https://acme.awsapps.com/start"`, `"sso_account_id":"111111111111"`, `"sso_role_name":"R"`} {
		t.Run("the retired "+strings.SplitN(field, `"`, 3)[1]+" field is refused", func(t *testing.T) {
			w := do(t, srv, http.MethodPut, "/api/v1/agent-providers", adminToken, `{"agents":[{"id":"claude-code",`+field+`}]}`)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("PUT = %d, want 400 — a retired field must not be accepted and ignored; body=%s", w.Code, w.Body.String())
			}
		})
	}
}

// TestSiteConfigDoorCarriesTheAgentRoster is the MDM half: /etc/wardyn/site-config.json
// is re-applied on every boot and predates this key, so silence must carry the
// roster forward — and an explicit {} must still clear it.
func TestSiteConfigDoorCarriesTheAgentRoster(t *testing.T) {
	stored := types.SiteConfig{
		AgentProviders: agentBlock(agentRow("claude-code")),
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
			`{"agent_providers":{"agents":[{"id":"ghost"}]}}`)
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
			`{"agent_providers":{"agents":[{"id":"claude-code"},{"id":"codex-cli","disabled":true}]}}`)
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
	cfg.Secrets = &memSecrets{m: map[string][]byte{govCorpSecret: []byte("v")}}
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
		srv := agentRosterFixture(t, agentBlock(agentRow("claude-code")))
		w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, `{"task":"echo hi","agent":"claude-code"}`)
		if w.Code == http.StatusUnprocessableEntity {
			t.Fatalf("an enabled agent was refused: %s", w.Body.String())
		}
	})

	t.Run("an agent with no row is refused with the member sentence", func(t *testing.T) {
		srv := agentRosterFixture(t, agentBlock(agentRow("codex-cli")))
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
		if got := errorReason(w); got != reasonAgentNotEnabled {
			t.Errorf("reason = %q, want %q; body=%s", got, reasonAgentNotEnabled, w.Body.String())
		}
	})

	// "Operators and members alike": the roster is the ORG's statement of what
	// this install runs, not a per-principal ceiling, so the same door answers the
	// same way to a member — and every other case here drives it as an operator.
	t.Run("a member is refused the same way, with the same sentence", func(t *testing.T) {
		srv := agentRosterFixture(t, agentBlock(agentRow("codex-cli")))
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
		srv := agentRosterFixture(t, agentBlock(agentRow("claude-code")))
		w := doSSO(t, srv, http.MethodPost, "/api/v1/runs",
			govSession(t, govMemberSub, []string{"eng"}, false), `{"task":"echo hi","agent":"claude-code"}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("member create = %d, want 201; body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("a disabled row is refused too", func(t *testing.T) {
		srv := agentRosterFixture(t, agentBlock(types.AgentProvider{ID: "claude-code", Disabled: true}))
		w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, `{"task":"echo hi","agent":"claude-code"}`)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("create = %d, want 422; body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("an exec run naming an image and no agent is not touched", func(t *testing.T) {
		// agentRequirementError deliberately admits this shape, and a roster of
		// agents has nothing to say about a run that names none.
		srv := agentRosterFixture(t, agentBlock(agentRow("codex-cli")))
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
		cfg.MaskRegistry = secretmask.NewRegistry()
		return New(cfg), fr
	}

	t.Run("a codex-only roster refuses the record launch", func(t *testing.T) {
		srv, _ := newSrv(t, agentBlock(agentRow("codex-cli")))
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
		srv, _ := newSrv(t, agentBlock(agentRow("claude-code")))
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
		sc:          types.SiteConfig{AgentProviders: agentBlock(agentRow("codex-cli"))},
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
// as disabled, never hidden.
func TestSetupHarnessToolsCarryTheRoster(t *testing.T) {
	sc := types.SiteConfig{AgentProviders: agentBlock(agentRow("claude-code"))}
	tools := setupHarnessTools(sc, nil, nil)
	if len(tools) != len(harnessCatalog) {
		t.Fatalf("len = %d, want %d — the server never filters this list by the roster",
			len(tools), len(harnessCatalog))
	}
	byID := map[string]SetupHarnessTool{}
	for _, tool := range tools {
		byID[tool.ID] = tool
	}
	if !byID["claude-code"].Enabled {
		t.Errorf("claude-code = %+v, want enabled", byID["claude-code"])
	}
	if byID["codex-cli"].Enabled {
		t.Errorf("codex-cli = %+v, want disabled — the roster does not name it", byID["codex-cli"])
	}
	raw, err := json.Marshal(tools)
	if err != nil {
		t.Fatal(err)
	}
	// enabled:false must be on the wire. omitempty here would make a disabled row
	// indistinguishable from an older daemon's silence, and the console renders
	// those two states differently on purpose.
	if !strings.Contains(string(raw), `"enabled":false`) {
		t.Errorf("a disabled row serialized without enabled=false: %s", raw)
	}
	for _, retired := range []string{`"mechanism"`, `"credential_source"`, `"credential_residency"`} {
		if strings.Contains(string(raw), retired) {
			t.Errorf("the harness projection still publishes %s: %s", retired, raw)
		}
	}
}

// TestRedactSetupStatusKeepsTheRoster: a member sees every harness row and
// whether it is on.
func TestRedactSetupStatusKeepsTheRoster(t *testing.T) {
	st := SetupStatus{Harnesses: setupHarnessTools(types.SiteConfig{AgentProviders: agentBlock(agentRow("claude-code"))}, nil, nil)}
	got := redactSetupStatusForUser(st)
	if len(got.Harnesses) != len(harnessCatalog) {
		t.Fatalf("a member sees %d harness rows, want all %d", len(got.Harnesses), len(harnessCatalog))
	}
	for _, tool := range got.Harnesses {
		if tool.ID == "claude-code" && !tool.Enabled {
			t.Errorf("the member reduction dropped the roster's enabled bit: %+v", tool)
		}
	}
}

// TestAgentProviderAuditData_IsNarrowed: agent_provider.write records the
// agents, which are off and each one's default provider — and no lane,
// credential source or pin, which live on the model provider now.
func TestAgentProviderAuditData_IsNarrowed(t *testing.T) {
	data := agentProviderAuditData(types.AgentProviders{Agents: []types.AgentProvider{
		{ID: "claude-code", DefaultProvider: "corp-key"},
		{ID: "codex-cli", Disabled: true},
	}})
	want := map[string]bool{"agent_count": true, "ids": true, "disabled": true, "defaults": true}
	for k := range data {
		if !want[k] {
			t.Errorf("agent_provider.write carries %q, want only %v", k, want)
		}
	}
	if d, _ := data["defaults"].([]string); len(d) != 1 || d[0] != "claude-code:corp-key" {
		t.Errorf("defaults = %v, want [claude-code:corp-key]", data["defaults"])
	}
	if d, _ := data["disabled"].([]string); len(d) != 1 || d[0] != "codex-cli" {
		t.Errorf("disabled = %v, want [codex-cli]", data["disabled"])
	}
}
