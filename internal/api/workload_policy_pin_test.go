// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/token"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPolicyPinRefusal: a launch with no pin, or on its pin, goes ahead; one off its pin is refused 409,
// naming both digests.
func TestPolicyPinRefusal(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	for _, want := range []string{"", digest} {
		if r := (&policyPinCheck{want: want, got: digest}).refusal(); r != nil {
			t.Fatalf("pin %q on a launch that resolved %s: refusal %+v, want none", want, digest, r)
		}
	}
	other := strings.Repeat("0", 64)
	r := (&policyPinCheck{want: other, got: digest}).refusal()
	if r == nil || r.status != http.StatusConflict || r.body.Reason != reasonWorkloadPolicyPinMismatch ||
		!strings.Contains(r.body.Error, other) || !strings.Contains(r.body.Error, digest) {
		t.Fatalf("pin off the policy: refusal %+v; want 409 %s naming %s and %s", r, reasonWorkloadPolicyPinMismatch, other, digest)
	}
}

// TestStepPolicyPin: the step takes the launch's digest onto the pin the request carries and answers the
// refusal itself; a request with no pin, every run that is not a workload's, goes on untouched.
func TestStepPolicyPin(t *testing.T) {
	s := &Server{}
	spec := types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"api.anthropic.com"}}
	fold := func(ctx context.Context) (*runFold, *httptest.ResponseRecorder) {
		w := httptest.NewRecorder()
		return &runFold{w: w, r: httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/runs", nil), req: &createRunRequest{}, spec: spec}, w
	}
	f, w := fold(context.Background())
	if !s.stepPolicyPin(f, nil) || w.Body.Len() != 0 {
		t.Fatalf("no pin on the request: step answered %d %s, want it to go on", w.Code, w.Body.String())
	}
	digest := foldDigest(f, types.RunnerAmounts{})
	ctx, pin := withPolicyPin(context.Background(), digest)
	if f, w = fold(ctx); !s.stepPolicyPin(f, nil) || pin.got != digest || w.Body.Len() != 0 {
		t.Fatalf("on its pin: step answered %d %s, recorded %q; want it to go on with %s", w.Code, w.Body.String(), pin.got, digest)
	}
	ctx, pin = withPolicyPin(context.Background(), strings.Repeat("0", 64))
	if f, w = fold(ctx); s.stepPolicyPin(f, nil) || pin.got != digest || w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), reasonWorkloadPolicyPinMismatch) {
		t.Fatalf("off its pin: step answered %d %s, recorded %q; want 409 %s", w.Code, w.Body.String(), pin.got, reasonWorkloadPolicyPinMismatch)
	}
}

// TestLaunchPolicyDigest_OneValueAtEveryDoor: when the pin step runs, a dry door's spec already holds the
// workspace's egress, unioned before the components' hosts, and a main drive; create's holds neither, and its
// component names the workspace's host in lower case, so create keeps that spelling and the dry door the
// workspace's. One launch seen from both doors is one digest, the fold's spec is left as it was, and a host
// more, a size more or a host fewer is another digest.
func TestLaunchPolicyDigest_OneValueAtEveryDoor(t *testing.T) {
	ws := types.Workspace{ID: uuid.New(), Name: "pin-ws", ApprovedEgress: []string{"WS.pin.example"}, DeniedEgress: []string{"blocked.pin.example"}}
	at := func(spec types.RunPolicySpec) *runFold {
		return &runFold{req: &createRunRequest{}, spec: spec, wsRefs: []types.Workspace{ws}}
	}
	asked := types.RunnerAmounts{CPUMillis: 1000, MemoryMiB: 1024, DiskMiB: 2048}
	create := types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"api.anthropic.com", "tool.pin.example", "ws.pin.example"},
		Resources: &types.ResourceLimits{CPUMillis: 1000, MemoryMiB: 1024}}
	dry := types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"api.anthropic.com", "WS.pin.example", "tool.pin.example"},
		DeniedDomains: []string{"blocked.pin.example"}, Resources: &types.ResourceLimits{CPUMillis: 1000, MemoryMiB: 1024, DiskMiB: 2048}}
	want := foldDigest(at(create), asked)
	if !workloadPolicyPinPattern.MatchString(want) {
		t.Fatalf("digest %q is not 64 lowercase hex characters", want)
	}
	if got := foldDigest(at(dry), asked); got != want {
		t.Fatalf("dry door's digest %s, create's %s: want one value", got, want)
	}
	if len(create.AllowedDomains) != 3 || create.DeniedDomains != nil || create.Resources.DiskMiB != 0 {
		t.Fatalf("the fold's spec was changed: %+v", create)
	}
	more := dry.Clone()
	more.AllowedDomains = append(more.AllowedDomains, "more.pin.example")
	fewer := at(create)
	fewer.wsRefs = nil
	bigger := asked
	bigger.DiskMiB = 4096
	for name, c := range map[string]struct {
		f    *runFold
		size types.RunnerAmounts
	}{"a host more": {at(more), asked}, "a bigger main drive asked": {at(create), bigger}, "no workspace": {fewer, asked}} {
		if foldDigest(c.f, c.size) == want {
			t.Errorf("%s: the same digest, want another", name)
		}
	}
}

// TestLaunchPolicyDigest_HoldsTheSizeAskedNotARunnersCut: two creates of one request, placed while the
// pool's runners gave one run different sizes, hold different applied sizes and main drives in their specs.
// Their digests are one: the digest holds the size asked. Another size asked is another digest.
func TestLaunchPolicyDigest_HoldsTheSizeAskedNotARunnersCut(t *testing.T) {
	asked := types.RunnerAmounts{CPUMillis: 1500, MemoryMiB: 2048, DiskMiB: 4096}
	placed := func(cpu, mem, disk int) *runFold {
		return &runFold{req: &createRunRequest{}, spec: types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"api.anthropic.com"},
			Resources: &types.ResourceLimits{CPUMillis: cpu, MemoryMiB: mem, DiskMiB: disk, PidsLimit: 512}}}
	}
	roomy, cut := placed(1500, 2048, 4096), placed(1000, 1024, 1024)
	want := foldDigest(roomy, asked)
	if got := foldDigest(cut, asked); got != want {
		t.Fatalf("a launch a runner cut: digest %s, the uncut launch's %s; want one value", got, want)
	}
	if r := cut.spec.Resources; r.CPUMillis != 1000 || r.MemoryMiB != 1024 || r.DiskMiB != 1024 {
		t.Fatalf("the fold's size was changed: %+v", *r)
	}
	other := asked
	other.CPUMillis = 1200
	if foldDigest(roomy, other) == want {
		t.Error("another size asked: the same digest, want another")
	}
}

// TestNoStepAfterThePolicyPinWritesTheSpec: the pin's digest holds every step that writes the run's policy
// only while no step after stepPolicyPin that create runs writes f.spec, takes its address, reads a part of it
// into a name, calls a method on f or f.spec, hands the fold to a helper, or hands f.spec or a part of it to a
// callee outside specReaders. A dry door's own step may: create adds what it adds after the mint.
// handleCreateRun's code from the fold to the mint is held by reading, not by this guard.
func TestNoStepAfterThePolicyPinWritesTheSpec(t *testing.T) {
	fset := token.NewFileSet()
	steps := runFoldStepDecls(t, fset)
	at := slices.IndexFunc(steps, func(st foldStepDecl) bool { return st.name == "stepPolicyPin" })
	if at < 0 {
		t.Fatal("runFoldSteps has no stepPolicyPin")
	}
	for _, st := range steps[at+1:] {
		if st.modes&foldCreate == 0 {
			continue
		}
		ast.Inspect(st.decl.Body, func(n ast.Node) bool {
			var bad []ast.Expr
			switch n := n.(type) {
			case *ast.AssignStmt:
				bad = slices.DeleteFunc(slices.Concat(n.Lhs, n.Rhs), func(e ast.Expr) bool { return !onFoldSpec(e) })
			case *ast.IncDecStmt:
				if onFoldSpec(n.X) {
					bad = []ast.Expr{n.X}
				}
			case *ast.UnaryExpr:
				if n.Op == token.AND && onFoldSpec(n.X) {
					bad = []ast.Expr{n.X}
				}
			case *ast.CallExpr:
				bad = slices.DeleteFunc(slices.Clone(n.Args), func(e ast.Expr) bool {
					if id, ok := e.(*ast.Ident); ok && id.Name == "f" {
						return false
					}
					return !onFoldSpec(e) || specReaders[calleeName(n)]
				})
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok {
					if id, isID := sel.X.(*ast.Ident); onFoldSpec(sel.X) || isID && id.Name == "f" {
						bad = append(bad, sel.X)
					}
				}
			}
			for _, e := range bad {
				t.Errorf("%s can change the spec at %s, after stepPolicyPin took its digest: move the step above it", st.name, fset.Position(e.Pos()))
			}
			return true
		})
	}
}

// specReaders are the callees a step after the pin may hand f.spec or a part of it: each only reads it.
var specReaders = map[string]bool{"len": true, "githubCLIRepoRefusal": true, "resolveADOEntraLane": true,
	"adoStandingAtDoor": true, "refuseADOStandingAtDoor": true, "createRecordingChoice": true, "runFitSpec": true}

// foldDigest is launchPolicyDigest over the parts of f that stepPolicyPin hands it.
func foldDigest(f *runFold, size types.RunnerAmounts) string {
	return launchPolicyDigest(f.spec, f.wsRefs, f.req.Repo, f.ceiling.Spec.AllowedDomains, size)
}

// onFoldSpec reports whether e is f.spec or reaches into it.
func onFoldSpec(e ast.Expr) bool {
	for {
		switch x := e.(type) {
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok && id.Name == "f" && x.Sel.Name == "spec" {
				return true
			}
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.StarExpr:
			e = x.X
		case *ast.ParenExpr:
			e = x.X
		default:
			return false
		}
	}
}

// TestQueueAuthority_KeepsThePolicyPin: the pin a launch carried survives its queue row and is put back,
// unrecorded, for the start's replayed fold; a launch that carried none gets none back.
func TestQueueAuthority_KeepsThePolicyPin(t *testing.T) {
	for _, want := range []string{strings.Repeat("ab", 32), ""} {
		ctx, _ := withPolicyPin(context.Background(), want)
		raw, err := json.Marshal(captureQueueAuthority(ctx))
		if err != nil {
			t.Fatal(err)
		}
		var back queueAuthority
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		p := policyPinOf(back.context(context.Background()))
		switch {
		case want == "" && p != nil:
			t.Fatalf("a launch without a pin restored %+v from %s, want none", p, raw)
		case want != "" && (p == nil || p.want != want || p.got != ""):
			t.Fatalf("restored pin = %+v from %s, want %q unrecorded", p, raw, want)
		}
	}
}
