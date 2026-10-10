// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The sign-in capture doors resolve a held run's re-auth request while they hold a sign-in lock, which the run
// lock must precede, so each door resumes the run itself, after the lock is released, from the run ids the
// resolver returned. These pin that wiring door by door: a door that drops its resume leaves an idle-paused
// run frozen, because the sweep's backstop resumes only waiting pauses.

// resumeStore answers the run the sign-in is held for as a paused one, with the pause marks of the wiring fixtures.
type resumeStore struct{ pauseMarks }

func (s *resumeStore) pausedRun(base types.AgentRun) types.AgentRun {
	base.State, base.SandboxRef = types.RunRunning, "sbx-signin"
	if paused, _ := s.state(); paused {
		now := time.Now().UTC()
		base.PausedAt, base.PausedReason = &now, types.PauseIdle
	}
	return base
}

type adoLoginResumeStore struct {
	*adoSignInStore
	resumeStore
}

func (s *adoLoginResumeStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	base, _ := s.adoSignInStore.GetRun(ctx, id)
	return s.pausedRun(base), nil
}

// TestRunPause_ADOLoginCaptureResumesAPausedRun: the console login's Azure DevOps capture (CaptureLoginGrant)
// resolving a paused run's sign-in request thaws it.
func TestRunPause_ADOLoginCaptureResumesAPausedRun(t *testing.T) {
	f := newADOSignInFixture(t)
	if w := f.resolveQ(t, "?phase=boot"); w.Code != http.StatusOK {
		t.Fatalf("boot resolve: %d %s", w.Code, w.Body.String())
	}
	f.fake.SetInvalidGrant(true)
	f.at(time.Now().Add(time.Minute))
	id := pendingID(t, f.resolveQ(t, ""), reauthPendingState)
	f.fake.SetInvalidGrant(false)
	f.at(time.Now().Add(2 * time.Minute))

	ps := &adoLoginResumeStore{adoSignInStore: f.st}
	ps.paused = true
	fr := &freezingRunner{Runner: f.srv.cfg.Runner}
	f.srv.cfg.Store, f.srv.cfg.Runner = ps, fr

	granted := strings.Join(append([]string{"openid", "profile", "email", entraOfflineAccessScope}, f.cfg.Scopes...), " ")
	f.srv.CaptureLoginGrant(context.Background(), f.subject, oidc.LoginGrant{
		RefreshToken: "the-refresh-token-this-login-earned", Scope: granted, Expiry: time.Now().Add(time.Hour),
	})
	if st := f.row(id).State; st != types.ApprovalApproved {
		t.Fatalf("after the login capture the sign-in request is %s, want APPROVED", st)
	}
	if n := fr.thawCount(); n != 1 {
		t.Fatalf("thaws = %d after the login capture resolved the request, want 1", n)
	}
	if paused, _ := ps.state(); paused {
		t.Error("the run is still marked paused after its request resolved")
	}
}

type azureResumeStore struct {
	*azA3Store
	resumeStore
}

func (s *azureResumeStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	base, _ := s.azA3Store.GetRun(ctx, id)
	return s.pausedRun(base), nil
}

// TestRunPause_AzureFoundryCaptureResumesAPausedRun: the Azure provider's sign-in callback resolving a paused
// run's sign-in request thaws it.
func TestRunPause_AzureFoundryCaptureResumesAPausedRun(t *testing.T) {
	f := newAzA3Fixture(t)
	f.signIn(t, azUIDAnthropic)
	plan, _, _, _ := f.dispatch(t, "claude-code", "foundry-claude")
	if w := f.resolve(t, plan, "?phase=boot"); w.Code != http.StatusOK {
		t.Fatalf("boot resolve: %d %s", w.Code, w.Body.String())
	}
	f.fake.SetInvalidGrant(true)
	id := pendingID(t, f.resolve(t, plan, ""), reauthPendingState)
	f.fake.SetInvalidGrant(false)
	f.srv.cfg.Now = func() time.Time { return time.Now().Add(2 * time.Minute) }

	ps := &azureResumeStore{azA3Store: f.st}
	ps.paused = true
	fr := &freezingRunner{Runner: f.srv.cfg.Runner}
	f.srv.cfg.Store, f.srv.cfg.Runner = ps, fr

	f.signIn(t, azUIDAnthropic)
	if row, _ := f.st.approvals.Get(context.Background(), id); row.State != types.ApprovalApproved {
		t.Fatalf("after the capture the request is %s, want APPROVED", row.State)
	}
	if n := fr.thawCount(); n != 1 {
		t.Fatalf("thaws = %d after the Azure capture resolved the request, want 1", n)
	}
	if paused, _ := ps.state(); paused {
		t.Error("the run is still marked paused after its request resolved")
	}
}

type awsUploadResumeStore struct {
	ssoLoginRunStore
	resumeStore
	approvals *fakeApprovals
}

func (s *awsUploadResumeStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	base, _ := s.ssoLoginRunStore.GetRun(ctx, id)
	base.CreatedAt = time.Now().Add(time.Minute) // the login run is newer than the raise (I6)
	return s.pausedRun(base), nil
}

func (s *awsUploadResumeStore) ResolveReauthApproval(_ context.Context, id uuid.UUID, d types.ApprovalDecision, _ types.AuditEvent) (types.ApprovalRequest, error) {
	s.approvals.mu.Lock()
	defer s.approvals.mu.Unlock()
	ap := s.approvals.byID[id]
	ap.State, ap.DecidedBy = d.State, d.DecidedBy
	s.approvals.byID[id] = ap
	return ap, nil
}

// TestRunPause_AWSCaptureResumesAPausedRun: the AWS sign-in's upload door (PUT /internal/sso-token) resolving a
// paused run's re-auth request thaws it. resolveReauth, the reconcile-on-read path, is pinned separately by
// TestRunPause_AWSReauthResolveResumesAPausedRun.
func TestRunPause_AWSCaptureResumesAPausedRun(t *testing.T) {
	runID := uuid.New()
	srv, _, tok := newSSOUploadSrvWith(t, uploadStamp(runID, defaultUploadProvider(), uploadOwner), types.SiteConfig{}, runID)
	fa := newFakeApprovals()
	apID := uuid.New()
	fa.byID[apID] = types.ApprovalRequest{
		ID: apID, RunID: uuid.New(), Kind: types.ApprovalCredentialReauth, State: types.ApprovalPending,
		RequestedAt: time.Now().Add(-time.Hour),
		RequestedScope: mustJSON(map[string]any{
			"owner": uploadOwner, "credential_source": string(types.CredentialSourcePerUser), "provider_uid": uploadProviderUID,
			"provider": awsSSOProvider, "mechanism": "aws_sso",
		}),
	}
	ps := &awsUploadResumeStore{ssoLoginRunStore: srv.cfg.Store.(ssoLoginRunStore), approvals: fa}
	ps.paused = true
	fr := &freezingRunner{Runner: srv.cfg.Runner}
	srv.cfg.Store, srv.cfg.Approvals, srv.cfg.Runner = ps, fa, fr

	w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+runID.String(), tok, validSSOBody)
	if w.Code != http.StatusNoContent {
		t.Fatalf("upload = %d %s, want 204", w.Code, w.Body.String())
	}
	if got, _ := fa.Get(context.Background(), apID); got.State != types.ApprovalApproved {
		t.Fatalf("after the upload the re-auth request is %s, want APPROVED", got.State)
	}
	if n := fr.thawCount(); n != 1 {
		t.Fatalf("thaws = %d after the AWS capture resolved the request, want 1", n)
	}
	if paused, _ := ps.state(); paused {
		t.Error("the run is still marked paused after its request resolved")
	}
}
