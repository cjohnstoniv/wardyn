// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneragent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runner/runnerio"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

// execPerDrain stands in for the docker driver: it keeps the sandbox's
// ExecOutput and copies each Exec's output into it as one drain, which ends
// before Exec returns (the selftest's copy has reached EOF by the time the
// org's Wait returns). ExecTo sends one exec's output to the writer it is given.
type execPerDrain struct {
	*runnertest.Fake
	exitSelftest int

	mu        sync.Mutex
	outputs   map[string]io.Writer
	exit      map[string]int
	writeErrs []error
}

func newExecPerDrain() *execPerDrain {
	return &execPerDrain{Fake: runnertest.NewFake(), outputs: map[string]io.Writer{}, exit: map[string]int{}}
}

func (f *execPerDrain) CreateSandbox(ctx context.Context, spec runner.SandboxSpec) (runner.Sandbox, error) {
	out := spec.ExecOutput
	spec.ExecOutput = nil
	sb, err := f.Fake.CreateSandbox(ctx, spec)
	if err == nil && out != nil {
		f.mu.Lock()
		f.outputs[sb.Ref] = out
		f.mu.Unlock()
	}
	return sb, err
}

func (f *execPerDrain) Exec(ctx context.Context, ref string, argv []string) (string, error) {
	f.mu.Lock()
	w := f.outputs[ref]
	f.mu.Unlock()
	if w == nil {
		w = io.Discard
	}
	return f.ExecTo(ctx, ref, argv, w)
}

func (f *execPerDrain) ExecTo(_ context.Context, ref string, argv []string, w io.Writer) (string, error) {
	text, code := "task output\n", 0
	if slices.Equal(argv, runner.SelftestArgv()) {
		text, code = "selftest output\n", f.exitSelftest
	}
	end := runner.BeginOutputDrain(w)
	_, err := io.WriteString(w, text)
	end(err)
	f.mu.Lock()
	f.exit[ref] = code
	if err != nil {
		f.writeErrs = append(f.writeErrs, err)
	}
	f.mu.Unlock()
	return "exec-" + ref, nil
}

func (f *execPerDrain) Wait(_ context.Context, ref string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exit[ref], nil
}

// sealedOutput is the org's ExecOutput: it records the bytes and the EOF the
// runner's output stream delivers.
type sealedOutput struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	err  error
	done chan struct{}
	once sync.Once
}

func (o *sealedOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.Write(p)
}
func (o *sealedOutput) BeginDrain() {}
func (o *sealedOutput) EndDrain(err error) {
	o.once.Do(func() { o.mu.Lock(); o.err = err; o.mu.Unlock(); close(o.done) })
}

// remoteRun wires an org-side remote substrate to a real Dispatcher and a real
// runnerio output buffer over a loopback link, with sub as the local driver.
func remoteRun(t *testing.T, sub *execPerDrain, image string) (*remote.Substrate, string, *sealedOutput) {
	t.Helper()
	id := uuid.NewString()
	server, err := runnerio.New(sub, id, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d, err := newTestDispatcher(Config{RunnerID: id, Substrate: sub,
		BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
		Created:      func(context.Context, runner.SandboxSpec, string) error { return nil },
		Output:       server.Output, BindOutput: server.BindOutput, ForgetOutput: server.ForgetOutput,
		Streams: func(ctx context.Context, p *runnerwire.Peer, call uint32, method string, raw json.RawMessage) (any, error) {
			return server.Call(ctx, p, call, method, raw)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	link := &remote.Link{}
	org := remote.New(id, link, remote.Options{})
	caps, _ := json.Marshal(runnerwire.Caps{Support: sub.Support})
	org.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: caps})
	orgEnd, runEnd := runnerwire.Loopback()
	op := link.Attach(t.Context(), orgEnd)
	var rp *runnerwire.Peer
	rp = runnerwire.NewPeer(runEnd, runnerwire.PeerConfig{OnCall: func(ctx context.Context, call uint32, method string, raw json.RawMessage) (any, error) {
		return d.Call(ctx, rp, call, method, raw)
	}})
	go rp.Run(t.Context())
	server.Online(rp)
	t.Cleanup(func() { rp.Close(); op.Close(); server.Offline(rp) })
	out := &sealedOutput{done: make(chan struct{})}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	sb, err := org.CreateSandbox(ctx, runner.SandboxSpec{RunID: uuid.New(), RunnerID: id, Image: image, ExecOutput: out})
	if err != nil {
		t.Fatal(err)
	}
	return org, sb.Ref, out
}

// execAndWait is the org's dispatch of one exec: Exec, then Wait for its exit.
func execAndWait(t *testing.T, org *remote.Substrate, ref string, argv []string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := org.Exec(ctx, ref, argv); err != nil {
		t.Fatalf("exec %v: %v", argv, err)
	}
	code, err := org.Wait(ctx, ref)
	if err != nil {
		t.Fatalf("wait %v: %v", argv, err)
	}
	return code
}

func (o *sealedOutput) sealed(t *testing.T) (string, error) {
	t.Helper()
	select {
	case <-o.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the run's output never reached its end")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String(), o.err
}

// A BYOI run on a registered runner: the selftest passes, then the task
// writes. The run's output is the task's own, complete, with no copy refused.
func TestRemoteBYOISelftestLeavesTheRunOutputToTheTask(t *testing.T) {
	sub := newExecPerDrain()
	org, ref, out := remoteRun(t, sub, "wardyn-byoi/"+uuid.NewString()+":latest")
	if code := execAndWait(t, org, ref, runner.SelftestArgv()); code != 0 {
		t.Fatalf("selftest exit = %d", code)
	}
	execAndWait(t, org, ref, []string{"/usr/local/bin/agent-run", "do the thing"})
	got, err := out.sealed(t)
	if got != "task output\n" || err != nil {
		t.Fatalf("sealed output = %q, %v; want the task's output with a clean end", got, err)
	}
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if len(sub.writeErrs) != 0 {
		t.Fatalf("output writes refused: %v", sub.writeErrs)
	}
}

// A failing BYOI selftest still reaches the org as its exit code, which is
// what refuses the run.
func TestRemoteBYOIFailingSelftestStillAnswersItsExit(t *testing.T) {
	sub := newExecPerDrain()
	sub.exitSelftest = 3
	org, ref, _ := remoteRun(t, sub, "wardyn-byoi/"+uuid.NewString()+":latest")
	if code := execAndWait(t, org, ref, runner.SelftestArgv()); code != 3 {
		t.Fatalf("selftest exit = %d, want 3", code)
	}
}

// A non-BYOI run is untouched: every exec, whatever its argv, is the run's
// output, as before.
func TestRemoteNonBYOIRunOutputUnchanged(t *testing.T) {
	for _, argv := range [][]string{{"/usr/local/bin/agent-run", "do the thing"}, runner.SelftestArgv()} {
		sub := newExecPerDrain()
		org, ref, out := remoteRun(t, sub, "wardyn/agent:latest")
		execAndWait(t, org, ref, argv)
		got, err := out.sealed(t)
		want := "task output\n"
		if slices.Equal(argv, runner.SelftestArgv()) {
			want = "selftest output\n"
		}
		if got != want || err != nil {
			t.Fatalf("argv %v: sealed output = %q, %v; want %q", argv, got, err, want)
		}
	}
}

// Only the first selftest of a BYOI sandbox is the org's contract check: a
// task that is itself `--selftest` is the run's output.
func TestRemoteBYOITaskNamedSelftestIsTheRunOutput(t *testing.T) {
	sub := newExecPerDrain()
	org, ref, out := remoteRun(t, sub, "wardyn-byoi/"+uuid.NewString()+":latest")
	execAndWait(t, org, ref, runner.SelftestArgv())
	execAndWait(t, org, ref, runner.SelftestArgv())
	got, err := out.sealed(t)
	if got != "selftest output\n" || err != nil {
		t.Fatalf("sealed output = %q, %v; want the task's output with a clean end", got, err)
	}
}

// The runner logs only the last selftestLogMax bytes of a selftest.
func TestSelftestLogKeepsABoundedTail(t *testing.T) {
	l := &selftestLog{ref: "sandbox"}
	big := bytes.Repeat([]byte("a"), selftestLogMax)
	for _, p := range [][]byte{big, []byte("end")} {
		if n, err := l.Write(p); n != len(p) || err != nil {
			t.Fatalf("write = %d, %v", n, err)
		}
	}
	if len(l.tail) != selftestLogMax || !bytes.HasSuffix(l.tail, []byte("end")) {
		t.Fatalf("kept %d bytes ending %q", len(l.tail), l.tail[len(l.tail)-3:])
	}
}

// A BYOI sandbox stopped, killed or ended before its selftest leaves nothing awaiting it.
func TestSelftestAwaitEndsWithTheSandbox(t *testing.T) {
	for _, method := range []string{runnerwire.MethodStop, runnerwire.MethodKill, runnerwire.MethodEnd} {
		d, err := newTestDispatcher(Config{RunnerID: uuid.NewString(), Substrate: newExecPerDrain(),
			BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
			Created:      func(context.Context, runner.SandboxSpec, string) error { return nil }})
		if err != nil {
			t.Fatal(err)
		}
		d.awaitSelftest("ref", "wardyn-byoi/x:latest")
		_, _ = d.dispatchRef(context.Background(), method, "ref", refArgs{})
		if _, awaited := d.selftests.Load("ref"); awaited {
			t.Errorf("%s left the sandbox awaiting its selftest", method)
		}
	}
}
