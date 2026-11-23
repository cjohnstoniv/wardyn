// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// classifyNow is the one clock every row of these tests is stamped from.
var classifyNow = time.Now()

// classifyWriterDirs are the packages whose non-test files write a rule_source, relative to internal/api.
var classifyWriterDirs = []string{"../egress/proxy", "../egress", "../runnerspool"}

// classifyAuditDoc is the audit action reference whose egress tables the census reads.
const classifyAuditDoc = "../../docs/AUDIT-ACTIONS.md"

// classifySourceRE is the shape of a rule_source string literal. A source that is a prefix (a count or an id
// follows it) is cut at its first % or <, so "egress:dropped-decisions-%d" and "approval:" are prefixes.
var classifySourceRE = regexp.MustCompile(`^(policy|builtin|brokered|approval|scan|credential|site-config|egress|vmnet|artifact|llm):[A-Za-z0-9:<>%_.\-]*$`)

// classifyPair is one (action, rule_source) a writer emits and the class the walls view must give it.
type classifyPair struct {
	action string
	source string // "approval:<id>", "vmnet:<reason>" and "egress:dropped-decisions-<n>" name a family; classifyConcrete fills one in
	want   wallsClass
	// reviewed is why a grey "?" is the right answer; it is required when want is wallsUnclassified.
	reviewed string
}

func classifyPairs(action string, want wallsClass, sources ...string) []classifyPair {
	out := make([]classifyPair, 0, len(sources))
	for _, s := range sources {
		out = append(out, classifyPair{action: action, source: s, want: want})
	}
	return out
}

func classifyJoin(groups ...[]classifyPair) []classifyPair {
	var out []classifyPair
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

func classifyConcrete(source string) string {
	r := strings.NewReplacer("<id>", "0b9f3c1a", "<n>", "7", "<reason>", "udp")
	return r.Replace(source)
}

// classifyKey is a source cut at its first % or <: the form a prefix is written in by a writer, in the doc and
// in the pin table alike.
func classifyKey(source string) string {
	if i := strings.IndexAny(source, "%<"); i >= 0 {
		return source[:i]
	}
	return source
}

func classifyRow(action, source, by string) store.DecisionRow {
	return store.DecisionRow{
		Action: action, RuleSource: classifyConcrete(source), By: by,
		Time: classifyNow, RecordedAt: classifyNow, DecidedAt: classifyNow, Weight: 1,
	}
}

// classifyScanResult is what the writers' Go files and the audit doc say.
type classifyScanResult struct {
	sources map[string]bool    // every rule_source key a writer spells
	pairs   map[[2]string]bool // (action, key) pairs found in one call or struct literal
}

var classifyActionOf = map[string]string{"Allow": "egress.allow", "Deny": "egress.deny", "Pending": "egress.hold"}

// classifyScanWriters parses the writers' non-test files. A source is a string literal of rule_source shape,
// the value of a ruleSource constant, or the value assigned to a RuleSource field. A pair is an egress action
// and a source in the same call or composite literal.
func classifyScanWriters(t *testing.T) classifyScanResult {
	t.Helper()
	fset := token.NewFileSet()
	var files []*ast.File
	for _, dir := range classifyWriterDirs {
		matches, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(matches) == 0 {
			t.Fatalf("no writer files under %s: %v", dir, err)
		}
		for _, m := range matches {
			if strings.HasSuffix(m, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, m, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", m, err)
			}
			files = append(files, f)
		}
	}

	consts := map[string]string{}
	for pass := 0; pass < 3; pass++ {
		for _, f := range files {
			for _, d := range f.Decls {
				gd, ok := d.(*ast.GenDecl)
				if !ok || gd.Tok != token.CONST {
					continue
				}
				for _, spec := range gd.Specs {
					vs := spec.(*ast.ValueSpec)
					for i, n := range vs.Names {
						if i >= len(vs.Values) {
							continue
						}
						if s, ok := classifyResolve(vs.Values[i], consts); ok {
							consts[n.Name] = s
						}
					}
				}
			}
		}
	}

	res := classifyScanResult{sources: map[string]bool{}, pairs: map[[2]string]bool{}}
	addSource := func(s string) {
		k := classifyKey(s)
		res.sources[k] = true
	}
	for name, v := range consts {
		if strings.HasPrefix(strings.ToLower(name), "rulesource") {
			addSource(v)
		}
	}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.BasicLit:
				if s, ok := classifyResolve(x, consts); ok && classifySourceRE.MatchString(s) {
					addSource(s)
				}
			case *ast.KeyValueExpr:
				if id, ok := x.Key.(*ast.Ident); ok && id.Name == "RuleSource" {
					if s, ok := classifyResolve(x.Value, consts); ok {
						addSource(s)
					}
				}
			case *ast.AssignStmt:
				for i, l := range x.Lhs {
					if sel, ok := l.(*ast.SelectorExpr); ok && sel.Sel.Name == "RuleSource" && i < len(x.Rhs) {
						if s, ok := classifyResolve(x.Rhs[i], consts); ok {
							addSource(s)
						}
					}
				}
			case *ast.CallExpr:
				classifyPairsOf(res, x.Args, consts)
			case *ast.CompositeLit:
				classifyPairsOf(res, x.Elts, consts)
			}
			return true
		})
	}
	return res
}

func classifyPairsOf(res classifyScanResult, args []ast.Expr, consts map[string]string) {
	var actions, sources []string
	for _, a := range args {
		if kv, ok := a.(*ast.KeyValueExpr); ok {
			a = kv.Value
		}
		if sel, ok := a.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "egress" {
				if act, ok := classifyActionOf[sel.Sel.Name]; ok {
					actions = append(actions, act)
					continue
				}
			}
		}
		if s, ok := classifyResolve(a, consts); ok && classifySourceRE.MatchString(s) {
			k := classifyKey(s)
			sources = append(sources, k)
		}
	}
	for _, act := range actions {
		for _, s := range sources {
			res.pairs[[2]string{act, s}] = true
		}
	}
}

// classifyResolve is the string an expression spells: a literal, a named constant, the literal head of a
// concatenation, or the format of an fmt.Sprintf.
func classifyResolve(e ast.Expr, consts map[string]string) (string, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(x.Value)
		return s, err == nil
	case *ast.Ident:
		s, ok := consts[x.Name]
		return s, ok
	case *ast.SelectorExpr:
		s, ok := consts[x.Sel.Name]
		return s, ok
	case *ast.ParenExpr:
		return classifyResolve(x.X, consts)
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			return classifyResolve(x.X, consts)
		}
	case *ast.CallExpr:
		if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Sprintf" && len(x.Args) > 0 {
			return classifyResolve(x.Args[0], consts)
		}
	}
	return "", false
}

// classifyScanDoc reads the rule_source keys of the egress tables in the audit action reference.
func classifyScanDoc(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(classifyAuditDoc)
	if err != nil {
		t.Fatalf("read %s: %v", classifyAuditDoc, err)
	}
	const heading = "### `egress.*`'s `rule_source` values"
	text := string(raw)
	i := strings.Index(text, heading)
	if i < 0 {
		t.Fatalf("%s has no section %q", classifyAuditDoc, heading)
	}
	text = text[i+len(heading):]
	if j := strings.Index(text, "\n## "); j >= 0 {
		text = text[:j]
	}
	cell := regexp.MustCompile("^\\| `([^`]+)` \\|")
	out := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		m := cell.FindStringSubmatch(line)
		if m == nil || m[1] == "rule_source" {
			continue
		}
		if strings.ContainsAny(m[1], "{}*") {
			t.Fatalf("doc row %q is a brace or glob form; the census must expand it", m[1])
		}
		k := classifyKey(m[1])
		out[k] = true
	}
	if len(out) < 50 {
		t.Fatalf("read only %d doc sources from %s; the table moved", len(out), classifyAuditDoc)
	}
	return out
}

func classifySorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// classifyWriterPairs is every (action, rule_source) the writers emit, with its class. A gateway refusal
// copies the source of the allow row it replaces, so some allow sources also reach a deny row; those are the
// reviewed unclassified rows at the end.
var classifyWriterPairs = classifyJoin(
	classifyPairs("egress.allow", wallsControl,
		"brokered:mint", "brokered:approvals", "brokered:recording", "brokered:scan-result",
		"brokered:sso-token", "brokered:mcp:list-filtered"),
	classifyPairs("egress.allow", wallsConfig, "builtin:upstream-proxy"),
	classifyPairs("egress.allow", wallsToolRule, "policy:tool-allow", "policy:tool-approve-each"),
	classifyPairs("egress.allow", wallsAllowed,
		"policy:allowed", "approval:<id>", "site-config:internal-host", "site-config:egress-redirect",
		"policy:host-rule:approval-approved", "scan:mitm", "scan:opaque-tunnel", "artifact:mitm", "llm:usage",
		"brokered:git", "brokered:git:branch-ns-off", "brokered:git:forge-read", "brokered:git-pat",
		"brokered:git-pat:api", "brokered:github-api", "brokered:azure-arm", "brokered:azure-arm:tenant-discovery",
		"brokered:kube-api", "brokered:aws-sigv4", "brokered:azure", "brokered:llm", "brokered:mcp",
		"brokered:mcp:approved", "brokered:ado", "brokered:ado-git", "brokered:ado:reresolved",
		"brokered:ado-git:reresolved", "brokered:ado:upstream-not-signed-in", "brokered:ado:upstream-refused",
		"brokered:ado:upstream-no-access", "brokered:ado-git:upstream-not-signed-in",
		"brokered:ado-git:upstream-refused", "brokered:ado-git:upstream-no-access"),

	classifyPairs("egress.hold", wallsHeld,
		"approval:pending", "approval:stale", "policy:host-rule:approval-wait",
		"brokered:kube-api:approval-wait", "brokered:mcp:approval-wait"),
	classifyPairs("egress.hold", wallsRefused, "approval:expired", "approval:refused", "approval:host-cap"),

	classifyPairs("egress.deny", wallsControl,
		"brokered:mint", "brokered:approvals", "brokered:recording", "brokered:scan-result", "brokered:sso-token"),
	classifyPairs("egress.deny", wallsFidelity,
		"egress:dropped-decisions-<n>", "builtin:spool-lost", "builtin:spool-full"),
	classifyPairs("egress.deny", wallsFault,
		"builtin:dial-failed", "builtin:tunnel-failed", "builtin:resolve-failed",
		"builtin:upstream-protocol-mismatch", "policy:evaluator-error", "credential:reauth-timeout"),
	classifyPairs("egress.deny", wallsToolRule,
		"policy:tool-deny", "policy:tool-approve-each-unroutable", "policy:tool-approve-each-declined",
		"brokered:mcp:tool-deny"),
	classifyPairs("egress.deny", wallsRefused,
		"vmnet:<reason>",
		"policy:denied", "policy:default-deny", "policy:method", "policy:port", "policy:require-tls",
		"policy:no-network", "policy:ip-literal", "policy:host-rule", "policy:host-rule-needs-inspection",
		"policy:host-rule:approval-denied", "policy:host-rule:approval-closed",
		"policy:host-rule:approval-refused", "policy:host-rule:approval-busy",
		"builtin:private-ip", "builtin:tunnel-limit", "builtin:ip-spelling", "builtin:non-ascii-host",
		"builtin:signin-via-org", "builtin:gateway-vet-failed", "builtin:audit-record-too-large",
		"approval:denied", "scan:blocked", "scan:reroute-keys", "scan:mitm",
		"brokered:git", "brokered:git:branch-ns", "brokered:git:branch-ns-encoding",
		"brokered:git:push-rules", "brokered:git:push-too-large", "brokered:git:push-uninspectable",
		"brokered:git:push-entry-underivable", "brokered:git:push-deny-all", "brokered:git:push-deny-all-forge",
		"brokered:git:push-held", "brokered:git:push-held-unattended", "brokered:git:push-capability",
		"brokered:git-pat", "brokered:git-pat:denied", "brokered:git-pat:repo", "brokered:git-pat:read-only",
		"brokered:git-pat:api:denied", "brokered:ado:denied", "brokered:ado-git:denied",
		"brokered:github-api:refused", "brokered:azure-arm:refused", "brokered:kube-api:refused",
		"brokered:kube-api:approval-denied", "brokered:kube-api:approval-closed",
		"brokered:kube-api:approval-refused", "brokered:aws-sigv4:refused", "brokered:azure:denied",
		"brokered:local-model", "brokered:llm", "brokered:allowed-models",
		"brokered:mcp:unbound", "brokered:mcp:not-enabled", "brokered:mcp:encoding", "brokered:mcp:request",
		"brokered:mcp:credential", "brokered:mcp:redirect", "brokered:mcp:direct",
		"brokered:mcp:approval-denied", "brokered:mcp:approval-closed", "brokered:mcp:approval-refused",
		"brokered:mcp:approval-sent", "brokered:mcp:list-unreadable"),
	classifyUnclassifiedDenies(),
)

// classifyUnclassifiedDenies are the deny rows no rule names. Each is a gateway-route refusal: the refusal row
// copies the rule_source of the allow row the request would have had, so the source says nothing about why it
// was refused. They stay grey until a rule names them, and each is reviewed here so a new one is not silent.
func classifyUnclassifiedDenies() []classifyPair {
	const copied = "a gateway-route refusal copies the allow row's source, so the source does not say why"
	var out []classifyPair
	for _, s := range []string{
		"policy:allowed", "approval:<id>", "site-config:internal-host", "site-config:egress-redirect",
		"policy:host-rule:approval-approved", "artifact:mitm", "brokered:ado", "brokered:azure", "brokered:mcp",
	} {
		out = append(out, classifyPair{action: "egress.deny", source: s, want: wallsUnclassified, reviewed: copied})
	}
	return out
}

func classifyHasPair(action, key string) bool {
	for _, p := range classifyWriterPairs {
		if p.action == action && classifyKey(p.source) == key {
			return true
		}
	}
	return false
}

func classifyHasSource(key string) bool {
	for _, p := range classifyWriterPairs {
		if classifyKey(p.source) == key {
			return true
		}
	}
	return false
}

func TestWallsClassifyCensus(t *testing.T) {
	scan := classifyScanWriters(t)
	doc := classifyScanDoc(t)

	for _, p := range classifyWriterPairs {
		got := wallsClassify(classifyRow(p.action, p.source, "")).Class
		if got != p.want {
			t.Errorf("%s %s is %q, want %q", p.action, p.source, got, p.want)
		}
		if p.want == wallsUnclassified && p.reviewed == "" {
			t.Errorf("%s %s is unclassified without a review note", p.action, p.source)
		}
		key := classifyKey(p.source)
		if !scan.sources[key] && !doc[key] {
			t.Errorf("%s %s is pinned but no writer or doc row names %q any more", p.action, p.source, key)
		}
		if p.want == wallsUnclassified && !scan.sources[key] {
			t.Errorf("%s %s is on the reviewed unclassified list but no writer spells %q any more", p.action, p.source, key)
		}
	}
	for _, key := range classifySorted(scan.sources) {
		if !classifyHasSource(key) {
			t.Errorf("a writer spells rule_source %q and no (action, source) pair is pinned for it", key)
		}
	}
	for _, key := range classifySorted(doc) {
		if !classifyHasSource(key) {
			t.Errorf("the audit doc lists rule_source %q and no (action, source) pair is pinned for it", key)
		}
	}
	var unpinned []string
	for pair := range scan.pairs {
		if !classifyHasPair(pair[0], pair[1]) {
			unpinned = append(unpinned, pair[0]+" "+pair[1])
		}
	}
	sort.Strings(unpinned)
	for _, u := range unpinned {
		t.Errorf("a writer emits %s and it is not pinned", u)
	}
	if len(scan.sources) < 100 {
		t.Errorf("scanned only %d rule_source keys; the writers moved", len(scan.sources))
	}
}

func TestWallsClassifyHeldRun(t *testing.T) {
	counts := map[wallsClass]int{}
	actions := []string{"egress.allow", "egress.hold", "egress.pending", "egress.deny"}
	for i := 0; i < 100; i++ {
		v := wallsClassify(classifyRow(actions[i%len(actions)], "brokered:approvals", ""))
		counts[v.Class]++
		if !v.controlPlaneHost() {
			t.Fatalf("row %d: a brokered:approvals row is not a control-plane host row", i)
		}
	}
	if counts[wallsControl] != 100 || counts[wallsHeld] != 0 || len(counts) != 1 {
		t.Fatalf("100 brokered:approvals rows on a held run: classes %v, want 100 control and no held", counts)
	}
}

func TestWallsClassifyNoRedOutsideRefused(t *testing.T) {
	refusalDeny := map[string]bool{}
	holdRefused := map[string]bool{}
	sources := map[string]bool{"": true, "policy:not-written": true, "vmnet:udp": true}
	for _, p := range classifyWriterPairs {
		key := classifyKey(p.source)
		sources[classifyConcrete(p.source)] = true
		switch {
		case p.action == "egress.deny" && p.want == wallsRefused:
			refusalDeny[classifyConcrete(p.source)] = true
		case p.action == "egress.hold" && p.want == wallsRefused:
			holdRefused[key] = true
		}
	}
	for _, action := range []string{"egress.allow", "egress.deny", "egress.hold", "egress.pending", "egress.other", ""} {
		for _, by := range []string{"", "wardyn", "outside", "unknown"} {
			for _, flow := range []string{"", "flow-1"} {
				for src := range sources {
					row := classifyRow(action, src, by)
					row.Flow = flow
					v := wallsClassify(row)
					viaHold := (action == "egress.hold" || action == "egress.pending") && holdRefused[src]
					viaDeny := action == "egress.deny" && by != "outside" && by != "unknown" &&
						(refusalDeny[src] || flow != "" || strings.HasPrefix(src, "vmnet:"))
					// A control source is control whatever the action, so it never gets here.
					if wallsClassifyIn(wallsClassifyControl, src) {
						if v.Class != wallsControl {
							t.Errorf("%q %q by=%q flow=%q is %q, want control", action, src, by, flow, v.Class)
						}
						continue
					}
					if v.Class == wallsRefused && !viaHold && !viaDeny {
						t.Errorf("%q %q by=%q flow=%q is refused with no refusal rule behind it", action, src, by, flow)
					}
					if v.Class != wallsRefused && (viaHold || viaDeny) && !classifyBeatsRefusal(action, src, by) {
						t.Errorf("%q %q by=%q flow=%q is %q, want refused", action, src, by, flow, v.Class)
					}
				}
			}
		}
	}
}

// classifyBeatsRefusal says whether a rule listed before the refusal rules owns the row: a deny the
// fidelity, fault or tool rules name.
func classifyBeatsRefusal(action, src, by string) bool {
	if action != "egress.deny" {
		return false
	}
	switch {
	case strings.HasPrefix(src, wallsClassifyDroppedPrefix), wallsClassifyIn(wallsClassifyFidelityDeny, src),
		wallsClassifyIn(wallsClassifyFault, src), wallsClassifyIn(wallsClassifyToolDeny, src):
		return true
	}
	return by == "outside" || by == "unknown"
}

func TestWallsClassifyDenyByAndGuest(t *testing.T) {
	// A refusal source whose destination or an unknown party refused is a fault.
	for _, by := range []string{"outside", "unknown"} {
		if got := wallsClassify(classifyRow("egress.deny", "policy:denied", by)).Class; got != wallsFault {
			t.Errorf("policy:denied by %s is %q, want fault", by, got)
		}
		if got := wallsClassify(classifyRow("egress.deny", "policy:denied", by)); got.Guest {
			t.Errorf("policy:denied by %s is flagged guest", by)
		}
	}
	for _, by := range []string{"", "wardyn"} {
		if got := wallsClassify(classifyRow("egress.deny", "policy:denied", by)).Class; got != wallsRefused {
			t.Errorf("policy:denied by %q is %q, want refused", by, got)
		}
	}
	// A deny the relay of a desktop VM made is a refusal flagged guest, by flow or by source.
	flow := classifyRow("egress.deny", "policy:default-deny", "")
	flow.Flow = "flow-1"
	if v := wallsClassify(flow); v.Class != wallsRefused || !v.Guest {
		t.Errorf("a deny with a flow id is %+v, want refused and guest", v)
	}
	if v := wallsClassify(classifyRow("egress.deny", "vmnet:<reason>", "")); v.Class != wallsRefused || !v.Guest {
		t.Errorf("a vmnet deny is %+v, want refused and guest", v)
	}
	if v := wallsClassify(classifyRow("egress.deny", "policy:default-deny", "")); v.Guest {
		t.Errorf("a deny with no flow id is flagged guest")
	}
	// A VM flow that failed to dial is still a fault, ahead of the guest rule.
	dial := classifyRow("egress.deny", "builtin:dial-failed", "")
	dial.Flow = "flow-1"
	if v := wallsClassify(dial); v.Class != wallsFault {
		t.Errorf("a VM flow's dial failure is %q, want fault", v.Class)
	}
	// An allow is never guest, and a flow id on an allow changes nothing.
	allow := classifyRow("egress.allow", "policy:allowed", "")
	allow.Flow = "flow-1"
	if v := wallsClassify(allow); v.Class != wallsAllowed || v.Guest {
		t.Errorf("an allow with a flow id is %+v, want allowed and not guest", v)
	}
	// A deny no rule names is grey, never refused.
	if got := wallsClassify(classifyRow("egress.deny", "policy:not-written", "")).Class; got != wallsUnclassified {
		t.Errorf("an unnamed deny is %q, want unclassified", got)
	}
	if got := wallsClassify(classifyRow("egress.other", "policy:denied", "")).Class; got != wallsUnclassified {
		t.Errorf("an unknown action is %q, want unclassified", got)
	}
}

func TestWallsClassifyAllowHoldAndLate(t *testing.T) {
	// An allow the destination answered no stays allowed: the grey tip is drawn by the view.
	fault := classifyRow("egress.allow", "policy:allowed", "")
	fault.UpstreamFault = "AccessDeniedException"
	if got := wallsClassify(fault).Class; got != wallsAllowed {
		t.Errorf("an allow with an upstream fault is %q, want allowed", got)
	}
	// The legacy action name reads as a hold.
	for _, action := range []string{"egress.hold", "egress.pending"} {
		if got := wallsClassify(classifyRow(action, "approval:pending", "")).Class; got != wallsHeld {
			t.Errorf("%s approval:pending is %q, want held", action, got)
		}
		if got := wallsClassify(classifyRow(action, "approval:expired", "")).Class; got != wallsRefused {
			t.Errorf("%s approval:expired is %q, want refused", action, got)
		}
	}
	// A hold written by a wait source is held, and a source no rule names is held too.
	if got := wallsClassify(classifyRow("egress.hold", "policy:not-written", "")).Class; got != wallsHeld {
		t.Errorf("a hold no rule names is %q, want held", got)
	}
	// Only the control-plane classes say the host is Wardyn's own.
	for class, want := range map[wallsClass]bool{
		wallsControl: true, wallsToolRule: true, wallsConfig: false, wallsAllowed: false, wallsRefused: false,
		wallsHeld: false, wallsFidelity: false, wallsFault: false, wallsUnclassified: false,
	} {
		if got := (wallsVerdict{Class: class}).controlPlaneHost(); got != want {
			t.Errorf("controlPlaneHost of %q is %v, want %v", class, got, want)
		}
	}
	// Late rides along with the class and never changes it.
	late := classifyRow("egress.deny", "policy:denied", "")
	late.RecordedAt = classifyNow.Add(time.Minute)
	if v := wallsClassify(late); v.Class != wallsRefused || !v.Late {
		t.Errorf("a row recorded a minute late is %+v, want refused and late", v)
	}
	if v := wallsClassify(classifyRow("egress.deny", "policy:denied", "")); v.Late {
		t.Errorf("a row recorded at once is flagged late")
	}
}
