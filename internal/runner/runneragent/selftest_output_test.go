// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneragent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
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
	argvs     [][]string // every argv the runner exec'd
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
	f.argvs = append(f.argvs, argv)
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

// selftestArgvForTest is what the org's BYOI dispatch execs before the task
// (internal/api/runs_dispatch_byoi.go), read from the one list both sides use.
var selftestArgvForTest = runner.SelftestArgv()

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

// taskGeneration is the generation the org's dispatch issued for the task.
var taskGeneration = uuid.MustParse("6f1c2c39-6a35-4c58-9d3e-0b8f6a3c1d01")

// admitTaskGeneration is CheckSelftest for a lease that carries taskGeneration, unattempted.
func admitTaskGeneration(_ context.Context, _ string, g uuid.UUID) error {
	if g != taskGeneration {
		return errors.New("selftest generation is not the lease's unattempted execution")
	}
	return nil
}

// remoteRun wires an org-side remote substrate to a real Dispatcher and a real
// runnerio output buffer over a loopback link, with sub as the local driver.
// The runner admits a selftest under taskGeneration and advertises it.
func remoteRun(t *testing.T, sub *execPerDrain, image string) (*remote.Substrate, string, *sealedOutput) {
	return remoteRunWith(t, sub, image, admitTaskGeneration, true)
}

// remoteRunWith is remoteRun with the runner's CheckSelftest and whether its
// caps advertise the selftest (false: a runner from before it).
func remoteRunWith(t *testing.T, sub *execPerDrain, image string, check func(context.Context, string, uuid.UUID) error, advertise bool) (*remote.Substrate, string, *sealedOutput) {
	t.Helper()
	org, _, ref, out := remoteRunD(t, sub, image, check, advertise)
	return org, ref, out
}

// remoteRunD is remoteRunWith that also returns the runner's dispatcher.
func remoteRunD(t *testing.T, sub *execPerDrain, image string, check func(context.Context, string, uuid.UUID) error, advertise bool) (*remote.Substrate, *Dispatcher, string, *sealedOutput) {
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
		CheckSelftest: check,
		Streams: func(ctx context.Context, p *runnerwire.Peer, call uint32, method string, raw json.RawMessage) (any, error) {
			return server.Call(ctx, p, call, method, raw)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	link := &remote.Link{}
	org := remote.New(id, link, remote.Options{ExecutionGeneration: func(context.Context, string, string) (uuid.UUID, error) { return taskGeneration, nil }})
	caps, _ := json.Marshal(runnerwire.Caps{Support: sub.Support, BYOISelftest: advertise && d.Selftests()})
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
	return org, d, sb.Ref, out
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

// selftest is the org's BYOI selftest of ref: one call, its exit in the reply.
func selftest(t *testing.T, org *remote.Substrate, ref string) (int, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	return org.Selftest(ctx, ref)
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
	if code, err := selftest(t, org, ref); code != 0 || err != nil {
		t.Fatalf("selftest = %d, %v", code, err)
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
	if code, err := selftest(t, org, ref); code != 3 || err != nil {
		t.Fatalf("selftest = %d, %v, want 3", code, err)
	}
}

// A non-BYOI run is untouched: every exec, whatever its argv, is the run's
// output, as before. It owes no selftest, so one is refused.
func TestRemoteNonBYOIRunOutputUnchanged(t *testing.T) {
	for _, argv := range [][]string{{"/usr/local/bin/agent-run", "do the thing"}, runner.SelftestArgv()} {
		sub := newExecPerDrain()
		org, ref, out := remoteRun(t, sub, "wardyn/agent:latest")
		if _, err := selftest(t, org, ref); err == nil || !strings.Contains(err.Error(), "owes no BYOI selftest") {
			t.Fatalf("selftest of a non-BYOI sandbox = %v, want refused", err)
		}
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

// The selftest is one call per BYOI sandbox, and an Exec is never diverted: a
// task that is itself `--selftest` is the run's output, and a second selftest
// is refused.
func TestRemoteBYOITaskNamedSelftestIsTheRunOutput(t *testing.T) {
	sub := newExecPerDrain()
	org, ref, out := remoteRun(t, sub, "wardyn-byoi/"+uuid.NewString()+":latest")
	if _, err := selftest(t, org, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := selftest(t, org, ref); err == nil || !strings.Contains(err.Error(), "owes no BYOI selftest") {
		t.Fatalf("second selftest = %v, want refused", err)
	}
	execAndWait(t, org, ref, selftestArgvForTest)
	got, err := out.sealed(t)
	if got != "selftest output\n" || err != nil {
		t.Fatalf("sealed output = %q, %v; want the task's output with a clean end", got, err)
	}
}

// The selftest call carries no argv the runner obeys: whatever argv a caller
// adds to it, the runner execs only its own selftest.
func TestRemoteBYOISelftestRunsOnlyItsOwnArgv(t *testing.T) {
	sub := newExecPerDrain()
	id := uuid.NewString()
	d, err := newTestDispatcher(Config{RunnerID: id, Substrate: sub, CheckSelftest: admitTaskGeneration,
		BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
		Created:      func(context.Context, runner.SandboxSpec, string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	sb, err := sub.CreateSandbox(t.Context(), runner.SandboxSpec{RunID: uuid.New(), RunnerID: id})
	if err != nil {
		t.Fatal(err)
	}
	d.awaitSelftest(sb.Ref, "wardyn-byoi/x:latest")
	raw, _ := json.Marshal(map[string]any{"ref": placement.RunnerRefPrefix(id) + sb.Ref, "execution_generation": taskGeneration,
		"argv": []string{"/bin/sh", "-c", "anything"}})
	if _, err := d.selftest(t.Context(), raw); err != nil {
		t.Fatal(err)
	}
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if len(sub.argvs) != 1 || !slices.Equal(sub.argvs[0], selftestArgvForTest) {
		t.Fatalf("exec'd %v, want only the selftest", sub.argvs)
	}
}

// A selftest under any generation but the lease's unattempted one is refused
// before anything runs, and leaves the sandbox's selftest owed.
func TestRemoteBYOISelftestRefusedOutsideItsGeneration(t *testing.T) {
	sub := newExecPerDrain()
	refuse := func(context.Context, string, uuid.UUID) error {
		return errors.New("execution generation already attempted")
	}
	org, ref, _ := remoteRunWith(t, sub, "wardyn-byoi/"+uuid.NewString()+":latest", refuse, true)
	if _, err := selftest(t, org, ref); err == nil || !strings.Contains(err.Error(), "already attempted") {
		t.Fatalf("selftest = %v, want the runner's refusal", err)
	}
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if len(sub.argvs) != 0 {
		t.Fatalf("a refused selftest exec'd %v", sub.argvs)
	}
}

// A runner that does not advertise the selftest (one from before it) is
// refused by the org before anything is sent: the run fails closed with the
// reason, never skipping the check.
func TestRemoteBYOISelftestRefusedOnARunnerWithoutIt(t *testing.T) {
	sub := newExecPerDrain()
	org, ref, _ := remoteRunWith(t, sub, "wardyn-byoi/"+uuid.NewString()+":latest", admitTaskGeneration, false)
	if _, err := selftest(t, org, ref); err == nil || !strings.Contains(err.Error(), "does not support the BYOI selftest") {
		t.Fatalf("selftest = %v, want the org's refusal", err)
	}
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if len(sub.argvs) != 0 {
		t.Fatalf("the selftest reached the runner: %v", sub.argvs)
	}
}

// hangingSelftest is a selftest that never exits.
type hangingSelftest struct{ *execPerDrain }

func (h hangingSelftest) Wait(ctx context.Context, _ string) (int, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}

// A selftest that never exits is bounded by the runner itself.
func TestBYOISelftestIsBoundedOnTheRunner(t *testing.T) {
	prev := selftestTimeout
	selftestTimeout = 50 * time.Millisecond
	t.Cleanup(func() { selftestTimeout = prev })
	sub := hangingSelftest{newExecPerDrain()}
	id := uuid.NewString()
	d, err := newTestDispatcher(Config{RunnerID: id, Substrate: sub, CheckSelftest: admitTaskGeneration,
		BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
		Created:      func(context.Context, runner.SandboxSpec, string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	sb, err := sub.CreateSandbox(t.Context(), runner.SandboxSpec{RunID: uuid.New(), RunnerID: id})
	if err != nil {
		t.Fatal(err)
	}
	d.awaitSelftest(sb.Ref, "wardyn-byoi/x:latest")
	raw, _ := json.Marshal(runnerwire.SelftestArgs{Ref: placement.RunnerRefPrefix(id) + sb.Ref, ExecutionGeneration: taskGeneration})
	if _, err := d.selftest(t.Context(), raw); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hanging selftest = %v, want the runner's deadline", err)
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

func owedSelftests(d *Dispatcher) (n int) {
	d.selftests.Range(func(any, any) bool { n++; return true })
	return n
}

// A sandbox's owed selftest is dropped once the task's Exec has attempted its
// generation (the selftest could no longer be admitted) or the sandbox is
// killed, so the runner's table does not grow with every BYOI create.
func TestBYOISelftestOwedEntriesDoNotAccumulate(t *testing.T) {
	image := "wardyn-byoi/" + uuid.NewString() + ":latest"
	for _, end := range []string{"exec", "kill"} {
		sub := newExecPerDrain()
		org, d, ref, _ := remoteRunD(t, sub, image, admitTaskGeneration, true)
		if got := owedSelftests(d); got != 1 {
			t.Fatalf("owed selftests after create = %d, want 1", got)
		}
		if end == "exec" {
			execAndWait(t, org, ref, []string{"/usr/local/bin/agent-run", "do the thing"})
		} else if err := org.KillSandbox(t.Context(), ref); err != nil {
			t.Fatal(err)
		}
		if got := owedSelftests(d); got != 0 {
			t.Fatalf("owed selftests after %s = %d, want 0", end, got)
		}
	}
}
