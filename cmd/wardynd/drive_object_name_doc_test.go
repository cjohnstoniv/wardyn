// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The sentinels are fed to the real naming function so the shapes below are its
// OUTPUT, not a restatement of it. Lowercase and alphanumeric so driveSlug
// passes them through unfolded, and distinctive enough that a substitution
// cannot collide with anything else in the produced name.
const (
	slugSentinel = "Zqslugz"
	homeSentinel = "zqhomez"
)

// driveNameToken matches a documented minted-object name and everything the doc
// writes after it, up to whatever punctuation ends it in prose or code.
// A placeholder may contain spaces (`<new drive-slug>`), so segments inside
// angle brackets are matched whole rather than terminated at whitespace.
var driveNameToken = regexp.MustCompile(`wardyn-drive-(?:<[^>]*>|[A-Za-z0-9_.*-])*`)

// mintedShapes asks types.DriveObjectName what each MINTED backend's object is
// called and rewrites its answer into the placeholder spelling the docs use.
// host_path is not here: a share's directory is named by whoever owns the tree,
// so it has no minted shape (docs/OPERATIONS.md's state-store row covers it, and
// TestStateStoreTableCoversUserDrives derives that arm the same way).
//
// Deriving instead of asserting is the whole point: when the naming rule
// changes — as it did when every minted name took the drive slug so a re-pointed
// grant could not make two drives share one volume — the expected documentation
// changes with it, and every stale page fails by name on the next run.
func mintedShapes(t *testing.T) map[types.DriveBackend]string {
	t.Helper()
	out := map[types.DriveBackend]string{}
	for _, b := range types.DriveBackends {
		if b == types.DriveBackendHostPath {
			continue
		}
		name := types.DriveObjectName(types.UserDrive{Backend: b, Name: slugSentinel}, homeSentinel)
		shape := strings.ReplaceAll(name, strings.ToLower(slugSentinel), "<drive-slug>")
		shape = strings.ReplaceAll(shape, homeSentinel, "<home>")
		if strings.Contains(shape, slugSentinel) || strings.Contains(shape, homeSentinel) {
			t.Fatalf("backend %s produced %q — the sentinels no longer survive the naming function; re-derive this guard", b, name)
		}
		out[b] = shape
	}
	if len(out) == 0 {
		t.Fatal("no minted backends found in types.DriveBackends — revisit this guard")
	}
	return out
}

// TestDocumentedDriveObjectNamesMatchTheFunction asserts, over EVERY markdown
// file in the repository, that a documented `wardyn-drive-…` name is a name
// types.DriveObjectName can actually produce.
//
// Repo-wide rather than over a list of files, and the reason is the defect this
// was written for: the runbook's reclaim command
// (`docker volume rm wardyn-drive-…`) is three hundred lines away from the
// state-store table, in a different section, and a table-scoped check could not
// see it. An operator running a name that matches no volume concludes the volume
// is already gone and reports the reclaim as done. The same shape is also
// restated in the threat model and the audit-actions reference, which no
// file-list drawn from the runbook would have named either.
func TestDocumentedDriveObjectNamesMatchTheFunction(t *testing.T) {
	shapes := mintedShapes(t)
	allowed := make([]string, 0, len(shapes))
	for _, s := range shapes {
		if !slices.Contains(allowed, s) {
			allowed = append(allowed, s)
		}
	}
	slices.Sort(allowed)

	// Prose forms that are not a single object's name: the glob an operator
	// greps with, and the rename discussion's "the name a rename moves TO".
	isProse := func(tok string) bool {
		if tok == "wardyn-drive-*" {
			return true
		}
		// The rename discussion names the shape a rename moves TO, as a glob:
		// the same derived shape with the slug placeholder qualified.
		bare := strings.ReplaceAll(tok, "<new drive-slug>", "<drive-slug>")
		bare = strings.TrimSuffix(bare, "-*")
		for _, s := range allowed {
			if bare == s || bare == strings.TrimSuffix(s, "-<home>") {
				return true
			}
		}
		return false
	}

	root := repoRoot(t)
	skipDir := map[string]bool{".git": true, "node_modules": true, "vendor": true, "dist": true}
	files, hits := 0, 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		files++
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		src := string(b)
		rel, _ := filepath.Rel(root, path)
		for _, loc := range driveNameToken.FindAllStringIndex(src, -1) {
			tok := src[loc[0]:loc[1]]
			hits++
			if slices.Contains(allowed, tok) || isProse(tok) {
				continue
			}
			line := 1 + strings.Count(src[:loc[0]], "\n")
			// A shape that is right but spelled with a different placeholder is
			// a different (smaller) problem than a shape the function cannot
			// produce, and the message should not conflate them.
			if slices.Contains(allowed, strings.ReplaceAll(tok, "<slug>", "<drive-slug>")) {
				t.Errorf("%s:%d writes %q — right shape, non-canonical placeholder; the rest of the docs spell it `<drive-slug>`",
					rel, line, tok)
				continue
			}
			t.Errorf("%s:%d documents the object name %q, which types.DriveObjectName does not produce for any backend "+
				"(it produces %v). An operator pasting that into a reclaim command matches nothing and reports the reclaim as done",
				rel, line, tok, allowed)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}
	if files < 20 || hits == 0 {
		t.Fatalf("scanned %d markdown files and %d names — the walk, not the docs, is what changed", files, hits)
	}
}

// TestDriveSubstrateSectionsUseTheirOwnShape catches the cross-substrate mixup
// the check above cannot: while two backends mint DIFFERENT names, every name
// is individually valid, so a Docker paragraph carrying the Kubernetes shape
// passes a repo-wide validity check and still sends an operator to a volume that
// does not exist. Scoped to the two substrate sections because that is the only
// place the document commits to one backend.
func TestDriveSubstrateSectionsUseTheirOwnShape(t *testing.T) {
	shapes := mintedShapes(t)
	docker, k8s := shapes[types.DriveBackendDockerVolume], shapes[types.DriveBackendK8sPVC]
	if docker == k8s {
		t.Skipf("both minted backends name objects %q — nothing to disagree about", docker)
	}
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "OPERATIONS.md"))
	if err != nil {
		t.Fatalf("read docs/OPERATIONS.md: %v", err)
	}
	src := string(b)
	for _, sec := range []struct{ heading, want string }{
		{"### User drives on Docker", docker},
		{"### User drives on Kubernetes", k8s},
	} {
		body, ok := sectionBody(src, sec.heading)
		if !ok {
			t.Errorf("docs/OPERATIONS.md has no %q section — it is where the reclaim commands live", sec.heading)
			continue
		}
		for _, tok := range driveNameToken.FindAllString(body, -1) {
			if strings.HasSuffix(tok, "*") || tok == sec.want {
				continue
			}
			t.Errorf("%q documents %q; that substrate's objects are named %q", sec.heading, tok, sec.want)
		}
	}
}

// sectionBody returns the text from heading to the next same-level heading.
func sectionBody(src, heading string) (string, bool) {
	i := strings.Index(src, heading)
	if i < 0 {
		return "", false
	}
	rest := src[i+len(heading):]
	if j := strings.Index(rest, "\n### "); j >= 0 {
		return rest[:j], true
	}
	return rest, true
}
