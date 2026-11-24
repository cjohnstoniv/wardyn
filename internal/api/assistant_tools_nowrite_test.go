// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// assistantNoWriteStore fails the test on any write, except the calling session's tool_record and proposal events.
type assistantNoWriteStore struct {
	*assistantToolSpy
	t       *testing.T
	session uuid.UUID
}

func (f *assistantNoWriteStore) fail(what string) {
	f.t.Helper()
	f.t.Errorf("a tool wrote: %s", what)
}

func (f *assistantNoWriteStore) AppendAssistantEvent(ctx context.Context, session uuid.UUID, kind string, payload []byte) (types.AssistantEvent, error) {
	if session != f.session || (kind != types.AssistantEventToolRecord && kind != types.AssistantEventProposal) {
		f.fail("event " + kind)
	}
	return f.assistantToolSpy.AppendAssistantEvent(ctx, session, kind, payload)
}

func (f *assistantNoWriteStore) AppendAssistantRunEvent(context.Context, uuid.UUID, string, []byte) (types.AssistantEvent, error) {
	f.fail("run event")
	return types.AssistantEvent{}, nil
}

func (f *assistantNoWriteStore) SetAssistantDraft(context.Context, uuid.UUID, []byte) error {
	f.fail("session draft")
	return nil
}

func (f *assistantNoWriteStore) EndAssistantSession(context.Context, uuid.UUID, string) (bool, error) {
	f.fail("session end")
	return false, nil
}

func (f *assistantNoWriteStore) CreateAssistantSession(context.Context, types.AssistantSession) (types.AssistantSession, error) {
	f.fail("session create")
	return types.AssistantSession{}, nil
}

func (f *assistantNoWriteStore) CreateRun(context.Context, types.AgentRun) (types.AgentRun, error) {
	f.fail("run")
	return types.AgentRun{}, nil
}

func (f *assistantNoWriteStore) CreatePolicy(context.Context, types.RunPolicy) (types.RunPolicy, error) {
	f.fail("policy")
	return types.RunPolicy{}, nil
}

func (f *assistantNoWriteStore) PutSiteConfig(context.Context, types.SiteConfig) (types.SiteConfig, error) {
	f.fail("site config")
	return types.SiteConfig{}, nil
}

func (f *assistantNoWriteStore) UpsertGovernanceUnit(context.Context, types.GovernanceUnit) (types.GovernanceUnit, error) {
	f.fail("unit")
	return types.GovernanceUnit{}, nil
}

func (f *assistantNoWriteStore) ProposeGovernanceChange(context.Context, types.GovernanceChange, time.Duration) (types.GovernanceChange, []store.ExpiredGovernanceChange, error) {
	f.fail("governance change")
	return types.GovernanceChange{}, nil, nil
}

// Every tool in the registry runs against a store that fails on any write but its own session's tool_record and
// proposal events. The test iterates the registry, so a new tool is covered with no new case; each tool is given
// arguments its schema takes, and whether it answers 200 or a refusal, it writes nothing else.
func TestAssistantToolsWriteNothing(t *testing.T) {
	args := map[string]string{
		types.AssistantToolRead:  `{"kind":"target"}`,
		types.AssistantToolDraft: `{"target_kind":"template","parts":[{"section":"name","value":"draft"}]}`,
	}
	names := make([]string, 0, len(assistantToolRegistry()))
	for n := range assistantToolRegistry() {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			spy, srv, s, run := assistantToolSession(t, types.AssistantCompose)
			spy.site.Assistant.RunHistoryCounts = true
			nw := &assistantNoWriteStore{assistantToolSpy: spy, t: t, session: s.ID}
			srv.cfg.Store = nw
			a, ok := args[name]
			if !ok {
				a = `{"target_kind":"template","draft":{"name":"draft","document":{"api_version":"v1","kind":"Template","intent":{}}}}`
			}
			w := assistantToolCall(t, srv, run, name, s.ID, a)
			if w.Code >= http.StatusInternalServerError {
				t.Logf("%s answered %d %s", name, w.Code, w.Body)
			}
		})
	}
}
