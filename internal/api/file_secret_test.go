// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func fileSecretGrant(file, secretName string, ownerOnly bool) types.GrantSpec {
	return types.GrantSpec{
		Kind:      types.GrantFileSecret,
		Scope:     mustJSON(map[string]any{"file": file, "secret_name": secretName}),
		OwnerOnly: ownerOnly,
	}
}

// fileSecretStore is memSecrets with the two things these tests watch: the pg
// store's OwnRowOnly rule (an owner_only read never falls back to the
// operator's row — memSecrets alone always falls back), and every name a Get
// asked for.
type fileSecretStore struct {
	*memSecrets
	mu   *sync.Mutex
	gets *[]string
}

func newFileSecretStore() fileSecretStore {
	return fileSecretStore{memSecrets: &memSecrets{m: map[string][]byte{}, owned: map[string]map[string][]byte{}}, mu: &sync.Mutex{}, gets: &[]string{}}
}

func (s fileSecretStore) For(owner string) secretstore.Store {
	return fileSecretStore{memSecrets: s.memSecrets.For(owner).(*memSecrets), mu: s.mu, gets: s.gets}
}

func (s fileSecretStore) Get(ctx context.Context, name string) ([]byte, error) {
	s.mu.Lock()
	*s.gets = append(*s.gets, name)
	s.mu.Unlock()
	if s.owner != "" && secretstore.OwnRowOnly(ctx) {
		memSecretsMu.Lock()
		_, own := s.owned[s.owner][name]
		memSecretsMu.Unlock()
		if !own {
			return nil, secretstore.ErrNotFound
		}
	}
	return s.memSecrets.Get(ctx, name)
}

func (s fileSecretStore) asked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(*s.gets)
}

const (
	fileOwner      = "alice@example.com"
	fileOwnValue   = "alice-own-file-secret-0001"
	fileOperValue  = "operator-file-secret-0001\n"
	fileOperBare   = "operator-file-secret-0001"
	fileOtherValue = "operator-only-value-0001"
)

// fileSecretServer is a harness whose store holds one secret of alice's own,
// one operator secret of the same name, and one operator-only secret, with a
// mask registry and a runner that delivers managed files.
func fileSecretServer(t *testing.T) (*harness, fileSecretStore, *secretmask.Registry) {
	t.Helper()
	h := newHarness(t)
	sec := newFileSecretStore()
	ctx := context.Background()
	for owner, rows := range map[string]map[string]string{
		fileOwner: {"deploy-token": fileOwnValue},
		"":        {"deploy-token": "operator-same-name-0001", "corp-token": fileOperValue, "operator-only": fileOtherValue},
	} {
		for name, v := range rows {
			if err := sec.For(owner).Put(ctx, name, []byte(v)); err != nil {
				t.Fatal(err)
			}
		}
	}
	reg := secretmask.NewRegistry()
	h.srv.cfg.Secrets, h.srv.cfg.MaskRegistry, h.srv.cfg.Runner = sec, reg, &fakeRunner{}
	return h, sec, reg
}

// fileResolveRows is every run.file_secret.resolve datum for run, in order.
func fileResolveRows(t *testing.T, events []types.AuditEvent, run uuid.UUID) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ev := range events {
		if ev.Action != "run.file_secret.resolve" || ev.RunID == nil || *ev.RunID != run {
			continue
		}
		var d map[string]any
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		d["outcome"] = ev.Outcome
		out = append(out, d)
	}
	return out
}

// noEventCarries fails when any audit event's data or target carries one of values.
func noEventCarries(t *testing.T, events []types.AuditEvent, values ...string) {
	t.Helper()
	for _, ev := range events {
		for _, v := range values {
			if strings.Contains(string(ev.Data), v) || strings.Contains(ev.Target, v) {
				t.Errorf("audit %s carries a secret value: %s", ev.Action, ev.Data)
			}
		}
	}
}

func TestStoredSecretGrantPairing_FileSecret(t *testing.T) {
	host, ref, kh, covered, err := storedSecretGrantPairing(fileSecretGrant("api-token", "corp-token", false))
	if host != "api-token" || ref != "corp-token" || kh != "" || !covered || err != nil {
		t.Fatalf("pairing = (%q, %q, %q, %v, %v), want the FILE name in the host slot", host, ref, kh, covered, err)
	}
	// The same pairing the shared comparator computes, so the write-time and
	// runtime halves agree.
	if ch, cr, _, _, ok := composer.GrantPairing(fileSecretGrant("api-token", "corp-token", false)); !ok || ch != host || cr != ref {
		t.Errorf("composer.GrantPairing = (%q, %q, %v), want (%q, %q)", ch, cr, ok, host, ref)
	}
	if _, _, _, covered, err := storedSecretGrantPairing(fileSecretGrant("../x", "corp-token", false)); !covered || err == nil {
		t.Errorf("a path for a file name: covered=%v err=%v, want covered with an error (fail closed)", covered, err)
	}
	ceiling := []types.GrantSpec{fileSecretGrant("api-token", "corp-token", false)}
	if !storedSecretPairingInCeiling(fileSecretGrant("api-token", "corp-token", false), ceiling) ||
		storedSecretPairingInCeiling(fileSecretGrant("elsewhere", "corp-token", false), ceiling) {
		t.Error("the ceiling match is not exact on (file, secret)")
	}
}

// What an author may write for a file_secret, refused identically by the
// strict door (stored, inline, default policy) and the lenient one (recorded).
// The file is a NAME: anything that could address a path, hide the file or
// carry an authored mode is refused.
func TestValidateEligibleGrant_FileSecretScope(t *testing.T) {
	gated := fileSecretGrant("api-token", "corp-token", false)
	gated.RequiresApproval = true
	withKey := func(k, v string) types.GrantSpec {
		return types.GrantSpec{Kind: types.GrantFileSecret, Scope: mustJSON(map[string]string{"file": "api-token", "secret_name": "corp-token", k: v})}
	}
	for _, c := range []struct {
		name  string
		grant types.GrantSpec
		want  string // "" accepts
	}{
		{"a name", fileSecretGrant("api-token", "corp-token", false), ""},
		{"dots, dashes and underscores inside", fileSecretGrant("corp_api-token.json", "corp-token", true), ""},
		{"63 characters", fileSecretGrant(strings.Repeat("a", 63), "corp-token", false), ""},
		{"64 characters", fileSecretGrant(strings.Repeat("a", 64), "corp-token", false), "a file name, not a path"},
		{"traversal", fileSecretGrant("../../etc/passwd", "corp-token", false), "a file name, not a path"},
		{"a separator", fileSecretGrant("sub/api-token", "corp-token", false), "a file name, not a path"},
		{"an absolute path", fileSecretGrant("/etc/shadow", "corp-token", false), "a file name, not a path"},
		{"dot", fileSecretGrant(".", "corp-token", false), "a file name, not a path"},
		{"dot-dot", fileSecretGrant("..", "corp-token", false), "a file name, not a path"},
		{"a hidden file", fileSecretGrant(".netrc", "corp-token", false), "a file name, not a path"},
		{"a leading dash", fileSecretGrant("-rf", "corp-token", false), "a file name, not a path"},
		{"upper case", fileSecretGrant("API_TOKEN", "corp-token", false), "a file name, not a path"},
		{"a NUL", fileSecretGrant("a\x00b", "corp-token", false), "a file name, not a path"},
		{"no file", fileSecretGrant("", "corp-token", false), "requires file and secret_name"},
		{"no secret", fileSecretGrant("api-token", "", false), "requires file and secret_name"},
		{"a bad secret name", fileSecretGrant("api-token", "Corp Token", false), "not a valid secret name"},
		{"an authored path", withKey("path", "/tmp/x"), "unknown field"},
		{"an authored mode", withKey("mode", "0644"), "unknown field"},
		{"the signing key", fileSecretGrant("api-token", "wardyn-signing-key", false), "reserved secret name"},
		{"a harness sign-in blob", fileSecretGrant("api-token", "wardyn-harness-adoown-x-oauth", false), "reserved secret name"},
		{"a model provider key", fileSecretGrant("api-token", "wardyn-provider-abc-key", false), "reserved secret name"},
		{"requires_approval", gated, "cannot require approval"},
	} {
		for _, strict := range []bool{true, false} {
			err := validateEligibleGrantMode(0, c.grant, strict)
			if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
				t.Errorf("%s (strict=%v): err = %v, want %q", c.name, strict, err, c.want)
			}
		}
		spec := types.RunPolicySpec{MinConfinementClass: types.CC2, EligibleGrants: []types.GrantSpec{c.grant}}
		if (validatePolicySpec(spec) == nil) != (c.want == "") || (validatePolicySpecLenient(spec) == nil) != (c.want == "") {
			t.Errorf("%s: validatePolicySpec/validatePolicySpecLenient disagree with the grant-level verdict", c.name)
		}
	}
}

// A policy-authored file_secret is admin-only exactly like env_secret, under
// the same one switch: dropped for a member, with a warning naming the kind,
// and env_secret's own sentence unchanged. The grant gate's defence-in-depth
// arm drops it too.
func TestFileSecretPosture_DroppedForAMemberUnderTheEnvSwitch(t *testing.T) {
	file := fileSecretGrant("api-token", "corp-token", false)
	env := envSecretGrant("CORP_API_TOKEN", "corp-token")
	apiKey := types.GrantSpec{Kind: types.GrantAPIKey, Scope: mustJSON(map[string]any{"host": "api.example.com", "secret_name": "corp-token"})}

	kept, warns, drops := dropAdminOnlyEnvSecretGrants([]types.GrantSpec{file, env, apiKey})
	if len(kept) != 1 || kept[0].Kind != types.GrantAPIKey || len(drops) != 2 {
		t.Fatalf("kept %v, %d drops; want only the api_key kept and both resident grants dropped", kept, len(drops))
	}
	wantWarns := []string{
		`dropped file_secret grant for "corp-token": file_secret is admin-only (an operator can open it with WARDYN_ALLOW_USER_ENV_SECRET)`,
		`dropped env_secret grant for "corp-token": env_secret is admin-only (an operator can open it with WARDYN_ALLOW_USER_ENV_SECRET)`,
	}
	if !slices.Equal(warns, wantWarns) {
		t.Errorf("warnings = %q, want %q", warns, wantWarns)
	}

	h := newHarness(t)
	h.srv.cfg.DefaultPolicy = types.RunPolicySpec{EligibleGrants: []types.GrantSpec{file}}
	if kept, warns, _, err := h.srv.filterUserGrants(context.Background(), "", nil, []types.GrantSpec{file}); err != nil || len(kept) != 0 || len(warns) != 1 {
		t.Fatalf("default posture, ceiling-listed pairing: kept=%d warns=%d err=%v, want dropped", len(kept), len(warns), err)
	}

	t.Setenv(envAllowMemberEnvSecret, "1")
	if kept, _, _ := dropAdminOnlyEnvSecretGrants([]types.GrantSpec{file, env}); len(kept) != 2 {
		t.Errorf("switch open: kept %d, want both", len(kept))
	}
	if kept, _, _, _ := h.srv.filterUserGrants(context.Background(), "", nil, []types.GrantSpec{file}); len(kept) != 1 {
		t.Errorf("switch open, ceiling-listed pairing: kept=%d, want 1", len(kept))
	}
	if kept, _, _, _ := h.srv.filterUserGrants(context.Background(), "", nil, []types.GrantSpec{fileSecretGrant("elsewhere", "corp-token", false)}); len(kept) != 0 {
		t.Errorf("switch open, a file name the operator never wrote: kept=%d, want 0", len(kept))
	}
}

// The sink's happy path: each value becomes an agent-owned 0400 file in the
// fixed directory, is on the run's mask registry (as stored, and without its
// trailing line break) before it is returned, and the audit rows name the
// file, the secret and whose row was read — never the value.
func TestResolveFileSecretGrants_Delivered(t *testing.T) {
	h, sec, reg := fileSecretServer(t)
	run := types.AgentRun{ID: uuid.New(), CreatedBy: fileOwner}
	policy := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{
		fileSecretGrant("api-token", "deploy-token", true),
		fileSecretGrant("corp", "corp-token", false), // not owner_only: env_secret's owner-then-operator read
	}}
	files, ok := h.srv.resolveFileSecretGrants(context.Background(), run, policy)
	if !ok {
		t.Fatal("resolve failed the run")
	}
	want := []runner.ManagedFile{
		{Path: runner.ComponentSecretDir + "/api-token", Mode: 0o400, AgentOwned: true, Content: []byte(fileOwnValue)},
		{Path: runner.ComponentSecretDir + "/corp", Mode: 0o400, AgentOwned: true, Content: []byte(fileOperValue)},
	}
	if len(files) != len(want) {
		t.Fatalf("files = %+v, want %+v", files, want)
	}
	for i := range want {
		if files[i].Path != want[i].Path || files[i].Mode != want[i].Mode || files[i].AgentOwned != want[i].AgentOwned || !bytes.Equal(files[i].Content, want[i].Content) {
			t.Errorf("file %d = %+v, want %+v", i, files[i], want[i])
		}
	}
	if err := runner.ValidateManagedFiles(files); err != nil {
		t.Errorf("the delivered files fail the driver contract: %v", err)
	}

	snap := reg.Snapshot(run.ID)
	for _, v := range []string{fileOwnValue, fileOperValue, fileOperBare} {
		if !slices.ContainsFunc(snap, func(b []byte) bool { return string(b) == v }) {
			t.Errorf("%q is not on the run's mask registry", v)
		}
	}
	masker := secretmask.NewMasker(snap)
	out := string(masker.Mask([]byte("cat: " + fileOwnValue + " | $(cat corp): " + fileOperBare + " done")))
	if strings.Contains(out, fileOwnValue) || strings.Contains(out, fileOperBare) {
		t.Errorf("verbatim output is not masked: %q", out)
	}
	// Stated residual, pinned so a change to it is a decision: masking is
	// verbatim, so a transformed rendering is NOT masked.
	enc := base64.StdEncoding.EncodeToString([]byte(fileOwnValue))
	if got := string(masker.Mask([]byte(enc))); got != enc {
		t.Errorf("a base64 rendering was masked (%q); update the documented residual if masking now covers encodings", got)
	}

	rows := fileResolveRows(t, h.audit.events, run.ID)
	wantRows := []map[string]any{
		{"file": "api-token", "secret_name": "deploy-token", "secret_scope": "own", "outcome": "success"},
		{"file": "corp", "secret_name": "corp-token", "secret_scope": "operator", "outcome": "success"},
	}
	if len(rows) != len(wantRows) {
		t.Fatalf("run.file_secret.resolve rows = %v, want %v", rows, wantRows)
	}
	for i, w := range wantRows {
		for k, v := range w {
			if rows[i][k] != v {
				t.Errorf("row %d %s = %v, want %v", i, k, rows[i][k], v)
			}
		}
	}
	noEventCarries(t, h.audit.events, fileOwnValue, fileOperBare)
	if got := sec.asked(); !slices.Equal(got, []string{"deploy-token", "corp-token"}) {
		t.Errorf("store reads = %v, want exactly the two grants' secrets", got)
	}
}

// owner_only is owner-only: a run whose owner has no row of that name gets no
// file, even though the operator holds one, and the operator's value is never
// put on the run's registry.
func TestResolveFileSecretGrants_OwnerOnlyMissing(t *testing.T) {
	h, _, reg := fileSecretServer(t)
	run := types.AgentRun{ID: uuid.New(), CreatedBy: fileOwner}
	files, ok := h.srv.resolveFileSecretGrants(context.Background(), run,
		types.RunPolicySpec{EligibleGrants: []types.GrantSpec{fileSecretGrant("other", "operator-only", true)}})
	if !ok || len(files) != 0 {
		t.Fatalf("files = %+v ok=%v, want none — the operator's row must not stand in for the owner's", files, ok)
	}
	rows := fileResolveRows(t, h.audit.events, run.ID)
	if len(rows) != 1 || rows[0]["outcome"] != "failure" || !strings.Contains(rows[0]["reason"].(string), "owner_only") || rows[0]["secret_scope"] != nil {
		t.Errorf("rows = %v, want one owner_only failure with no row read", rows)
	}
	if snap := reg.Snapshot(run.ID); len(snap) != 0 {
		t.Errorf("the run's registry holds %d values; nothing was delivered", len(snap))
	}
}

// refusingMaskBackend is a shared corpus that cannot commit a run's value.
type refusingMaskBackend struct{ *readBackend }

func (refusingMaskBackend) PutRun(uuid.UUID, []byte) error {
	return errors.New("the masking store refused the write")
}

// A value the run's masking cannot record is not delivered at all.
func TestResolveFileSecretGrants_MaskManifestRefused(t *testing.T) {
	h, _, _ := fileSecretServer(t)
	reg := secretmask.NewRegistry()
	reg.SetBackend(refusingMaskBackend{&readBackend{reg: reg}})
	h.srv.cfg.MaskRegistry = reg
	run := types.AgentRun{ID: uuid.New(), CreatedBy: fileOwner}
	files, ok := h.srv.resolveFileSecretGrants(context.Background(), run,
		types.RunPolicySpec{EligibleGrants: []types.GrantSpec{fileSecretGrant("api-token", "deploy-token", true)}})
	if !ok || len(files) != 0 {
		t.Fatalf("files = %+v ok=%v, want none — an unmasked value must never reach the spec", files, ok)
	}
	if rows := fileResolveRows(t, h.audit.events, run.ID); len(rows) != 1 || !strings.Contains(rows[0]["reason"].(string), "masking manifest") {
		t.Errorf("rows = %v, want the masking failure", rows)
	}
}

// A platform-internal name is refused before the store is read, whoever holds
// a row of it — and the Bedrock key, which is proxy-injected only.
func TestResolveFileSecretGrants_ReservedName(t *testing.T) {
	h, sec, _ := fileSecretServer(t)
	names := []string{"wardyn-signing-key", "wardyn-harness-adoown-x-oauth", "wardyn-provider-abc-key", bedrockAPIKeySecret}
	var grants []types.GrantSpec
	for i, n := range names {
		if err := sec.Put(context.Background(), n, []byte("platform-value-0001")); err != nil {
			t.Fatal(err)
		}
		grants = append(grants, fileSecretGrant("f"+string(rune('a'+i)), n, false))
	}
	run := types.AgentRun{ID: uuid.New(), CreatedBy: fileOwner}
	files, ok := h.srv.resolveFileSecretGrants(context.Background(), run, types.RunPolicySpec{EligibleGrants: grants})
	if !ok || len(files) != 0 {
		t.Fatalf("files = %+v, want none", files)
	}
	if got := sec.asked(); len(got) != 0 {
		t.Errorf("the store was read for %v; a reserved name must be refused before any read", got)
	}
	if rows := fileResolveRows(t, h.audit.events, run.ID); len(rows) != len(names) {
		t.Errorf("rows = %v, want one failure per grant", rows)
	}
	noEventCarries(t, h.audit.events, "platform-value-0001")
}

// Nothing a grant carries can put a file outside the fixed directory: a scope
// that is a path is refused before any read, and a second grant for a file
// already delivered is skipped rather than failing the whole sandbox.
func TestResolveFileSecretGrants_PathCannotLeaveTheDirectory(t *testing.T) {
	h, sec, _ := fileSecretServer(t)
	run := types.AgentRun{ID: uuid.New(), CreatedBy: fileOwner}
	grants := []types.GrantSpec{
		fileSecretGrant("../../etc/cron.d/x", "deploy-token", true),
		fileSecretGrant("sub/x", "deploy-token", true),
		fileSecretGrant("/abs", "deploy-token", true),
		fileSecretGrant(".hidden", "deploy-token", true),
		{Kind: types.GrantFileSecret, Scope: json.RawMessage(`{"file":"x","secret_name":"deploy-token","path":"/tmp/x"}`), OwnerOnly: true},
		fileSecretGrant("api-token", "deploy-token", true),
		fileSecretGrant("api-token", "corp-token", false),
	}
	files, ok := h.srv.resolveFileSecretGrants(context.Background(), run, types.RunPolicySpec{EligibleGrants: grants})
	if !ok || len(files) != 1 || files[0].Path != runner.ComponentSecretDir+"/api-token" || string(files[0].Content) != fileOwnValue {
		t.Fatalf("files = %+v, want only the first api-token grant's file", files)
	}
	for _, f := range files {
		if path.Dir(f.Path) != runner.ComponentSecretDir || path.Clean(f.Path) != f.Path {
			t.Errorf("a file landed at %s", f.Path)
		}
	}
	if got := sec.asked(); !slices.Equal(got, []string{"deploy-token"}) {
		t.Errorf("store reads = %v, want one (the refused scopes and the duplicate read nothing)", got)
	}
	rows := fileResolveRows(t, h.audit.events, run.ID)
	if len(rows) != len(grants) {
		t.Fatalf("rows = %v, want one per grant", rows)
	}
	if !strings.Contains(rows[len(rows)-1]["reason"].(string), "already delivers this file") {
		t.Errorf("duplicate row = %v", rows[len(rows)-1])
	}
}

// A run with no file_secret grant is today's run: the lane asks the runner
// nothing, reads nothing and writes no row.
func TestResolveFileSecretGrants_NoGrantIsInert(t *testing.T) {
	h, sec, _ := fileSecretServer(t)
	h.srv.cfg.Runner = &fakeRunner{capsErr: errors.New("the runner must not be asked")}
	run := types.AgentRun{ID: uuid.New(), CreatedBy: fileOwner}
	files, ok := h.srv.resolveFileSecretGrants(context.Background(), run,
		types.RunPolicySpec{EligibleGrants: []types.GrantSpec{envSecretGrant("CORP_TOKEN", "corp-token")}})
	if files != nil || !ok {
		t.Fatalf("resolve = %+v, %v; want (nil, true)", files, ok)
	}
	if len(sec.asked()) != 0 || len(fileResolveRows(t, h.audit.events, run.ID)) != 0 {
		t.Error("a run with no file_secret grant read the store or wrote a row")
	}
}

// fileDispatch dispatches run under policy through dispatchRun on a fake
// runner, with fileSecretServer's store and a fresh registry.
func fileDispatch(t *testing.T, rn *fakeRunner, policy types.RunPolicySpec) (*Server, *dispatchTestStore, *recRecorder, fileSecretStore, types.AgentRun) {
	t.Helper()
	srv, st, audit, run := dispatchTeardownFixture(t, rn, types.RunPending)
	run.Task, run.CreatedBy = "", fileOwner
	st.run = run
	_, sec, _ := fileSecretServer(t)
	srv.cfg.Secrets, srv.cfg.MaskRegistry = sec, secretmask.NewRegistry()
	dispatchOf(srv, run, policy)
	return srv, st, audit, sec, run
}

// End to end through dispatchRun: the value reaches the sandbox spec as one
// agent-owned file and nowhere else — not the environment, not the proxy
// config (what revive reloads), not an audit row, not a log line, not the run
// row's failure hint.
func TestDispatch_FileSecretReachesTheSpecAndNothingElse(t *testing.T) {
	logs := captureSlog(t)
	rn := &fakeRunner{}
	srv, st, audit, _, run := fileDispatch(t, rn, types.RunPolicySpec{EligibleGrants: []types.GrantSpec{fileSecretGrant("api-token", "deploy-token", true)}})
	if rn.createCalls != 1 {
		t.Fatalf("the sandbox was not created; events: %s", auditDump(audit.events, run.ID))
	}
	spec := rn.lastSpec
	var got []runner.ManagedFile
	for _, f := range spec.ManagedFiles {
		if f.AgentOwned {
			got = append(got, f)
		}
	}
	if len(got) != 1 || got[0].Path != runner.ComponentSecretDir+"/api-token" || string(got[0].Content) != fileOwnValue {
		t.Fatalf("agent-owned files on the spec = %+v", got)
	}
	if !slices.ContainsFunc(srv.cfg.MaskRegistry.Snapshot(run.ID), func(b []byte) bool { return string(b) == fileOwnValue }) {
		t.Error("the delivered value is not on the run's mask registry")
	}
	for k, v := range spec.Env {
		if strings.Contains(v, fileOwnValue) {
			t.Errorf("Env[%s] carries the value", k)
		}
	}
	for k, v := range spec.SecretEnv {
		if strings.Contains(v, fileOwnValue) {
			t.Errorf("SecretEnv[%s] carries the value", k)
		}
	}
	rendered, err := runner.BuildProxyConfig(run.ID, spec.ProxyConfig, runner.ProxyListenPort)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(rendered, []byte(fileOwnValue)) {
		t.Error("the rendered proxy config (kept for revive) carries the value")
	}
	noEventCarries(t, audit.events, fileOwnValue)
	if strings.Contains(logs.String(), fileOwnValue) {
		t.Error("a log line carries the value")
	}
	if strings.Contains(st.FailureHint(), fileOwnValue) {
		t.Error("the run row's failure hint carries the value")
	}
	if rows := fileResolveRows(t, audit.events, run.ID); len(rows) != 1 || rows[0]["outcome"] != "success" {
		t.Errorf("resolve rows = %v", rows)
	}
}

// A run with no file_secret grant dispatches exactly as before: no
// agent-owned file, no resolve row, no runner question the lane added.
func TestDispatch_NoFileSecretGrantIsTodaysSpec(t *testing.T) {
	rn := &fakeRunner{}
	_, _, audit, sec, run := fileDispatch(t, rn, types.RunPolicySpec{})
	if rn.createCalls != 1 {
		t.Fatalf("the sandbox was not created; events: %s", auditDump(audit.events, run.ID))
	}
	if rn.lastSpec.ManagedFiles != nil {
		t.Errorf("ManagedFiles = %+v, want nil for a run with no managed settings and no file_secret grant", rn.lastSpec.ManagedFiles)
	}
	if len(sec.asked()) != 0 || len(fileResolveRows(t, audit.events, run.ID)) != 0 {
		t.Error("the lane read the store or wrote a row for a run with no file_secret grant")
	}
}

// A runner that cannot deliver a file, or cannot say whether it can, fails the
// run before any value is read: every grant would otherwise go silently
// missing. The member-visible hint carries no driver text.
func TestResolveFileSecretGrants_RunnerUnsupported(t *testing.T) {
	for name, rn := range map[string]*fakeRunner{
		"no managed files":     {noManagedFiles: true},
		"capabilities unknown": {capsErr: errors.New("dial unix /var/run/docker.sock: connection refused")},
	} {
		t.Run(name, func(t *testing.T) {
			_, st, audit, sec, run := fileDispatch(t, rn, types.RunPolicySpec{EligibleGrants: []types.GrantSpec{fileSecretGrant("api-token", "deploy-token", true)}})
			if rn.createCalls != 0 {
				t.Error("a sandbox was created for a run whose file could not be delivered")
			}
			if st.State() != types.RunFailed {
				t.Errorf("state = %s, want FAILED", st.State())
			}
			if len(sec.asked()) != 0 {
				t.Errorf("the store was read (%v) for a run that was always going to fail", sec.asked())
			}
			ev := findAudit(audit.events, run.ID, "run.create", "failure")
			if ev == nil || !strings.Contains(string(ev.Data), `"reason":"managed_files_unsupported"`) {
				t.Fatalf("no run.create failure with reason managed_files_unsupported; events: %s", auditDump(audit.events, run.ID))
			}
			if hint := st.FailureHint(); hint == "" || strings.Contains(hint, "docker.sock") {
				t.Errorf("failure hint = %q, want a sentence and no driver text", hint)
			}
		})
	}
}

// The compose checklist names where each resident secret will live.
func TestDeriveSetupItems_ResidentSecretRows(t *testing.T) {
	srv := newSetupTestServer()
	spec := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{
		envSecretGrant("CORP_TOKEN", "env-token"),
		fileSecretGrant("api-token", "file-token", true),
		apiKeyGrant("api.example.com", "header-token"),
	}}
	items := setupSecretItems(spec, secretsWith("env-token"))
	for id, want := range map[string]SetupItem{
		"secret:env-token":    {RequiredBy: "an env_secret grant (CORP_TOKEN)", Residency: "resident_env", Status: "satisfied"},
		"secret:file-token":   {RequiredBy: "a file_secret grant (api-token)", Residency: "resident_file", Status: "missing"},
		"secret:header-token": {RequiredBy: "an api_key grant (api.example.com)", Residency: "proxy_injected", Status: "missing"},
	} {
		got, ok := findItem(items, id)
		if !ok || got.RequiredBy != want.RequiredBy || got.Residency != want.Residency || got.Status != want.Status {
			t.Errorf("%s = %+v, want %+v", id, got, want)
		}
	}
	_ = srv
}

// Revive replaces the PROXY from the config kept at dispatch; the agent's
// container, and the file delivered into it at create, stay as they are. So a
// revive neither reads the secret again nor delivers it again, and the kept
// config it reloads never carried the value: the grant rides it as a scope
// (file and secret NAME), frozen as launched.
func TestReviveRun_AFileSecretIsNeitherReadNorDeliveredAgain(t *testing.T) {
	f := newReviveFixture(t)
	grant := fileSecretGrant("api-token", "deploy-token", true)
	f.editConfig(t, func(c *proxy.Config) { c.Policy.EligibleGrants = append(c.Policy.EligibleGrants, grant) })
	sec := newFileSecretStore()
	if err := sec.For(f.run.CreatedBy).Put(context.Background(), "deploy-token", []byte(fileOwnValue)); err != nil {
		t.Fatal(err)
	}
	f.srv.cfg.Secrets = sec
	creates := f.rr.createCalls

	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("revive = %d, want 200", code)
	}
	revived := f.newConfig(t)
	if !slices.ContainsFunc(revived.Policy.EligibleGrants, func(g types.GrantSpec) bool { return g.Kind == types.GrantFileSecret }) {
		t.Error("the revived proxy lost the run's file_secret grant; the policy is frozen as launched")
	}
	if bytes.Contains(f.rr.replaced[0], []byte(fileOwnValue)) {
		t.Error("the revived proxy's config carries the secret's value")
	}
	if got := sec.asked(); len(got) != 0 {
		t.Errorf("revive read the store (%v); the file was delivered once, at create", got)
	}
	if f.rr.createCalls != creates {
		t.Error("revive created a sandbox; it replaces the proxy only")
	}
	if ev := f.audit.eventsFor(f.run.ID, "run.file_secret.resolve"); len(ev) != 0 {
		t.Errorf("revive resolved the file again: %+v", ev)
	}
}
