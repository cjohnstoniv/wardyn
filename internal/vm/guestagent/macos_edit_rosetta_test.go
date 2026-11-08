// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"testing"
)

// Install Rosetta runs the one softwareupdate argv as root and nothing else, and answers Installed only on
// exit 0. Tools exec still refuses the same argv: the install happens only through this handler.
func TestEditInstallRosettaRunsOneArgv(t *testing.T) {
	want := [][]string{{"/usr/sbin/softwareupdate", "--install-rosetta", "--agree-to-license"}}
	rig, dir, _ := newEditRig(t)
	rig.t.run = func(_ context.Context, argv []string) ([]byte, int, error) {
		rig.ran = append(rig.ran, argv)
		return []byte("Install of Rosetta 2 finished successfully\n"), 0, nil
	}
	w := rig.do("POST", "/edit/install-rosetta", `{"argv":["/bin/sh"]}`)
	var res EditActionResult
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &res) != nil || !res.Installed {
		t.Fatalf("install-rosetta = %d %s", w.Code, w.Body)
	}
	if !slices.EqualFunc(rig.ran, want, slices.Equal[[]string]) {
		t.Errorf("ran %q, want %q", rig.ran, want)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("install-rosetta wrote %v", ents)
	}

	for name, run := range map[string]func(context.Context, []string) ([]byte, int, error){
		"nonzero exit": func(context.Context, []string) ([]byte, int, error) { return []byte("no network"), 1, nil },
		"run failed":   func(context.Context, []string) ([]byte, int, error) { return nil, 0, errors.New("killed") },
	} {
		rig, _, _ := newEditRig(t)
		rig.t.run = run
		if w := rig.do("POST", "/edit/install-rosetta", ""); w.Code != 500 || json.Valid(w.Body.Bytes()) {
			t.Errorf("%s: install-rosetta = %d %s, want 500 and no success body", name, w.Code, w.Body)
		}
	}

	rig, _, _ = newEditRig(t)
	for _, argv := range [][]string{rosettaArgv, {rosettaTool, "--install-rosetta"}} {
		if err := ToolsExecAllowed(argv, rig.bin); !errors.Is(err, ErrToolsRefused) {
			t.Errorf("ToolsExecAllowed(%q) = %v, want ErrToolsRefused", argv, err)
		}
	}
}
