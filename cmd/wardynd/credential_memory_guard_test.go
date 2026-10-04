// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Credential memory hygiene (credential-storage design §2.8, CS-4), pinned
// mechanically:
//
//   - Decrypt sites. A stored credential becomes plaintext in exactly the
//     places listed in decryptSites below: the envelope open, the key-service
//     unwrap, the one-time age conversion and the external store read. A new
//     call to any of those primitives anywhere else in cmd/ or internal/ is a
//     new place plaintext appears, and this guard makes it a reviewed change
//     instead of an accident. It does not pin who calls Store.Get: that is the
//     audit guard's question (every Get audited once, CS-13). The scan also
//     covers every file a shipped binary links, wherever it lives (go list).
//     Test files are not scanned, nor are the exact testSupportFiles, which
//     hold only while no shipped binary links them.
//   - Core dumps. Every binary that holds credentials calls nodump.Disable as
//     the first statement of its entry point.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// decryptSite is one call of a primitive that turns a stored credential into
// plaintext, found in a function.
type decryptSite struct {
	relFile, fn, kind string
	line              int
}

// fileScope is which decrypt primitives a file can reach. inKEK marks a file of
// package kek itself, whose own Open is called unqualified; the other three are
// set by importing the package that defines the method.
type fileScope struct{ inKEK, kek, aead, external bool }

func scopeOf(f *ast.File) fileScope {
	sc := fileScope{inKEK: f.Name.Name == "kek"}
	sc.kek = sc.inKEK
	for _, im := range f.Imports {
		switch strings.Trim(im.Path.Value, `"`) {
		case "crypto/cipher":
			sc.aead = true
		case "github.com/cjohnstoniv/wardyn/internal/secretstore/kek":
			sc.kek = true
		case "github.com/cjohnstoniv/wardyn/internal/secretstore":
			sc.external = true
		}
	}
	return sc
}

// classifyDecryptCall names the decrypt primitive call is, or "". Unwrap, Open
// and Get are matched by name and arity alone, so each counts only in a file
// that imports the package defining it.
func classifyDecryptCall(call *ast.CallExpr, sc fileScope) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		if sc.inKEK && fun.Name == "Open" {
			return "kek.Open"
		}
	case *ast.SelectorExpr:
		x, _ := fun.X.(*ast.Ident)
		switch {
		case x != nil && x.Name == "kek" && fun.Sel.Name == "Open":
			return "kek.Open"
		case x != nil && x.Name == "age" && fun.Sel.Name == "Decrypt":
			return "age.Decrypt"
		case sc.kek && fun.Sel.Name == "Unwrap" && len(call.Args) == 3:
			return "KEK.Unwrap" // kek.KEK: Unwrap(ctx, wrapped, bind)
		case sc.aead && fun.Sel.Name == "Open" && len(call.Args) == 4:
			return "AEAD.Open" // cipher.AEAD: Open(dst, nonce, ciphertext, aad)
		case sc.external && fun.Sel.Name == "Get" && len(call.Args) == 4:
			return "External.Get" // secretstore.External: Get(ctx, owner, name, ref)
		}
	}
	return ""
}

// scanDecryptSites parses every non-test .go file under cmd/ and internal/,
// and each of extra (paths relative to root), except the testSupportFiles.
func scanDecryptSites(t *testing.T, root string, extra map[string]bool) (sites []decryptSite, scanned int) {
	t.Helper()
	files := map[string]bool{}
	maps.Copy(files, extra)
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == "testdata" {
				return filepath.SkipDir
			}
			if !d.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
				rel, _ := filepath.Rel(root, path)
				files[filepath.ToSlash(rel)] = true
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scan %s: %v", top, err)
		}
	}
	for _, rel := range slices.Sorted(maps.Keys(files)) {
		if _, ok := testSupportFiles[rel]; ok {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filepath.Join(root, filepath.FromSlash(rel)), nil, 0)
		if err != nil {
			t.Fatalf("scan %s: %v", rel, err)
		}
		scanned++
		sc := scopeOf(f)
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			name := fd.Name.Name
			if fd.Recv != nil && len(fd.Recv.List) == 1 {
				name = recvTypeName(fd.Recv.List[0].Type) + "." + name
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if kind := classifyDecryptCall(call, sc); kind != "" {
						sites = append(sites, decryptSite{relFile: rel, fn: name, kind: kind, line: fset.Position(call.Pos()).Line})
					}
				}
				return true
			})
		}
	}
	return sites, scanned
}

// shippedFiles returns, relative to root, every non-test Go file of each module
// package a shipped binary links: go list -deps ./cmd/... for every build the
// repo ships (CGO_ENABLED=0; linux, and darwin for the CLI, each on amd64 and
// arm64; tagless, docker, k8s, docker+k8s), whatever a file's own build
// constraints. Unlike a walk, it follows an import into testdata, a . or _
// directory or a symlink, which the go tool compiles when the import names it.
func shippedFiles(t *testing.T, root string) map[string]bool {
	t.Helper()
	files := map[string]bool{}
	for _, platform := range []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"} {
		goos, goarch, _ := strings.Cut(platform, "/")
		for _, tags := range []string{"", "docker", "k8s", "docker,k8s"} {
			cmd := exec.Command("go", "list", "-deps", "-buildvcs=false", "-tags="+tags, "-f",
				`{{.ImportPath}}{{range .GoFiles}}{{"\t"}}{{$.Dir}}/{{.}}{{end}}{{range .IgnoredGoFiles}}{{"\t"}}{{$.Dir}}/{{.}}{{end}}`, "./cmd/...")
			var stderr strings.Builder
			cmd.Dir, cmd.Env, cmd.Stderr = root, append(os.Environ(), "PWD="+root, "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0"), &stderr
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("go list -deps -tags=%q ./cmd/... (%s): %v\n%s", tags, platform, err, stderr.String())
			}
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				f := strings.Split(line, "\t")
				if !strings.HasPrefix(f[0], guardModPath+"/") {
					continue
				}
				for _, p := range f[1:] {
					if !strings.HasSuffix(p, "_test.go") {
						rel, _ := filepath.Rel(root, p)
						files[filepath.ToSlash(rel)] = true
					}
				}
			}
		}
	}
	return files
}

func recvTypeName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.StarExpr:
		return recvTypeName(v.X)
	case *ast.IndexExpr:
		return recvTypeName(v.X)
	case *ast.Ident:
		return v.Name
	}
	return "?"
}

// decryptSites is every place a stored credential becomes plaintext, keyed
// "file|function|primitive", with why it may. Exactly what is there today;
// nothing added for headroom.
var decryptSites = map[string]string{
	"internal/secretstore/kek/kek.go|Open|AEAD.Open":                               "the envelope's AES-256-GCM open, the one definition every local-mode read goes through",
	"internal/secretstore/kek/kek.go|Local.Unwrap|kek.Open":                        "the local KEK unwraps a row's data key",
	"internal/secretstore/pg/pg.go|Store.open|KEK.Unwrap":                          "a local-mode Get unwraps the row's data key, bound to the row's owner and name",
	"internal/secretstore/pg/pg.go|Store.open|kek.Open":                            "a local-mode Get opens the value, bound to the row's owner and name",
	"internal/secretstore/pg/pg.go|rewrap|KEK.Unwrap":                              "-rotate-age-key rewraps a data key; the value itself is never opened",
	"internal/secretstore/pg/principal_rows.go|Store.openPrincipal|kek.Open":       "a Get of a row under its owner's principal key (enc_version 3) opens the data key, then the value, each bound to the row",
	"internal/secretstore/pg/principal_rows.go|Store.sealV1ToPrincipal|KEK.Unwrap": "-rewrap-principal-keys unwraps a v1 data key to wrap it under the owner's principal key; the value itself is never opened",
	"internal/secretstore/subjectkey/subjectkey.go|Manager.Reseal|kek.Open":        "-rewrap-principal-keys opens a data key under an owner's earlier key generation to seal it under the current one; the value the data key seals is never opened",
	"internal/secretstore/pg/convert.go|ageDecrypt|age.Decrypt":                    "the one-time conversion of a pre-envelope (age) row to envelope v1",
	"internal/secretstore/pg/external.go|Store.openExternal|External.Get":          "a store-mode Get reads the value from the organisation's store, after the pointer row is checked",
	"internal/secretstore/vaultkv/transit.go|Transit.selfTest|KEK.Unwrap":          "the Transit boot self-test unwraps a random probe data key it just wrapped, never a stored one",
	"internal/secretstore/azurekv/kek.go|KEK.selfTest|KEK.Unwrap":                  "the Key Vault KEK boot self-test unwraps a random probe data key it just wrapped, never a stored one",
	"internal/secretstore/subjectkey/subjectkey.go|Manager.fill|KEK.Unwrap":        "a use opens a per-subject key, bound to its owner, purpose, version and domain; it is the key that seals a person's rows, never a stored credential value",
	"internal/secretstore/subjectkey/rewrap.go|Rewrap|KEK.Unwrap":                  "-rewrap and -rotate-age-key move a per-subject key's wrap onto the current KEK; no sealed value is opened",
	"internal/api/run_proxy_config.go|Server.loadRunProxyConfig|kek.Open":          "a revive or an extend opens its run's stored proxy config (#1176), bound to the run, to rebuild the proxy it hands the config to",
	"internal/audit/seal_actor.go|Sealer.resealActor|kek.Open":                     "the spool drain opens a pending human actor under the platform pending key, to store it as the person's subject id before the store sees the row; a person's own name from one audit row, never a stored credential",
	"internal/audit/seal.go|Sealer.unsealData|kek.Open":                            "a read opens a sealed audit field under its subject's key, bound to the event, action, path, subject and key version; the value is a person's own text from one audit row, never a stored credential",
	"internal/audit/seal.go|Sealer.Reseal|kek.Open":                                "the spool drain opens a pending audit field under the platform pending key, to seal it under its subject's own key before the store sees the row; never a stored credential",
	"internal/adorunpat/adorunpat.go|Store.Load|kek.Open":                          "a load opens a minted_pat run's current Azure DevOps token, sealed under the run owner's per-subject key and bound to the run, the rendering and the key version, so a resolve served by any replica hands out the token another created; the token is one the run already holds and is registered for masking, never read through Store.Get",
	"internal/maskmanifest/maskmanifest.go|Manifests.load|kek.Open":                "a load opens a run's sealed masking-manifest values, bound to the run, the value's ordinal and the key version, under the run owner's per-subject key, to put them in the masking registry; each is a rendering the run already holds, never read through Store.Get",
	"internal/maskstore/sync.go|Store.open|kek.Open":                               "a read of the shared masking registry opens a value another replica committed, bound to its bucket, row, owner, run or credential name and the key version, under the owner's per-subject key, to put it in this process's masking registry; the value is one a run or a sign-in already holds, never read through Store.Get",
}

// testSupportFiles are the files the scan skips: test support that calls a
// decrypt primitive only on data keys it wrapped itself from random bytes.
// Each entry is one exact file, never a package or a directory, and it holds
// only while no shipped binary links it (shippedFiles).
var testSupportFiles = map[string]string{
	"internal/secretstore/kek/kektest/kektest.go": "the key-encryption-key conformance suite (credential-storage design §2.2, §2.3)",
}

// testSupportProblems returns why a testSupportFiles entry no longer holds: its
// file is gone, or a shipped binary links it.
func testSupportProblems(root string, shipped map[string]bool) []string {
	var problems []string
	for rel := range testSupportFiles {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			problems = append(problems, fmt.Sprintf("testSupportFiles exempts %s, which does not exist — drop the stale entry", rel))
		}
		if shipped[rel] {
			problems = append(problems, fmt.Sprintf("%s is linked into a shipped binary (go list -deps ./cmd/...), and the "+
				"decrypt-site scan skips it as test support: import %s from _test.go files only, or pin its calls in decryptSites",
				rel, path.Dir(rel)))
		}
	}
	sort.Strings(problems)
	return problems
}

func TestDecryptSitesArePinned(t *testing.T) {
	root := repoRoot(t)
	shipped := shippedFiles(t, root)
	for rel := range credentialEntryPoints {
		if !shipped[rel] {
			t.Fatalf("go list -deps ./cmd/... did not list %s: the shipped-file set is broken", rel)
		}
	}
	for _, p := range testSupportProblems(root, shipped) {
		t.Error(p)
	}
	sites, scanned := scanDecryptSites(t, root, shipped)
	if scanned == 0 || len(sites) == 0 {
		t.Fatalf("scanned %d files and matched %d decrypt sites — the root or the matcher is broken", scanned, len(sites))
	}
	seen := map[string]bool{}
	for _, s := range sites {
		key := s.relFile + "|" + s.fn + "|" + s.kind
		seen[key] = true
		if _, ok := decryptSites[key]; !ok {
			t.Errorf("%s:%d: %s calls %s, which turns a stored credential into plaintext, outside the pinned "+
				"decrypt sites. Read it through secretstore.Store.Get instead, or add it to decryptSites with why "+
				"plaintext has to appear there", s.relFile, s.line, s.fn, s.kind)
		}
	}
	var stale []string
	for key := range decryptSites {
		if !seen[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	for _, key := range stale {
		t.Errorf("decryptSites pins %s, but no such call exists — drop the stale entry", key)
	}
}

// The test-support exemption is the exact file alone, and it fails once a
// shipped binary links the file, even through a package under testdata, which
// a walk of the tree never enters. A decrypt call in any other file linked that
// way, one of the helper's own package included, is still a site.
func TestDecryptTestSupportExemptionIsExact(t *testing.T) {
	root := t.TempDir()
	const (
		kekImport = `"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"`
		helper    = `"github.com/cjohnstoniv/wardyn/internal/secretstore/kek/kektest"`
		unwrap    = "func f(k kek.KEK) { k.Unwrap(nil, nil, nil) }\n"
	)
	for rel, src := range map[string]string{
		"go.mod":                          "module github.com/cjohnstoniv/wardyn\n\ngo 1.21\n",
		"internal/secretstore/kek/kek.go": "package kek\ntype KEK interface{ Unwrap(ctx, w, b any) ([]byte, error) }\n",
		"internal/secretstore/kek/kektest/kektest.go": "package kektest\nimport " + kekImport + "\n" + unwrap,
		"internal/secretstore/kek/kektest/more.go":    "package kektest\nimport " + kekImport + "\n" + unwrap,
		"internal/secretstore/pg/testdata/zz/zz.go":   "package zz\nimport (\n" + kekImport + "\n_ " + helper + "\n)\n" + unwrap,
		"cmd/x/main.go": "package main\nimport _ \"github.com/cjohnstoniv/wardyn/internal/secretstore/pg/testdata/zz\"\nfunc main() {}\n",
	} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	shipped := shippedFiles(t, root)
	sites, _ := scanDecryptSites(t, root, shipped)
	var got []string
	for _, s := range sites {
		got = append(got, s.relFile)
	}
	if want := []string{"internal/secretstore/kek/kektest/more.go", "internal/secretstore/pg/testdata/zz/zz.go"}; !slices.Equal(got, want) {
		t.Errorf("decrypt sites in %v, want %v: only the exempt file itself is skipped, and a linked testdata package is scanned", got, want)
	}
	problems := testSupportProblems(root, shipped)
	if len(problems) != 1 || !strings.HasPrefix(problems[0], "internal/secretstore/kek/kektest/kektest.go is linked into a shipped binary") {
		t.Errorf("testSupportProblems = %q, want exactly the exempt file linked through the testdata package", problems)
	}
}

// The primitives matched by method name and arity count only in a file that
// can reach them: an unrelated four-argument Get (a cache, a lookup) in a
// package that never touches a credential is not a decrypt site.
func TestDecryptMatcherNeedsTheDefiningImport(t *testing.T) {
	const src = `package x
import %s
func f() { c.Get(ctx, a, b, d); k.Unwrap(ctx, w, b); g.Open(nil, n, ct, aad) }`
	for _, tc := range []struct {
		imports string
		want    []string
	}{
		{`"sync"`, nil},
		{`("crypto/cipher"; "github.com/cjohnstoniv/wardyn/internal/secretstore"; "github.com/cjohnstoniv/wardyn/internal/secretstore/kek")`,
			[]string{"External.Get", "KEK.Unwrap", "AEAD.Open"}},
	} {
		f, err := parser.ParseFile(token.NewFileSet(), "x.go", fmt.Sprintf(src, tc.imports), 0)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if kind := classifyDecryptCall(call, scopeOf(f)); kind != "" {
					got = append(got, kind)
				}
			}
			return true
		})
		if !slices.Equal(got, tc.want) {
			t.Errorf("imports %s: matched %v, want %v", tc.imports, got, tc.want)
		}
	}
}

// credentialEntryPoints are the functions that start a process holding
// credentials: wardynd (every mode, maintenance ones included, reads them) and
// the proxy sidecar (it resolves and injects them).
var credentialEntryPoints = map[string]string{
	"cmd/wardynd/main.go":      "main",
	"cmd/wardyn-proxy/main.go": "main",
}

func TestCredentialBinariesDisableCoreDumpsFirst(t *testing.T) {
	root := repoRoot(t)
	for rel, fn := range credentialEntryPoints {
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, filepath.FromSlash(rel)), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		var body *ast.BlockStmt
		for _, decl := range f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == fn {
				body = fd.Body
			}
		}
		if body == nil || len(body.List) == 0 {
			t.Fatalf("%s: no %s() to check", rel, fn)
		}
		if !callsNodumpDisable(body.List[0]) {
			t.Errorf("%s: the first statement of %s() must be `if err := nodump.Disable(); err != nil {…}` — "+
				"this process holds credentials, and until it runs a crash can write them to a core file", rel, fn)
		}
	}
}

func callsNodumpDisable(stmt ast.Stmt) bool {
	ifs, ok := stmt.(*ast.IfStmt)
	if !ok {
		return false
	}
	as, ok := ifs.Init.(*ast.AssignStmt)
	if !ok || len(as.Rhs) != 1 {
		return false
	}
	call, ok := as.Rhs[0].(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "nodump" && sel.Sel.Name == "Disable"
}
