// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

const hostMountSource = "/srv/nfs/team"

// hostReadAs serves evs through GET /audit to a session of role and returns each row's data by action.
func hostReadAs(t *testing.T, evs []types.AuditEvent, role string) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, ev := range hostAuditRows(t, hostAuditServer(t, evs), "/api/v1/audit", ssoSession(t, "sub-"+role, role+"@corp.example", role)) {
		got[ev.Action] = string(ev.Data)
	}
	return got
}

func hostRedactedFields(t *testing.T, data string) []string {
	t.Helper()
	var d struct {
		Redacted []string `json:"redacted_fields"`
	}
	if err := json.Unmarshal([]byte(data), &d); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return d.Redacted
}

// hostMountStore onboards hostMountSource as a local directory, so a spec may mount it.
type hostMountStore struct{ store.Store }

func (hostMountStore) ListWorkspaces(context.Context) ([]types.Workspace, error) {
	return []types.Workspace{{ID: uuid.New(), Name: "team", Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: hostMountSource}}}}, nil
}

// The policy writes and a preset write, as their handlers really record them, each carrying a workspace mount:
// a security_admin reads every mount source as <redacted> with the path of each named in redacted_fields,
// and a super admin reads the host path as recorded.
func TestAuditHostDetail_PolicyAndPresetWritesWithoutMountSources(t *testing.T) {
	h := newHarness(t)
	srv := New(baseTestConfig(h, hostMountStore{&memPolicyStore{rows: map[uuid.UUID]types.RunPolicy{}}}))
	spec := func(target string) string {
		return `{"min_confinement_class":"CC2","workspace_mounts":[{"source":"` + hostMountSource + `","target":"` + target + `"}]}`
	}
	w := do(t, srv, http.MethodPost, "/api/v1/policies", adminToken, `{"name":"prod","spec":`+spec("/workspace/a")+`}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body.String())
	}
	var created types.RunPolicy
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if w := do(t, srv, http.MethodPut, "/api/v1/policies/"+created.ID.String(), adminToken, `{"name":"prod","spec":`+spec("/workspace/b")+`}`); w.Code != http.StatusOK {
		t.Fatalf("update = %d %s", w.Code, w.Body.String())
	}
	if w := do(t, srv, http.MethodDelete, "/api/v1/policies/"+created.ID.String(), adminToken, ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", w.Code, w.Body.String())
	}
	ph := newHarness(t)
	pcfg := baseTestConfig(ph, hostMountStore{&memPresetStore{utype: []types.UserType{{ID: "contractor", Name: "Contractor"}}}})
	pcfg.OIDC = &oidc.Authenticator{}
	f := memPresetFixture{srv: New(pcfg), audit: ph.audit}
	mount := []types.WorkspaceMount{{Source: hostMountSource, Target: "/workspace/c"}}
	req := client.PresetRequest{UserTypes: []string{"contractor"}, Request: client.CreateRunRequest{Agent: "claude-code", Repo: "acme/widgets",
		InlinePolicy: &types.RunPolicySpec{MinConfinementClass: types.CC2, WorkspaceMounts: mount}}}
	if w := f.put(t, "widgets", req); w.Code != http.StatusCreated {
		t.Fatalf("preset create = %d %s", w.Code, w.Body.String())
	}
	req.Request.InlinePolicy.WorkspaceMounts = append(mount, types.WorkspaceMount{Source: hostMountSource, Target: "/workspace/d"})
	if w := f.put(t, "widgets", req); w.Code != http.StatusOK {
		t.Fatalf("preset update = %d %s", w.Code, w.Body.String())
	}

	var evs []types.AuditEvent
	for _, action := range []string{"policy.create", "policy.update", "policy.delete"} {
		evs = append(evs, lastAuditEvent(t, h.audit.snapshot(), action))
	}
	for _, action := range []string{"preset.create", "preset.update"} {
		evs = append(evs, lastAuditEvent(t, f.audit.snapshot(), action))
	}
	for _, ev := range evs {
		if !strings.Contains(string(ev.Data), hostMountSource) {
			t.Fatalf("%s recorded %s, want the mount source in the stored row", ev.Action, ev.Data)
		}
	}
	want := map[string][]string{
		"policy.create": {"after.workspace_mounts.source"},
		"policy.update": {"after.spec.workspace_mounts.source", "before.spec.workspace_mounts.source"},
		"policy.delete": {"before.workspace_mounts.source"},
		"preset.create": {"request.inline_policy.workspace_mounts.source"},
		"preset.update": {"request.inline_policy.workspace_mounts.source"},
	}
	sec := hostReadAs(t, evs, oidc.RoleSecurityAdmin)
	for action, fields := range want {
		data := sec[action]
		if strings.Contains(data, hostMountSource) || !strings.Contains(data, `"source":`+string(mustJSON("<redacted>"))) {
			t.Errorf("security_admin read %s data %s, want every mount source <redacted>", action, data)
		}
		if got := hostRedactedFields(t, data); !slices.Equal(got, fields) {
			t.Errorf("security_admin read %s redacted_fields %v, want %v", action, got, fields)
		}
	}
	for action, data := range hostReadAs(t, evs, oidc.RoleAdmin) {
		if !strings.Contains(data, hostMountSource) || strings.Contains(data, auditRedactedFieldsKey) {
			t.Errorf("super admin read %s data %s, want it as recorded", action, data)
		}
	}
}

// A preset's policy_overlay names workspace_mounts too; its sources are blanked on the same path.
func TestAuditHostDetail_PresetOverlayWithoutMountSources(t *testing.T) {
	data := mustJSON(map[string]any{"version": 1, "user_types": []string{"contractor"}, "request": map[string]any{
		"agent": "claude-code", "policy_overlay": map[string]any{"workspace_mounts": []types.WorkspaceMount{{Source: hostMountSource, Target: "/w"}}},
	}})
	got, ok := hostViewOf("preset.update", data)
	if !ok || strings.Contains(string(got), hostMountSource) {
		t.Fatalf("preset.update overlay served %s (%v), want the mount source withheld", got, ok)
	}
	if fields := hostRedactedFields(t, string(got)); !slices.Equal(fields, []string{"request.policy_overlay.workspace_mounts.source"}) {
		t.Errorf("redacted_fields = %v, want [request.policy_overlay.workspace_mounts.source]", fields)
	}
}

// A site_config.write as PUT /site-config records it: a security_admin reads it without the logo path, the
// internal hosts, the upstream proxy URL or the redirects and their targets, and keeps the counts.
func TestAuditHostDetail_SiteConfigWriteWithoutTopology(t *testing.T) {
	srv, audit := newSiteConfigHarness(t, &fakeSiteConfigStore{})
	body := `{
		"upstream_proxy_url": "http://proxy.corp.internal:3128",
		"egress_redirects": [
			{"from": "https://proxy.golang.org/", "to": "https://artifactory.corp.internal/api/go/go-remote/", "ecosystem": "go", "go_checksum_db_off": true}
		],
		"internal_hosts": [{"host_suffix": "corp.internal"}],
		"scm_hosts": ["dev.azure.com"]
	}`
	if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, body); w.Code != http.StatusOK {
		t.Fatalf("PUT /site-config = %d %s", w.Code, w.Body.String())
	}
	ev := lastAuditEvent(t, audit.snapshot(), "site_config.write")
	if !strings.Contains(string(ev.Data), "corp.internal") {
		t.Fatalf("site_config.write recorded %s, want the topology in the stored row", ev.Data)
	}
	data := hostReadAs(t, []types.AuditEvent{ev}, oidc.RoleSecurityAdmin)["site_config.write"]
	if strings.Contains(data, "corp.internal") {
		t.Errorf("security_admin read site_config.write %s, want no internal host, proxy or redirect target", data)
	}
	var d map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &d); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"egress_redirects_count", "internal_hosts_count", "scm_hosts_count", "upstream_proxy_configured"} {
		if _, ok := d[k]; !ok {
			t.Errorf("security_admin read site_config.write without %s, want it kept", k)
		}
	}
	want := []string{"egress_redirects", "go_checksum_db_off", "internal_hosts", "upstream_proxy_url"}
	if got := hostRedactedFields(t, data); !slices.Equal(got, want) {
		t.Errorf("redacted_fields = %v, want %v", got, want)
	}
}

// run.policy.resolve's data is the spec itself (effectivePolicyDatum embeds it), so its mounts sit at the top.
func TestAuditHostDetail_PolicyResolveMountsAtTheTop(t *testing.T) {
	var d struct {
		Mounts []types.WorkspaceMount `json:"workspace_mounts"`
	}
	raw := mustJSON(effectivePolicyDatum{RunPolicySpec: types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{{Source: hostMountSource}}}})
	if err := json.Unmarshal(raw, &d); err != nil || len(d.Mounts) != 1 || d.Mounts[0].Source != hostMountSource {
		t.Fatalf("run.policy.resolve data %s, want workspace_mounts at the top (auditHostViews mounts path {})", raw)
	}
	if m := auditHostViews["run.policy.resolve"].mounts; len(m) != 1 || len(m[0]) != 0 {
		t.Errorf("run.policy.resolve mounts = %v, want the top of data", auditHostViews["run.policy.resolve"].mounts)
	}
}

// auditEmitFiles are the sources TestAuditHostViewsMatchTheirEmits reads emits from: this package and the
// maintenance-mode audit of cmd/wardynd (the recovery rehearsal).
var auditEmitFiles = []string{"*.go", "../../cmd/wardynd/recovery_rehearse.go"}

// TestAuditHostViewsMatchTheirEmits reads every emit of each auditHostViews action from source and fails
// when the keys it records are not the action's documented fields, or a key a view allows or withholds is
// never recorded. The census reads the doc, so this is what keeps a key added to an emit from going unseen.
// run.policy.resolve is the whole spec (TestAuditHostDetail_PolicyResolveMountsAtTheTop).
func TestAuditHostViewsMatchTheirEmits(t *testing.T) {
	emitted := auditEmittedKeys(t)
	rows := auditDocFields(t, readAuditActionsDoc(t))
	// drive.write's data has a builder, so its keys are also read from a real emit with every branch taken.
	var real []string
	for k := range driveWriteAuditData(types.UserDrive{Name: "n", Team: true}, driveRehome{confirmed: true}) {
		real = append(real, k)
	}
	slices.Sort(real)
	if doc := auditDocTopFields(rows["drive.write"]); !slices.Equal(real, doc) {
		t.Errorf("audit action \"drive.write\" emits keys %v but docs/AUDIT-ACTIONS.md documents %v", real, doc)
	}
	for action, view := range auditHostViews {
		if action == "run.policy.resolve" {
			continue
		}
		keys, ok := emitted[action]
		if !ok {
			t.Errorf("auditHostViews names %q, which no emit in %v records", action, auditEmitFiles)
			continue
		}
		doc := auditDocTopFields(rows[action])
		slices.Sort(keys)
		if !slices.Equal(keys, doc) {
			t.Errorf("audit action %q records keys %v but docs/AUDIT-ACTIONS.md documents %v", action, keys, doc)
		}
		for _, k := range append(slices.Clone(view.allow), view.withhold...) {
			if !slices.Contains(keys, k) {
				t.Errorf("auditHostViews %q names %q, which its emits never record", action, k)
			}
		}
		for _, path := range view.mounts {
			if len(path) > 0 && !slices.Contains(keys, path[0]) {
				t.Errorf("auditHostViews %q blanks mounts under %q, which its emits never record", action, path[0])
			}
		}
	}
}

var auditDocParen = regexp.MustCompile(`\([^()]*\)`)

// auditDocTopFields is the field names a doc cell lists outside any parenthesis, sorted.
func auditDocTopFields(cell string) []string {
	for prev := ""; prev != cell; {
		prev, cell = cell, auditDocParen.ReplaceAllString(cell, "")
	}
	var out []string
	for _, m := range auditDocBacktick.FindAllStringSubmatch(cell, -1) {
		if auditHostFieldName.MatchString(m[1]) && !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	slices.Sort(out)
	return out
}

// auditEmitSource is the parsed emit sources: every function and method, every plain function by name (the
// callees followed), and every string constant.
type auditEmitSource struct {
	decls  []*ast.FuncDecl
	funcs  map[string]*ast.FuncDecl
	consts map[string]string
}

func auditEmittedKeys(t *testing.T) map[string][]string {
	t.Helper()
	src := auditEmitSource{funcs: map[string]*ast.FuncDecl{}, consts: map[string]string{}}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, pat := range auditEmitFiles {
		paths, err := filepath.Glob(pat)
		if err != nil || len(paths) == 0 {
			t.Fatalf("glob %s: %v (%d files)", pat, err, len(paths))
		}
		for _, p := range paths {
			if strings.HasSuffix(p, "_test.go") {
				continue
			}
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			f, err := parser.ParseFile(fset, p, b, 0)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, f)
		}
	}
	for _, f := range files {
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				src.decls = append(src.decls, d)
				if d.Recv == nil {
					src.funcs[d.Name.Name] = d
				}
			case *ast.GenDecl:
				if d.Tok != token.CONST {
					continue
				}
				for _, spec := range d.Specs {
					vs := spec.(*ast.ValueSpec)
					for i, n := range vs.Names {
						if i < len(vs.Values) {
							if s, ok := stringLit(vs.Values[i]); ok {
								src.consts[n.Name] = s
							}
						}
					}
				}
			}
		}
	}
	out := map[string][]string{}
	for _, fn := range src.decls {
		if fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 || !isAuditEmit(call) {
				return true
			}
			actions := src.actionsOf(fn, call.Args)
			if len(actions) == 0 {
				return true
			}
			keys := src.keysOf(fn, call.Args[len(call.Args)-1], 0)
			for _, a := range actions {
				for _, k := range keys {
					if !slices.Contains(out[a], k) {
						out[a] = append(out[a], k)
					}
				}
			}
			return true
		})
	}
	return out
}

func isAuditEmit(call *ast.CallExpr) bool {
	switch f := call.Fun.(type) {
	case *ast.SelectorExpr:
		return f.Sel.Name == "auditEvent"
	case *ast.Ident:
		return f.Name == "recordMaintenanceAudit"
	}
	return false
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

// actionsOf is every auditHostViews action an emit's arguments name: a literal, a constant, or a local
// variable assigned literals in fn.
func (src auditEmitSource) actionsOf(fn *ast.FuncDecl, args []ast.Expr) []string {
	var out []string
	add := func(s string) {
		if _, ok := auditHostViews[s]; ok && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	for _, a := range args {
		if s, ok := stringLit(a); ok {
			add(s)
		}
		id, ok := a.(*ast.Ident)
		if !ok {
			continue
		}
		if s, ok := src.consts[id.Name]; ok {
			add(s)
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if as, ok := n.(*ast.AssignStmt); ok {
				for i, l := range as.Lhs {
					if li, ok := l.(*ast.Ident); ok && li.Name == id.Name && i < len(as.Rhs) {
						if s, ok := stringLit(as.Rhs[i]); ok {
							add(s)
						}
					}
				}
			}
			return true
		})
	}
	return out
}

// keysOf is the top-level keys data expression e records in fn: a map literal's, mustJSON's argument's, a
// local map's (its literal, its index assignments, and those of each function it is passed to), or what a
// package function returns.
func (src auditEmitSource) keysOf(fn *ast.FuncDecl, e ast.Expr, depth int) []string {
	if depth > 4 {
		return nil
	}
	switch e := e.(type) {
	case *ast.CompositeLit:
		var out []string
		for _, el := range e.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				if s, ok := stringLit(kv.Key); ok {
					out = append(out, s)
				}
			}
		}
		return out
	case *ast.CallExpr:
		if id, ok := e.Fun.(*ast.Ident); ok {
			if id.Name == "mustJSON" && len(e.Args) == 1 {
				return src.keysOf(fn, e.Args[0], depth+1)
			}
			if callee, ok := src.funcs[id.Name]; ok && callee.Body != nil {
				var out []string
				ast.Inspect(callee.Body, func(n ast.Node) bool {
					if ret, ok := n.(*ast.ReturnStmt); ok && len(ret.Results) > 0 {
						out = append(out, src.keysOf(callee, ret.Results[0], depth+1)...)
					}
					return true
				})
				return out
			}
		}
	case *ast.Ident:
		return src.mapKeysIn(fn, e.Name, depth)
	}
	return nil
}

// mapKeysIn is every key fn sets on its map variable name, following it into the functions it is passed to.
func (src auditEmitSource) mapKeysIn(fn *ast.FuncDecl, name string, depth int) []string {
	var out []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			for i, l := range n.Lhs {
				if li, ok := l.(*ast.Ident); ok && li.Name == name && i < len(n.Rhs) {
					if lit, ok := n.Rhs[i].(*ast.CompositeLit); ok {
						out = append(out, src.keysOf(fn, lit, depth+1)...)
					}
				}
				if ix, ok := l.(*ast.IndexExpr); ok {
					if xi, ok := ix.X.(*ast.Ident); ok && xi.Name == name {
						if s, ok := stringLit(ix.Index); ok {
							out = append(out, s)
						}
					}
				}
			}
		case *ast.CallExpr:
			id, ok := n.Fun.(*ast.Ident)
			callee, found := src.funcs[func() string {
				if ok {
					return id.Name
				}
				return ""
			}()]
			if !found || callee.Body == nil || depth > 4 {
				return true
			}
			var params []string
			for _, f := range callee.Type.Params.List {
				for _, pn := range f.Names {
					params = append(params, pn.Name)
				}
			}
			for i, a := range n.Args {
				if ai, ok := a.(*ast.Ident); ok && ai.Name == name && i < len(params) {
					out = append(out, src.mapKeysIn(callee, params[i], depth+1)...)
				}
			}
		}
		return true
	})
	return out
}
