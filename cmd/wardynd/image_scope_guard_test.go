// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// These guards hold scripts/image-scope.sh, which decides on a pull request
// which of the nine trivy image scans can be skipped. A skipped scan that
// should have run ships an unscanned image change, so the oracle here is built
// from the Dockerfiles and `go list` and shares no code with the script: every
// input the oracle derives is planted as a changed path, and the script must
// answer "scan runs" for the image.

type scannedImage struct{ name, dockerfile string }

// scannedImages reads the nine images from the trivy jobs' env, as the script does.
func scannedImages(t *testing.T, root string) []scannedImage {
	t.Helper()
	wf, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	var out []scannedImage
	for _, m := range regexp.MustCompile(`(?m)^\s+TRIVY_IMAGE: (\S+)\n\s+TRIVY_DOCKERFILE: (\S+)`).FindAllStringSubmatch(string(wf), -1) {
		out = append(out, scannedImage{m[1], m[2]})
	}
	if len(out) != 9 {
		t.Fatalf("ci.yml has %d trivy images, want the 9 the release publishes", len(out))
	}
	return out
}

// dockerfileCode returns a Dockerfile's lines without comments.
func dockerfileCode(t *testing.T, root, path string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []string
	for _, ln := range strings.Split(string(b), "\n") {
		if s := strings.TrimSpace(ln); s != "" && !strings.HasPrefix(s, "#") {
			out = append(out, s)
		}
	}
	return out
}

// imageInputs derives what an image is built from: Dockerfiles (its own and
// those of the local bases it builds FROM), COPY sources, and the Go main
// packages. ui/ is the console bundle and is not an input (see the script).
func imageInputs(t *testing.T, root string, img scannedImage, all []scannedImage) (dockerfiles, copies, mains []string) {
	t.Helper()
	dockerfiles = []string{img.dockerfile}
	for i := 0; i < len(dockerfiles); i++ {
		for _, ln := range dockerfileCode(t, root, dockerfiles[i]) {
			for _, m := range regexp.MustCompile(`wardyn/([a-z0-9-]+):local`).FindAllStringSubmatch(ln, -1) {
				k := slices.IndexFunc(all, func(a scannedImage) bool { return a.name == m[1] })
				if k < 0 {
					t.Fatalf("%s builds FROM wardyn/%s:local, which no trivy job scans", dockerfiles[i], m[1])
				}
				if !slices.Contains(dockerfiles, all[k].dockerfile) {
					dockerfiles = append(dockerfiles, all[k].dockerfile)
				}
			}
			w := strings.Fields(ln)
			if w[0] == "COPY" || w[0] == "ADD" {
				from, args := "", []string{}
				for _, a := range w[1:] {
					switch {
					case strings.HasPrefix(a, "--from="):
						from = strings.TrimPrefix(a, "--from=")
					case !strings.HasPrefix(a, "--"):
						args = append(args, a)
					}
				}
				for _, s := range args[:len(args)-1] {
					if from != "" {
						if from != "builder" || !strings.HasPrefix(s, "/build/") {
							continue
						}
						s = strings.TrimPrefix(s, "/build/")
					}
					if s != "." && s != "ui" && !strings.HasPrefix(s, "ui/") {
						copies = append(copies, s)
					}
				}
			}
			if strings.Contains(ln, "go build") {
				mains = append(mains, regexp.MustCompile(`\./cmd/[A-Za-z0-9_-]+`).FindAllString(ln, -1)...)
			}
		}
	}
	slices.Sort(mains)
	return dockerfiles, copies, slices.Compact(mains)
}

// plantFor names a path the COPY source s would match: the source itself, the
// first file under a directory, or a glob with its star filled in.
func plantFor(root, s string) string {
	if strings.Contains(s, "*") {
		if m, _ := filepath.Glob(filepath.Join(root, s)); len(m) > 0 {
			rel, _ := filepath.Rel(root, m[0])
			return rel
		}
		return strings.ReplaceAll(s, "*", "x")
	}
	if st, err := os.Stat(filepath.Join(root, s)); err == nil && st.IsDir() {
		planted := filepath.Join(s, "zz-planted")
		_ = filepath.WalkDir(filepath.Join(root, s), func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				planted, _ = filepath.Rel(root, p)
				return fs.SkipAll
			}
			return nil
		})
		return planted
	}
	return s
}

// goClosure is what `go list -deps` says the mains link from this module: the
// package directories, and the files they embed, as repo-relative paths.
func goClosure(t *testing.T, root string, env, mains []string) (dirs, embeds []string) {
	t.Helper()
	tmpl := `{{with .Module}}{{if .Main}}D {{$.Dir}}{{"\n"}}{{range $.EmbedFiles}}E {{$.Dir}}/{{.}}{{"\n"}}{{end}}{{end}}{{end}}`
	for _, tags := range []string{"", "docker,k8s"} {
		cmd := exec.Command("go", append([]string{"list", "-e", "-tags", tags, "-deps", "-f", tmpl}, mains...)...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOPROXY=off", "GOFLAGS=-mod=readonly")
		cmd.Env = append(cmd.Env, env...)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list %v: %v", mains, err)
		}
		for _, ln := range strings.Split(string(out), "\n") {
			kind, p, ok := strings.Cut(ln, " ")
			if !ok {
				continue
			}
			rel, _ := filepath.Rel(root, p)
			if kind == "D" {
				dirs = append(dirs, rel)
			} else {
				embeds = append(embeds, rel)
			}
		}
	}
	slices.Sort(dirs)
	slices.Sort(embeds)
	return slices.Compact(dirs), slices.Compact(embeds)
}

// firstGoFile is a non-test Go file of a package directory, repo-relative.
func firstGoFile(t *testing.T, root, dir string) string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(root, dir))
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range ents {
		if n := e.Name(); strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
			return filepath.Join(dir, n)
		}
	}
	t.Fatalf("%s has no non-test Go file", dir)
	return ""
}

// scopeRows maps image -> changed path -> whether the scan runs.
type scopeRows map[string]map[string]bool

// runImageScope feeds changed paths to the script in its per-path mode.
func runImageScope(t *testing.T, root string, env []string, files []string) scopeRows {
	t.Helper()
	cmd := exec.Command("bash", "scripts/image-scope.sh")
	cmd.Dir = root
	cmd.Env = append(append(os.Environ(), "IMAGE_SCOPE_PER_FILE=1"), env...)
	cmd.Stdin = strings.NewReader(strings.Join(files, "\n") + "\n")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	rows := scopeRows{}
	for _, ln := range strings.Split(string(out), "\n") {
		if f := strings.Fields(ln); len(f) == 4 && f[0] == "row" {
			if rows[f[1]] == nil {
				rows[f[1]] = map[string]bool{}
			}
			rows[f[1]][f[3]] = f[2] == "true"
		}
	}
	if err != nil || len(rows) == 0 {
		t.Fatalf("scripts/image-scope.sh gave no rows (it falls back to scanning everything on any failure): %v\n%s\n%s", err, out, stderr.String())
	}
	return rows
}

// TestImageScopeScansWhatAnInputReaches plants every build input of every
// image as a changed path and requires the script to say "scan runs". A rule
// the script lost (the Dockerfile, a COPY source, a base's sources, a Go
// package of a binary, an embedded file, a shared input) fails here by name.
func TestImageScopeScansWhatAnInputReaches(t *testing.T) {
	root := repoRoot(t)
	images := scannedImages(t, root)
	shared := []string{"go.mod", "go.sum", "go.work", "go.work.sum", "deploy/base-images.txt", ".trivyignore",
		".trivyignore.yaml", "trivy.yaml", "trivy.yml", ".dockerignore", "vendor/modules.txt", "vendor/github.com/x/y/y.go",
		".github/workflows/ci.yml", ".github/actions/base-image-mirror/action.yml", "scripts/image-scope.sh"}

	type plant struct{ img, path, why string }
	var plants []plant
	var files []string
	add := func(img, path, why string) {
		plants = append(plants, plant{img, path, why})
		files = append(files, path)
	}
	for _, img := range images {
		dockerfiles, copies, mains := imageInputs(t, root, img, images)
		if len(mains) == 0 && !strings.HasPrefix(img.name, "agent-") {
			t.Fatalf("%s: no `go build ./cmd/…` found in %v", img.name, dockerfiles)
		}
		for _, df := range dockerfiles {
			add(img.name, df, "its Dockerfile")
			// BuildKit reads <Dockerfile>.dockerignore instead of the root .dockerignore.
			add(img.name, df+".dockerignore", "its per-Dockerfile .dockerignore")
		}
		for _, c := range copies {
			add(img.name, plantFor(root, c), "COPY "+c)
		}
		for _, p := range shared {
			add(img.name, p, "a shared input")
		}
		if len(mains) == 0 {
			continue
		}
		dirs, embeds := goClosure(t, root, nil, mains)
		for _, d := range dirs {
			add(img.name, firstGoFile(t, root, d), "a Go package of "+strings.Join(mains, " "))
		}
		for _, e := range embeds {
			add(img.name, e, "a file a Go package of "+strings.Join(mains, " ")+" embeds")
		}
	}
	files = slices.Compact(slices.Sorted(slices.Values(files)))

	rows := runImageScope(t, root, nil, files)
	for _, p := range plants {
		if !rows[p.img][p.path] {
			t.Errorf("planted change %s (%s): trivy (%s) would be SKIPPED, so a change to an input would ship unscanned", p.path, p.why, p.img)
		}
	}
	if len(plants) < 100 {
		t.Errorf("only %d planted inputs: the oracle derived too little to hold the script", len(plants))
	}
}

// TestImageScopeSkipsWhatNoInputReaches is the other half: a scan that always
// runs is the old behaviour. These changes reach no image, or only some.
func TestImageScopeSkipsWhatNoInputReaches(t *testing.T) {
	root := repoRoot(t)
	images := scannedImages(t, root)
	const (
		testFile = "internal/api/zz_planted_test.go"
		apiFile  = "internal/api/helpers.go"
	)
	rows := runImageScope(t, root, nil, []string{
		"docs/CI.md", "ui/src/app/zz-planted.tsx", "ui/package.json", testFile, apiFile,
		"deploy/images/codex-cli/agent-run", "deploy/images/aws-sso/signin-pane.sh",
		"deploy/images/common/agent-run-lib.sh", "deploy/images/base/Dockerfile",
	})
	scans := func(path string) []string {
		var out []string
		for _, img := range images {
			if rows[img.name][path] {
				out = append(out, img.name)
			}
		}
		slices.Sort(out)
		return out
	}
	for _, none := range []string{"docs/CI.md", "ui/src/app/zz-planted.tsx", "ui/package.json", testFile} {
		if got := scans(none); len(got) != 0 {
			t.Errorf("%s reaches no image input but would scan %v", none, got)
		}
	}
	for path, want := range map[string][]string{
		// internal/api is the daemon's: no other image links it.
		apiFile:                                {"wardynd", "wardynd-fips"},
		"deploy/images/codex-cli/agent-run":    {"agent-codex-cli"},
		"deploy/images/aws-sso/signin-pane.sh": {"agent-aws-sso"},
		// The shared boot library: every image that copies it, and so the two UIs built on agent-base.
		"deploy/images/common/agent-run-lib.sh": {"agent-aws-sso", "agent-base", "agent-codex-cli", "agent-novnc", "agent-vscode", "wardynd", "wardynd-fips"},
		// A base image change reaches the two images built FROM it.
		"deploy/images/base/Dockerfile": {"agent-base", "agent-novnc", "agent-vscode"},
	} {
		if got := scans(path); !slices.Equal(got, want) {
			t.Errorf("%s: scans %v, want exactly %v", path, got, want)
		}
	}
}

// TestImageScopeFollowsANewImport holds the reason the rule is `go list -deps`
// and not a path list: a main package that starts importing a package already in
// the module reaches that package's files, and the script reads the tree under
// test. The import is planted with an overlay, so the tree is never edited.
func TestImageScopeFollowsANewImport(t *testing.T) {
	root := repoRoot(t)
	const target = "internal/store"
	changed := firstGoFile(t, root, target)
	only := []string{"IMAGE_SCOPE_ONLY=wardyn-proxy"}

	if rows := runImageScope(t, root, only, []string{changed}); rows["wardyn-proxy"][changed] {
		t.Fatalf("%s is already in wardyn-proxy's closure: pick another package for this guard", target)
	}

	dir := t.TempDir()
	planted := filepath.Join(dir, "planted.go")
	src := "package main\n\nimport _ \"github.com/cjohnstoniv/wardyn/" + target + "\"\n"
	if err := os.WriteFile(planted, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	overlay, _ := json.Marshal(map[string]map[string]string{"Replace": {filepath.Join(root, "cmd", "wardyn-proxy", "zz_planted.go"): planted}})
	ovFile := filepath.Join(dir, "overlay.json")
	if err := os.WriteFile(ovFile, overlay, 0o600); err != nil {
		t.Fatal(err)
	}
	env := append(only, "GOFLAGS=-overlay="+ovFile)
	if rows := runImageScope(t, root, env, []string{changed}); !rows["wardyn-proxy"][changed] {
		t.Errorf("wardyn-proxy imports %s in the planted tree, yet a change to %s would skip its scan", target, changed)
	}
}

// TestCIScopesTheTrivyJobsOnPullRequests holds the workflow half: each scan
// job keeps its required name and its place in the existing skip pattern, reads
// its own answer from image-scope, and the full-tree release marker is only
// written when nothing was narrowed.
func TestCIScopesTheTrivyJobsOnPullRequests(t *testing.T) {
	root := repoRoot(t)
	wf, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	for _, img := range scannedImages(t, root) {
		job := ciJobBlock(t, string(wf), "trivy-"+img.name)
		for _, want := range []string{
			"name: trivy (" + img.name + ")",
			"needs: [changes, diagrams, image-scope]",
			"!cancelled() && needs.changes.outputs.images != 'false' && needs.image-scope.outputs." + img.name + " != 'false'",
		} {
			if !strings.Contains(job, want) {
				t.Errorf("job trivy-%s no longer contains %q", img.name, want)
			}
		}
	}
	scope := ciJobBlock(t, string(wf), "image-scope")
	for _, want := range []string{
		"scripts/image-scope.sh",
		"[ \"$EVENT\" != pull_request ] || [[ \"$HEAD_REF\" == train/* ]] || [[ \"$BASE_REF\" == release/* ]]",
		"git diff --name-only --no-renames HEAD^1 HEAD",
	} {
		if !strings.Contains(scope, want) {
			t.Errorf("image-scope no longer contains %q: a main push, a train or a release pull request could be narrowed", want)
		}
	}
	if !strings.Contains(string(wf), "if: env.TRIVY_IMAGE == 'agent-aws-sso' || env.TRIVY_IMAGE == 'agent-codex-cli'") {
		t.Error("the agent-run --selftest step no longer names agent-aws-sso and agent-codex-cli")
	}
	if strings.Contains(ciJobBlock(t, string(wf), "changes"), "tested-tree.txt") {
		t.Error("the changes job writes the ci-full-tree marker; it cannot see image-scope's answer, so a narrowed scan would still count as a full run")
	}
	if strings.Contains(scope, "needs:") {
		t.Error("image-scope has `needs`: it would queue behind the long jobs that became ready with it (see the start-order note in `changes`)")
	}
	build := ciJobBlock(t, string(wf), "build")
	if !strings.Contains(build, "needs: [changes, go, image-scope]") {
		t.Error("build no longer needs image-scope, so it cannot read the scan answers the ci-full-tree marker depends on")
	}
	for _, img := range scannedImages(t, root) {
		if want := "needs.image-scope.outputs." + img.name + " == 'true'"; !strings.Contains(build, want) {
			t.Errorf("the ci-full-tree marker in build does not require %q: a narrowed scan would still count as a full run", want)
		}
	}
}

// TestImageScopePinnedSharedInputsScanEveryImage holds, in a list written by
// hand and independent of the script and of the Dockerfile oracle above, the
// files that change every image's build or scan without being a COPY source or
// a Go package: the build context's ignore file, the module files, workspace and
// vendor mode (`COPY . .` copies them and the build switches to them), and the
// config trivy loads from the workspace. The derived oracle cannot see these.
func TestImageScopePinnedSharedInputsScanEveryImage(t *testing.T) {
	root := repoRoot(t)
	pinned := []string{
		".dockerignore", "go.mod", "go.sum", "go.work", "go.work.sum", "vendor/modules.txt",
		"vendor/github.com/x/y/y.go", "trivy.yaml", "trivy.yml", ".trivyignore", ".trivyignore.yaml",
		"deploy/base-images.txt", ".github/workflows/ci.yml",
	}
	rows := runImageScope(t, root, nil, pinned)
	for _, img := range scannedImages(t, root) {
		for _, p := range pinned {
			if !rows[img.name][p] {
				t.Errorf("%s changed: trivy (%s) would be SKIPPED, but every image's build or scan reads it", p, img.name)
			}
		}
	}
}

// scratchScope runs the script, as ci.yml does and from its own directory by a
// relative path, in a one-image tree built from the given Dockerfile. It
// returns the image's answer and the script's stderr.
func scratchScope(t *testing.T, dockerfile, changed string) (bool, string) {
	t.Helper()
	root := repoRoot(t)
	dir := t.TempDir()
	script, err := os.ReadFile(filepath.Join(root, "scripts", "image-scope.sh"))
	if err != nil {
		t.Fatal(err)
	}
	ci := "jobs:\n  trivy-foo:\n    env:\n      TRIVY_IMAGE: foo\n      TRIVY_DOCKERFILE: deploy/foo/Dockerfile\n"
	for path, body := range map[string]string{
		"scripts/image-scope.sh":   string(script),
		".github/workflows/ci.yml": ci,
		"deploy/foo/Dockerfile":    dockerfile,
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, path), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("bash", "./image-scope.sh")
	cmd.Dir = filepath.Join(dir, "scripts")
	cmd.Stdin = strings.NewReader(changed + "\n")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("image-scope.sh: %v\n%s", err, stderr.String())
	}
	switch strings.TrimSpace(string(out)) {
	case "foo=true":
		return true, stderr.String()
	case "foo=false":
		return false, stderr.String()
	}
	t.Fatalf("image-scope.sh printed %q", out)
	return false, ""
}

// TestImageScopeFailsClosedOnDockerfileFormsItDoesNotModel pins the fallback:
// a Dockerfile form the script cannot list the inputs of must scan the image
// whatever changed, and the plain Dockerfile beside it must not. The plain
// case also holds two other things: an image with no `go build` is answered
// by its COPY sources alone, and the script works run by a relative path from
// its own directory.
func TestImageScopeFailsClosedOnDockerfileFormsItDoesNotModel(t *testing.T) {
	const plain = "FROM scratch\nCOPY a /a\n"
	if got, why := scratchScope(t, plain, "unrelated.txt"); got {
		t.Fatalf("an unrelated change scans an image with no go build, or the script failed when run by a relative path:\n%s", why)
	}
	if got, why := scratchScope(t, plain, "a"); !got {
		t.Fatalf("a changed COPY source does not scan the image:\n%s", why)
	}
	for name, extra := range map[string]string{
		"JSON-form COPY":                 "COPY [\"b\", \"/b\"]\n",
		"COPY continued over lines":      "COPY b \\\n  c /\n",
		"bind mount of the context":      "RUN --mount=type=bind,source=gen,target=/gen true\n",
		"cgo build":                      "ENV CGO_ENABLED=1\n",
		"RUN of a script after COPY . .": "COPY . .\nRUN sh ./gen.sh\n",
		"script run on a continued line": "COPY . .\nRUN go generate \\\n && ./gen.sh\n",
	} {
		if got, why := scratchScope(t, plain+extra, "unrelated.txt"); !got {
			t.Errorf("%s: an unrelated change skips the scan, so the script modelled nothing of it:\n%s", name, why)
		}
	}
}
