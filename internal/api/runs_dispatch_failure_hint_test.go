// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// driverText is what a pgx failure actually says: the database host, port,
// user and SQLSTATE. None of it may reach a member-visible failure hint.
const driverText = `failed to connect to "host=pg.internal port=5432 user=wardyn": FATAL: terminating connection (SQLSTATE 57P01)`

// grantDownStore fails every grant write with driver text, wins the FAILED
// CAS, and records the hint failAndRevoke persists.
type grantDownStore struct {
	store.Store
	hints []string
}

func (*grantDownStore) CreateGrant(context.Context, types.CredentialGrant) (types.CredentialGrant, error) {
	return types.CredentialGrant{}, errors.New(driverText)
}

func (*grantDownStore) UpdateRunStateIf(context.Context, uuid.UUID, types.RunState, types.RunState) (bool, error) {
	return true, nil
}

func (s *grantDownStore) SetRunFailureHint(_ context.Context, _ uuid.UUID, hint string) error {
	s.hints = append(s.hints, hint)
	return nil
}

// #445: a grant write that fails at dispatch fails the run with a FIXED hint.
// The run's owner reads that hint, so the store's own text (host, port,
// SQLSTATE) must stay in the log. Each lane below is one CreateGrant site.
func TestDispatchGrantWriteFailure_HintCarriesNoDriverText(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		wantHint string
		author   func(s *Server, run types.AgentRun) bool
	}{
		{"azure devops entra", "could not record the Azure DevOps credential grant",
			func(s *Server, run types.AgentRun) bool {
				_, _, ok := s.authorADOEntraInjection(ctx, run, adoTestRun(t), "CERT", "KEY",
					&types.RunPolicySpec{}, map[string]string{}, nil)
				return ok
			}},
		{"aws sso", "could not record the AWS SSO credential grant",
			func(s *Server, run types.AgentRun) bool {
				_, _, ok := s.authorBedrockSSOInjection(ctx, run, llmTransport{bedrock: ssoInjectAuth()},
					awsSSOScope{perUser: true, owner: "member@corp.example"}, nil)
				return ok
			}},
		{"subscription", "could not record the subscription credential grant",
			func(s *Server, run types.AgentRun) bool {
				_, _, ok := s.authorSubscriptionInjection(ctx, run, llmTransport{}, &types.RunPolicySpec{}, nil)
				return ok
			}},
		{"bedrock bearer", "could not record the Bedrock bearer credential grant",
			func(s *Server, run types.AgentRun) bool {
				tr := llmTransport{bedrock: bedrockAuth{runtimeHost: "bedrock-runtime.us-east-1.amazonaws.com", runtimePort: 443}}
				_, _, ok := s.authorBedrockBearerInjection(ctx, run, tr, nil)
				return ok
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &grantDownStore{}
			audit := &memAudit{}
			s := fullyConfiguredBedrockServer()
			s.cfg.Store, s.cfg.Audit, s.cfg.Now = st, audit, time.Now
			if tc.author(s, types.AgentRun{ID: uuid.New()}) {
				t.Fatal("dispatch went ahead after the grant write failed")
			}
			if len(st.hints) != 1 || st.hints[0] != tc.wantHint {
				t.Fatalf("failure hints = %q, want exactly %q", st.hints, tc.wantHint)
			}
			// refuseADOEntraDispatch copies the hint into the run.create audit
			// detail; that copy must be the fixed sentence too.
			for _, row := range audit.find("run.create") {
				if detail := string(row.Data); strings.Contains(detail, `"detail"`) && strings.Contains(detail, "SQLSTATE") {
					t.Errorf("run.create audit detail carries driver text: %s", detail)
				}
			}
		})
	}
}

// recordHintStore is recordAbortStore keeping the two hints a failed record
// launch writes: the record card's and the run's.
type recordHintStore struct {
	*recordAbortStore
	cards []RecordTaskResult
	hints []string
}

func (s *recordHintStore) SetWorkspaceRecordResult(ctx context.Context, id uuid.UUID, key string, raw json.RawMessage, only string) (types.Workspace, bool, error) {
	var res RecordTaskResult
	_ = json.Unmarshal(raw, &res)
	s.cards = append(s.cards, res)
	return s.recordAbortStore.SetWorkspaceRecordResult(ctx, id, key, raw, only)
}

func (s *recordHintStore) SetRunFailureHint(_ context.Context, _ uuid.UUID, hint string) error {
	s.hints = append(s.hints, hint)
	return nil
}

// #445, record lane: a record launch whose grant write fails after CreateRun
// aborts through abort() and release(). The record card and the run's failure
// hint are both member-visible, so neither may carry the store's text.
func TestLaunchRecordRun_GrantWriteFailure_HintsCarryNoDriverText(t *testing.T) {
	h := newHarness(t)
	fake := &recordHintStore{recordAbortStore: &recordAbortStore{
		ws:       types.Workspace{ID: uuid.New(), Kind: types.WorkspaceKindLocalDir, Source: "/w", Status: types.WorkspaceScanned},
		grantErr: errors.New(driverText),
	}}
	cfg := baseTestConfig(h, fake)
	cfg.Runner = &fakeRunner{}
	cfg.Broker = h.broker
	// The api-key branch, whose CreateGrant then fails (as in
	// TestLaunchRecordRun_CreateGrantFailureFinalizesRun).
	cfg.Secrets = &memSecrets{m: map[string][]byte{"anthropic-api-key": []byte("sk-ant-test")}}
	cfg.DefaultPolicy = types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}, MinConfinementClass: types.CC2}
	srv := New(cfg)

	if _, _, err := srv.launchRecordRun(context.Background(), "alice@example.com", fake.ws, "build", "build", false); err == nil {
		t.Fatal("launchRecordRun went ahead after the grant write failed")
	}
	if len(fake.hints) != 1 || fake.hints[0] != "the workspace import step could not start" {
		t.Errorf("run failure hints = %q, want exactly the fixed sentence", fake.hints)
	}
	last := fake.cards[len(fake.cards)-1]
	if last.Status != recordStatusFailed || last.FailureHint != "launch failed: the run could not be recorded" {
		t.Errorf("record card = %q / %q, want failed with the fixed sentence", last.Status, last.FailureHint)
	}
}
