// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package docker

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

// The setting reaches the helper in run.json, and Sandbox.DesktopForcedViewOnly is what the helper's
// running shim answers, not an echo of the spec: a helper that doesn't report, or reports false, reads
// as not forced on a run whose spec asked for it.
func TestMacVMForcedViewOnlyIsTheHelpersAnswer(t *testing.T) {
	for name, c := range map[string]struct {
		spec bool
		shim macvm.DesktopShim
		want bool
	}{
		"a forced shim":                 {true, &macvm.RFBShim{ViewOnly: true, ForceViewOnly: true}, true},
		"a helper that doesn't report":  {true, nil, false},
		"a helper reporting not forced": {true, &macvm.RFBShim{ViewOnly: true}, false},
		"an interactive run's boot":     {false, &macvm.RFBShim{ViewOnly: true}, false},
		"a filter that lets input pass": {true, &macvm.RFBShim{ForceViewOnly: true}, false},
		"an interactive run, no shim":   {false, nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			r := newMacRig(t)
			r.desktop = c.shim
			spec := macSpec()
			spec.DesktopViewOnly = c.spec
			sb, err := r.d.CreateSandbox(context.Background(), spec)
			if err != nil {
				t.Fatal(err)
			}
			if sb.DesktopForcedViewOnly != c.want {
				t.Errorf("Sandbox.DesktopForcedViewOnly = %v, want %v", sb.DesktopForcedViewOnly, c.want)
			}
			cfg, err := macvm.ReadRunConfig(r.c.runDir(spec.RunID))
			if err != nil || cfg.DesktopViewOnly != c.spec {
				t.Errorf("run.json's DesktopViewOnly = %v (%v), want %v", cfg.DesktopViewOnly, err, c.spec)
			}
			if err := r.d.StopSandbox(context.Background(), sb.Ref); err != nil {
				t.Fatal(err)
			}
		})
	}
}
