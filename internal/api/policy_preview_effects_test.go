// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func forbidPreviewSideEffects(t *testing.T, srv *Server) {
	t.Helper()
	srv.cfg.Secrets = previewSecretsSpy{Store: srv.cfg.Secrets, t: t}
	srv.cfg.Runner = previewRunnerSpy{t: t}
	srv.cfg.Identity = previewIdentitySpy{Provider: srv.cfg.Identity, t: t}
	srv.cfg.Broker = previewBrokerSpy{t: t}
	srv.cfg.Store = previewQuotaSpy{Store: srv.cfg.Store, t: t}
}

type previewIdentitySpy struct {
	identity.Provider
	t *testing.T
}

func (s previewIdentitySpy) MintRunIdentity(context.Context, uuid.UUID, string, string, string, bool) (identity.RunIdentity, error) {
	s.t.Fatal("preview minted a run identity")
	return identity.RunIdentity{}, nil
}

type previewBrokerSpy struct {
	MintBroker
	t *testing.T
}

func (s previewBrokerSpy) MintForGrant(context.Context, *identity.Claims, uuid.UUID) (broker.Minted, error) {
	s.t.Fatal("preview minted a grant")
	return broker.Minted{}, nil
}

func (s previewQuotaSpy) CreateRun(context.Context, types.AgentRun) (types.AgentRun, error) {
	s.t.Fatal("preview persisted a run")
	return types.AgentRun{}, nil
}

func (s previewQuotaSpy) CreateRunUnderCap(context.Context, types.AgentRun, int) (types.AgentRun, error) {
	s.t.Fatal("preview persisted a run under quota")
	return types.AgentRun{}, nil
}

func (s previewQuotaSpy) CreateGrant(context.Context, types.CredentialGrant) (types.CredentialGrant, error) {
	s.t.Fatal("preview persisted a grant")
	return types.CredentialGrant{}, nil
}

type previewWorkspaceLookupSpy struct {
	*ownerStore
	t *testing.T
}

func (s previewWorkspaceLookupSpy) GetWorkspace(context.Context, uuid.UUID) (types.Workspace, error) {
	s.t.Fatal("preview looked up an unattached workspace override")
	return types.Workspace{}, nil
}
