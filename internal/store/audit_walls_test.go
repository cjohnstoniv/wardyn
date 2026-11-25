// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// auditWallsGoldenUpdate rewrites testdata/decision_rows.golden from the cases below.
var auditWallsGoldenUpdate = flag.Bool("update-decision-rows", false, "rewrite testdata/decision_rows.golden")

// The fixed ids the golden cases use, so the golden file holds no random value.
var (
	auditWallsGoldenRun      = uuid.MustParse("3c9f6a1e-5d2b-4b8e-9f0a-1e2d3c4b5a69")
	auditWallsGoldenRunner   = uuid.MustParse("7a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d")
	auditWallsGoldenApproval = uuid.MustParse("6f1c0c2e-3b0a-4d7e-9c1a-0a5b3c2d1e0f")
)

// auditWallsGoldenCase is one stored row, as its writer leaves it, and the recorded_at it was appended at.
type auditWallsGoldenCase struct {
	name       string
	action     string
	actor      string
	data       map[string]any
	recordedAt time.Duration // after the row's own time
}

// auditWallsGoldenCases is one stored row per writer of egress.* rows. A new writer, or a new key an
// existing writer adds, appends a case here, and the golden file is regenerated with -update-decision-rows.
func auditWallsGoldenCases(base time.Time) []auditWallsGoldenCase {
	agent := "spiffe://wardyn.test/agent-run/" + auditWallsGoldenRun.String()
	runner := "runner:" + auditWallsGoldenRunner.String()
	spooled := func(d map[string]any, observed time.Duration) map[string]any {
		d["observed_at"] = base.Add(observed)
		d["spooled"] = true
		d["evidence_source"] = "runner_asserted"
		d["spool_id"] = "spool-1"
		d["spool_seq"] = 4
		d["spool_hash"] = "abc123"
		return d
	}
	return []auditWallsGoldenCase{
		{name: "a single post", action: "egress.allow", actor: agent, recordedAt: time.Second, data: map[string]any{
			"host": "api.github.com", "port": 443, "method": "GET", "path": "/repos", "rule_source": "policy", "approval_id": nil,
		}},
		{name: "a runner-spooled row with count", action: "egress.deny", actor: runner, recordedAt: time.Second, data: spooled(map[string]any{
			"host": "registry.npmjs.org", "port": 443, "method": "GET", "path": "/", "rule_source": "policy:default-deny",
			"approval_id": nil, "count": 3, "by": "wardyn", "refusal_reason": "default_deny",
		}, -50*time.Second)},
		{name: "a runner-spooled row without count", action: "egress.allow", actor: runner, recordedAt: time.Second, data: spooled(map[string]any{
			"host": "proxy.golang.org", "port": 443, "method": "GET", "path": "/", "rule_source": "policy", "approval_id": nil,
		}, -5*time.Second)},
		{name: "a dropped-decisions summary", action: "egress.deny", actor: agent, recordedAt: time.Second, data: map[string]any{
			"host": "", "port": 0, "method": "", "path": "", "rule_source": "egress:dropped-decisions-7", "approval_id": nil,
		}},
		{name: "a repeat summary", action: "egress.deny", actor: agent, recordedAt: time.Second, data: map[string]any{
			"host": "example.com", "port": 443, "method": "GET", "path": "/", "rule_source": "policy:denied", "approval_id": nil,
			"repeat": 12, "by": "wardyn", "refusal_reason": "policy_denied", "layer": "unit", "layer_name": "Payments",
		}},
		{name: "spool-lost", action: "egress.deny", actor: runner, recordedAt: time.Second, data: spooled(map[string]any{
			"host": "", "port": 0, "method": "", "path": "", "rule_source": "builtin:spool-lost", "approval_id": nil,
			"cause": "41 egress decisions were not recorded (spool full)",
		}, -10*time.Second)},
		{name: "a legacy egress.pending", action: "egress.pending", actor: agent, recordedAt: time.Second, data: map[string]any{
			"host": "pypi.org", "port": 443, "method": "GET", "path": "/simple/", "rule_source": "approval:pending",
			"approval_id": auditWallsGoldenApproval,
		}},
		{name: "a VM row with flow", action: "egress.deny", actor: agent, recordedAt: time.Second, data: map[string]any{
			"host": "10.0.0.5", "port": 22, "method": "", "path": "", "rule_source": "builtin:private-ip", "approval_id": nil,
			"flow": "vmnet-flow-9", "by": "wardyn", "refusal_reason": "private_address",
		}},
		{name: "an allow with upstream_fault", action: "egress.allow", actor: agent, recordedAt: time.Second, data: map[string]any{
			"host": "bedrock-runtime.us-east-1.amazonaws.com", "port": 443, "method": "POST", "path": "/model/invoke",
			"rule_source": "policy", "approval_id": nil, "via": "direct", "upstream_fault": "ThrottlingException",
		}},
		{name: "a tool rule allow", action: "egress.allow", actor: agent, recordedAt: time.Second, data: map[string]any{
			"host": "wardyn.internal", "port": 443, "method": "POST", "path": "/tool", "rule_source": "policy:tool-allow",
			"approval_id": nil, "matched_rule": "agent:Bash", "cause": `"Bash"`,
		}},
		{name: "a sponsored service row", action: "egress.allow", actor: agent, recordedAt: time.Second, data: map[string]any{
			"host": "llm.example", "port": 443, "method": "POST", "path": "/v1/chat", "rule_source": "policy", "approval_id": nil,
			"sponsor": "alice@corp.example",
		}},
	}
}

// auditWallsRender writes r as the golden file holds it: every set field, times as offsets from base.
func auditWallsRender(b *strings.Builder, name string, base time.Time, r store.DecisionRow) {
	fmt.Fprintf(b, "== %s\n", name)
	str := func(k, v string) {
		if v != "" {
			fmt.Fprintf(b, "%s: %q\n", k, v)
		}
	}
	str("action", r.Action)
	str("actor", r.Actor)
	str("host", r.Host)
	if r.Port != 0 {
		fmt.Fprintf(b, "port: %d\n", r.Port)
	}
	str("rule_source", r.RuleSource)
	str("approval_id", r.ApprovalID)
	str("by", r.By)
	str("reason", r.Reason)
	str("layer_name", r.LayerName)
	str("matched_rule", r.MatchedRule)
	fmt.Fprintf(b, "weight: %d\n", r.Weight)
	str("cause", r.Cause)
	str("flow", r.Flow)
	str("upstream_fault", r.UpstreamFault)
	str("via", r.Via)
	fmt.Fprintf(b, "contact_lost: %t\n", r.ContactLost)
	fmt.Fprintf(b, "contact_age: %s\n", r.ContactAge)
	str("owner", r.Owner)
	str("sponsor", r.Sponsor)
	str("workload", r.Workload)
	fmt.Fprintf(b, "format: %d\n", r.Format)
	fmt.Fprintf(b, "seq: %d\n", r.Seq)
	fmt.Fprintf(b, "time: %s\n", r.Time.Sub(base))
	fmt.Fprintf(b, "recorded_at: %s\n", r.RecordedAt.Sub(base))
	fmt.Fprintf(b, "decided_at: %s\n", r.DecidedAt.Sub(base))
	fmt.Fprintf(b, "late: %t\n", r.Late())
}

// TestDecodeDecisionRow_Golden pins DecodeDecisionRow over one stored row per writer of egress.* rows: the
// proxy's post, the runner spool's replay with and without a count, the proxy's dropped-decisions summary,
// a repeat summary, the spool's loss row, a legacy egress.pending, a desktop VM row and an allow with an
// upstream fault. New writers append a case (auditWallsGoldenCases) and regenerate the golden file.
func TestDecodeDecisionRow_Golden(t *testing.T) {
	base := time.Unix(0, 0).UTC()
	var b strings.Builder
	b.WriteString("# DecodeDecisionRow over one stored row per writer; times are offsets from the row's own time.\n")
	for i, c := range auditWallsGoldenCases(base) {
		raw, err := json.Marshal(c.data)
		if err != nil {
			t.Fatalf("%s: marshal data: %v", c.name, err)
		}
		runID := auditWallsGoldenRun
		ev := types.AuditEvent{ID: uuid.New(), Time: base, RunID: &runID, ActorType: types.ActorAgent, Actor: c.actor,
			Action: c.action, Target: "", Outcome: "success", Data: raw}
		r, err := store.DecodeDecisionRow(ev, base.Add(c.recordedAt), int64(i+1))
		if err != nil {
			t.Fatalf("%s: decode: %v", c.name, err)
		}
		if r.ID != ev.ID || r.RunID != runID {
			t.Errorf("%s: decoded id %s and run %s, want the row's %s and %s", c.name, r.ID, r.RunID, ev.ID, runID)
		}
		if r.Weight < 1 {
			t.Errorf("%s: weight %d, want at least 1", c.name, r.Weight)
		}
		auditWallsRender(&b, c.name, base, r)
	}
	path := filepath.Join("testdata", "decision_rows.golden")
	if *auditWallsGoldenUpdate {
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden missing (run with -update-decision-rows): %v", err)
	}
	if got := b.String(); got != string(want) {
		t.Errorf("decoded rows differ from %s (rerun with -update-decision-rows and review the diff):\n--- got\n%s\n--- want\n%s", path, got, want)
	}

	t.Run("a row with no run decodes a zero run id", func(t *testing.T) {
		r, err := store.DecodeDecisionRow(types.AuditEvent{ID: uuid.New(), Time: base, Action: "egress.allow"}, base, 1)
		if err != nil || r.RunID != uuid.Nil || r.Weight != 1 || !r.DecidedAt.Equal(base) {
			t.Errorf("decode = %+v, %v; want a zero run id, weight 1 and the row's time", r, err)
		}
	})
	t.Run("data that does not decode is an error", func(t *testing.T) {
		for _, raw := range []string{`[1,2]`, `"text"`, `{"port":"443"}`, `{"observed_at":"soon"}`} {
			ev := types.AuditEvent{ID: uuid.New(), Time: base, Action: "egress.deny", Data: json.RawMessage(raw)}
			if _, err := store.DecodeDecisionRow(ev, base, 1); err == nil {
				t.Errorf("decode of data %s succeeded, want an error", raw)
			}
		}
	})
}

// auditWallsAllowedBodies are the accessor methods whose bodies may hold audit_events SQL. They live in
// audit_walls.go only; a function of the same name anywhere else gets no exception.
var auditWallsAllowedBodies = []string{"AuditWallsRows", "AuditWallsCounts", "AuditWallsFirstAllows"}

// auditWallsAllowedReads are the audit reads walls code may call besides the accessor: RunPolicyEnvelopes
// reads policy rows, never decision rows, and readRunPolicyEvidence and runPolicyServed are the policy
// view's own evidence read, which the run walls route shares with GET /runs/{id}/policy.
var auditWallsAllowedReads = []string{
	"AuditWallsRows", "AuditWallsCounts", "AuditWallsFirstAllows", "RunPolicyEnvelopes", "readRunPolicyEvidence", "runPolicyServed",
}

// auditWallsStoreAuditMethods is every method of store.PG that names the audit log or returns its rows:
// each is an audit store read (or write) walls code must not call.
func auditWallsStoreAuditMethods(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || len(fd.Recv.List) != 1 || !auditWallsIsPG(fd.Recv.List[0].Type) {
				continue
			}
			if strings.Contains(fd.Name.Name, "Audit") || auditWallsReturnsAuditEvent(fd.Type.Results) {
				out[fd.Name.Name] = true
			}
		}
	}
	for _, name := range auditWallsAllowedReads {
		delete(out, name)
	}
	return out
}

func auditWallsIsPG(e ast.Expr) bool {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "PG"
}

func auditWallsReturnsAuditEvent(results *ast.FieldList) bool {
	if results == nil {
		return false
	}
	found := false
	ast.Inspect(results, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "AuditEvent" {
			found = true
		}
		return !found
	})
	return found
}

// auditWallsViolations is every audit read in one parsed file outside the accessor: a call to one of
// forbidden, and a string literal naming audit_events or reading a data key (data->), unless it sits in
// an accessor body of audit_walls.go. Comments are not literals, so they are never read.
func auditWallsViolations(fset *token.FileSet, f *ast.File, base string, forbidden map[string]bool) []string {
	var out []string
	for _, decl := range f.Decls {
		allowed := false
		if fd, ok := decl.(*ast.FuncDecl); ok && fd.Recv != nil && base == "audit_walls.go" {
			allowed = slices.Contains(auditWallsAllowedBodies, fd.Name.Name)
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BasicLit:
				if x.Kind != token.STRING || allowed {
					return true
				}
				for _, bad := range []string{"audit_events", "data->"} {
					if strings.Contains(x.Value, bad) {
						out = append(out, fmt.Sprintf("%s: %s outside the accessor", fset.Position(x.Pos()), bad))
					}
				}
			case *ast.CallExpr:
				name := ""
				switch fn := x.Fun.(type) {
				case *ast.SelectorExpr:
					name = fn.Sel.Name
				case *ast.Ident:
					name = fn.Name
				}
				if forbidden[name] {
					out = append(out, fmt.Sprintf("%s: calls %s, an audit store read outside the accessor", fset.Position(x.Pos()), name))
				}
			}
			return true
		})
	}
	return out
}

// TestWallsReadsAuditOnlyThroughTheAccessor keeps the Walls view's audit reads on the accessor: no walls file
// calls an audit store read but AuditWallsRows, AuditWallsCounts, AuditWallsFirstAllows, RunPolicyEnvelopes and
// the policy view's evidence read, and no SQL outside the accessor bodies names audit_events or a data key.
func TestWallsReadsAuditOnlyThroughTheAccessor(t *testing.T) {
	forbidden := auditWallsStoreAuditMethods(t)
	for _, must := range []string{"QueryAuditEvents", "QueryAuditEventsFilteredPage", "QueryAuditEventsForUnits"} {
		if !forbidden[must] {
			t.Fatalf("the audit store reads found in package store miss %s; the guard would not see a call to it", must)
		}
	}
	files, err := filepath.Glob(filepath.Join("..", "api", "walls_*.go"))
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, "audit_walls.go")
	scanned := 0
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		scanned++
		for _, v := range auditWallsViolations(fset, f, filepath.Base(path), forbidden) {
			t.Error(v)
		}
	}
	if scanned < 2 {
		t.Fatalf("scanned %d files; want audit_walls.go and the api walls files", scanned)
	}

	t.Run("a planted violation is caught", func(t *testing.T) {
		src := `package api

func (s *Server) wallsPlanted(ctx context.Context, id uuid.UUID) {
	// A comment may name audit_events and data->'host' freely.
	_, _ = s.cfg.Store.QueryAuditEvents(ctx, id, 10)
	_ = "SELECT data->>'host' FROM audit_events"
}

func (s PG) AuditWallsRows() { _ = "SELECT id FROM audit_events" }
`
		pfset := token.NewFileSet()
		f, err := parser.ParseFile(pfset, "walls_planted.go", src, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		got := auditWallsViolations(pfset, f, "walls_planted.go", forbidden)
		if len(got) != 4 {
			t.Errorf("planted file gave %d violations, want 4 (the call, both strings of the first literal, the accessor name outside audit_walls.go): %v", len(got), got)
		}
	})
}
