// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// fakeExecutor is the test double Driver.execFactory hands back instead of a
// real SPDY/WebSocket executor (which would dial an apiserver the fake
// clientset never runs). It records newExecutor's own call args
// (podName/container/cmd/stdin/tty) AND, separately, the remotecommand.
// StreamOptions the k8sStream goroutine actually builds when it runs — the
// two are distinct facts about distinct call sites, so a test can pin each on
// its own.
type fakeExecutor struct {
	podName, container string
	cmd                []string
	stdinArg, ttyArg   bool

	mu        sync.Mutex
	started   bool
	gotTTY    bool
	gotStdin  bool
	gotStdout bool
	gotStderr bool
	resizes   chan remotecommand.TerminalSize
}

func newFakeExecutor() *fakeExecutor {
	return &fakeExecutor{resizes: make(chan remotecommand.TerminalSize, 8)}
}

// execFactory is a Driver.execFactory value bound to this fake.
func (f *fakeExecutor) execFactory(podName, container string, cmd []string, stdin, tty bool) (remotecommand.Executor, error) {
	f.podName, f.container, f.cmd, f.stdinArg, f.ttyArg = podName, container, cmd, stdin, tty
	return f, nil
}

func (f *fakeExecutor) StreamWithContext(ctx context.Context, opts remotecommand.StreamOptions) error {
	f.mu.Lock()
	f.started = true
	f.gotTTY = opts.Tty
	f.gotStdin = opts.Stdin != nil
	f.gotStdout = opts.Stdout != nil
	f.gotStderr = opts.Stderr != nil
	f.mu.Unlock()

	// Drain the size queue for as long as the stream is "connected", the same
	// way a real executor would forward TerminalSize events — so Resize
	// (which pushes into k8sStream.sizeQ) is observable from outside.
	if opts.TerminalSizeQueue != nil {
		go func() {
			for {
				sz := opts.TerminalSizeQueue.Next()
				if sz == nil {
					return
				}
				select {
				case f.resizes <- *sz:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	<-ctx.Done() // "connected" until Close cancels the stream context
	return nil
}

func (f *fakeExecutor) Stream(opts remotecommand.StreamOptions) error {
	return f.StreamWithContext(context.Background(), opts)
}

func (f *fakeExecutor) snapshot() (tty, stdin, stdout, stderr bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotTTY, f.gotStdin, f.gotStdout, f.gotStderr
}

func (f *fakeExecutor) waitStarted(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		started := f.started
		f.mu.Unlock()
		if started {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("fakeExecutor.StreamWithContext never started")
}

// TestAttach_RejectsEmptyRef pins Attach("") erroring before any pod lookup.
func TestAttach_RejectsEmptyRef(t *testing.T) {
	d, _ := newTestDriver(t, Config{})
	if _, err := d.Attach(context.Background(), "", runner.AttachOptions{}); err == nil {
		t.Error("Attach with empty ref must error")
	}
}

// TestAttach_MissingPodProducesGetPodSentence pins the wording Attach uses
// when the sandbox ref names no pod: callers (and operators reading logs)
// key off "get pod" to tell this apart from other Attach failures.
func TestAttach_MissingPodProducesGetPodSentence(t *testing.T) {
	d, _ := newTestDriver(t, Config{})
	_, err := d.Attach(context.Background(), "does-not-exist", runner.AttachOptions{})
	if err == nil {
		t.Fatal("Attach against a missing pod must error")
	}
	if !strings.Contains(err.Error(), "get pod") {
		t.Errorf("Attach error = %q, want it to contain the %q sentence", err.Error(), "get pod")
	}
}

// TestAttach_ResolveExecContainerPicksAgentWhenEphemeralExists pins
// resolveExecContainer's choice directly (no Driver involved): the ephemeral
// "wardyn-agent" exec container when Exec has already added one, else the
// pod's main placeholder container.
func TestAttach_ResolveExecContainerPicksAgentWhenEphemeralExists(t *testing.T) {
	withEphemeral := &corev1.Pod{Spec: corev1.PodSpec{
		EphemeralContainers: []corev1.EphemeralContainer{
			{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: execContainerName}},
		},
	}}
	if got := resolveExecContainer(withEphemeral); got != execContainerName {
		t.Errorf("resolveExecContainer with an ephemeral %q container = %q, want %q", execContainerName, got, execContainerName)
	}

	noEphemeral := &corev1.Pod{}
	if got := resolveExecContainer(noEphemeral); got != mainContainerName {
		t.Errorf("resolveExecContainer with no ephemeral container = %q, want %q", got, mainContainerName)
	}
}

// TestAttach_ShellScriptKeepsFallbackChainAndTermExport pins
// attachShellScript's literal content. The expected text is spelled out
// here, NOT built from attachShellScript itself (or from any shared
// constant/builder the production code also uses) — a test that compared the
// constant to a value derived from the constant would keep passing even if
// the tmux/bash/sh chain or the TERM export were deleted, per issue #130's
// warning.
func TestAttach_ShellScriptKeepsFallbackChainAndTermExport(t *testing.T) {
	const want = `export TERM=xterm-256color LANG=C.UTF-8 LC_ALL=C.UTF-8
if command -v tmux >/dev/null 2>&1; then exec tmux new-session -A -s wardyn bash; elif command -v bash >/dev/null 2>&1; then exec bash -i; else exec /bin/sh -i; fi`
	if attachShellScript != want {
		t.Errorf("attachShellScript =\n%s\nwant\n%s", attachShellScript, want)
	}
}

// TestAttach_ExecSemanticsResizeAndClose drives Attach end to end through the
// execFactory seam: no live cluster, the fake clientset backs the pod Get and
// the fakeExecutor stands in for the SPDY/WebSocket exec. Pins the exec
// semantics (TTY on, stdin on, stderr off — read from the recorded
// StreamOptions the k8sStream goroutine actually builds, never compared
// against the code's own choice), that Resize reaches the terminal size
// queue, and that Close is idempotent.
func TestAttach_ExecSemanticsResizeAndClose(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	runID := uuid.New()
	ref := createAgentPodFixture(t, cs, runID, "wardyn/agent-claude:local", nil)

	fe := newFakeExecutor()
	d.execFactory = fe.execFactory

	sess, err := d.Attach(context.Background(), ref, runner.AttachOptions{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// newExecutor's own call args: resolved against the recorded argv, never
	// attachShellScript compared to itself.
	if fe.podName != ref {
		t.Errorf("executor podName = %q, want %q", fe.podName, ref)
	}
	if fe.container != mainContainerName {
		t.Errorf("executor container = %q, want %q (no ephemeral exec container exists yet)", fe.container, mainContainerName)
	}
	wantCmd := []string{"/bin/sh", "-c", attachShellScript}
	if !slices.Equal(fe.cmd, wantCmd) {
		t.Errorf("executor cmd = %v, want %v", fe.cmd, wantCmd)
	}
	if !fe.stdinArg || !fe.ttyArg {
		t.Errorf("newExecutor(stdin=%v, tty=%v), want both true", fe.stdinArg, fe.ttyArg)
	}

	fe.waitStarted(t)
	tty, stdin, stdout, stderr := fe.snapshot()
	if !tty || !stdin || !stdout || stderr {
		t.Errorf("StreamOptions = {Tty:%v Stdin:%v Stdout:%v Stderr:%v}, want {true true true false}", tty, stdin, stdout, stderr)
	}

	// Attach seeds the initial PTY size from AttachOptions.
	select {
	case sz := <-fe.resizes:
		if sz.Width != 80 || sz.Height != 24 {
			t.Errorf("initial size = %+v, want 80x24", sz)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("initial resize never reached the size queue")
	}

	if err := sess.Resize(context.Background(), 120, 40); err != nil {
		t.Errorf("Resize: %v", err)
	}
	select {
	case sz := <-fe.resizes:
		if sz.Width != 120 || sz.Height != 40 {
			t.Errorf("resize = %+v, want 120x40", sz)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Resize never reached the size queue")
	}

	if err := sess.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Errorf("second Close must be idempotent, got %v", err)
	}
}
