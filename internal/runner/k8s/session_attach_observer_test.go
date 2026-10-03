// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// An observer's attach script carries tmux's ignore-size flag and a writer's
// does not; Attach picks the script from AttachOptions.Observer.
func TestAttach_ObserverScriptCarriesIgnoreSize(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	ref := createAgentPodFixture(t, cs, uuid.New(), "wardyn/agent-claude:local", nil)
	for _, observer := range []bool{false, true} {
		fe := newFakeExecutor()
		d.execFactory = fe.execFactory
		sess, err := d.Attach(context.Background(), ref, runner.AttachOptions{Cols: 120, Rows: 40, Observer: observer})
		if err != nil {
			t.Fatalf("Attach(observer=%v): %v", observer, err)
		}
		_ = sess.Close()
		if len(fe.cmd) < 3 {
			t.Fatalf("executor cmd = %v", fe.cmd)
		}
		if got := strings.Contains(fe.cmd[2], "-A -f ignore-size -s wardyn"); got != observer {
			t.Errorf("observer=%v: script carries ignore-size = %v", observer, got)
		}
	}
}
