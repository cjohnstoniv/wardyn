// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/token"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// kernelResolverKindArg is the calls that reach the one grant resolver, and the
// position of their kind argument. kernelDoorCalls extends it with every
// function that forwards its own parameter as the kind (capSeamAllowed,
// capFilter, capVisible, denyUserCapability, ...): those are layers of the
// resolver, not doors.
var kernelResolverKindArg = map[string]int{
	"capGranted":       1,
	"capAllowedForSub": 3,
	"allowed":          1, // (*capBatch).allowed: see kernelDoorCalls
}

// kernelDoors is design G2's Kind x Action table in 0.8's vocabulary: every
// function outside the resolver that asks it for a decision, with the kind
// arguments it passes (a constant, or the field it reads one from). A new
// door, a door that asks about another kind, and a door that stopped asking
// all fail TestKernelDoorsAreDeclared, so none of them lands without a
// reviewer reading this table.
var kernelDoors = map[string][]string{
	// run.create (and its dry run): the request's own fields, then the inline
	// policy's entries, then the image a workspace seeds and the git rows its
	// repos resolve to, then the model provider it will use.
	"denyUserRequest":            {"capAgent", "capImage", "capIntegration", "capPolicy", "capWorkspace"},
	"narrowUserInlinePolicy":     {"capEgressHost", "capSecret", "capWorkspace"},
	"denyUserSeededImage":        {"capImage"},
	"denyUserWorkspaceProviders": {"capWorkspaceProvider"},
	"enforceRunModelProvider":    {"capModelProvider"},
	// A record session's model provider, chosen as a run's would be.
	"recordProviderChoice": {"capModelProvider"},
	// A revive or an extension, re-checked as the run's owner.
	"ownerCapabilityRefusal": {"d.kind"},
	// Deciding an approval the person's own run raised.
	"authorizeUserDecision": {"capEgressHost"},
	// Minting a personal credential.
	"handleAddSSHKey":             {"capFeature"},
	"handleCreateAPIToken":        {"capFeature"},
	"handlePutProviderCredential": {"capModelProvider"},
	"signInProvider":              {"capModelProvider"},
	"authorizeHarnessLogin":       {"capAgent"},
	// Reading one stored policy.
	"handleGetPolicy": {"capPolicy"},
	// The list carriers (design K2): what a person is offered.
	"handleListIntegrations":         {"capIntegration"},
	"handleListPolicies":             {"capPolicy"},
	"handleSetupStatus":              {"capAgent", "capIntegration"},
	"setupModelProviders":            {"capAgent"},
	"setupModelProviderState":        {"capModelProvider"},
	"computeSCMAccessRowsFor":        {"capWorkspaceProvider"},
	"userVisibleOperatorSecretNames": {"capSecret"},
}

// kernelDoorCalls walks internal/api's non-test sources and returns, per
// enclosing function, the kind expressions its resolver calls pass. A function
// that forwards one of its own parameters as the kind becomes an entry point
// itself and is left out, until nothing new forwards. Server methods are
// matched on the receiver name s, the package convention serverCallSets also
// relies on.
func kernelDoorCalls(t *testing.T) map[string][]string {
	t.Helper()
	entries := maps.Clone(kernelResolverKindArg)
	srcs := parseAPISources(t)
	for {
		out := map[string][]string{}
		grew := false
		for _, f := range srcs {
			for _, d := range f.file.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				params := paramNames(fn)
				batches := capBatchVars(fn)
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					name, recv := kernelCallee(call.Fun)
					pos, ok := entries[name]
					if !ok || len(call.Args) <= pos {
						return true
					}
					if name == "allowed" && !isCapBatch(recv, batches) || name != "allowed" && recv != nil && !isIdent(recv, "s") {
						return true
					}
					kind := exprString(call.Args[pos])
					if i := slices.Index(params, kind); i >= 0 {
						if _, known := entries[fn.Name.Name]; !known {
							entries[fn.Name.Name] = i
							grew = true
						}
						return true
					}
					out[fn.Name.Name] = append(out[fn.Name.Name], kind)
					return true
				})
			}
		}
		if grew {
			continue
		}
		for k, v := range out {
			slices.Sort(v)
			out[k] = slices.Compact(v)
		}
		return out
	}
}

// paramNames is fn's parameter names in order, receiver excluded.
func paramNames(fn *ast.FuncDecl) []string {
	var names []string
	for _, field := range fn.Type.Params.List {
		for _, n := range field.Names {
			names = append(names, n.Name)
		}
	}
	return names
}

// capBatchVars is the identifiers fn assigns a *capBatch to.
func capBatchVars(fn *ast.FuncDecl) map[string]bool {
	vars := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != len(as.Rhs) {
			return true
		}
		for i, rhs := range as.Rhs {
			if id, ok := as.Lhs[i].(*ast.Ident); ok && isCapBatch(rhs, nil) {
				vars[id.Name] = true
			}
		}
		return true
	})
	return vars
}

// isCapBatch reports whether e is a *capBatch: a call building one, or a
// variable holding one.
func isCapBatch(e ast.Expr, vars map[string]bool) bool {
	if id, ok := e.(*ast.Ident); ok {
		return vars[id.Name]
	}
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	name, _ := kernelCallee(call.Fun)
	return name == "capBatchFor" || name == "newCapBatch"
}

func kernelCallee(fun ast.Expr) (string, ast.Expr) {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name, nil
	case *ast.SelectorExpr:
		return f.Sel.Name, f.X
	case *ast.IndexExpr: // capVisible[T]
		return kernelCallee(f.X)
	}
	return "", nil
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

func exprString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return exprString(x.X) + "." + x.Sel.Name
	}
	return fmt.Sprintf("<%T>", e)
}

// TestKernelDoorsAreDeclared is G2's completeness check: the doors found in
// the source are exactly kernelDoors, and every kind in the kind table is
// asked about by name at some door.
func TestKernelDoorsAreDeclared(t *testing.T) {
	found := kernelDoorCalls(t)
	for _, door := range slices.Sorted(maps.Keys(found)) {
		want, ok := kernelDoors[door]
		if !ok {
			t.Errorf("%s asks the capability resolver about %v but is not in kernelDoors — declare the door and the kinds it gates", door, found[door])
			continue
		}
		if !slices.Equal(found[door], want) {
			t.Errorf("%s asks about %v, kernelDoors declares %v", door, found[door], want)
		}
	}
	for _, door := range slices.Sorted(maps.Keys(kernelDoors)) {
		if _, ok := found[door]; !ok {
			t.Errorf("kernelDoors declares %s, which no longer asks the resolver — drop the entry", door)
		}
	}
	named := map[string]bool{}
	for _, kinds := range found {
		for _, k := range kinds {
			named[k] = true
		}
	}
	consts := capKindConsts(t)
	for kind := range capKinds {
		if !named[consts[kind]] {
			t.Errorf("no door asks the resolver about %q by name: a kind nothing gates is a row with no Action", kind)
		}
	}
}

// capKindConsts maps each kind string to the name of the constant declaring it.
func capKindConsts(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, f := range parseAPISources(t) {
		ast.Inspect(f.file, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					break
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING || !strings.HasPrefix(name.Name, "cap") {
					continue
				}
				if kind := strings.Trim(lit.Value, `"`); capKinds[kind].reason != "" {
					out[kind] = name.Name
				}
			}
			return true
		})
	}
	if len(out) != len(capKinds) {
		t.Fatalf("found constants for %d of %d kinds: %v", len(out), len(capKinds), out)
	}
	return out
}

// kernelLaunchField is one request field run.create decides on a kind, and the
// audit target its refusal carries.
type kernelLaunchField struct {
	kind, value, target, field string
}

var (
	kernelWorkspaceID = uuid.MustParse("7d1c1f4e-0000-4000-8000-000000000740")
	kernelPolicyID    = uuid.MustParse("7d1c1f4e-0000-4000-8000-000000000741")
)

var kernelLaunchFields = []kernelLaunchField{
	{capImage, "ghcr.io/acme/tool:1", "runs.image", "image"},
	{capWorkspace, kernelWorkspaceID.String(), "runs.workspace", "workspace_id"},
	{capAgent, "claude-code", "runs.agent", "agent"},
	{capIntegration, "int-kernel", "runs.integration", "integration_id"},
	{capPolicy, kernelPolicyID.String(), "runs.policy", "policy_id"},
}

// kernelTier is one principal the launch door can be asked by, and the tier
// the resolver's oracle must see it as.
type kernelTier struct {
	name, oracle string
	session      func(t *testing.T, groups []string) *http.Cookie
}

func kernelSession(t *testing.T, sess oidc.Session) *http.Cookie {
	t.Helper()
	sess.V, sess.Sub, sess.Email = oidc.SessionCodecVersion, capSub, capEmail
	sess.UserType, sess.Expiry = types.UserTypeStandard, time.Now().UTC().Add(time.Hour)
	payload, err := json.Marshal(sess)
	if err != nil {
		t.Fatal(err)
	}
	return signedSessionCookie(payload)
}

var kernelTiers = []kernelTier{
	{"admin", oidc.RoleAdmin, func(t *testing.T, g []string) *http.Cookie {
		return kernelSession(t, oidc.Session{Role: oidc.RoleAdmin, Groups: g})
	}},
	{"security admin", oidc.RoleSecurityAdmin, func(t *testing.T, g []string) *http.Cookie {
		return kernelSession(t, oidc.Session{Role: oidc.RoleSecurityAdmin, Groups: g})
	}},
	{"user", oidc.RoleUser, func(t *testing.T, g []string) *http.Cookie {
		return kernelSession(t, oidc.Session{Role: oidc.RoleUser, Groups: g})
	}},
	// An admin looking through the user view: the session still says admin,
	// and every decision must be the user's.
	{"user in view", oidc.RoleUser, func(t *testing.T, g []string) *http.Cookie {
		return kernelSession(t, oidc.Session{Role: oidc.RoleAdmin, MemberMode: true, UserViewType: types.UserTypeStandard, Groups: g})
	}},
}

// TestKernelNonescape is design G2 at the launch door: every kind run.create
// decides x every tier that can reach it x every grant state, through the real
// router, against the resolver's seven-step oracle (resolverCase.want). A
// field the door stops asking about, a tier the door exempts that the oracle
// does not (or the reverse), and a grant state the door reads differently all
// fail here.
func TestKernelNonescape(t *testing.T) {
	for _, f := range kernelLaunchFields {
		for _, tier := range kernelTiers {
			t.Run(f.kind+"/"+tier.name, func(t *testing.T) {
				cs := &capStore{}
				srv, st, rec := govEscapeFixture(t, cs)
				st.workspaces = []types.Workspace{{ID: kernelWorkspaceID, Name: "kernel"}}
				st.policies[kernelPolicyID] = types.RunPolicy{ID: kernelPolicyID, Name: "kernel", Spec: govDeployment()}
				for _, c := range resolverCases() {
					if c.tier != tier.oracle || c.noStore {
						continue
					}
					cs.grants = kernelGrants(c, f)
					cs.enf = map[string]bool{f.kind: c.enforced}
					cs.restricted = nil
					if c.restricted {
						cs.restricted = map[string]map[string]bool{f.kind: {f.value: true}}
					}
					var groups []string
					if !c.stale {
						groups = []string{"eng"}
					}
					before := len(rec.snapshot())
					body := map[string]any{"agent": "claude-code", "task": "kernel nonescape"}
					body[f.field] = f.value
					raw, _ := json.Marshal(body)
					w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", tier.session(t, groups), string(raw))
					refused := false
					for _, ev := range rec.snapshot()[before:] {
						if ev.Action == "authz.denied" && ev.Target == f.target {
							refused = true
						}
					}
					allowed, _ := c.want(resolverDoor{}, f.kind)
					if refused == allowed || refused && w.Code != http.StatusForbidden {
						t.Errorf("%v: status %d, refused on %s = %v; the resolver allows = %v\n%s", c, w.Code, f.target, refused, allowed, w.Body.String())
					}
				}
			})
		}
	}
}

// kernelGrants is c's grant rows for field f's own value.
func kernelGrants(c resolverCase, f kernelLaunchField) []types.CapabilityGrant {
	gs := c.grants(f.kind)
	for i := range gs {
		gs[i].Value = strings.Replace(gs[i].Value, resolverValue(f.kind), f.value, 1)
	}
	return gs
}

// TestKernelNonescapeDevice is G2's device tier. A device (the laptop daemon)
// reaches no launch door, but a device request carries no human, which is also
// what the admin token looks like: every resolver door must answer it as a
// principal with no subjects at all — never as the exempt operator. Only an
// everyone-allow can list it, and a group deny still refuses it, because its
// group snapshot is absent (stale), not empty.
func TestKernelNonescapeDevice(t *testing.T) {
	ctx := context.WithValue(context.Background(), deviceCtxKey{}, types.Device{ID: uuid.New(), Name: "laptop"})
	for _, door := range resolverDoors {
		t.Run(door.name, func(t *testing.T) {
			for _, kind := range capabilityKinds {
				for _, c := range resolverCases() {
					if c.tier != oidc.RoleUser || !c.stale {
						continue
					}
					srv := &Server{}
					if !c.noStore {
						st := &resolverTableStore{grants: c.grants(kind), enf: map[string]bool{kind: c.enforced}}
						if c.restricted {
							st.restricted = map[string]map[string]bool{kind: {resolverValue(kind): true}}
						}
						srv = capServer(st)
					}
					// A device matches no user, user-type or user-deny row.
					oracle := c
					if oracle.allow == "user" || oracle.allow == "type" {
						oracle.allow = ""
					}
					if oracle.deny == "user" {
						oracle.deny = ""
					}
					got, err := door.ask(srv, ctx, kind, resolverValue(kind))
					want, wantErr := oracle.want(door, kind)
					if (err != nil) != wantErr || got != want {
						t.Errorf("%s(%s) device %v = %v, %v; want %v (error %v)", door.name, kind, c, got, err, want, wantErr)
					}
				}
			}
		})
	}
}
