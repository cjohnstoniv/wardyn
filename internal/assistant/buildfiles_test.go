// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package assistant

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Every name on the fixed list matches, group by group.
func TestBuildFilesEveryListedNameMatches(t *testing.T) {
	want := map[string]int{"Container and dev setup": 3, "Toolchain pins": 8, "Manifests": 9, "Lockfiles": 13}
	got := map[string]int{}
	for _, g := range BuildFileGroups() {
		got[g.Label] = len(g.Names)
		for _, n := range g.Names {
			if !IsBuildFileName(n) {
				t.Errorf("%s: %q does not match", g.Label, n)
			}
		}
	}
	for label, n := range want {
		if got[label] != n {
			t.Errorf("%s holds %d names, want %d", label, got[label], n)
		}
	}
	if len(BuildFileNames()) != 33 {
		t.Errorf("the list holds %d names, want 33", len(BuildFileNames()))
	}
}

// What is not on the list never matches: a file that can name a credential, a build script, a nested path, and
// a name that differs in case.
func TestBuildFilesNamesOffTheListDoNotMatch(t *testing.T) {
	for _, n := range []string{"Gemfile", "settings.gradle", "settings.gradle.kts", "build.gradle", ".npmrc", "settings.xml", ".yarnrc.yml",
		"mix.exs", "Makefile", "docker-compose.yml", ".github/workflows/ci.yml", ".devcontainer/devcontainer.json", "src/package.json",
		"sub/Dockerfile", "dockerfile", "Package.json", "package.json ", "./package.json", ""} {
		if IsBuildFileName(n) {
			t.Errorf("%q matches the fixed list", n)
		}
	}
}

// One file is at most 64 KiB and all of them at most 256 KiB; a file left out is named.
func TestBuildFilesSizeCaps(t *testing.T) {
	big := strings.Repeat("a", MaxBuildFileBytes)
	files := []File{
		{Name: "package.json", Text: big},
		{Name: "go.mod", Text: big + "b"},
		{Name: "Dockerfile", Text: big},
		{Name: "go.sum", Text: big},
		{Name: "uv.lock", Text: big},
		{Name: "yarn.lock", Text: "x"},
	}
	kept, skipped := KeepWithinCaps(files)
	names := func(fs []File) []string {
		var out []string
		for _, f := range fs {
			out = append(out, f.Name)
		}
		return out
	}
	if !slices.Equal(names(kept), []string{"package.json", "Dockerfile", "go.sum", "uv.lock"}) {
		t.Errorf("kept %v", names(kept))
	}
	if !slices.Equal(skipped, []string{"go.mod", "yarn.lock"}) {
		t.Errorf("skipped %v, want the file over its cap and the one past the total", skipped)
	}
	total := 0
	for _, f := range kept {
		total += len(f.Text)
	}
	if total > MaxBuildFilesBytes {
		t.Errorf("kept %d bytes, over the total cap", total)
	}
}

// The spec turns on the catalog of well-known secret formats and nothing else, and its engine is not nil with
// no corpus: a nil engine would scan nothing.
func TestBuildFilesScanSpecBuildsARealEngine(t *testing.T) {
	spec := BuildFilesScanSpec()
	if spec.Mode != "block" || !spec.DetectSecretPatterns || spec.BlockMinSeverity != "low" || spec.OnScannerError != "block" || spec.MaxScanBytes != MaxBuildFileBytes {
		t.Errorf("spec = %+v", spec)
	}
	if spec.DetectSecrets || spec.DetectEntropy || spec.DetectPII {
		t.Errorf("spec turns on a detector it has no input for: %+v", spec)
	}
	eng, err := contentscan.NewEngine(spec, nil)
	if err != nil || eng == nil {
		t.Fatalf("engine = %v, %v: want an engine with no corpus", eng, err)
	}
}

// No lockfile hash trips the catalog. Each testdata file stands for a real lockfile of the same shape, with
// seeded values that are never real; each is scanned whole, and line by line where it is not JSON, and must
// give zero findings. A value in a well-known secret format, assembled here, must still hold a lockfile back:
// without that, an engine that scans nothing would pass too.
func TestBuildFilesLockfileHashesPassTheCatalog(t *testing.T) {
	eng, err := contentscan.NewEngine(BuildFilesScanSpec(), nil)
	if err != nil || eng == nil {
		t.Fatalf("engine = %v, %v", eng, err)
	}
	scan := func(body string) contentscan.Result {
		res, _, err := eng.ScanRequest(contentscan.ChannelGeneric, []byte(body))
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		return res
	}
	paths, err := filepath.Glob(filepath.Join("testdata", "buildfiles", "*.sample"))
	if err != nil || len(paths) != 5 {
		t.Fatalf("testdata = %v, %v: want five samples", paths, err)
	}
	for _, p := range paths {
		name := strings.TrimSuffix(filepath.Base(p), ".sample")
		if !IsBuildFileName(name) {
			t.Errorf("%s does not stand for a listed file", p)
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		body := string(raw)
		if res := scan(body); len(res.Findings) != 0 || eng.ShouldBlock(res) || res.Skipped || !res.Scanned {
			t.Errorf("%s: %+v, held back %v; a lockfile must be scanned whole and pass", name, res, eng.ShouldBlock(res))
		}
		if strings.HasSuffix(name, ".json") {
			continue
		}
		for i, line := range strings.Split(body, "\n") {
			if res := scan(line); len(res.Findings) != 0 || eng.ShouldBlock(res) || res.Skipped {
				t.Errorf("%s line %d: %+v", name, i+1, res)
			}
		}
	}
	secret := "ghp_" + strings.Repeat("a", 36)
	for _, body := range []string{
		`{"packages": {"node_modules/x": {"integrity": "sha512-AAAA==", "token": "` + secret + `"}}}`,
		"checksum = \"" + secret + "\"",
	} {
		if res := scan(body); !eng.ShouldBlock(res) || len(res.Findings) == 0 {
			t.Errorf("a value in a well-known secret format did not hold %q back: %+v", body[:20], res)
		}
	}
}

// The files a payload carries are read back as they were added, name for name.
func TestBuildFilesRoundTripThroughThePayload(t *testing.T) {
	files := []File{{Name: "Dockerfile", Text: "FROM node:20\n"}, {Name: "package.json", Text: "{\n  \"name\": \"x\"\n}\n"}, {Name: "go.mod", Text: ""}, {Name: "go.sum", Text: "--- not a header\nrest"}}
	p := AddBuildFiles(Payload{Sections: []types.AssistantSection{{Label: LabelUnit, Text: "u", Untrusted: true}}}, files)
	if len(p.Sections) != 2 || p.Sections[1].Label != LabelBuild || !p.Sections[1].Untrusted {
		t.Fatalf("sections = %+v, want the build files last and untrusted", p.Sections)
	}
	got := FilesOf(p.Sections)
	if len(got) != len(files) {
		t.Fatalf("read back %d files, want %d: %+v", len(got), len(files), got)
	}
	for i := range files {
		if got[i] != files[i] {
			t.Errorf("file %d = %+v, want %+v", i, got[i], files[i])
		}
	}
	if q := AddBuildFiles(Payload{}, nil); len(q.Sections) != 0 {
		t.Errorf("no files added a section: %+v", q.Sections)
	}
}

// A literal counts only as a whole token: a lockfile that fetches from a host holds it, one that merely names a
// longer host or a longer package does not.
func TestLiteralInWholeTokens(t *testing.T) {
	lock := []File{{Name: "package-lock.json", Text: `{"resolved": "https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz"}`}}
	for _, c := range []struct {
		value string
		files []File
		want  bool
	}{
		{"registry.npmjs.org", lock, true},
		{"left-pad", lock, true},
		{"registry.npmjs.org", []File{{Name: "package-lock.json", Text: `"resolved": "https://registry.npmjs.org.evil.example/x"`}}, false},
		{"npmjs.org.evil.example", lock, false},
		{"npmjs.org", lock, false},
		{"left", lock, false},
		{"pad", lock, false},
		{"foo.com", []File{{Name: "package.json", Text: `"author": "a@foo.com"`}}, false},
		{"@types/node", []File{{Name: "package.json", Text: `"@types/node": "^20"`}}, true},
		{"node", []File{{Name: "Dockerfile", Text: "FROM node:20\n"}}, true},
		{"node:20", []File{{Name: "Dockerfile", Text: "FROM node:20\n"}}, true},
		{"node:2", []File{{Name: "Dockerfile", Text: "FROM node:20\n"}}, false},
		{"pypi.org", []File{{Name: "pyproject.toml", Text: "index = \"https://pypi.org.\"\n"}}, true},
		{"", lock, false},
		{"registry.npmjs.org", nil, false},
		{"Registry.npmjs.org", lock, false},
	} {
		if got := LiteralIn(c.value, c.files); got != c.want {
			t.Errorf("LiteralIn(%q) = %v, want %v", c.value, got, c.want)
		}
	}
}

// A literal found in the second place of a file is found even when the first place is part of a longer name.
func TestLiteralInKeepsLookingAfterAPartialMatch(t *testing.T) {
	f := []File{{Name: "go.mod", Text: "require foo.example.com v1\nrequire foo.example v1\n"}}
	if !LiteralIn("foo.example", f) {
		t.Error("foo.example is a whole token on the second line")
	}
}
