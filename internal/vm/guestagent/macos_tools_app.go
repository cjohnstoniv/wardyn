// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"fmt"
	"os"
	"path/filepath"
)

// itemAppStage is the folder, inside an app item's own stage directory (root's, 0700), that the app is unpacked
// or copied into before it goes to /Applications. Its leading dot keeps it apart from every file a push may
// write there (ItemSecretPattern), and the host's rm -rf of the stage directory removes it with the rest.
const itemAppStage = ".app-stage"

// installStep is one step of an app item's install: an argv to run, or, when clearOld names a stage directory,
// the removal of the /Applications counterpart of each of that directory's top-level entries (read when the step
// is reached, after the unpack has made them).
type installStep struct {
	argv     []string
	clearOld string
}

// appInstallSteps is what an app item's install form, already allowed, runs as; nil for every other argv. An
// archive or disk image keeps the owners and modes it was made with, and ditto as root would put files the
// run's user owns, or that anyone may write, into /Applications, and would merge into an older copy of the app
// that is still there. So the app goes into the item's stage first, is made root's there (links included) with
// no group or other write and no set-id bit (neither command follows a link), and only then, with the old copy
// removed, is copied into place. The host sends the same form to every agent; an older one runs it as given.
func appInstallSteps(argv []string) []installStep {
	var stage string
	var unpack, clearOld, place installStep
	var top []installStep // the zip form copies the stage folder itself onto /Applications: give it /Applications' own owner and mode
	switch {
	case len(argv) == 5 && argv[0] == "/usr/bin/ditto" && argv[1] == "-x" && argv[2] == "-k" &&
		isItemFile(argv[3], ItemZip) && argv[4] == MacApplications:
		stage = filepath.Dir(argv[3]) + "/" + itemAppStage
		unpack = installStep{argv: []string{"/usr/bin/ditto", "-x", "-k", argv[3], stage}}
		clearOld = installStep{clearOld: stage}
		place = installStep{argv: []string{"/usr/bin/ditto", "--noacl", stage, MacApplications}} // keeps /Applications' own ACL
		top = []installStep{{argv: []string{"/usr/sbin/chown", "root:admin", stage}}, {argv: []string{"/bin/chmod", "0775", stage}}}
	case len(argv) == 3 && argv[0] == "/usr/bin/ditto" && ItemAppPattern.MatchString(filepath.Base(argv[1])) &&
		isItemFile(filepath.Dir(argv[1]), ItemMount) && argv[2] == MacApplications+"/"+filepath.Base(argv[1]):
		stage = filepath.Dir(filepath.Dir(argv[1])) + "/" + itemAppStage
		app := stage + "/" + filepath.Base(argv[1])
		unpack = installStep{argv: []string{"/usr/bin/ditto", argv[1], app}}
		clearOld = installStep{argv: []string{"/bin/rm", "-rf", argv[2]}}
		place = installStep{argv: []string{"/usr/bin/ditto", app, argv[2]}}
	default:
		return nil
	}
	steps := []installStep{
		{argv: []string{"/bin/rm", "-rf", stage}},
		unpack,
		{argv: []string{"/usr/sbin/chown", "-h", "-R", "-P", "root:wheel", stage}},
		{argv: []string{"/bin/chmod", "-R", "-P", "go-w,ug-s", stage}},
	}
	return append(append(steps, top...), clearOld, place)
}

// clearOldSteps are the rm -rf argvs, one per top-level app bundle (ItemAppPattern) of the unpacked stage, that
// remove its counterpart in /Applications before the copy.
func (t *Tools) clearOldSteps(stage string) ([][]string, error) {
	list := t.listDir
	if list == nil {
		list = func(dir string) ([]string, error) {
			es, err := os.ReadDir(dir)
			names := make([]string, 0, len(es))
			for _, e := range es {
				names = append(names, e.Name())
			}
			return names, err
		}
	}
	names, err := list(stage)
	if err != nil {
		return nil, fmt.Errorf("read the unpacked app: %w", err)
	}
	argvs := make([][]string, 0, len(names))
	for _, n := range names {
		if ItemAppPattern.MatchString(n) { // only an app bundle: a top-level Utilities or other folder is never removed
			argvs = append(argvs, []string{"/bin/rm", "-rf", MacApplications + "/" + n})
		}
	}
	return argvs, nil
}
