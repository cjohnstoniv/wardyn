// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Pins for the secret.write failure rows beyond secret_write_outcome_test.go: a lost reply followed by another
// writer's pair is a superseded renewal, a refusal stamp refused for another reason writes no refusal row, and each
// row names the writer that made the save.

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

// pairOvertakenSecrets is landedReplyLostSecrets whose second Put finds that another writer stored its own pair
// first: the row then holds that pair (this renewal's blob with access and refresh in place of its tokens), and the
// compare-and-set refuses this renewal's write.
type pairOvertakenSecrets struct {
	landedReplyLostSecrets
	access, refresh string
}

func (o pairOvertakenSecrets) For(owner string) secretstore.Store {
	return pairOvertakenSecrets{landedReplyLostSecrets{revisionedSecrets{o.Store.For(owner)}, o.puts}, o.access, o.refresh}
}

func (o pairOvertakenSecrets) Put(ctx context.Context, name string, v []byte) error {
	if o.puts.Load() == 1 {
		var b awsSSOBlob
		if err := json.Unmarshal(v, &b); err != nil {
			return err
		}
		b.AccessToken, b.RefreshToken = o.access, o.refresh
		raw, err := json.Marshal(b)
		if err != nil {
			return err
		}
		if err := o.Store.Put(ctx, name, raw); err != nil {
			return err
		}
	}
	return o.landedReplyLostSecrets.Put(ctx, name, v)
}

// putRefusedSecrets answers every Put with err and stores nothing, as a save refused before it wrote does (an erase
// of the owner since the write began answers ErrGenerationChanged). It counts the Puts it refused.
type putRefusedSecrets struct {
	secretstore.Store
	err  error
	puts *atomic.Int32
}

func (p putRefusedSecrets) For(owner string) secretstore.Store {
	return putRefusedSecrets{p.Store.For(owner), p.err, p.puts}
}

func (p putRefusedSecrets) Put(context.Context, string, []byte) error {
	p.puts.Add(1)
	return p.err
}

// After a lost reply, a compare-and-set refused because another writer stored a different pair is a superseded
// renewal, not this renewal's own landed write: the row keeps that writer's pair. Each case differs from the renewed
// pair in at least one token, so comparing only one token, or neither, records it as a success instead.
func TestAWSSSORefresh_PersistOvertakenAfterALostReplyKeepsTheNewerRow(t *testing.T) {
	for _, c := range []struct {
		name            string
		access, refresh string
	}{
		{"both tokens differ", "other-access-token-abcdefghij", "other-refresh-token-abcdefghij"},
		{"only the refresh token differs", "fresh-access-token-abcdefghij", "other-refresh-token-abcdefghij"},
		{"only the access token differs", "other-access-token-abcdefghij", "rotated-refresh-token-abcdefghij"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, audit, blob := ssoRefreshServer(t)
			s.cfg.Secrets = pairOvertakenSecrets{landedReplyLostSecrets{revisionedSecrets{s.cfg.Secrets}, &atomic.Int32{}}, c.access, c.refresh}
			fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"accessToken": "fresh-access-token-abcdefghij", "expiresIn": 3600,
					"refreshToken": "rotated-refresh-token-abcdefghij",
				})
			})
			if _, msg := s.refreshAWSSSOBlob(context.Background(), awsSSOTestScope(), blob); msg != "" {
				t.Fatalf("refresh answered %q, want the renewed pair served", msg)
			}
			rows := audit.find("harness.credential.refresh")
			if len(rows) != 1 || rows[0].Outcome != "success" || !strings.Contains(string(rows[0].Data), `"superseded":true`) {
				t.Fatalf("refresh rows = %+v, want one success marked superseded", rows)
			}
			if b := storedSSOBlob(t, s); b.AccessToken != c.access || b.RefreshToken != c.refresh {
				t.Fatalf("stored pair = %q/%q, want the other writer's %q/%q", b.AccessToken, b.RefreshToken, c.access, c.refresh)
			}
		})
	}
}

// A refusal stamp whose save was refused for a reason other than a lost reply did not land, so the next confirmed
// refusal stamps again: its refusal row is not written now, and no secret.write row is.
func TestResolveADOOwnPAT_StampThatDidNotLandWritesNoRefusalRow(t *testing.T) {
	f := newOwnPATRun(t)
	ownPATConfirm(t, http.StatusUnauthorized)
	f.token(t, ownPATToken, "contoso", f.now.Add(30*24*time.Hour))
	puts := &atomic.Int32{}
	f.srv.cfg.Secrets = putRefusedSecrets{f.srv.cfg.Secrets, secretstore.ErrGenerationChanged, puts}
	if w := f.resolve(t, capSub, "dev.azure.com", "?stale_jti=jti-old"); w.Code != http.StatusOK {
		t.Fatalf("resolve after a refusal = %d %s, want the token still injected", w.Code, w.Body)
	}
	if n := puts.Load(); n != 1 {
		t.Fatalf("the stamp tried %d saves, want 1", n)
	}
	for _, row := range f.audit.find(adoPATAuditOwnStore) {
		if strings.Contains(string(row.Data), `"reason":"upstream_refused"`) {
			t.Fatalf("a stamp that did not land wrote its refusal row: %+v", row)
		}
	}
	if rows := f.audit.find("secret.write"); len(rows) != 0 {
		t.Fatalf("secret.write rows = %+v, want none for a save refused before it wrote", rows)
	}
}

// Each secret.write failure row names the writer that made the save: the person for their own key or sign-in, the
// run's agent for its own-token refusal stamp, and wardynd for its own sign-in and probe writes.
func TestSecretWriteFailure_RowNamesItsWriter(t *testing.T) {
	t.Run("a person's provider key", func(t *testing.T) {
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
		wantWriter(t, lastAuditEvent(t, audit.snapshot(), "secret.write"), types.ActorHuman, "sub-member")
	})
	t.Run("a person's provider sign-in", func(t *testing.T) {
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
		if len(rows) != 1 {
			t.Fatalf("secret.write rows = %+v, want one", rows)
		}
		wantWriter(t, rows[0], types.ActorHuman, "sub-member")
	})
	t.Run("the run's own-token refusal stamp", func(t *testing.T) {
		f := newOwnPATRun(t)
		ownPATConfirm(t, http.StatusUnauthorized)
		f.token(t, ownPATToken, "contoso", f.now.Add(30*24*time.Hour))
		f.srv.cfg.Secrets = landedReplyLostSecrets{revisionedSecrets{f.srv.cfg.Secrets}, &atomic.Int32{}}
		if w := f.resolve(t, capSub, "dev.azure.com", "?stale_jti=jti-old"); w.Code != http.StatusOK {
			t.Fatalf("resolve after a refusal = %d %s, want the token still injected", w.Code, w.Body)
		}
		rows := f.audit.find("secret.write")
		if len(rows) != 1 {
			t.Fatalf("secret.write rows = %+v, want one", rows)
		}
		wantWriter(t, rows[0], types.ActorAgent, "spiffe://wardyn.local/run")
	})
	t.Run("wardynd's own writes", func(t *testing.T) {
		sec := &memSecrets{m: map[string][]byte{}}
		h, srv := secretsRBACServer(t, sec)
		srv.cfg.Secrets = rowFailingSecrets{sec}
		if err := srv.probeSecretWrite(context.Background(), awsSSOScope{perUser: true, owner: "bob"}); !errors.Is(err, secretstore.ErrRowNotWritten) {
			t.Fatalf("probe = %v, want ErrRowNotWritten", err)
		}
		wantWriter(t, lastAuditEvent(t, h.audit.snapshot(), "secret.write"), types.ActorSystem, "wardynd")
		if err := srv.storeAWSSSOBlob(context.Background(), awsSSOScope{perUser: true, owner: "bob"}, awsSSOBlob{}); !errors.Is(err, secretstore.ErrRowNotWritten) {
			t.Fatalf("aws sso store = %v, want ErrRowNotWritten", err)
		}
		wantWriter(t, lastAuditEvent(t, h.audit.snapshot(), "secret.write"), types.ActorSystem, "wardynd")
		if err := srv.storeADOEntraBlob(context.Background(), "alice", "ado-1", adoEntraBlob{}); !errors.Is(err, secretstore.ErrRowNotWritten) {
			t.Fatalf("ado entra store = %v, want ErrRowNotWritten", err)
		}
		wantWriter(t, lastAuditEvent(t, h.audit.snapshot(), "secret.write"), types.ActorSystem, "wardynd")
	})
}

// wantWriter fails t unless ev names actorType and actor.
func wantWriter(t *testing.T, ev types.AuditEvent, actorType types.ActorType, actor string) {
	t.Helper()
	if ev.ActorType != actorType || ev.Actor != actor {
		t.Fatalf("secret.write %s on %s names (%s, %q), want (%s, %q)", ev.Outcome, ev.Target, ev.ActorType, ev.Actor, actorType, actor)
	}
}
