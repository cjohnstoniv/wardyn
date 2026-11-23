// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// withSiteFeatureHooks swaps the registered hooks for hooks (nil reads none) and marks them unread, so a test can
// register; both are restored when the test ends. A caller never runs in parallel.
func withSiteFeatureHooks(t *testing.T, hooks map[siteFeatureKey]siteFeatureHook) {
	t.Helper()
	saved, savedRead := siteFeatureHooks, siteFeatureHooksRead.Load()
	if hooks == nil {
		hooks = map[siteFeatureKey]siteFeatureHook{}
	}
	siteFeatureHooks = hooks
	siteFeatureHooksRead.Store(false)
	t.Cleanup(func() {
		siteFeatureHooks = saved
		siteFeatureHooksRead.Store(savedRead)
	})
}

// availableSiteFeatures is a hook table that turns on every key in keys and checks nothing.
func availableSiteFeatures(keys ...siteFeatureKey) map[siteFeatureKey]siteFeatureHook {
	m := map[siteFeatureKey]siteFeatureHook{}
	for _, k := range keys {
		m[k] = siteFeatureHook{available: func() bool { return true }}
	}
	return m
}

func allSiteFeatureKeys() []siteFeatureKey {
	keys := make([]siteFeatureKey, 0, len(siteFeatureRows))
	for _, row := range siteFeatureRows {
		keys = append(keys, row.key)
	}
	return keys
}

// newSetupFeatureFlags are the setup status keys the features block adds.
var newSetupFeatureFlags = []string{
	"org_sites", "windows_vms", "run_move", "machine_snapshots", "local_window", "preview_urls",
	"component_request_rules", "org_runner_drives", "preview_max_visibility",
}

// siteFeaturesDoc decodes a site config document the way a PUT body or a stored row would.
func siteFeaturesDoc(t *testing.T, doc string) types.SiteConfig {
	t.Helper()
	var sc types.SiteConfig
	if err := json.Unmarshal([]byte(doc), &sc); err != nil {
		t.Fatalf("decode %s: %v", doc, err)
	}
	return sc
}

func putSiteFeatures(t *testing.T, fake *fakeSiteConfigStore, body string) *httptest.ResponseRecorder {
	t.Helper()
	srv, _ := newSiteConfigHarness(t, fake)
	return do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, body)
}

// wantSiteFeatureRefusal fails unless w is a 400 with reason, and nothing reached the store.
func wantSiteFeatureRefusal(t *testing.T, w *httptest.ResponseRecorder, fake *fakeSiteConfigStore, reason string) {
	t.Helper()
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"reason":"`+reason+`"`) {
		t.Errorf("code = %d body = %s, want 400 %s", w.Code, w.Body.String(), reason)
	}
	if fake.putSeen != nil {
		t.Errorf("a refused write reached the store: %+v", fake.putSeen.FeatureSwitches)
	}
}

// setupFeatureFlags is GET /setup/status's features object for a deployment whose site config is sc.
func setupFeatureFlags(t *testing.T, sc types.SiteConfig) map[string]json.RawMessage {
	t.Helper()
	w := do(t, modelProvidersStatusSrv(t, sc, &capStore{}), http.MethodGet, "/api/v1/setup/status", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /setup/status = %d; body=%s", w.Code, w.Body.String())
	}
	var st struct {
		Features map[string]json.RawMessage `json:"features"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	return st.Features
}

// wantNoNewSetupFlags fails if flags carries any key the features block adds.
func wantNoNewSetupFlags(t *testing.T, flags map[string]json.RawMessage) {
	t.Helper()
	for _, k := range newSetupFeatureFlags {
		if v, ok := flags[k]; ok {
			t.Errorf("GET /setup/status features.%s = %s, want it absent", k, v)
		}
	}
}

func TestSiteFeatureRowsMatchTheBlock(t *testing.T) {
	block := reflect.TypeFor[types.SiteFeatureSwitches]()
	if len(siteFeatureRows) != block.NumField() {
		t.Fatalf("siteFeatureRows has %d rows, the features block %d keys", len(siteFeatureRows), block.NumField())
	}
	for i, row := range siteFeatureRows {
		f := block.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if row.field != f.Name || string(row.key) != name {
			t.Errorf("row %d = {%s %s}, want {%s %s}, the block's field in its order", i, row.key, row.field, name, f.Name)
		}
		if row.what == "" {
			t.Errorf("row %s names no feature", row.key)
		}
	}
}

// The default-off pin: nothing in the features block reads on unless it is stored on and this release can act on it.
func TestSiteFeaturesDefaultOff(t *testing.T) {
	t.Run("the tree's real registrations: an empty site config reads every flag off", func(t *testing.T) {
		wantNoNewSetupFlags(t, setupFeatureFlags(t, types.SiteConfig{}))
		srv, _ := newSiteConfigHarness(t, &fakeSiteConfigStore{})
		w := do(t, srv, http.MethodGet, "/api/v1/site-config", adminToken, "")
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"features"`) {
			t.Errorf("GET /site-config = %d %s, want 200 with no features key", w.Code, w.Body.String())
		}
	})
	withSiteFeatureHooks(t, availableSiteFeatures(allSiteFeatureKeys()...))
	empty, off := map[string]any{}, map[string]any{}
	for _, k := range allSiteFeatureKeys() {
		empty[string(k)] = map[string]any{}
		off[string(k)] = map[string]any{"enabled": false}
	}
	for name, block := range map[string]map[string]any{"every key {}": empty, `every key {"enabled": false}`: off} {
		raw, _ := json.Marshal(map[string]any{"features": block})
		sc := siteFeaturesDoc(t, string(raw))
		for _, k := range allSiteFeatureKeys() {
			if siteFeatureOn(types.SiteConfig{}, k) || siteFeatureOn(sc, k) {
				t.Errorf("%s: %s reads on with every key available", name, k)
			}
		}
	}
}

func TestSiteFeaturesUnknownKeyRefused(t *testing.T) {
	for _, body := range []string{
		`{"features":{"teleport":{"enabled":true}}}`,
		`{"features":{"preview_urls":{"enabled":true,"bogus":1}}}`,
		`{"features":{"org_sites":{"enabled":"yes"}}}`,
	} {
		fake := &fakeSiteConfigStore{}
		wantSiteFeatureRefusal(t, putSiteFeatures(t, fake, body), fake, reasonSiteConfigRequestInvalid)
	}
}

func TestSiteFeaturesUnavailableRefused(t *testing.T) {
	withSiteFeatureHooks(t, nil)
	for _, k := range allSiteFeatureKeys() {
		fake := &fakeSiteConfigStore{}
		w := putSiteFeatures(t, fake, `{"features":{"`+string(k)+`":{"enabled":true}}}`)
		wantSiteFeatureRefusal(t, w, fake, reasonSiteFeatureUnavailable)
		if !strings.Contains(w.Body.String(), "features."+string(k)+":") {
			t.Errorf("%s: refusal %s does not name features.%s", k, w.Body.String(), k)
		}
	}
	for _, body := range []string{
		`{"features":{"run_move":{"max_carried_gib":5}}}`,
		`{"features":{"run_move":{"to_personal_runners":true}}}`,
		`{"features":{"machine_snapshots":{"idle_days":3}}}`,
		`{"features":{"preview_urls":{"max_visibility":"org"}}}`,
	} {
		fake := &fakeSiteConfigStore{}
		wantSiteFeatureRefusal(t, putSiteFeatures(t, fake, body), fake, reasonSiteFeatureUnavailable)
	}
	accepted := []string{`{"features":{}}`, `{"features":null}`}
	for _, k := range allSiteFeatureKeys() {
		accepted = append(accepted, `{"features":{"`+string(k)+`":{}}}`, `{"features":{"`+string(k)+`":{"enabled":false}}}`)
	}
	for _, body := range accepted {
		if w := putSiteFeatures(t, &fakeSiteConfigStore{}, body); w.Code != http.StatusOK {
			t.Errorf("PUT %s = %d %s, want 200", body, w.Code, w.Body.String())
		}
	}
}

// The move refusal: run_move registered the way moving a run registers it while it cannot be reached.
func TestRunMoveRefusedUntilReachable(t *testing.T) {
	if siteFeatureAvailable(featureRunMove) {
		t.Fatal("run_move is available on this tree: only the commit that makes moving a run reachable may change this assertion")
	}
	withSiteFeatureHooks(t, map[siteFeatureKey]siteFeatureHook{featureRunMove: {available: func() bool { return false }}})
	fake := &fakeSiteConfigStore{}
	wantSiteFeatureRefusal(t, putSiteFeatures(t, fake, `{"features":{"run_move":{"enabled":true}}}`), fake, reasonSiteFeatureUnavailable)
	if siteFeatureOn(siteFeaturesDoc(t, `{"features":{"run_move":{"enabled":true}}}`), featureRunMove) {
		t.Error("a stored run_move true reads on while moving a run cannot be reached")
	}
}

// directStoreFeatures is every key stored on, with extras, as a database edit or a downgrade leaves them.
const directStoreFeatures = `{"features":{"org_sites":{"enabled":true},"windows_vm":{"enabled":true},` +
	`"run_move":{"enabled":true,"to_personal_runners":true,"max_carried_gib":9},` +
	`"machine_snapshots":{"enabled":true,"warm_pool":[{"image":"img","count":2}],"sleep_after_disconnect_sec":60,"max_per_person":3,"idle_days":7},` +
	`"local_window":{"enabled":true},"preview_urls":{"enabled":true,"max_visibility":"org"},` +
	`"component_request_rules":{"enabled":true},"org_runner_drives":{"enabled":true}}}`

// The direct-store bypass: a switch stored where this release cannot act on it never reads on.
func TestSiteFeaturesDirectStoreNeverReads(t *testing.T) {
	for name, hooks := range map[string]map[siteFeatureKey]siteFeatureHook{
		"no hooks":               nil,
		"run_move not reachable": {featureRunMove: {available: func() bool { return false }}},
	} {
		t.Run(name, func(t *testing.T) {
			withSiteFeatureHooks(t, hooks)
			stored := siteFeaturesDoc(t, directStoreFeatures)
			for _, k := range allSiteFeatureKeys() {
				if siteFeatureOn(stored, k) {
					t.Errorf("%s reads on from a direct store", k)
				}
			}
			wantNoNewSetupFlags(t, setupFeatureFlags(t, stored))
			wantSettingsWithoutSwitches(t, stored)
			wantDirectStorePuts(t, stored)
		})
	}
}

// wantSettingsWithoutSwitches fails unless siteFeatureSettings is the stored block with every Enabled taken out.
func wantSettingsWithoutSwitches(t *testing.T, stored types.SiteConfig) {
	t.Helper()
	got := siteFeatureSettings(stored)
	for _, row := range siteFeatureRows {
		if e := featureEnabled(row.in(&got)); e != nil {
			t.Errorf("siteFeatureSettings %s.enabled = %v, want nil", row.key, *e)
		}
		if e := featureEnabled(row.in(stored.FeatureSwitches)); e == nil || !*e {
			t.Errorf("siteFeatureSettings changed the stored %s switch", row.key)
		}
	}
	if m := got.RunMove; m == nil || m.MaxCarriedGiB != 9 || m.ToPersonalRunners == nil || !*m.ToPersonalRunners {
		t.Errorf("run_move settings = %+v, want the stored extras", m)
	}
	if s := got.MachineSnapshots; s == nil || len(s.WarmPool) != 1 || s.IdleDays != 7 || s.SleepAfterDisconnectSec != 60 {
		t.Errorf("machine_snapshots settings = %+v, want the stored extras", s)
	}
	if p := got.PreviewURLs; p == nil || p.MaxVisibility != "org" {
		t.Errorf("preview_urls settings = %+v, want max_visibility org", p)
	}
}

// wantDirectStorePuts: sending back what is stored is accepted and changes nothing, a changed setting is refused,
// and turning the key off is accepted.
func wantDirectStorePuts(t *testing.T, stored types.SiteConfig) {
	t.Helper()
	fake := &fakeSiteConfigStore{cfg: stored}
	srv, _ := newSiteConfigHarness(t, fake)
	get := do(t, srv, http.MethodGet, "/api/v1/site-config", adminToken, "")
	if w := putSiteFeatures(t, fake, get.Body.String()); w.Code != http.StatusOK {
		t.Fatalf("PUT of GET's body = %d %s, want 200", w.Code, w.Body.String())
	}
	if !reflect.DeepEqual(fake.putSeen.FeatureSwitches, stored.FeatureSwitches) {
		t.Errorf("PUT of GET's body stored %+v, want the block unchanged", fake.putSeen.FeatureSwitches)
	}
	fake = &fakeSiteConfigStore{cfg: stored}
	wantSiteFeatureRefusal(t, putSiteFeatures(t, fake, `{"features":{"run_move":{"max_carried_gib":5}}}`), fake, reasonSiteFeatureUnavailable)
	fake = &fakeSiteConfigStore{cfg: stored}
	if w := putSiteFeatures(t, fake, `{"features":{"run_move":{"enabled":false}}}`); w.Code != http.StatusOK {
		t.Fatalf("turning run_move off = %d %s, want 200", w.Code, w.Body.String())
	}
	off := false
	if got := fake.putSeen.FeatureSwitches.RunMove; !reflect.DeepEqual(got, &types.RunMoveFeature{Enabled: &off}) {
		t.Errorf("stored run_move = %+v, want {enabled: false}", got)
	}
}

// TestSiteFeatureSwitchesReadOnlyThroughAccessor is the guard that keeps every read of a switch behind
// siteFeatureOn: across the repo's non-test Go files, only the field's declaration and this package's site_features.go
// name FeatureSwitches, only site_features.go names the hook table, and a hook registers only from func init().
func TestSiteFeatureSwitchesReadOnlyThroughAccessor(t *testing.T) {
	var findings []string
	walkRepoGoFiles(t, func(rel string, f *ast.File) {
		findings = append(findings, siteFeatureGuardFindings(rel, f)...)
	})
	for _, f := range findings {
		t.Error(f)
	}
}

func TestSiteFeatureSwitchesReadOnlyThroughAccessor_PlantedViolation(t *testing.T) {
	for src, want := range map[string]int{
		`func f() { _ = sc.FeatureSwitches.RunMove }`:                   1,
		`var _ = types.SiteConfig{FeatureSwitches: x}`:                  1,
		`func f() { siteFeatureHooks[k] = h }`:                          1,
		`func f() { siteFeatureHooksRead.Store(false) }`:                1,
		`func f() { registerSiteFeature(k, h) }`:                        1,
		`func init() { registerSiteFeature(k, h) }`:                     0,
		`func (x T) init() { registerSiteFeature(k, h) }`:               1,
		`func f() { _ = siteFeatureOn(sc, featureRunMove) }`:            0,
		`var _ = reflect.TypeFor[types.SiteFeatureSwitches]()`:          0,
		`func f() { _ = strings.Fields("Images FeatureSwitches") }`:     0,
		`func init() { go func() { registerSiteFeature(k, h) }() }`:     0,
		`var g = func() { registerSiteFeature(k, h) }`:                  1,
		`func f() { var s types.SiteConfig; _ = s.FeatureSwitches }`:    1,
		`type x struct{ FeatureSwitches *types.SiteFeatureSwitches }`:   1,
		`func f() { s := siteFeatureHooksRead; _ = s }`:                 1,
		`func f() { _ = siteFeatureHookFor; _ = siteFeatureAvailable }`: 0,
	} {
		f, err := parser.ParseFile(token.NewFileSet(), "planted.go", "package api\n"+src+"\n", parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		if got := siteFeatureGuardFindings("internal/api/planted.go", f); len(got) != want {
			t.Errorf("%s: %d findings %v, want %d", src, len(got), got, want)
		}
	}
}

// walkRepoGoFiles parses every non-test .go file in the repo, skipping .git and node_modules at any depth and ui
// and docs only at the root, and hands each to visit with its slash path from the root.
func walkRepoGoFiles(t *testing.T, visit func(rel string, f *ast.File)) {
	t.Helper()
	root := filepath.Join("..", "..")
	n := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "node_modules" || rel == "ui" || rel == "docs" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		n++
		visit(rel, f)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 100 {
		t.Fatalf("read %d non-test Go files from %s, want the whole repo", n, root)
	}
}

// siteFeatureGuardFindings is the accessor guard's check of one parsed non-test file at rel.
func siteFeatureGuardFindings(rel string, f *ast.File) []string {
	own := rel == "internal/api/site_features.go"
	field := own || rel == "internal/types/site_config.go"
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		switch {
		case !ok:
		case id.Name == "FeatureSwitches" && !field:
			out = append(out, rel+": names FeatureSwitches; read a switch only through siteFeatureOn and siteFeatureSettings")
		case (id.Name == "siteFeatureHooks" || id.Name == "siteFeatureHooksRead") && !own:
			out = append(out, rel+": names "+id.Name+"; register a hook only through registerSiteFeature")
		}
		return true
	})
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "init" {
			continue
		}
		ast.Inspect(d, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "registerSiteFeature" {
					out = append(out, rel+": calls registerSiteFeature outside func init()")
				}
			}
			return true
		})
	}
	return out
}

func TestSiteConfigSeedRefusesFeatures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seed.json")
	if err := os.WriteFile(path, []byte(`{"features":{"run_move":{"enabled":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSiteConfigSeed(path); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("LoadSiteConfigSeed = %v, want an unknown field error: the seed must never store a switch", err)
	}
}

func TestSiteFeaturesCarryForward(t *testing.T) {
	withSiteFeatureHooks(t, availableSiteFeatures(featurePreviewURLs, featureOrgSites))
	stored := siteFeaturesDoc(t, `{"features":{"preview_urls":{"enabled":true,"max_visibility":"org"},"org_sites":{"enabled":false}}}`)
	for _, body := range []string{`{}`, `{"features":{}}`, `{"features":null}`} {
		fake := &fakeSiteConfigStore{cfg: stored}
		if w := putSiteFeatures(t, fake, body); w.Code != http.StatusOK {
			t.Fatalf("PUT %s = %d %s", body, w.Code, w.Body.String())
		}
		if !reflect.DeepEqual(fake.putSeen.FeatureSwitches, stored.FeatureSwitches) {
			t.Errorf("PUT %s stored %+v, want both keys kept", body, fake.putSeen.FeatureSwitches)
		}
	}
	for body, want := range map[string]string{
		`{"features":{"preview_urls":{"max_visibility":"public"}}}`: `{"preview_urls":{"enabled":true,"max_visibility":"public"},"org_sites":{"enabled":false}}`,
		`{"features":{"preview_urls":{"enabled":false}}}`:           `{"preview_urls":{"enabled":false},"org_sites":{"enabled":false}}`,
	} {
		fake := &fakeSiteConfigStore{cfg: stored}
		if w := putSiteFeatures(t, fake, body); w.Code != http.StatusOK {
			t.Fatalf("PUT %s = %d %s", body, w.Code, w.Body.String())
		}
		if got := siteFeaturesDoc(t, `{"features":`+want+`}`).FeatureSwitches; !reflect.DeepEqual(fake.putSeen.FeatureSwitches, got) {
			t.Errorf("PUT %s stored %+v, want %s", body, fake.putSeen.FeatureSwitches, want)
		}
	}
	wantEmptyKeysStoreNothing(t)
}

// wantEmptyKeysStoreNothing: every key sent as {} on an empty store stores no features key, so GET reads as before.
func wantEmptyKeysStoreNothing(t *testing.T) {
	t.Helper()
	empty := map[string]any{}
	for _, k := range allSiteFeatureKeys() {
		empty[string(k)] = map[string]any{}
	}
	raw, _ := json.Marshal(map[string]any{"features": empty})
	fake := &fakeSiteConfigStore{}
	srv, _ := newSiteConfigHarness(t, fake)
	before := do(t, srv, http.MethodGet, "/api/v1/site-config", adminToken, "").Body.String()
	if w := putSiteFeatures(t, fake, string(raw)); w.Code != http.StatusOK {
		t.Fatalf("PUT %s = %d %s", raw, w.Code, w.Body.String())
	}
	if fake.putSeen.FeatureSwitches != nil {
		t.Errorf("every key {} stored %+v, want no features block", fake.putSeen.FeatureSwitches)
	}
	if after := do(t, srv, http.MethodGet, "/api/v1/site-config", adminToken, "").Body.String(); after != before {
		t.Errorf("GET after every key {} = %s, want it byte-identical to %s", after, before)
	}
}

// siteFeatureHookCalls records what a test hook was called with.
type siteFeatureHookCalls struct {
	validates  int
	afterWrite [][2]types.SiteConfig
	refuse     error
}

func (c *siteFeatureHookCalls) hook(t *testing.T) siteFeatureHook {
	return siteFeatureHook{
		available: func() bool { return true },
		validate: func(_ context.Context, s *Server, next, _ *types.SiteFeatureSwitches) error {
			c.validates++
			if s == nil || next == nil || next.OrgSites == nil {
				t.Errorf("validate got server %v and next %+v, want both set", s != nil, next)
			}
			return c.refuse
		},
		afterWrite: func(_ context.Context, _ *Server, before, after types.SiteConfig) {
			c.afterWrite = append(c.afterWrite, [2]types.SiteConfig{before, after})
		},
	}
}

func TestSiteFeatureHooks(t *testing.T) {
	calls := &siteFeatureHookCalls{}
	withSiteFeatureHooks(t, map[siteFeatureKey]siteFeatureHook{featureOrgSites: calls.hook(t)})
	fake := &fakeSiteConfigStore{}
	on := `{"features":{"org_sites":{"enabled":true}}}`
	if w := putSiteFeatures(t, fake, on); w.Code != http.StatusOK {
		t.Fatalf("PUT %s = %d %s", on, w.Code, w.Body.String())
	}
	if calls.validates != 1 || len(calls.afterWrite) != 1 {
		t.Fatalf("validate %d, afterWrite %d calls, want 1 each", calls.validates, len(calls.afterWrite))
	}
	if b, a := calls.afterWrite[0][0], calls.afterWrite[0][1]; b.FeatureSwitches != nil || !siteFeatureOn(a, featureOrgSites) {
		t.Errorf("afterWrite got before %+v after %+v, want the documents before and after the write", b.FeatureSwitches, a.FeatureSwitches)
	}
	for _, body := range []string{on, `{}`, `{"features":{"windows_vm":{"enabled":false}}}`} {
		if w := putSiteFeatures(t, fake, body); w.Code != http.StatusOK {
			t.Fatalf("PUT %s = %d %s", body, w.Code, w.Body.String())
		}
	}
	if calls.validates != 1 || len(calls.afterWrite) != 1 {
		t.Errorf("validate %d, afterWrite %d calls after writes that leave org_sites as it is, want 1 each", calls.validates, len(calls.afterWrite))
	}
	for refuse, reason := range map[error]string{
		errors.New("org_sites: no"): reasonSiteConfigInvalid,
		&siteFeatureRefusal{reasonSiteFeatureUnavailable, "org_sites: not here"}: reasonSiteFeatureUnavailable,
	} {
		calls.refuse = refuse
		fake := &fakeSiteConfigStore{}
		wantSiteFeatureRefusal(t, putSiteFeatures(t, fake, on), fake, reason)
	}
	if len(calls.afterWrite) != 1 {
		t.Errorf("afterWrite ran %d times, want never on a refused write", len(calls.afterWrite))
	}
}

// siteFeaturesWriteDatum is the datum of the one site_config.write in events.
func siteFeaturesWriteDatum(t *testing.T, events []types.AuditEvent) map[string]json.RawMessage {
	t.Helper()
	for _, ev := range events {
		if ev.Action != "site_config.write" {
			continue
		}
		var d map[string]json.RawMessage
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	t.Fatal("no site_config.write event")
	return nil
}

func TestSiteFeaturesAudited(t *testing.T) {
	withSiteFeatureHooks(t, availableSiteFeatures(featureOrgSites))
	srv, audit := newSiteConfigHarness(t, &fakeSiteConfigStore{})
	if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{"features":{"org_sites":{"enabled":true}}}`); w.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", w.Code, w.Body.String())
	}
	d := siteFeaturesWriteDatum(t, audit.snapshot())
	for _, k := range []string{"features_enabled", "features_changed"} {
		var keys []string
		if err := json.Unmarshal(d[k], &keys); err != nil || !slices.Equal(keys, []string{"org_sites"}) {
			t.Errorf("site_config.write %s = %s (%v), want [org_sites]", k, d[k], err)
		}
	}
	srv, audit = newSiteConfigHarness(t, &fakeSiteConfigStore{})
	if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{}`); w.Code != http.StatusOK {
		t.Fatalf("PUT {} = %d %s", w.Code, w.Body.String())
	}
	d = siteFeaturesWriteDatum(t, audit.snapshot())
	for _, k := range []string{"features_enabled", "features_changed"} {
		if v, ok := d[k]; ok {
			t.Errorf("site_config.write with no block stored or named carries %s = %s", k, v)
		}
	}
}

// A key this release can turn on has its Before-you-upgrade line, naming its switch, in the CHANGELOG.
func TestEveryAvailableSiteFeatureHasABeforeYouUpgradeLine(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "CHANGELOG.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range allSiteFeatureKeys() {
		if siteFeatureAvailable(k) && !strings.Contains(string(raw), "`features."+string(k)+".enabled`") {
			t.Errorf("%s can be turned on, but CHANGELOG.md has no Before-you-upgrade line naming `features.%s.enabled`", k, k)
		}
	}
}

// panicText runs f and returns what it panicked with, or "" when it returned.
func panicText(f func()) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			msg, _ = r.(string)
			if msg == "" {
				msg = "a panic with no text"
			}
		}
	}()
	f()
	return ""
}

func TestRegisterSiteFeatureRefuses(t *testing.T) {
	withSiteFeatureHooks(t, nil)
	h := siteFeatureHook{available: func() bool { return true }}
	if msg := panicText(func() { registerSiteFeature("teleport", h) }); !strings.Contains(msg, "teleport") {
		t.Errorf("an unknown key: panic %q, want it to name teleport", msg)
	}
	if msg := panicText(func() { registerSiteFeature(featureOrgSites, h) }); msg != "" {
		t.Fatalf("a first registration panicked: %q", msg)
	}
	if msg := panicText(func() { registerSiteFeature(featureOrgSites, h) }); !strings.Contains(msg, "registered twice") {
		t.Errorf("a second registration: panic %q, want registered twice", msg)
	}
	siteFeatureAvailable(featureOrgSites)
	if msg := panicText(func() { registerSiteFeature(featureWindowsVM, h) }); !strings.Contains(msg, "after the hooks were read") {
		t.Errorf("a registration after a read: panic %q, want after the hooks were read", msg)
	}
}

// The documented re-upgrade behaviour (OPERATIONS.md, the site config features items): a switch stored while its
// key was unavailable comes on, with no write, once a release can turn the feature on.
func TestSiteFeatureStoredWhileUnavailableComesOnWhenAvailable(t *testing.T) {
	const doc = "docs/OPERATIONS.md: a switch stored while its key was unavailable takes effect with no new write"
	fake := &fakeSiteConfigStore{cfg: siteFeaturesDoc(t, `{"features":{"run_move":{"enabled":true},"org_sites":{"enabled":true}}}`)}
	withSiteFeatureHooks(t, nil)
	for _, k := range []siteFeatureKey{featureRunMove, featureOrgSites} {
		if siteFeatureOn(fake.cfg, k) {
			t.Errorf("%s reads on with no hook (%s)", k, doc)
		}
	}
	wantNoNewSetupFlags(t, setupFeatureFlags(t, fake.cfg))
	withSiteFeatureHooks(t, availableSiteFeatures(featureRunMove, featureOrgSites))
	flags := setupFeatureFlags(t, fake.cfg)
	for _, k := range []siteFeatureKey{featureRunMove, featureOrgSites} {
		if !siteFeatureOn(fake.cfg, k) || string(flags[string(k)]) != "true" {
			t.Errorf("%s: on %v, setup flag %s, want on with no write (%s)", k, siteFeatureOn(fake.cfg, k), flags[string(k)], doc)
		}
	}
	if fake.putSeen != nil {
		t.Error("the switch came on through a write")
	}
}

// siteFeatureSQLPattern is SQL that names the features block: a jsonb key, a path or a JSON path.
var siteFeatureSQLPattern = regexp.MustCompile(`'features'|\{features[,}]|\$\.features`)

const siteFeatureSQLWhy = "SQL that reads a features switch skips this release's check of whether it can turn the feature on, " +
	"so only the Windows VM kind migration may read one"

// siteFeatureSQLFileFindings checks the migrations: only one named *_windows_vm_kind.sql may name the block.
func siteFeatureSQLFileFindings(files map[string]string) []string {
	var out []string
	for name, text := range files {
		if siteFeatureSQLPattern.MatchString(text) && !strings.HasSuffix(name, "_windows_vm_kind.sql") {
			out = append(out, "migration "+name+" names the features block: "+siteFeatureSQLWhy)
		}
	}
	slices.Sort(out)
	return out
}

// siteFeatureSQLLiteralFindings checks one parsed non-test Go file's string literals.
func siteFeatureSQLLiteralFindings(rel string, f *ast.File) []string {
	if rel == "internal/api/site_features.go" {
		return nil
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && siteFeatureSQLPattern.MatchString(lit.Value) {
			out = append(out, rel+": the string "+lit.Value+" names the features block: "+siteFeatureSQLWhy)
		}
		return true
	})
	return out
}

// The one SQL reader of a switch is allow-listed.
func TestSiteFeatureSQLReadersAllowListed(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("..", "db", "migrations"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("read the migrations: %d entries, %v", len(entries), err)
	}
	files := map[string]string{}
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join("..", "db", "migrations", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[e.Name()] = string(raw)
	}
	findings := siteFeatureSQLFileFindings(files)
	walkRepoGoFiles(t, func(rel string, f *ast.File) {
		findings = append(findings, siteFeatureSQLLiteralFindings(rel, f)...)
	})
	for _, f := range findings {
		t.Error(f)
	}
	planted := map[string]string{
		"0400_x.sql":               `SELECT config->'features' FROM site_config;`,
		"0401_y.sql":               `SELECT config #>> '{features,run_move}' FROM site_config;`,
		"0402_windows_vm_kind.sql": `SELECT (config->'features'->'windows_vm'->'enabled') = 'true'::jsonb FROM site_config;`,
	}
	if got := siteFeatureSQLFileFindings(planted); len(got) != 2 {
		t.Errorf("planted migrations: %d findings %v, want 2 (the allowed migration is not one)", len(got), got)
	}
	src := "package p\nconst q = \"$.features.org_sites\"\n"
	pf, err := parser.ParseFile(token.NewFileSet(), "planted.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	if got := siteFeatureSQLLiteralFindings("internal/store/planted.go", pf); len(got) != 1 {
		t.Errorf("planted Go literal: %d findings %v, want 1", len(got), got)
	}
}

// A stored ceiling other than org reads private, the safest.
func TestSiteFeatureFlagsPreviewCeilingReadsSafest(t *testing.T) {
	withSiteFeatureHooks(t, availableSiteFeatures(featurePreviewURLs))
	for stored, want := range map[string]string{"": "private", "private": "private", "public": "private", "bogus": "private", "org": "org"} {
		raw, _ := json.Marshal(map[string]any{"features": map[string]any{"preview_urls": map[string]any{"enabled": true, "max_visibility": stored}}})
		sc := siteFeaturesDoc(t, string(raw))
		if got := string(setupFeatureFlags(t, sc)["preview_max_visibility"]); got != `"`+want+`"` {
			t.Errorf("stored max_visibility %q: preview_max_visibility %s, want %q", stored, got, want)
		}
	}
}
