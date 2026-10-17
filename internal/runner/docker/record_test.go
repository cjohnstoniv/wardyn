// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// TestRecord_ConcurrentRunsFollowTheirOwnChoice: one driver with recording on takes a recorded and
// an unrecorded run at once. Each run's create, exec and revive follow its own SandboxSpec.Record,
// the driver's Config.Record is never changed, and a run whose organisation predates the field (nil)
// records as before.
func TestRecord_ConcurrentRunsFollowTheirOwnChoice(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev", Record: true})
	on, off := true, false
	choices := []*bool{&on, &off, nil, &off, &on}
	refs := make([]string, len(choices))
	var wg sync.WaitGroup
	for i, rec := range choices {
		wg.Add(1)
		go func() {
			defer wg.Done()
			spec := testSpec()
			spec.RunID, spec.Record = uuid.New(), rec
			sb, err := d.CreateSandbox(context.Background(), spec)
			if err != nil {
				t.Errorf("CreateSandbox %d: %v", i, err)
				return
			}
			refs[i] = sb.Ref
		}()
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	if !d.cfg.Record {
		t.Fatal("a run changed the driver's Config.Record")
	}
	for i, rec := range choices {
		want := rec == nil || *rec
		cfg := f.containers[refs[i]].cfg
		if got := slices.Contains(cfg.Env, "WARDYN_RECORDING=1"); got != want {
			t.Errorf("run %d: WARDYN_RECORDING in env = %v, want %v", i, got, want)
		}
		if _, err := d.Exec(context.Background(), refs[i], []string{"claude"}); err != nil {
			t.Fatalf("Exec %d: %v", i, err)
		}
		if got := len(f.lastExecCmd) > 0 && f.lastExecCmd[0] == "wardyn-rec"; got != want {
			t.Errorf("run %d: exec wrapped in wardyn-rec = %v, want %v (%v)", i, got, want, f.lastExecCmd)
		}
		if got := d.recordsAgent(context.Background(), refs[i]); got != want {
			t.Errorf("run %d: revive prepares recording = %v, want %v", i, got, want)
		}
	}
}

// TestRecord_CeilingOffNeverRecords: --recording-store off is a ceiling an organisation cannot lift.
func TestRecord_CeilingOffNeverRecords(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f) // Record false
	on := true
	spec := testSpec()
	spec.Record = &on
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if _, err := d.Exec(context.Background(), sb.Ref, []string{"bash"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := f.lastExecCmd; len(got) != 1 || got[0] != "bash" {
		t.Errorf("a runner with recording off wrapped the exec: %v", got)
	}
	cs, err := d.Classes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cs.SessionRecording || !cs.RecordPerRun {
		t.Errorf("caps = SessionRecording %v RecordPerRun %v, want false and true", cs.SessionRecording, cs.RecordPerRun)
	}
}
