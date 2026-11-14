// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A context with no deadline of its own, made in a driver, lets a call outlive the lock its caller holds: the
// caller's bound never reaches it. Each one in this tree is listed here with why it may be unbounded, so a new one
// is a test failure until it gets a bound or a reason.
const (
	ctxCreate        = "create path: a rollback or step of a sandbox create, which runs under no run lock"
	ctxOwnLoop       = "own loop: a follower or recheck that lives as long as what it follows and is stopped by it"
	ctxOwnHandle     = "own handle: the root of a VM's lifetime, cancelled by its own stop"
	ctxProbe         = "probe: a host capability check at start, under no run lock"
	ctxBoot          = "boot: daemon start, under no run lock"
	ctxSettingsCheck = "settings check: an image check an admin starts from settings, under no run lock"
	ctxCallerBound   = "caller bound: the caller ends the wait by closing the session (the api's waitExecWithin)"
)

type runnerContextSite struct{ file, fn, kind string }

type runnerContextRow struct {
	n      int
	reason string
}

// runnerContextCensus is every context.Background, context.TODO and context.WithoutCancel in the non-test files of
// internal/runner and the folders below it that is not the parent handed straight to context.WithTimeout or
// context.WithDeadline, keyed by file (relative to internal/runner), function and kind.
var runnerContextCensus = map[runnerContextSite]runnerContextRow{
	{"docker/drive_pin.go", "Driver.dropDrivePin", "Background"}:              {1, ctxCreate},
	{"docker/drive_pin.go", "Driver.pinDriveVolumes", "WithoutCancel"}:        {1, ctxCreate},
	{"docker/driver_dns.go", "Driver.mountProxyResolver", "Background"}:       {1, ctxCreate},
	{"docker/driver_exec.go", "Driver.followMainProcessOutput", "Background"}: {1, ctxOwnLoop},
	{"docker/driver_network.go", "Driver.CreateSandbox", "Background"}:        {1, ctxCreate},
	{"docker/driver_network.go", "Driver.createRunNetwork", "Background"}:     {1, ctxCreate},
	{"docker/driver_network.go", "Driver.daemonSubnet", "Background"}:         {1, ctxCreate},
	{"docker/driver_network.go", "Driver.runNetworkAndProxy", "Background"}:   {3, ctxCreate},
	{"docker/macvm.go", "Driver.createMacVM", "Background"}:                   {1, ctxCreate},
	{"docker/macvm.go", "newMacVMRun", "Background"}:                          {1, ctxOwnHandle},
	{"docker/macvm_prep.go", "Driver.StartMacPrepNet", "Background"}:          {3, ctxCreate},
	{"docker/vm.go", "Driver.createVM", "Background"}:                         {2, ctxCreate},
	{"docker/vm.go", "Driver.exportImage", "Background"}:                      {1, ctxCreate},
	{"docker/vm.go", "Driver.newVMRun", "Background"}:                         {1, ctxOwnHandle},
	{"docker/vm.go", "Driver.runOneShot", "Background"}:                       {1, ctxCreate},
	{"docker/vm_probe.go", "Driver.probeQEMU", "Background"}:                  {1, ctxProbe},
	{"docker/vm_probe.go", "Driver.probeVirtiofsd", "Background"}:             {1, ctxProbe},
	{"k8s/canary_recheck.go", "Driver.startCanaryRecheck", "Background"}:      {1, ctxOwnLoop},
	{"k8s/driver.go", "New", "Background"}:                                    {1, ctxBoot},
	{"k8s/exec.go", "Driver.followExecOutput", "Background"}:                  {1, ctxOwnLoop},
	{"k8s/imagecheck.go", "Driver.CheckPrewrappedImage", "WithoutCancel"}:     {1, ctxSettingsCheck},
	{"k8s/imagecheck.go", "Driver.checkImageOnArch", "WithoutCancel"}:         {1, ctxSettingsCheck},
	{"k8s/sandbox.go", "Driver.CreateSandbox", "Background"}:                  {1, ctxCreate},
	{"offline/retirement.go", "Manager.ReleaseSettledOutput", "Background"}:   {1, ctxBoot},
	{"remote/streams.go", "Substrate.ExecStream", "Background"}:               {1, ctxCallerBound},
	{"runnerio/server.go", "Server.life", "Background"}:                       {1, ctxOwnLoop},
}

// isContextCall reports whether e is a call of context.<one of names>.
func isContextCall(e ast.Expr, names ...string) bool {
	c, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "context" {
		return false
	}
	for _, n := range names {
		if sel.Sel.Name == n {
			return true
		}
	}
	return false
}

// runnerContextSites counts the unbounded root contexts in each function of files, keyed by their path.
func runnerContextSites(files map[string]*ast.File) map[runnerContextSite]int {
	out := map[runnerContextSite]int{}
	for path, f := range files {
		for _, d := range f.Decls {
			fn := "(package level)"
			if fd, ok := d.(*ast.FuncDecl); ok {
				fn = fd.Name.Name
				if fd.Recv != nil && len(fd.Recv.List) == 1 {
					t := fd.Recv.List[0].Type
					if st, ok := t.(*ast.StarExpr); ok {
						t = st.X
					}
					if ix, ok := t.(*ast.IndexExpr); ok {
						t = ix.X
					}
					if id, ok := t.(*ast.Ident); ok {
						fn = id.Name + "." + fn
					}
				}
			}
			bounded := map[ast.Expr]bool{}
			ast.Inspect(d, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok && len(c.Args) > 0 &&
					isContextCall(c, "WithTimeout", "WithDeadline", "WithTimeoutCause", "WithDeadlineCause") {
					bounded[c.Args[0]] = true
				}
				return true
			})
			ast.Inspect(d, func(n ast.Node) bool {
				c, ok := n.(*ast.CallExpr)
				if !ok || bounded[c] {
					return true
				}
				for _, kind := range []string{"Background", "TODO", "WithoutCancel"} {
					if isContextCall(c, kind) {
						out[runnerContextSite{path, fn, kind}]++
					}
				}
				return true
			})
		}
	}
	return out
}

// runnerContextProblems is what found and table disagree on.
func runnerContextProblems(found map[runnerContextSite]int, table map[runnerContextSite]runnerContextRow) []string {
	var out []string
	for site, n := range found {
		row, ok := table[site]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("%s %s: %d unlisted context.%s; bound it with context.WithTimeout, or list it with why it may be unbounded", site.file, site.fn, n, site.kind))
		case row.n != n:
			out = append(out, fmt.Sprintf("%s %s: %d context.%s, the census lists %d", site.file, site.fn, n, site.kind, row.n))
		}
	}
	for site, row := range table {
		if _, ok := found[site]; !ok {
			out = append(out, fmt.Sprintf("%s %s: the census lists %d context.%s and there are none; drop the row", site.file, site.fn, row.n, site.kind))
		}
		if row.reason == "" {
			out = append(out, fmt.Sprintf("%s %s: no reason", site.file, site.fn))
		}
	}
	sort.Strings(out)
	return out
}

// parseRunnerTree parses the non-test Go files under internal/runner, skipping testdata, keyed by their path under it.
func parseRunnerTree(t *testing.T) map[string]*ast.File {
	t.Helper()
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && d.Name() == "testdata":
			return filepath.SkipDir
		case d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		files[filepath.ToSlash(path)] = f
		return nil
	})
	if err != nil || len(files) < 50 {
		t.Fatalf("parsed %d files under internal/runner (%v): the walk broke, not the tree", len(files), err)
	}
	return files
}

// Every unbounded root context in the runner drivers is in the frozen census with why it may be unbounded; every row
// matches one. A planted one is found, and one handed straight to context.WithTimeout is not.
func TestRunnerContextCensus(t *testing.T) {
	files := parseRunnerTree(t)
	for _, p := range runnerContextProblems(runnerContextSites(files), runnerContextCensus) {
		t.Error(p)
	}

	t.Run("a planted unbounded context is found", func(t *testing.T) {
		const src = `package docker

func (d *Driver) plantedRemove(id string) {
	_ = d.remove(context.Background(), id)
	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	_ = d.remove(context.WithoutCancel(ctx), id)
}
`
		f, err := parser.ParseFile(token.NewFileSet(), "planted.go", src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		withPlant := map[string]*ast.File{"docker/planted.go": f}
		for k, v := range files {
			withPlant[k] = v
		}
		problems := runnerContextProblems(runnerContextSites(withPlant), runnerContextCensus)
		var bg, wc bool
		for _, p := range problems {
			bg = bg || strings.Contains(p, "docker/planted.go Driver.plantedRemove: 1 unlisted context.Background")
			wc = wc || strings.Contains(p, "docker/planted.go Driver.plantedRemove: 1 unlisted context.WithoutCancel")
		}
		if !bg || !wc || len(problems) != 2 {
			t.Errorf("problems %q, want exactly the planted Background and WithoutCancel", problems)
		}
	})
}
