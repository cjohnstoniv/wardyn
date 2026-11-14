// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// EndVM is the end wardyn-runnerd asks for when a VM run's audit spool is full, and asks again until it
// answers no error. A failed AWS cache wipe alone is a finished end, the first time and when the end is
// asked again of the stopped VM, so that rule stops; a file share that would not go is still answered, with
// VM sandboxes on and with them off since the VM was made.
func TestVMEndVMTakesAWipeWarningAloneAsFinished(t *testing.T) {
	old := awsCacheWipeArgv
	awsCacheWipeArgv = []string{"sh", "-c", "exit 3"}
	t.Cleanup(func() { awsCacheWipeArgv = old })
	for _, off := range []bool{false, true} {
		t.Run(map[bool]string{false: "VM sandboxes on", true: "VM sandboxes off"}[off], func(t *testing.T) {
			r := newVMRig(t)
			spec := vmSpec()
			spec.Env = map[string]string{runner.AWSProfileRecordEnv: "cHJvZmlsZQ=="}
			spec.Mounts = []runner.Mount{{Source: t.TempDir(), Target: "/home/agent/work"}, {Source: t.TempDir(), Target: "/workspace/ref", ReadOnly: true}}
			ctx := context.Background()
			if _, err := r.d.CreateSandbox(ctx, spec); err != nil {
				t.Fatal(err)
			}
			d := r.d
			if off {
				d = newTestDriver(r.f) // restarted with VM sandboxes off: the VM is known by its container only
			}
			second := vmfsName(spec.RunID, 1)
			r.f.mu.Lock()
			r.f.failRemove = map[string]int{second: 1}
			r.f.mu.Unlock()
			if isVM, err := d.EndVM(ctx, spec.RunID); !isVM || err == nil || !strings.Contains(err.Error(), second) {
				t.Fatalf("EndVM with a share left = %v, %v; want true and the share's failure", isVM, err)
			}
			for _, step := range []string{"retrying", "repeating"} {
				if isVM, err := d.EndVM(ctx, spec.RunID); !isVM || err != nil {
					t.Fatalf("EndVM %s = %v, %v; want true, nil", step, isVM, err)
				}
			}
			if !r.container(t, second).removed {
				t.Fatal("the retry left share 1")
			}
		})
	}
}
