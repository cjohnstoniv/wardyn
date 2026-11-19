// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Rule 18 for the writers whose store-mode Put can answer
// secretstore.ErrRowNotWritten: the write may have landed behind the error, so
// each one records a secret.write failure with reason "row".

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// landedReplyLostSecrets is revisionedSecrets whose Put is the compare-and-set
// the Postgres store's is, and whose first Put lands but answers
// ErrRowNotWritten, as a commit whose reply was lost does.
type landedReplyLostSecrets struct {
	revisionedSecrets
	puts *atomic.Int32
}

func (l landedReplyLostSecrets) For(owner string) secretstore.Store {
	return landedReplyLostSecrets{revisionedSecrets{l.Store.For(owner)}, l.puts}
}

func (l landedReplyLostSecrets) Put(ctx context.Context, name string, v []byte) error {
	if want, ok := secretstore.IfRevisionFrom(ctx); ok {
		if rev, err := l.Revision(ctx, name); err != nil || rev != want {
			return errors.Join(secretstore.ErrRevisionChanged, err)
		}
	}
	if err := l.Store.Put(ctx, name, v); err != nil || l.puts.Add(1) > 1 {
		return err
	}
	return fmt.Errorf("pg secretstore: put: the value reached azurekv, but whether the row did is unknown: %w", secretstore.ErrRowNotWritten)
}

func TestPutProviderCredential_RowNotWrittenIsAudited(t *testing.T) {
	site := credentialSite(keyProvider("anthropic", "claude-code"))
	srv := modelProvidersStatusSrv(t, site, &capStore{})
	srv.cfg.Secrets = rowFailingSecrets{&memSecrets{m: map[string][]byte{}}}
	srv.router = srv.routes()
	audit := srv.cfg.Audit.(*recRecorder)
	member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser)
	w := doSSO(t, srv, http.MethodPut, "/api/v1/model-providers/anthropic/credential", member, `{"value":"sk-ant-member-own-key-0001"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("PUT = %d %s, want 500", w.Code, w.Body.String())
	}
	ev := lastAuditEvent(t, audit.snapshot(), "secret.write")
	d := lastAuditData(t, audit, "secret.write")
	if ev.Outcome != "failure" || ev.Target != providerSecretName(site.ModelProviders.Providers[0].UID, providerKeyPart) ||
		d["reason"] != "row" || d["secret_owner"] == nil || d["secret_owner"] == "" || len(d) != 2 {
		t.Fatalf("secret.write = (%s, %s, %v); want a failure on the provider's key with reason row in the person's namespace", ev.Outcome, ev.Target, d)
	}
	for _, row := range audit.snapshot() {
		if row.Action == "model_provider.credential.write" {
			t.Fatalf("a save that answered 500 wrote %s", row.Action)
		}
	}
}

func TestProviderSignInCapture_RowNotWrittenIsAudited(t *testing.T) {
	site := credentialSite(subProvider("claude"), subProvider("claude-team"), ssoProvider())
	p := site.ModelProviders.Providers[0]
	member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser)
	srv, _, audit, sec := signInFixture(t, nil, site)
	srv.cfg.Secrets = rowFailingSecrets{sec}
	srv.router = srv.routes()
	code, body := signIn(t, srv, member, p.ID)
	if code != http.StatusOK {
		t.Fatalf("sign-in = %d %s", code, body)
	}
	w := doSSO(t, srv, http.MethodPut, "/api/v1/model-providers/"+p.ID+"/sign-in", member,
		fmt.Sprintf(`{"run_id":%q,"token":%q}`, signInRunID(t, body), "sk-ant-oat01-member-own-claude-sign-in"))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("capture = %d %s, want 500", w.Code, w.Body.String())
	}
	rows := audit.find("secret.write")
	want := `{"reason":"row","secret_owner":"` + loginStamp(t, audit).Owner + `"}`
	if len(rows) != 1 || rows[0].Outcome != "failure" || rows[0].Target != providerSecretName(p.UID, providerOAuthPart) || string(rows[0].Data) != want {
		t.Fatalf("secret.write rows = %+v, want one failure on the provider's sign-in with %s", rows, want)
	}
	if rows := audit.find("harness.credential.capture"); len(rows) != 0 {
		t.Fatalf("a capture that answered 500 wrote %d capture rows", len(rows))
	}
}

// The refusal stamp's save may land behind ErrRowNotWritten, and a landed
// stamp ends every later attempt, so its refusal row is written at once.
func TestResolveADOOwnPAT_StampWhoseSaveMayHaveLandedIsAudited(t *testing.T) {
	f := newOwnPATRun(t)
	ownPATConfirm(t, http.StatusUnauthorized)
	f.token(t, ownPATToken, "contoso", f.now.Add(30*24*time.Hour))
	f.srv.cfg.Secrets = landedReplyLostSecrets{revisionedSecrets{f.srv.cfg.Secrets}, &atomic.Int32{}}
	for _, jti := range []string{"jti-old", "jti-older"} {
		if w := f.resolve(t, capSub, "dev.azure.com", "?stale_jti="+jti); w.Code != http.StatusOK {
			t.Fatalf("resolve after a refusal = %d %s, want the token still injected", w.Code, w.Body)
		}
	}
	writes := f.audit.find("secret.write")
	if len(writes) != 1 || writes[0].Outcome != "failure" || writes[0].Target != adoOwnPATSecretName(ownPATRowID) ||
		writes[0].ActorType != types.ActorAgent || string(writes[0].Data) != `{"reason":"row","secret_owner":"`+capSub+`"}` {
		t.Fatalf("secret.write rows = %+v, want one agent failure on the own token with reason row", writes)
	}
	if rows := f.audit.find(adoPATAuditOwnStore); len(rows) != 1 || !strings.Contains(string(rows[0].Data), `"reason":"upstream_refused"`) {
		t.Fatalf("ado_pat.own.store rows = %+v, want one with reason upstream_refused", rows)
	}
	blob, found, err := f.srv.readADOOwnPAT(secretstore.WithPurpose(context.Background(), secretstore.PurposeStatus), capSub, ownPATRowID)
	if err != nil || !found || blob.RefusedAt == nil {
		t.Fatalf("stored = %+v (found %v, err %v), want the stamp that landed", blob, found, err)
	}
}

// A persist attempt whose commit landed behind a lost reply leaves the next
// attempt's compare-and-set facing the renewal's own row: a stored renewal,
// recorded as a success, not as a newer row kept.
func TestAWSSSORefresh_PersistMeetingItsOwnLandedWriteIsASuccess(t *testing.T) {
	s, audit, blob := ssoRefreshServer(t)
	s.cfg.Secrets = landedReplyLostSecrets{revisionedSecrets{s.cfg.Secrets}, &atomic.Int32{}}
	fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accessToken": "fresh-access-token-abcdefghij", "expiresIn": 3600,
			"refreshToken": "rotated-refresh-token-abcdefghij",
		})
	})
	got, msg := s.refreshAWSSSOBlob(context.Background(), awsSSOTestScope(), blob)
	if msg != "" || got.AccessToken != "fresh-access-token-abcdefghij" {
		t.Fatalf("refresh = %q %q, want the renewed pair served", got.AccessToken, msg)
	}
	rows := audit.find("harness.credential.refresh")
	if len(rows) != 1 || rows[0].Outcome != "success" || strings.Contains(string(rows[0].Data), "superseded") {
		t.Fatalf("refresh rows = %+v, want one success that is not superseded", rows)
	}
	if b := storedSSOBlob(t, s); b.AccessToken != got.AccessToken || b.RefreshToken != "rotated-refresh-token-abcdefghij" {
		t.Fatalf("stored pair = %q/%q, want the renewed pair", b.AccessToken, b.RefreshToken)
	}
	if w := audit.find("secret.write"); len(w) != 1 || w[0].Outcome != "failure" {
		t.Fatalf("secret.write rows = %+v, want the first attempt's failure only", w)
	}
}
