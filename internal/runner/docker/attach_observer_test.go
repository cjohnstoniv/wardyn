// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// An observer's exec carries tmux's ignore-size flag and a writer's does not;
// Attach picks the command from AttachOptions.Observer.
func TestAttach_ObserverExecCarriesIgnoreSize(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)
	sb, err := d.CreateSandbox(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	for _, observer := range []bool{false, true} {
		sess, err := d.Attach(context.Background(), sb.Ref, runner.AttachOptions{Cols: 120, Rows: 40, Observer: observer})
		if err != nil {
			t.Fatalf("Attach(observer=%v): %v", observer, err)
		}
		_ = sess.Close()
		if len(f.lastExecCmd) < 3 {
			t.Fatalf("attach exec argv = %v", f.lastExecCmd)
		}
		if got := strings.Contains(f.lastExecCmd[2], "-A -f ignore-size -s wardyn"); got != observer {
			t.Errorf("observer=%v: exec carries ignore-size = %v", observer, got)
		}
	}
}
