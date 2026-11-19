// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// With detect_secrets on, the known-secret check covers every secret the run
// is given: an env_secret value, a file_secret value, and the forms a line
// break gives them. These tests dispatch whole runs and read what the proxy
// sidecar is configured with and what the revive row stores.

const (
	// The operator's "corp-token" ends in a line break (fileOperValue), so it has two forms.
	knownEnvTokenSecret = "corp-token"
	knownEnvValue       = fileOperValue
	knownEnvTrimmed     = "operator-file-secret-0001"
)

// failsSecondPut is a proxyConfigStore whose second write fails: the first
// store (before the proxy exists) succeeds, the one after the files joined
// the spec does not.
type failsSecondPut struct {
	*proxyConfigStore
	puts atomic.Int32
}

func (s *failsSecondPut) PutRunProxyConfig(ctx context.Context, id uuid.UUID, sealed []byte) error {
	if s.puts.Add(1) >= 2 {
		return errors.New("pgx: SQLSTATE 53300 too many connections")
	}
	return s.proxyConfigStore.PutRunProxyConfig(ctx, id, sealed)
}

type knownSecretFixture struct {
	srv   *Server
	rn    *fakeRunner
	st    *dispatchTestStore
	ps    *proxyConfigStore
	sec   fileSecretStore
	audit *recRecorder
	run   types.AgentRun
}

func newKnownSecretFixture(t *testing.T) *knownSecretFixture {
	t.Helper()
	rn := &fakeRunner{}
	srv, st, audit, run := dispatchTeardownFixture(t, rn, types.RunPending)
	run.Task, run.CreatedBy = "", fileOwner
	st.run = run
	ps := &proxyConfigStore{dispatchTestStore: st, sealed: map[uuid.UUID][]byte{}}
	_, sec, _ := fileSecretServer(t)
	srv.cfg.Store, srv.cfg.Secrets, srv.cfg.MaskRegistry = ps, sec, secretmask.NewRegistry()
	srv.cfg.RunConfigKey = make([]byte, kek.DEKSize)
	srv.cfg.ControlPlaneURL = "http://127.0.0.1:8080" // the fixture's in-cluster name is refused as plain http when the stored row loads
	return &knownSecretFixture{srv: srv, rn: rn, st: st, ps: ps, sec: sec, audit: audit, run: run}
}

func (f *knownSecretFixture) dispatch(t *testing.T, policy types.RunPolicySpec) {
	t.Helper()
	dispatchOf(f.srv, f.run, policy)
	if f.rn.createCalls != 1 {
		t.Fatalf("the sandbox was not created; events: %s", auditDump(f.audit.snapshot(), f.run.ID))
	}
}

// sidecarValues is the corpus the created sandbox's proxy sidecar is configured with.
func (f *knownSecretFixture) sidecarValues() []string {
	if li := f.rn.lastSpec.ProxyConfig.Policy.LLMInspection; li != nil {
		return li.WorkspaceSecretValues
	}
	return nil
}

// storedValues is the corpus in the sealed row a revive rebuilds the proxy from.
func (f *knownSecretFixture) storedValues(t *testing.T) []string {
	t.Helper()
	raw, err := f.srv.loadRunProxyConfig(context.Background(), f.run.ID)
	if err != nil {
		t.Fatalf("loadRunProxyConfig: %v", err)
	}
	cfg, err := proxy.LoadConfigBytes(raw)
	if err != nil {
		t.Fatalf("stored config: %v", err)
	}
	if cfg.Policy.LLMInspection == nil {
		return nil
	}
	return cfg.Policy.LLMInspection.WorkspaceSecretValues
}

func detectingPolicy(grants ...types.GrantSpec) types.RunPolicySpec {
	return types.RunPolicySpec{
		AllowedDomains: []string{"api.corp.test"},
		LLMInspection:  &types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true},
		EligibleGrants: grants,
	}
}

func TestKnownSecrets_EnvSecretValueJoinsTheCorpus(t *testing.T) {
	f := newKnownSecretFixture(t)
	f.dispatch(t, detectingPolicy(envSecretGrant("CORP_TOKEN", knownEnvTokenSecret)))
	for _, want := range []string{knownEnvValue, knownEnvTrimmed} {
		if !slices.Contains(f.sidecarValues(), want) {
			t.Errorf("sidecar corpus %q lacks the env_secret form %q", f.sidecarValues(), want)
		}
		if !slices.Contains(f.storedValues(t), want) {
			t.Errorf("stored row corpus %q lacks the env_secret form %q", f.storedValues(t), want)
		}
	}
}

// The file's value joins after the first store, so the sidecar spec and the
// row a revive reads must both be written again with it.
func TestKnownSecrets_FileSecretValueIsInTheSidecarConfigAndTheStoredRow(t *testing.T) {
	f := newKnownSecretFixture(t)
	f.dispatch(t, detectingPolicy(fileSecretGrant("api-token", "deploy-token", true)))
	if !slices.Contains(f.sidecarValues(), fileOwnValue) {
		t.Errorf("sidecar corpus %q lacks the file_secret value", f.sidecarValues())
	}
	if !slices.Contains(f.storedValues(t), fileOwnValue) {
		t.Errorf("stored row corpus %q lacks the file_secret value: a revived proxy would not check it", f.storedValues(t))
	}
}

// The whole value only: as stored, without its trailing line break, and with
// LF and CRLF line ends. Never one line of it alone.
func TestKnownSecrets_FormsAreTheWholeValueOnly(t *testing.T) {
	f := newKnownSecretFixture(t)
	const multi = "first-line-secret-0001\nsecond-line-secret-0002\n"
	if err := f.sec.For("").Put(context.Background(), "multi-line", []byte(multi)); err != nil {
		t.Fatal(err)
	}
	f.dispatch(t, detectingPolicy(envSecretGrant("MULTI_TOKEN", "multi-line")))
	got := f.sidecarValues()
	for _, want := range []string{
		multi,
		"first-line-secret-0001\nsecond-line-secret-0002",
		"first-line-secret-0001\r\nsecond-line-secret-0002",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("corpus %q lacks the whole-value form %q", got, want)
		}
	}
	for _, line := range []string{"first-line-secret-0001", "second-line-secret-0002"} {
		if slices.Contains(got, line) {
			t.Errorf("corpus holds one line %q of a multi-line value; only whole values are checked", line)
		}
	}
}

// No new setting: with detect_secrets off, or with no inspection block, a
// placed secret adds nothing to any policy.
func TestKnownSecrets_OffWithoutDetectSecrets(t *testing.T) {
	grants := []types.GrantSpec{envSecretGrant("CORP_TOKEN", knownEnvTokenSecret), fileSecretGrant("api-token", "deploy-token", true)}
	t.Run("detect_secrets off", func(t *testing.T) {
		f := newKnownSecretFixture(t)
		pol := detectingPolicy(grants...)
		pol.LLMInspection = &types.LLMInspectionSpec{Mode: "alert", DetectPII: true}
		f.dispatch(t, pol)
		if got := f.sidecarValues(); len(got) != 0 {
			t.Errorf("sidecar corpus %q; want none with detect_secrets off", got)
		}
		if got := f.storedValues(t); len(got) != 0 {
			t.Errorf("stored corpus %q; want none with detect_secrets off", got)
		}
	})
	t.Run("no inspection block", func(t *testing.T) {
		f := newKnownSecretFixture(t)
		pol := detectingPolicy(grants...)
		pol.LLMInspection = nil
		f.dispatch(t, pol)
		if f.rn.lastSpec.ProxyConfig.Policy.LLMInspection != nil {
			t.Errorf("a run with no llm_inspection block was given one: %+v", f.rn.lastSpec.ProxyConfig.Policy.LLMInspection)
		}
	})
}

// The policy the caller passed in is shared (a pointer to the inspection
// block); dispatch writes its corpus on a copy, so the caller's block keeps
// its names and gains no values, from names resolved or from placed secrets.
func TestKnownSecrets_DispatchLeavesTheSharedPolicyAlone(t *testing.T) {
	f := newKnownSecretFixture(t)
	li := &types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true, WorkspaceSecretNames: []string{knownEnvTokenSecret}}
	pol := detectingPolicy(envSecretGrant("CORP_TOKEN", knownEnvTokenSecret), fileSecretGrant("api-token", "deploy-token", true))
	pol.LLMInspection = li
	f.dispatch(t, pol)
	if len(li.WorkspaceSecretValues) != 0 {
		t.Errorf("dispatch wrote %d value(s) into the shared policy's inspection block", len(li.WorkspaceSecretValues))
	}
	if len(f.sidecarValues()) == 0 {
		t.Error("the sidecar corpus is empty; the copy should carry the resolved and placed values")
	}
}

func TestKnownSecrets_ResolvingNamesCopiesTheBlockBeforeAppending(t *testing.T) {
	f := newKnownSecretFixture(t)
	li := &types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true, WorkspaceSecretNames: []string{knownEnvTokenSecret}}
	pol := types.RunPolicySpec{LLMInspection: li}
	f.srv.resolveLLMInspectionSecrets(context.Background(), f.run, &pol)
	if len(li.WorkspaceSecretValues) != 0 {
		t.Errorf("the original block gained %d value(s); the append must land on a copy", len(li.WorkspaceSecretValues))
	}
	if pol.LLMInspection == li || len(pol.LLMInspection.WorkspaceSecretValues) == 0 {
		t.Errorf("the resolved values must be on the policy's own copy; got block %p (original %p) with %v",
			pol.LLMInspection, li, pol.LLMInspection.WorkspaceSecretValues)
	}
}

// The policy.resolve row keeps a count, taken before the files join, so it
// counts the env values and not the file's; no row carries a value.
func TestKnownSecrets_AuditCountsExcludeFileValuesAndCarryNoValue(t *testing.T) {
	for _, c := range []struct {
		name   string
		grants []types.GrantSpec
		want   []string
	}{
		{"file only", []types.GrantSpec{fileSecretGrant("api-token", "deploy-token", true)}, nil},
		// The env value has two forms (as stored, without its line break); the file's adds none to the count.
		{"env and file", []types.GrantSpec{envSecretGrant("CORP_TOKEN", knownEnvTokenSecret), fileSecretGrant("api-token", "deploy-token", true)},
			[]string{"<2 value(s) redacted>"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newKnownSecretFixture(t)
			f.dispatch(t, detectingPolicy(c.grants...))
			var resolved *types.AuditEvent
			evs := f.audit.snapshot()
			for i := range evs {
				ev := &evs[i]
				if ev.Action == "run.policy.resolve" {
					resolved = ev
				}
				for _, v := range []string{fileOwnValue, knownEnvTrimmed} {
					if strings.Contains(string(ev.Data), v) {
						t.Errorf("audit row %s carries a placed secret's value", ev.Action)
					}
				}
			}
			if resolved == nil {
				t.Fatal("no run.policy.resolve row")
			}
			var datum struct {
				LLMInspection *types.LLMInspectionSpec `json:"llm_inspection"`
			}
			if err := json.Unmarshal(resolved.Data, &datum); err != nil || datum.LLMInspection == nil {
				t.Fatalf("policy.resolve datum: %v, %s", err, resolved.Data)
			}
			if got := datum.LLMInspection.WorkspaceSecretValues; !slices.Equal(got, c.want) {
				t.Errorf("policy.resolve counts %v, want %v: the count is taken before files join", got, c.want)
			}
		})
	}
}

func TestKnownSecrets_ASecondStoreThatFailsFailsTheRun(t *testing.T) {
	f := newKnownSecretFixture(t)
	fail := &failsSecondPut{proxyConfigStore: f.ps}
	f.srv.cfg.Store = fail
	dispatchOf(f.srv, f.run, detectingPolicy(fileSecretGrant("api-token", "deploy-token", true)))
	if f.rn.createCalls != 0 {
		t.Error("CreateSandbox ran for a run whose final proxy config was not stored")
	}
	if got := f.st.State(); got != types.RunFailed {
		t.Errorf("state = %s, want FAILED", got)
	}
	if hint := f.st.FailureHint(); strings.Contains(hint, "53300") {
		t.Errorf("failure hint %q carries the database's error text", hint)
	}
}
