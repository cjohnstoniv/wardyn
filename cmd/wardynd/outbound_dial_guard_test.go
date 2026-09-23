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
// KNOWN ON-SEGMENT address (Proxy: nil, B11a-F13) or deliberately rides the
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
	kind    string // "net.Dial*", "tls.Dial*", "http.Transport", "http.Client"
	line    int
}

// classifyDialSite reports the kind of an AST node the checklist cares about,
// or "" for anything else. Only a CALL to net.Dial*/tls.Dial* and a COMPOSITE
// LITERAL of http.Transport/http.Client count — exactly the four shapes the
// spec names, no more (a `&net.Dialer{}` that only ever feeds one of these two
// literals is not itself one of them, and is not double-counted here).
func classifyDialSite(n ast.Node) string {
	switch v := n.(type) {
	case *ast.CallExpr:
		sel, ok := v.Fun.(*ast.SelectorExpr)
		if !ok {
			return ""
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return ""
		}
		if (id.Name == "net" || id.Name == "tls") && strings.HasPrefix(sel.Sel.Name, "Dial") {
			return id.Name + ".Dial*"
		}
	case *ast.CompositeLit:
		sel, ok := v.Type.(*ast.SelectorExpr)
		if !ok {
			return ""
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok || id.Name != "http" {
			return ""
		}
		if sel.Sel.Name == "Transport" || sel.Sel.Name == "Client" {
			return "http." + sel.Sel.Name
		}
	}
	return ""
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
			ast.Inspect(f, func(n ast.Node) bool {
				if kind := classifyDialSite(n); kind != "" {
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
		kinds:   kindSet("http.Transport", "http.Client"),
		why: "the ONE shared forward-egress transport factory (mkTransport, building both " +
			"p.transport and p.controlTransport) and the control-plane client (p.localClient, " +
			"riding p.controlTransport). egressDial vs directDial — which one a request's " +
			"DialContext resolves to — is a CLOSURE choice made here, not a second transport " +
			"literal elsewhere; that is what makes this the shared seam every forward-egress " +
			"caller (evaluate, serveMITMRequest, the git/PAT brokers) already routes through.",
	},
	{
		relFile: "internal/egress/proxy/approvals.go",
		kinds:   kindSet("http.Client"),
		why: "fallback control-plane client (approval polling), constructed ONLY when the " +
			"caller passes a nil *http.Client. Production (server.go's NewServer) always supplies " +
			"the shared control-plane client with Proxy stripped, so this literal never carries " +
			"live traffic — it exists for tests and any future caller that forgets the argument.",
	},
	{
		relFile: "internal/egress/proxy/inject.go",
		kinds:   kindSet("http.Client"),
		why:     "same nil-fallback control-plane client shape as approvals.go, for credential/injection resolve and the approvals reader.",
	},
	{
		relFile: "internal/egress/proxy/decisions.go",
		kinds:   kindSet("http.Client"),
		why:     "same nil-fallback control-plane client shape as approvals.go, for the decision-log POST to the control plane.",
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
