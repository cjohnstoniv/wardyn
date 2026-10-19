// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
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
		if got, err := d.recordsAgent(context.Background(), refs[i]); err != nil || got != want {
			t.Errorf("run %d: revive prepares recording = %v, %v; want %v", i, got, err, want)
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

// agentInspectFails is fakeDocker whose next fails inspects of one container fail outright.
type agentInspectFails struct {
	*fakeDocker
	ref   string
	fails int
}

func (a *agentInspectFails) ContainerInspect(ctx context.Context, id string, o client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	if id == a.ref && a.fails > 0 {
		a.fails--
		return client.ContainerInspectResult{}, errors.New("Error response from daemon: i/o timeout")
	}
	return a.fakeDocker.ContainerInspect(ctx, id, o)
}

// TestStartSandbox_RecordingChoiceInspectFailure: revive's agent start follows Exec's rule. One
// failed inspect of the agent is retried; a second refuses with the named reason before the agent
// starts, never falling back to the driver-wide Config.Record.
func TestStartSandbox_RecordingChoiceInspectFailure(t *testing.T) {
	for _, tc := range []struct {
		fails   int
		refused bool
	}{{1, false}, {2, true}} {
		f := newFakeDocker()
		f.images["busybox:latest"] = true
		a := &agentInspectFails{fakeDocker: f}
		d := newWithClient(a, Config{ProxyImage: "wardyn-proxy:dev", InternalNetwork: "wardyn-internal", Record: true})
		ctx := context.Background()
		sb, err := d.CreateSandbox(ctx, testSpec())
		if err != nil {
			t.Fatalf("CreateSandbox: %v", err)
		}
		if err := d.EndSandbox(ctx, sb.Ref); err != nil {
			t.Fatalf("EndSandbox: %v", err)
		}
		if err := d.ReplaceProxy(ctx, sb.Ref, renderedTestConfig(t, "fresh")); err != nil {
			t.Fatalf("ReplaceProxy: %v", err)
		}
		a.ref, a.fails = sb.Ref, tc.fails
		err = d.StartSandbox(ctx, sb.Ref)
		running := f.containers[sb.Ref].state.Running
		if tc.refused && (!errors.Is(err, runner.ErrRecordingChoiceUnknown) || running) {
			t.Errorf("%d failed inspects: StartSandbox = %v, agent running %v; want ErrRecordingChoiceUnknown and the agent left stopped", tc.fails, err, running)
		}
		if !tc.refused && (err != nil || !running) {
			t.Errorf("%d failed inspect: StartSandbox = %v, agent running %v; want the retried inspect to start it", tc.fails, err, running)
		}
	}
}
