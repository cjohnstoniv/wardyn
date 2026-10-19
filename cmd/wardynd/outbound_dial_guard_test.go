// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// The operator-reported MITM/upstream-proxy defect (0.7.8) was the THIRD time a
// new forward-egress code path failed to inherit SiteConfig.UpstreamProxyURL:
// the git broker and the brokered LLM route each once called their own
// resolve+dial instead of routing through egressTarget/dialThroughUpstream,
// and the MITM lane went unpinned for a third instance of the same shape. Each was found by reading the code, never by a
// test — there was no checklist item asking "does this path honour
// upstream_proxy_url?" of a NEW net.Dial/tls.Dial/http.Transport/http.Client.
//
// This guard is that checklist item, made mechanical: it parses every non-test
// .go file in the packages that actually implement the corp-upstream hop
// (internal/egress/proxy, the sidecar it ships in) for the four shapes a hand-
// rolled dial takes, and fails on anything outside the SANCTIONED homes below
// — each of which states, in one line, why it is exempt. cmd/wardynd is
// scanned too even though it has none today: a `&http.Client{}` literal added
// to this package in the future would be exactly the same anti-pattern,
// bypassing the shared, boot-patched http.DefaultTransport that
// installDaemonProxy/installTrustedCA already wire (WARDYN_DAEMON_PROXY_URL,
// WARDYN_TRUSTED_CA_FILE) — see daemon_proxy.go.
//
// SCOPE, stated rather than silently narrow: wardynd's own outbound calls
// (OIDC discovery/JWKS, the GitHub App client, Entra sync, the audit webhook
// sink, the AWS SSO token refresh — internal/auth/oidc, internal/broker,
// internal/directory, internal/audit/sinks, internal/api/awssso_refresh.go)
// are a SIBLING concern governed by a DIFFERENT knob (WARDYN_DAEMON_PROXY_URL,
// mutating the shared http.DefaultTransport in place — see
// cmd/wardynd/daemon_proxy.go's own doc). "Does this path honour
// upstream_proxy_url" is not even a coherent question for them, and they
// already carry their own doc-comment justification. Folding them into this
// allowlist would answer a question this guard does not ask. Same reasoning
// keeps in-sandbox helper binaries (wardyn-toolgate, wardyn-git-helper,
// wardyn-aws-sso, the UI reverse-proxy gateway) out of scope: each reaches a
// KNOWN ON-SEGMENT address (Proxy: nil) or deliberately rides the
// SANDBOX's own HTTP_PROXY (which is wardyn-proxy, already governed here).
//
// Test helpers need no entry: _test.go files are never scanned at all.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dialScanDirs are the packages that implement the corp-upstream hop itself.
var dialScanDirs = []string{
	"internal/egress/proxy",
	"cmd/wardyn-proxy",
	"cmd/wardynd",
}

// dialSite is one occurrence of a hand-rolled dial/transport/client shape.
type dialSite struct {
	relFile string // repo-relative, forward-slashed
	kind    string // "net.Dial*", "tls.Dial*", "tls.Dialer", "net.Dialer.Dial*", "tls.Dialer.Dial*", "http.Transport", "http.Client"
	line    int
}

// dialerTypeOf names the dialer type of a type or value expression: "net" for net.Dialer, "tls"
// for tls.Dialer (through any & or *), "" for anything else.
func dialerTypeOf(e ast.Expr) string {
	for {
		switch v := e.(type) {
		case *ast.StarExpr:
			e = v.X
			continue
		case *ast.UnaryExpr:
			e = v.X
			continue
		case *ast.ParenExpr:
			e = v.X
			continue
		case *ast.CompositeLit:
			e = v.Type
			continue
		case *ast.SelectorExpr:
			if id, ok := v.X.(*ast.Ident); ok && v.Sel.Name == "Dialer" && (id.Name == "net" || id.Name == "tls") {
				return id.Name
			}
		}
		return ""
	}
}

// declaredDialers maps every name a file declares as a net.Dialer or tls.Dialer to the set of packages
// it is declared with: `var d net.Dialer` (the zero value), `var d = &tls.Dialer{}`, `d := net.Dialer{}`,
// a parameter or a struct field typed *net.Dialer. It is by NAME and file-wide, not by scope: a
// type-checked scan would load every package. A name declared as both kinds keeps both, and a name
// reused for something else only makes the guard stricter.
func declaredDialers(f *ast.File) map[string]map[string]bool {
	m := map[string]map[string]bool{}
	note := func(name, kind string) {
		if kind == "" {
			return
		}
		if m[name] == nil {
			m[name] = map[string]bool{}
		}
		m[name][kind] = true
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.ValueSpec:
			for i, id := range v.Names {
				if v.Type != nil {
					note(id.Name, dialerTypeOf(v.Type))
				} else if i < len(v.Values) {
					note(id.Name, dialerTypeOf(v.Values[i]))
				}
			}
		case *ast.AssignStmt:
			for i, lhs := range v.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && i < len(v.Rhs) {
					note(id.Name, dialerTypeOf(v.Rhs[i]))
				}
			}
		case *ast.Field:
			for _, id := range v.Names {
				note(id.Name, dialerTypeOf(v.Type))
			}
		}
		return true
	})
	return m
}

// dialMethodKinds is the "<pkg>.Dialer.Dial*" site kind for each package a name was declared with.
func dialMethodKinds(declared map[string]bool) []string {
	var kinds []string
	for _, pkg := range []string{"net", "tls"} {
		if declared[pkg] {
			kinds = append(kinds, pkg+".Dialer.Dial*")
		}
	}
	return kinds
}

// classifyDialSite reports the kinds of an AST node the checklist cares about,
// or none for anything else: a CALL to net.Dial*/tls.Dial*, a COMPOSITE LITERAL
// of http.Transport/http.Client/tls.Dialer, and a Dial* method (called or taken
// as a value) on a net.Dialer or tls.Dialer, whether that is a literal or a
// name the file declared with that type (dialers). A name declared as both
// dialer types yields one site per kind. A `&net.Dialer{}` that is never
// dialled through is not itself one of them.
func classifyDialSite(n ast.Node, dialers map[string]map[string]bool) []string {
	switch v := n.(type) {
	case *ast.SelectorExpr:
		if !strings.HasPrefix(v.Sel.Name, "Dial") {
			return nil
		}
		switch x := v.X.(type) {
		case *ast.Ident:
			return dialMethodKinds(dialers[x.Name])
		case *ast.SelectorExpr: // s.dialer.DialContext
			return dialMethodKinds(dialers[x.Sel.Name])
		default: // net.Dialer{}.DialContext, (&tls.Dialer{}).DialContext
			if kind := dialerTypeOf(v.X); kind != "" {
				return []string{kind + ".Dialer.Dial*"}
			}
		}
	case *ast.CallExpr:
		sel, ok := v.Fun.(*ast.SelectorExpr)
		if !ok {
			return nil
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return nil
		}
		if (id.Name == "net" || id.Name == "tls") && strings.HasPrefix(sel.Sel.Name, "Dial") {
			return []string{id.Name + ".Dial*"}
		}
	case *ast.CompositeLit:
		sel, ok := v.Type.(*ast.SelectorExpr)
		if !ok {
			return nil
		}
		id, ok := sel.X.(*ast.Ident)
		if ok && id.Name == "tls" && sel.Sel.Name == "Dialer" {
			return []string{"tls.Dialer"}
		}
		if !ok || id.Name != "http" {
			return nil
		}
		if sel.Sel.Name == "Transport" || sel.Sel.Name == "Client" {
			return []string{"http." + sel.Sel.Name}
		}
	}
	return nil
}

// scanDialSites parses every non-test .go file directly under each of dirs
// (repo-relative to root) and returns every classifyDialSite match, plus the
// number of files it actually parsed — the guard-the-guard count.
func scanDialSites(t *testing.T, root string, dirs []string) (sites []dialSite, filesScanned int) {
	t.Helper()
	for _, dir := range dirs {
		full := filepath.Join(root, filepath.FromSlash(dir))
		entries, err := os.ReadDir(full)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(full, name)
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				t.Fatalf("parse %s: %v", path, perr)
			}
			filesScanned++
			relFile := dir + "/" + name
			dialers := declaredDialers(f)
			ast.Inspect(f, func(n ast.Node) bool {
				for _, kind := range classifyDialSite(n, dialers) {
					sites = append(sites, dialSite{relFile: relFile, kind: kind, line: fset.Position(n.Pos()).Line})
				}
				return true
			})
		}
	}
	return sites, filesScanned
}

// dialAllowance is one sanctioned home: a file, the kinds of dial-shape it may
// carry, and WHY — see the package doc above for the two exemption families
// (the control-plane transport that must never ride the corp proxy; the
// substrate canary probe that is never egress at all).
type dialAllowance struct {
	relFile string
	kinds   map[string]bool
	why     string
}

func kindSet(kinds ...string) map[string]bool {
	m := make(map[string]bool, len(kinds))
	for _, k := range kinds {
		m[k] = true
	}
	return m
}

// sanctionedDialHomes is the allowlist. Every entry is EXACTLY what is there
// today (grep -rnE 'net\.Dial|tls\.Dial|&http\.Transport\{|&http\.Client\{'
// over internal/egress/proxy and cmd/wardyn-proxy, non-test files) — nothing
// added for headroom, per AGENTS.md ("no scaffolding for later").
var sanctionedDialHomes = []dialAllowance{
	{
		relFile: "internal/egress/proxy/proxy.go",
		kinds:   kindSet("http.Transport", "http.Client", "net.Dialer.Dial*"),
		why: "the ONE shared forward-egress transport factory (mkTransport, building both " +
			"p.transport and p.controlTransport) and the control-plane client (p.localClient, " +
			"riding p.controlTransport). egressDial vs directDial — which one a request's " +
			"DialContext resolves to — is a CLOSURE choice made here, not a second transport " +
			"literal elsewhere; that is what makes this the shared seam every forward-egress " +
			"caller (evaluate, serveMITMRequest, the git/PAT brokers) already routes through. The " +
			"net.Dialer method value is newProxy's default Options.Dial, the direct dial the closure " +
			"choice above ends in.",
	},
	{
		relFile: "internal/egress/proxy/server.go",
		kinds:   kindSet("http.Client"),
		why: "NewServer builds the control-plane client when its caller passes nil, so no callee " +
			"can fall back to a client on http.DefaultTransport; the client then rides the pinned " +
			"transport NewServer owns (internal CA only, Proxy stripped) like any caller-supplied one.",
	},
	{
		relFile: "internal/egress/proxy/remote_sink.go",
		kinds:   kindSet("tls.Dialer", "tls.Dialer.Dial*"),
		why: "the remote decision sink's dial to its configured, SPKI-pinned sink address: a " +
			"control-plane stream to a fixed endpoint, never a sandbox's forward egress.",
	},
	{
		relFile: "cmd/wardyn-proxy/main.go",
		kinds:   kindSet("net.Dial*", "http.Client"),
		why: "net.DialTimeout is the k8s substrate's -egress-canary probe: a throwaway pod TCP-dials " +
			"the in-cluster apiserver ONLY (never external egress, never a sandbox's traffic) to prove " +
			"NetworkPolicy is enforced. The http.Client is the control-plane API client handed to " +
			"proxy.NewServer (injection resolve, decisions, approvals) — never the egress path, which " +
			"is proxy.go's own transport above.",
	},
}

func allowedDialSite(s dialSite) (bool, string) {
	for _, a := range sanctionedDialHomes {
		if a.relFile == s.relFile && a.kinds[s.kind] {
			return true, a.why
		}
	}
	return false, ""
}

// TestOutboundDialsStayInSanctionedHomes is the checklist item the operator
// asked for, run mechanically instead of trusted to a reviewer's memory: any
// net.Dial*/tls.Dial*/&http.Transport{}/&http.Client{} in the packages that
// implement the corp-upstream hop must be one of the entries above, or it is
// a new outbound path nobody has yet asked "does this honour
// SiteConfig.upstream_proxy_url?" about.
func TestOutboundDialsStayInSanctionedHomes(t *testing.T) {
	root := repoRoot(t)

	sites, scanned := scanDialSites(t, root, dialScanDirs)
	// Guard the guard: a wrong root, a moved directory, or an emptied package
	// would otherwise scan nothing and pass vacuously.
	if scanned == 0 {
		t.Fatal("scanned 0 files across dialScanDirs — the package list or module root is wrong")
	}
	if len(sites) == 0 {
		t.Fatal("scanned files but matched 0 dial/transport/client sites — classifyDialSite's AST " +
			"matching broke (this package has known sanctioned sites today, so zero is never correct)")
	}

	exercised := make(map[string]bool, len(sanctionedDialHomes))
	for _, s := range sites {
		ok, why := allowedDialSite(s)
		if !ok {
			t.Errorf("%s:%d constructs a %s outside the sanctioned dial homes — checklist: does this "+
				"new path honour SiteConfig.upstream_proxy_url (see internal/egress/proxy/egress_target.go's "+
				"egressTarget)? If yes, route it through Proxy.dialThroughUpstream/p.transport like every "+
				"other forward-egress caller; if it must legitimately bypass the corp proxy (a control-plane "+
				"call, a local-only address), add it to sanctionedDialHomes here with a one-line why",
				s.relFile, s.line, s.kind)
			continue
		}
		exercised[s.relFile+"|"+s.kind] = true
		t.Logf("%s:%d %s — sanctioned: %s", s.relFile, s.line, s.kind, why)
	}

	// An allowlist entry nothing matched is dead weight that would silently
	// stop meaning anything the moment the code it describes is refactored
	// away — the same rot TestThreatModelDocGuardSubjectsExist (this package's
	// sibling guard) checks for on the doc side.
	for _, a := range sanctionedDialHomes {
		for k := range a.kinds {
			if !exercised[a.relFile+"|"+k] {
				t.Errorf("sanctionedDialHomes lists %s as a permitted %s, but no such site was found — "+
					"drop the stale entry or the allowlist stops meaning what it claims", a.relFile, k)
			}
		}
	}
}

// The scan reaches a dial through a net.Dialer or tls.Dialer however it is spelled: a zero-value
// declaration, a literal, a parameter or a struct field, called or taken as a method value.
func TestClassifyDialSiteSeesEveryDialerShape(t *testing.T) {
	const src = `package p

import (
	"context"
	"crypto/tls"
	"net"
)

type holder struct{ nd *net.Dialer }

func zeroNet(ctx context.Context)           { var d net.Dialer; _, _ = d.DialContext(ctx, "tcp", "x") }
func zeroTLS(ctx context.Context)           { var d tls.Dialer; _, _ = d.DialContext(ctx, "tcp", "x") }
func literalNet(ctx context.Context)        { _, _ = (&net.Dialer{}).DialContext(ctx, "tcp", "x") }
func shortNet(ctx context.Context)          { d := &net.Dialer{}; f := d.DialContext; _ = f }
func param(ctx context.Context, d *net.Dialer) { _, _ = d.Dial("tcp", "x") }
func field(ctx context.Context, h holder)   { _, _ = h.nd.DialContext(ctx, "tcp", "x") }
func notADialer(x interface{ DialNot() }) { x.DialNot() }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	dialers := declaredDialers(f)
	got := map[string]int{}
	ast.Inspect(f, func(n ast.Node) bool {
		for _, kind := range classifyDialSite(n, dialers) {
			got[kind]++
		}
		return true
	})
	// `d` is declared as a net.Dialer (zeroNet, shortNet, param) and as a tls.Dialer (zeroTLS), so each of the
	// four `d.Dial*` uses is one site per kind; literalNet and field add one net.Dialer site each.
	if got["net.Dialer.Dial*"] != 6 || got["tls.Dialer.Dial*"] != 4 || len(got) != 2 {
		t.Errorf("sites = %v, want 6 net.Dialer.Dial* and 4 tls.Dialer.Dial* (and nothing else)", got)
	}
}
