// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/erasure"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The AWS CLI row's session at the sink, its capture, and its lifetime.

// cliResolveStore is the re-auth fixture's store with the component store the
// CLI arm re-derives from: the live org row and the run's own snapshot.
type cliResolveStore struct {
	*reauthStore
	fakeComponentStore
	attached []types.RunComponent
}

func (c *cliResolveStore) ListRunComponents(context.Context, uuid.UUID) ([]types.RunComponent, error) {
	return c.attached, nil
}

func cliSnapshotScope(t *testing.T, sn awsSSOScopeSnapshot) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"host": cliAWSPortal, "header": awsSSOInjectHeader, "format": "%s",
		"secret_name": types.AWSSSOAccessTokenSecret, "snapshot": sn,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func cliSnapshot(owner string) awsSSOScopeSnapshot {
	a := cliAWSRow()
	return awsSSOScopeSnapshot{OwnerSubject: owner, CredentialSource: string(types.CredentialSourcePerUser),
		Mechanism: cliAWSMechanism, SSOAccountID: a.AccountID, SSORoleName: a.RoleName, Region: a.SSORegion,
		ComponentID: cliAWSID, SSOStartURL: a.StartURL}
}

// cliResolveFixture is a run that attached the AWS CLI row, whose grant
// carries sn and whose live row is def (nil: deleted).
func cliResolveFixture(t *testing.T, sn awsSSOScopeSnapshot, def *types.ComponentDefinition, attached bool) (*reauthFixture, *cliResolveStore) {
	t.Helper()
	f := newReauthFixture(t, nil)
	f.st.run.ModelProviderID = ""
	f.st.grants[0].Spec.Scope = cliSnapshotScope(t, sn)
	f.brk.setHost(cliAWSPortal)
	id := uuid.MustParse(cliAWSID)
	st := &cliResolveStore{reauthStore: f.st}
	if def != nil {
		if _, err := st.CreateComponent(context.Background(), types.Component{ID: id, Name: "AWS", Definition: *def}); err != nil {
			t.Fatal(err)
		}
	}
	if attached {
		st.attached = []types.RunComponent{{ComponentID: &id, Name: "AWS", Definition: cliAWSRowDef()}}
	}
	f.srv.cfg.Store = st
	return f, st
}

func putCLIBlobIn(t *testing.T, f *reauthFixture, owner string, b awsSSOBlob) {
	t.Helper()
	putCLIBlob(t, f.secrets, owner, b)
}

// The sink re-derives a CLI grant's scope from the live row and the run's own
// snapshot on every resolve and requires equality: a row re-pinned, deleted or
// not on the run, a session for another role, or another person's grant is a
// 403, never a different credential.
func TestAWSCLI_SnapshotDriftRefused(t *testing.T) {
	live := cliAWSRowDef()
	repinned := cliAWSRowDef()
	repinned.CLI.AWS.RoleName = "Admin"
	otherRole := cliAWSBlob()
	otherRole.RoleName = "Admin"
	for _, tc := range []struct {
		name     string
		sn       awsSSOScopeSnapshot
		def      *types.ComponentDefinition
		attached bool
		blob     awsSSOBlob
		code     int
	}{
		{"live and equal", cliSnapshot(subOwner), &live, true, cliAWSBlob(), http.StatusOK},
		{"the row was re-pinned to another role", cliSnapshot(subOwner), &repinned, true, cliAWSBlob(), http.StatusForbidden},
		{"the row was deleted", cliSnapshot(subOwner), nil, true, cliAWSBlob(), http.StatusForbidden},
		{"the run does not carry the row", cliSnapshot(subOwner), &live, false, cliAWSBlob(), http.StatusForbidden},
		{"the stored session is for another role", cliSnapshot(subOwner), &live, true, otherRole, http.StatusForbidden},
		{"the grant names another owner", cliSnapshot("bob@example.com"), &live, true, cliAWSBlob(), http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := cliResolveFixture(t, tc.sn, tc.def, tc.attached)
			putCLIBlobIn(t, f, subOwner, tc.blob)
			w := f.resolve(t)
			if w.Code != tc.code {
				t.Fatalf("resolve = %d %s, want %d", w.Code, w.Body.String(), tc.code)
			}
			if tc.code == http.StatusOK {
				var resp types.ResolvedInjection
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.Value != cliAWSToken || resp.Header != awsSSOInjectHeader {
					t.Fatalf("resolve = %+v (%v), want the owner's session on the header", resp, err)
				}
				return
			}
			if reasonOf(t, w) != reasonScopeChanged {
				t.Errorf("reason = %q, want %q", reasonOf(t, w), reasonScopeChanged)
			}
			if f.audit.has("secret.read", "success") {
				t.Error("a refused resolve recorded a successful read")
			}
		})
	}
}

// A lapsed CLI session holds the run on a request naming the row, which only
// that person's sign-in for that row answers.
func TestAWSCLI_LapsedSessionHoldsOnTheRow(t *testing.T) {
	live := cliAWSRowDef()
	f, _ := cliResolveFixture(t, cliSnapshot(subOwner), &live, true)
	dead := cliAWSBlob()
	dead.ExpiresAt = time.Now().Add(-time.Hour).UTC()
	putCLIBlobIn(t, f, subOwner, dead)
	if w := f.resolve(t); w.Code != http.StatusLocked {
		t.Fatalf("resolve = %d %s, want 423", w.Code, w.Body.String())
	}
	rows, _ := f.srv.cfg.Approvals.List(context.Background(), types.ApprovalPending)
	if len(rows) != 1 {
		t.Fatalf("pending rows = %d, want 1", len(rows))
	}
	var sc map[string]string
	_ = json.Unmarshal(rows[0].RequestedScope, &sc)
	if sc["component_id"] != cliAWSID || sc["provider_uid"] != "" || sc["mechanism"] != cliAWSMechanism {
		t.Errorf("requested scope = %v, want the row and no provider", sc)
	}
	login := types.AgentRun{CreatedAt: time.Now().Add(time.Minute)}
	if !reauthResolvableBy(rows[0], cliAWSScope(cliAWSID, subOwner), login) {
		t.Error("a sign-in for the row does not answer its hold")
	}
	if reauthResolvableBy(rows[0], reauthScope(subOwner), login) || reauthResolvableBy(rows[0], cliAWSScope(uuid.NewString(), subOwner), login) {
		t.Error("a sign-in for a provider, or for another row, answers the row's hold")
	}
}

// cliLoginRunStore is the upload fixture's store with the component store the
// capture re-checks the row in.
type cliLoginRunStore struct {
	ssoLoginRunStore
	fakeComponentStore
}

// cliUpload is an AWS sign-in sandbox launched through the CLI row's door by
// owner, its stamp as launchHarnessLoginRun writes it, with the live row def
// (nil: deleted).
func cliUpload(t *testing.T, owner string, def *types.ComponentDefinition) (*Server, *memSecrets, *recRecorder, string, uuid.UUID) {
	t.Helper()
	runID := uuid.New()
	a := cliAWSRow()
	events := []types.AuditEvent{{
		ID: uuid.New(), RunID: &runID, ActorType: types.ActorSystem, Actor: "wardynd",
		Action: "harness.login.start", Target: runID.String(), Outcome: "success",
		Data: mustJSON(map[string]any{
			"provider": awsSSOProvider, "sso_start_url": a.StartURL,
			"credential_source": string(types.CredentialSourcePerUser), "owner": owner,
			"sso_account_id": a.AccountID, "sso_role_name": a.RoleName,
			"cli_component_id": cliAWSID, "sso_region": a.SSORegion,
		}),
	}}
	srv, sec, tok := newSSOUploadSrvWith(t, events, types.SiteConfig{}, runID)
	sec.owned = map[string]map[string][]byte{}
	st := &cliLoginRunStore{ssoLoginRunStore: srv.cfg.Store.(ssoLoginRunStore)}
	if def != nil {
		if _, err := st.CreateComponent(context.Background(), types.Component{ID: uuid.MustParse(cliAWSID), Name: "AWS", Definition: *def}); err != nil {
			t.Fatal(err)
		}
	}
	rec := &recRecorder{}
	srv.cfg.Store, srv.cfg.Audit = st, rec
	return srv, sec, rec, tok, runID
}

func cliUploadBody(b awsSSOBlob) string {
	raw, _ := json.Marshal(map[string]any{
		"access_token": b.AccessToken, "refresh_token": "cli-refresh", "start_url": b.StartURL, "region": b.Region,
		"account_id": b.AccountID, "role_name": b.RoleName, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
	return string(raw)
}

func cliCaptureRows(rec *recRecorder) []types.AuditEvent {
	return slices.DeleteFunc(rec.snapshot(), func(ev types.AuditEvent) bool { return ev.Action != "cli.signin.capture" })
}

// A CLI sign-in lands under the row's own name in the signer's namespace,
// audited as cli.signin.capture, and only while the row still pins what it
// was launched with.
func TestAWSCLI_CaptureLandsUnderTheRow(t *testing.T) {
	live := cliAWSRowDef()
	srv, sec, rec, tok, runID := cliUpload(t, subOwner, &live)
	if w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+runID.String(), tok, cliUploadBody(cliAWSBlob())); w.Code != http.StatusNoContent {
		t.Fatalf("upload = %d %s, want 204", w.Code, w.Body.String())
	}
	if got := slices.Collect(maps.Keys(sec.owned[subOwner])); len(got) != 1 || got[0] != cliSecretName(cliAWSID, providerSSOPart) {
		t.Fatalf("the signer's namespace holds %v, want only the row's -sso name", got)
	}
	if rows := cliCaptureRows(rec); len(rows) != 1 || rows[0].Outcome != "success" {
		t.Fatalf("cli.signin.capture rows = %+v, want one success", rows)
	}

	repinned := cliAWSRowDef()
	repinned.CLI.AWS.AccountID = "999999999999"
	for name, def := range map[string]*types.ComponentDefinition{"re-pinned": &repinned, "deleted": nil} {
		t.Run(name, func(t *testing.T) {
			srv, sec, rec, tok, runID := cliUpload(t, subOwner, def)
			w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+runID.String(), tok, cliUploadBody(cliAWSBlob()))
			if w.Code != http.StatusConflict || reasonOf(t, w) != reasonCLISignInCaptureChanged {
				t.Fatalf("upload = %d %s, want 409 %s", w.Code, w.Body.String(), reasonCLISignInCaptureChanged)
			}
			if len(sec.owned) != 0 {
				t.Fatalf("a refused capture stored %v", sec.owned)
			}
			if rows := cliCaptureRows(rec); len(rows) != 1 || rows[0].Outcome != "failure" {
				t.Fatalf("cli.signin.capture rows = %+v, want one failure", rows)
			}
		})
	}
}

// A CLI sign-in name is reserved at the generic secrets API and every sink,
// like a provider's sign-in.
func TestAWSCLI_SignInNameReserved(t *testing.T) {
	for _, name := range []string{cliSecretName(cliAWSID, providerSSOPart), cliSecretName(cliAWSID, providerEntraPart)} {
		if !reservedSecret(name) || !sinkReservedSecret(name) || !nameSinkReservedSecret(name) || !secretsAPIReserved(name) ||
			!broker.ReservedSecretName(name) {
			t.Errorf("%q: reserved=%t sink=%t value-lane=%t api=%t broker=%t, want all", name, reservedSecret(name),
				sinkReservedSecret(name), nameSinkReservedSecret(name), secretsAPIReserved(name), broker.ReservedSecretName(name))
		}
	}
	future := cliSecretPrefix + cliAWSID + "-future"
	if !reservedSecret(future) || !sinkReservedSecret(future) || !secretsAPIReserved(future) || !nameSinkReservedSecret(future) ||
		!broker.ReservedSecretName(future) {
		t.Error("a sink, the secrets API, the value lanes or the broker do not reserve the whole CLI prefix")
	}
	if reservedSecret("wardyn-client-notes") {
		t.Error("an ordinary name was reserved")
	}
	srv, _, _ := componentsServer(t)
	member := ssoSession(t, "sub-member", "m@corp.example", oidc.RoleUser)
	for _, part := range []string{providerSSOPart, providerEntraPart} {
		w := doSSO(t, srv, http.MethodPut, "/api/v1/me/secrets/"+cliSecretName(cliAWSID, part), member, `{"value":"planted"}`)
		if w.Code != http.StatusForbidden || reasonOf(t, w) != reasonSecretNameReserved {
			t.Fatalf("PUT of the CLI %s sign-in name through the secrets API = %d %s, want the reserved-name refusal", part, w.Code, w.Body.String())
		}
	}
}

// Person erasure takes their CLI sign-ins with every other credential.
func TestAWSCLI_PersonErasureRemovesSignIn(t *testing.T) {
	f := newReauthFixture(t, nil)
	putCLIBlobIn(t, f, subOwner, cliAWSBlob())
	if _, err := f.srv.erasureOrchestrator(&secretstore.EraseReport{}, nil).Orchestrate(context.Background(), subOwner,
		[]erasure.Scope{erasure.Credentials}); err != nil {
		t.Fatal(err)
	}
	if names, _ := f.secrets.For(subOwner).List(context.Background()); slices.Contains(names, cliSecretName(cliAWSID, providerSSOPart)) {
		t.Fatalf("after erasure the namespace still holds %v", names)
	}
}

// An org row's delete, or a change to its portal, region, account or role,
// purges every person's sign-in for it; a change to its default region keeps
// them.
func TestAWSCLI_RowWritesPurgeSignIns(t *testing.T) {
	srv, _, _ := componentsServer(t)
	sec := srv.cfg.Secrets.(*memSecrets)
	admin := ssoSession(t, "sub-admin", "adm@corp.example", oidc.RoleAdmin)
	name := cliSecretName(cliAWSID, providerSSOPart)
	seed := func() {
		for _, owner := range []string{"person-a", "person-b"} {
			if err := sec.For(owner).Put(context.Background(), name, []byte("v")); err != nil {
				t.Fatal(err)
			}
		}
	}
	held := func() int {
		holders, err := sec.Holders(context.Background(), []string{name})
		if err != nil {
			t.Fatal(err)
		}
		return len(holders[name])
	}
	put := func(def types.ComponentDefinition) {
		t.Helper()
		raw, _ := json.Marshal(def)
		if w := doSSO(t, srv, http.MethodPut, "/api/v1/components/"+cliAWSID, admin, saveComponentBody("AWS", string(raw))); w.Code >= 300 {
			t.Fatalf("PUT = %d %s", w.Code, w.Body.String())
		}
	}
	put(cliAWSRowDef())
	seed()
	region := cliAWSRowDef()
	region.CLI.AWS.Region = "us-east-1"
	put(region)
	if held() != 2 {
		t.Fatalf("a default-region change removed sign-ins: %d left", held())
	}
	role := region
	role.CLI = &types.ComponentCLI{Kind: types.CLIKindAWS, AWS: cliAWSRow()}
	role.CLI.AWS.RoleName = "Admin"
	put(role)
	if held() != 0 {
		t.Fatalf("a role change kept %d sign-ins", held())
	}
	seed()
	if w := doSSO(t, srv, http.MethodDelete, "/api/v1/components/"+cliAWSID, admin, ""); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s", w.Code, w.Body.String())
	}
	if held() != 0 {
		t.Fatalf("a deleted row kept %d sign-ins", held())
	}
}

// cliSignInStore is the sign-in fixture's store with the component store the
// CLI door reads the row from.
type cliSignInStore struct {
	*signInStore
	fakeComponentStore
}

// The CLI row's door launches the person's AWS sign-in seeded with the row's
// portal, region and pin, stamped for the row; a row the person may not
// attach answers as the run doors do, and a non-AWS row has nothing to sign
// in to.
func TestAWSCLI_SignInDoor(t *testing.T) {
	member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser)
	door := "/api/v1/components/" + cliAWSID + "/sign-in"
	fixture := func(cs *capStore, def types.ComponentDefinition) (*Server, *memAudit) {
		srv, st, audit, _ := signInFixture(t, cs, types.SiteConfig{})
		cst := &cliSignInStore{signInStore: st}
		if _, err := cst.CreateComponent(context.Background(), types.Component{ID: uuid.MustParse(cliAWSID), Name: "AWS", Definition: def}); err != nil {
			t.Fatal(err)
		}
		srv.cfg.Store = cst
		return srv, audit
	}

	srv, audit := fixture(nil, cliAWSRowDef())
	w := doSSO(t, srv, http.MethodPost, door, member, "")
	if w.Code != http.StatusOK {
		t.Fatalf("sign-in = %d %s, want 200", w.Code, w.Body.String())
	}
	signInRunID(t, w.Body.String())
	a := cliAWSRow()
	st := loginStamp(t, audit)
	want := loginRunStamp{SSOStartURL: a.StartURL, CredentialSource: string(types.CredentialSourcePerUser), Owner: st.Owner,
		SSOAccountID: a.AccountID, SSORoleName: a.RoleName, SSORegion: a.SSORegion, CLIComponentID: cliAWSID}
	if st != want || st.Owner == "" {
		t.Fatalf("stamp = %+v\nwant  %+v with the member as owner", st, want)
	}

	ungranted, _ := fixture(&capStore{enf: map[string]bool{capComponent: true}}, cliAWSRowDef())
	if w := doSSO(t, ungranted, http.MethodPost, door, member, ""); w.Code != http.StatusForbidden || reasonOf(t, w) != "capability_component" {
		t.Errorf("ungranted sign-in = %d %s, want the attach door's refusal", w.Code, w.Body.String())
	}
	notAWS, _ := fixture(nil, types.ComponentDefinition{Hosts: []string{"api.github.com"},
		CLI: &types.ComponentCLI{Kind: types.CLIKindGitHub, GitHub: &types.CLIGitHub{}}})
	if w := doSSO(t, notAWS, http.MethodPost, door, member, ""); w.Code != http.StatusUnprocessableEntity || reasonOf(t, w) != reasonCLISignInNotAWS {
		t.Errorf("non-AWS sign-in = %d %s, want 422 %s", w.Code, w.Body.String(), reasonCLISignInNotAWS)
	}
}

// A revive replays the stored proxy config through the model-credential strip.
// Only the AWS CLI lane's own grant survives it: the AWS SSO sentinel, on a
// grant whose snapshot names mechanism aws_cli and an AWS CLI row the run
// carries. Every forged or partial shape is stripped, as is a Bedrock sign-in
// grant for a provider the run no longer chose.
func TestAWSCLI_ReviveKeepsThePortalGrant(t *testing.T) {
	f, st := cliResolveFixture(t, cliSnapshot(subOwner), nil, true)
	withSnapshot := func(edit func(*awsSSOScopeSnapshot)) json.RawMessage {
		sn := cliSnapshot(subOwner)
		edit(&sn)
		return cliSnapshotScope(t, sn)
	}
	cases := []struct {
		name   string
		scope  json.RawMessage
		secret string
		kept   bool
	}{
		{"the lane's own grant", withSnapshot(func(*awsSSOScopeSnapshot) {}), types.AWSSSOAccessTokenSecret, true},
		{"another mechanism", withSnapshot(func(sn *awsSSOScopeSnapshot) { sn.Mechanism = string(types.ModelProviderBedrockSSO) }), types.AWSSSOAccessTokenSecret, false},
		{"aws_cli without a row", withSnapshot(func(sn *awsSSOScopeSnapshot) { sn.ComponentID = "" }), types.AWSSSOAccessTokenSecret, false},
		{"a row the run does not carry", withSnapshot(func(sn *awsSSOScopeSnapshot) { sn.ComponentID = uuid.NewString() }), types.AWSSSOAccessTokenSecret, false},
		{"the right shape on another secret", withSnapshot(func(*awsSSOScopeSnapshot) {}), bedrockAPIKeySecret, false},
		{"a Bedrock sign-in for an unchosen provider", reauthSnapshotScope(t, subOwner, reauthRegion, "111122223333", "WardynAgent"), types.AWSSSOAccessTokenSecret, false},
	}
	cfg := &proxy.Config{}
	ids := make([]uuid.UUID, len(cases))
	st.grants = nil
	for i, tc := range cases {
		ids[i] = uuid.New()
		st.grants = append(st.grants, types.CredentialGrant{ID: ids[i], RunID: f.runID, Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: tc.scope}})
		cfg.Injection = append(cfg.Injection, proxy.InjectionConfig{GrantID: ids[i],
			InjectionRule: egress.InjectionRule{Host: cliAWSPortal, Header: awsSSOInjectHeader, Format: "%s", SecretName: tc.secret}})
	}
	if rerr := f.srv.stripRevivedModelInjections(context.Background(), f.st.run, cfg); rerr != nil {
		t.Fatalf("strip refused: %+v", rerr)
	}
	for i, tc := range cases {
		kept := slices.ContainsFunc(cfg.Injection, func(in proxy.InjectionConfig) bool { return in.GrantID == ids[i] })
		if kept != tc.kept {
			t.Errorf("%s: kept = %t, want %t", tc.name, kept, tc.kept)
		}
	}
}

// A CLI row's hold is repaired on the sidecar's own poll when the eager resolve
// after the capture never landed: the stored session was captured by a login
// run created after the raise, so the row is a resolved one not yet written down.
func TestAWSCLI_ReconcileOnReadResolvesAStrandedHold(t *testing.T) {
	live := cliAWSRowDef()
	f, _ := cliResolveFixture(t, cliSnapshot(subOwner), &live, true)
	dead := cliAWSBlob()
	dead.ExpiresAt = time.Now().Add(-time.Hour).UTC()
	putCLIBlobIn(t, f, subOwner, dead)
	if w := f.resolve(t); w.Code != http.StatusLocked {
		t.Fatalf("resolve = %d %s, want 423", w.Code, w.Body.String())
	}
	ap := onlyReauthRow(t, f.srv)

	loginRun := types.AgentRun{ID: uuid.New(), CreatedBy: subOwner, CreatedAt: time.Now().Add(time.Second)}
	f.st.loginRun = loginRun
	fresh := cliAWSBlob()
	fresh.SourceRunID = loginRun.ID.String()
	putCLIBlobIn(t, f, subOwner, fresh)

	w := do(t, f.srv, http.MethodGet, "/api/v1/internal/approvals/"+ap.ID.String(), f.token, "")
	var got types.ApprovalRequest
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK || got.State != types.ApprovalApproved {
		t.Fatalf("poll = %d %s (%v), want the CLI hold APPROVED from the stored capture", w.Code, w.Body.String(), err)
	}
}
