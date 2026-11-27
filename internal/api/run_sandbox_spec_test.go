// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sandboxSpecStore is the known-secret fixture's store, keeping each run's sealed sandbox spec.
type sandboxSpecStore struct {
	*proxyConfigStore
	specMu sync.Mutex
	specs  map[uuid.UUID][]byte
}

var _ store.RunSandboxSpecs = (*sandboxSpecStore)(nil)

func (s *sandboxSpecStore) PutRunSandboxSpec(_ context.Context, id uuid.UUID, sealed []byte) error {
	s.specMu.Lock()
	defer s.specMu.Unlock()
	if _, ok := s.specs[id]; ok {
		return errors.New("a run's sandbox spec is written once")
	}
	s.specs[id] = sealed
	return nil
}

func (s *sandboxSpecStore) GetRunSandboxSpec(_ context.Context, id uuid.UUID) ([]byte, error) {
	s.specMu.Lock()
	defer s.specMu.Unlock()
	if b, ok := s.specs[id]; ok {
		return b, nil
	}
	return nil, store.ErrNotFound
}

// recordedSpec dispatches the fixture's run under policy and answers the record dispatch kept, the spec the sandbox
// was created with, and the record's sealed and opened bytes.
func recordedSpec(t *testing.T, policy types.RunPolicySpec) (runSandboxSpecRecord, runner.SandboxSpec, []byte, []byte) {
	t.Helper()
	f := newKnownSecretFixture(t)
	ss := &sandboxSpecStore{proxyConfigStore: f.ps, specs: map[uuid.UUID][]byte{}}
	f.srv.cfg.Store = ss
	f.dispatch(t, policy)
	rec, err := f.srv.loadRunSandboxSpec(context.Background(), f.run.ID)
	if err != nil {
		t.Fatalf("the dispatched run has no readable sandbox spec: %v", err)
	}
	sealed := ss.specs[f.run.ID]
	plain, err := kek.Open(f.srv.cfg.RunConfigKey, sealed, runSandboxSpecAAD(f.run.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kek.Open(f.srv.cfg.RunConfigKey, sealed, runSandboxSpecAAD(uuid.New())); err == nil {
		t.Error("the record opens as another run's")
	}
	return rec, f.rn.lastSpec, sealed, plain
}

// wantNoCredentialFields fails if the opened record carries an agent-owned file or a field the record never holds.
func wantNoCredentialFields(t *testing.T, rec runSandboxSpecRecord, plain []byte) {
	t.Helper()
	if slices.ContainsFunc(rec.ManagedFiles, func(f runner.ManagedFile) bool { return f.AgentOwned }) {
		t.Error("the record holds an agent-owned file")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(plain, &fields); err != nil {
		t.Fatal(err)
	}
	for k := range fields {
		if strings.Contains(k, "proxy") || strings.Contains(k, "secret") {
			t.Errorf("the record has a %s field", k)
		}
	}
}

// TestDispatch_RecordsSandboxSpec: dispatch records the run's sandbox spec once, sealed to the run, without a
// credential, and flags what it could not carry; a record with a flag refuses a move.
func TestDispatch_RecordsSandboxSpec(t *testing.T) {
	t.Run("dispatch, no secret", func(t *testing.T) {
		rec, spec, _, plain := recordedSpec(t, types.RunPolicySpec{})
		wantNoCredentialFields(t, rec, plain)
		if rec.Image != spec.Image || rec.ConfinementClass != spec.ConfinementClass || !maps.Equal(rec.Env, spec.Env) || rec.Interactive != spec.Interactive {
			t.Errorf("record %+v does not match the spec the sandbox was created with", rec)
		}
		resident := slices.ContainsFunc(spec.ManagedFiles, func(f runner.ManagedFile) bool { return f.AgentOwned })
		for k := range spec.SecretEnv {
			resident = resident || k != codexMCPEnv
		}
		if rec.Resident != resident || rec.MCPToken {
			t.Errorf("Resident %v MCPToken %v, want Resident %v and no MCP token", rec.Resident, rec.MCPToken, resident)
		}
	})
	t.Run("dispatch, file_secret delivered as a file", func(t *testing.T) {
		rec, spec, sealed, plain := recordedSpec(t, types.RunPolicySpec{EligibleGrants: []types.GrantSpec{fileSecretGrant("api-token", "deploy-token", true)}})
		if !slices.ContainsFunc(spec.ManagedFiles, func(f runner.ManagedFile) bool { return f.AgentOwned }) {
			t.Fatal("the spec carries no delivered file; the case tests nothing")
		}
		wantNoCredentialFields(t, rec, plain)
		if strings.Contains(string(plain), fileOwnValue) || strings.Contains(string(sealed), fileOwnValue) {
			t.Error("the record carries the delivered value")
		}
		wantMoveRefusal(t, rec, reasonMoveResidentCredential, "resident")
	})

	launch := &types.ComponentMCPLaunch{Command: "npx", Args: []string{"-y", "@example/mcp-files@1.4.2"}}
	for _, tc := range []struct {
		name, harness string
		local         bool
	}{{"claude code, brokered MCP", "claude-code", false}, {"codex, brokered MCP", "codex-cli", false}, {"claude code, local MCP only", "claude-code", true}} {
		t.Run(tc.name, func(t *testing.T) {
			server := componentMCP{Key: "linear", URL: "https://mcp.linear.test/mcp", Host: "mcp.linear.test", Harnesses: []string{tc.harness}}
			if tc.local {
				server = componentMCP{Key: "files", Harnesses: []string{tc.harness}, Launch: launch}
			}
			env := map[string]string{}
			out := dispatchMCP([]componentMCP{server}, withholding{}, &[]runner.InjectionGrant{}, "http://wardyn-proxy:3128", tc.local, env)
			spec := runner.SandboxSpec{Image: "img", Env: env, ManagedFiles: out.files, ProxyConfig: runner.ProxyConfig{MCPBindings: out.bindings}}
			if u, ok := env[codexMCPEnv]; ok {
				spec.SecretEnv = map[string]string{codexMCPEnv: u}
				delete(env, codexMCPEnv)
			}
			rec := runSandboxSpecFor(spec, nil, nil, runAgentPolicy{})
			plain, err := json.Marshal(rec)
			if err != nil {
				t.Fatal(err)
			}
			if tc.local {
				if rec.MCPToken || rec.Resident || len(rec.ManagedFiles) != 1 || rec.ManagedFiles[0].Path != claudeMCPInteractivePath {
					t.Errorf("record %+v: want the local server's MCP file recorded and no MCP token", rec)
				}
				wantMoveRefusal(t, rec, "", "")
				return
			}
			if len(out.bindings) == 0 {
				t.Fatal("no MCP token was minted; the case tests nothing")
			}
			if !rec.MCPToken || rec.Resident {
				t.Errorf("MCPToken %v Resident %v, want an MCP token and no resident credential", rec.MCPToken, rec.Resident)
			}
			toks := minted(spec)
			if len(toks) == 0 {
				t.Fatal("no token found in the spec's MCP URLs; the case tests nothing")
			}
			for _, tok := range toks {
				if strings.Contains(string(plain), tok) {
					t.Errorf("the record carries the minted token")
				}
			}
			wantMoveRefusal(t, rec, reasonMoveMCPToken, "mcp")
		})
	}

	t.Run("resident grants", func(t *testing.T) {
		file := types.CredentialGrant{ID: uuid.New(), Delivery: types.GrantDeliveryResident, Spec: types.GrantSpec{Kind: types.GrantFileSecret,
			Scope: json.RawMessage(`{"file":"res.key","secret_name":"op-secret","host":"api.corp.test"}`)}}
		key := types.CredentialGrant{ID: uuid.New(), Delivery: types.GrantDeliveryResident, Spec: types.GrantSpec{Kind: types.GrantSSHKey,
			Scope: json.RawMessage(`{"host":"git.corp.example","key_secret_ref":"k"}`)}}
		placeholder := []runner.ManagedFile{{Path: runner.ComponentSecretDir + "/res.key", AgentOwned: true, Content: []byte(placement.ResidentPlaceholder(file.ID))}}
		for name, c := range map[string]struct {
			files    []runner.ManagedFile
			resident []residentValue
		}{
			"file_secret resident":    {placeholder, []residentValue{{grant: file, class: placement.ClassFileSecret, value: []byte("file-value")}}},
			"file_secret as a file":   {[]runner.ManagedFile{{Path: runner.ComponentSecretDir + "/api-token", AgentOwned: true, Content: []byte("file-value")}}, nil},
			"ssh_key resident":        {nil, []residentValue{{grant: key, class: placement.ClassSSHKey, value: []byte("key-value")}}},
			"env_secret in SecretEnv": {nil, nil},
		} {
			spec := runner.SandboxSpec{Image: "img", ManagedFiles: c.files}
			if c.files == nil && c.resident == nil {
				spec.SecretEnv = map[string]string{"RES_TOKEN": "env-value"}
			}
			rec := runSandboxSpecFor(spec, c.resident, nil, runAgentPolicy{})
			plain, _ := json.Marshal(rec)
			if !rec.Resident || len(rec.ManagedFiles) != 0 || strings.Contains(string(plain), "-value") {
				t.Errorf("%s: record %s, want Resident and no delivered file or value", name, plain)
			}
			wantMoveRefusal(t, rec, reasonMoveResidentCredential, "resident")
		}
	})

	t.Run("org config keys and managed settings", func(t *testing.T) {
		ap := runAgentPolicy{path: "/etc/claude-code/managed-settings.json", bytes: 812, hold: true, locked: true, toolRules: 3}
		rec := runSandboxSpecFor(runner.SandboxSpec{Image: "img", Env: map[string]string{"ORG_REGION": "eu"}}, nil, []string{"ORG_REGION"}, ap)
		want := runSandboxSpecAgentPolicy{Path: ap.path, Bytes: 812, Hold: true, Locked: true, ToolRules: 3}
		if !slices.Equal(rec.OrgConfigKeys, []string{"ORG_REGION"}) || rec.AgentPolicy != want {
			t.Errorf("OrgConfigKeys %v AgentPolicy %+v, want [ORG_REGION] and %+v", rec.OrgConfigKeys, rec.AgentPolicy, want)
		}
		wantMoveRefusal(t, rec, "", "")
	})
}

// minted is every per-run MCP token in spec: the path segment after the broker route in each MCP URL it carries.
func minted(spec runner.SandboxSpec) []string {
	var texts []string
	for _, f := range spec.ManagedFiles {
		texts = append(texts, string(f.Content))
	}
	for _, v := range spec.SecretEnv {
		texts = append(texts, v)
	}
	var out []string
	for _, text := range texts {
		for _, part := range strings.Split(text, "/wardyn/mcp/")[1:] {
			if tok, _, _ := strings.Cut(part, "/"); tok != "" {
				out = append(out, tok)
			}
		}
	}
	return out
}

func wantMoveRefusal(t *testing.T, rec runSandboxSpecRecord, reason, layer string) {
	t.Helper()
	if r, l := rec.moveRefusal(); r != reason || l != layer {
		t.Errorf("moveRefusal = %q %q, want %q %q", r, l, reason, layer)
	}
}
