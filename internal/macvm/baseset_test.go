// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

// After a swap the new base must hold only a base's own files; one that
// holds anything else (here a boot configuration a stage-2 boot left) is
// swapped back out, and the old generation stays the base.
func TestSwapRefusesABaseWithForeignFiles(t *testing.T) {
	f, tf, _ := stage2Prep(t)
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	base := f.opts.Layout.BaseDir(0)
	o := updateOpts(f, tf, newGuest(), "0.10.0")
	o.Force = true
	o.HelperVersion = "planted"
	o.Stage = hookStage(func(_ context.Context, in macvm.Stage2Input) (macvm.ToolsResult, error) {
		return noneOnly, os.WriteFile(filepath.Join(in.Dir, "boot.json"), []byte(`{"tools_mode":true}`), 0o600)
	})
	_, changed, err := macvm.UpdateTools(context.Background(), o)
	if err == nil || changed || !strings.Contains(err.Error(), `"boot.json", which is not a base file`) || !strings.Contains(err.Error(), "old base was kept") {
		t.Fatalf("update: changed %v err %v", changed, err)
	}
	m, err := macvm.ReadyBase(base)
	if err != nil || m.HelperVersion == "planted" {
		t.Fatalf("the base is the refused generation: %+v %v", m, err)
	}
	if _, err := os.Stat(filepath.Join(base, "boot.json")); !os.IsNotExist(err) {
		t.Fatalf("the planted file is in the live base: %v", err)
	}
	// An ordinary update still swaps.
	o.Stage = hookStage(func(context.Context, macvm.Stage2Input) (macvm.ToolsResult, error) { return noneOnly, nil })
	if _, changed, err := macvm.UpdateTools(context.Background(), o); err != nil || !changed {
		t.Fatalf("clean update: changed %v err %v", changed, err)
	}
}
