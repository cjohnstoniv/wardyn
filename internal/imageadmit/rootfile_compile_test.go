// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/imageadmit"
	"github.com/cjohnstoniv/wardyn/internal/imageadmit/rootfile"
	"github.com/cjohnstoniv/wardyn/internal/releasesig/releasesigtest"
)

// TestCompileReadsRootsOnlyInTheRootDir: with no ReadFile, a stored root path is read through rootfile, so a
// path that leaves the directory as written (../) or through a link refuses the compile and is never read,
// while a file in it, or a link that stays in it (a mounted Secret's ..data layout), compiles.
func TestCompileReadsRootsOnlyInTheRootDir(t *testing.T) {
	s := releasesigtest.New(t)
	rootJSON, err := testRoot(t, s).MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	dir, outside := t.TempDir(), t.TempDir()
	write := func(path string, b []byte) string {
		t.Helper()
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	link := func(target, name string) string {
		t.Helper()
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
		return filepath.Join(dir, name)
	}
	inDir := write(filepath.Join(dir, "private.json"), rootJSON)
	outsideRoot := write(filepath.Join(outside, "root.json"), rootJSON)
	if err := os.Mkdir(filepath.Join(dir, "..2026_11_07"), 0o700); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(dir, "..2026_11_07", "k8s.json"), rootJSON)
	link("..2026_11_07", "..data")
	compile := func(path string) error {
		_, err := imageadmit.Compile(imageadmit.Config{TrustedRoots: []imageadmit.RootSpec{{Name: "private", Path: path}}},
			imageadmit.Options{Version: "1.2.3", PublicGoodRoot: testRoot(t, s), RootDir: dir})
		return err
	}
	for _, path := range []string{inDir, link(inDir, "linked.json"), link("..data/k8s.json", "k8s.json")} {
		if err := compile(path); err != nil {
			t.Errorf("%s: refused: %v", path, err)
		}
	}
	for name, path := range map[string]string{
		"outside the directory": outsideRoot,
		"a ../ escape":          dir + "/../" + filepath.Base(outside) + "/root.json",
		"a link out":            link(outsideRoot, "escape.json"),
	} {
		err := compile(path)
		if err == nil || !errors.Is(err, imageadmit.ErrPolicy) || !strings.Contains(err.Error(), "must be in "+dir) {
			t.Errorf("%s: Compile = %v, want ErrPolicy naming the directory", name, err)
		}
	}
	// With no RootDir the directory is rootfile.Dir, so a file in the test's directory is outside it.
	_, err = imageadmit.Compile(imageadmit.Config{TrustedRoots: []imageadmit.RootSpec{{Name: "private", Path: inDir}}},
		imageadmit.Options{Version: "1.2.3", PublicGoodRoot: testRoot(t, s)})
	if err == nil || !strings.Contains(err.Error(), "must be in "+rootfile.Dir) {
		t.Errorf("default directory: Compile = %v, want a refusal naming %s", err, rootfile.Dir)
	}
}
