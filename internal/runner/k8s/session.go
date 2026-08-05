// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	k8sexec "k8s.io/client-go/util/exec"
	"k8s.io/streaming/pkg/httpstream"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// attachShellScript is docker session.go's attachShell script, byte-identical
// in its shell logic (tmux persistent session, bash fallback, sh last
// resort — see internal/runner/docker/session.go's attachShell doc for the
// full rationale) but reshaped for k8s's exec subresource, which has NO Env
// field: PodExecOptions carries Stdin/Stdout/Stderr/TTY/Container/Command
// only, so TERM/LANG/LC_ALL ride as shell-level `export` statements ahead of
// the same tmux/bash/sh chain instead of a separate Env list. Duplicated
// rather than hoisted: docker's attachShell lives in a `//go:build docker`
// file this package cannot import, and internal/runner's tagless files are
// outside this lane's touch scope. Keep the shell chain in lockstep with
// docker's attachShell if that ever changes.
const attachShellScript = `export TERM=xterm-256color LANG=C.UTF-8 LC_ALL=C.UTF-8
if command -v tmux >/dev/null 2>&1; then exec tmux new-session -A -s wardyn bash; elif command -v bash >/dev/null 2>&1; then exec bash -i; else exec /bin/sh -i; fi`

// resolveExecContainer picks the container Attach/ExecStream target: the
// ephemeral "wardyn-agent" exec container if Exec has already added one
// (so an attach/exec-stream shares the real task's environment), else the
// pod's main placeholder container (an interactive run with no task exec'd
// yet, or ExecStream opened before any Exec).
func resolveExecContainer(pod *corev1.Pod) string {
	for _, ec := range pod.Spec.EphemeralContainers {
		if ec.Name == execContainerName {
			return execContainerName
		}
	}
	return mainContainerName
}

// Attach opens a NEW interactive exec (attachShellScript, via the exec
// subresource — NOT k8s's native "attach" subresource, which targets a pod's
// existing main process rather than starting a fresh one) inside the running
// sandbox and returns a live PTY runner.Session. Mirrors docker's Attach: a
// distinct, human-owned stream, never registered as the tracked agent exec,
// bounded by the sandbox's existing NetworkPolicy confinement (invariant 3 —
// no new network path opens; the stream flows apiserver -> kubelet -> pod).
func (d *Driver) Attach(ctx context.Context, ref string, opts runner.AttachOptions) (runner.Session, error) {
	if ref == "" {
		return nil, errors.New("k8s: attach: empty sandbox ref")
	}
	pod, err := d.clientset.CoreV1().Pods(d.cfg.Namespace).Get(ctx, ref, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("k8s: attach: get pod %q: %w", ref, err)
	}
	container := resolveExecContainer(pod)

	exec, err := d.newExecutor(ref, container, []string{"/bin/sh", "-c", attachShellScript}, true, true)
	if err != nil {
		return nil, err
	}
	stream := newK8sStream(ctx, exec, true)
	if opts.Cols > 0 && opts.Rows > 0 {
		_ = stream.resize(opts.Cols, opts.Rows) // seed the initial PTY size
	}
	return &k8sSession{stream: stream}, nil
}

// k8sSession is the runner.Session backed by a k8sStream over the exec
// subresource with TTY semantics (stdout carries the merged PTY stream).
type k8sSession struct{ stream *k8sStream }

var _ runner.Session = (*k8sSession)(nil)

func (s *k8sSession) Read(p []byte) (int, error)  { return s.stream.stdoutR.Read(p) }
func (s *k8sSession) Write(p []byte) (int, error) { return s.stream.stdinW.Write(p) }

func (s *k8sSession) Resize(_ context.Context, cols, rows uint16) error {
	return s.stream.resize(cols, rows)
}

// Close tears down ONLY this exec stream — never the sandbox, the agent, or
// any sidecar.
func (s *k8sSession) Close() error { return s.stream.close() }

// ExecStream launches spec.Argv inside ref as a fresh, streamable exec via
// the exec subresource — repeatable against the same ref (unlike Exec's
// one-shot ephemeral container; see substrate.Substrate.ExecStream's doc).
// Env rides the same shell-export prefix trick Attach uses (the exec
// subresource has no Env field) ONLY when spec.Env is non-empty, so the
// common case (no exec-scoped env) runs argv directly with no shell
// dependency in the target image.
func (d *Driver) ExecStream(ctx context.Context, ref string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	if len(spec.Argv) == 0 {
		return nil, errors.New("k8s: exec stream: empty argv")
	}
	pod, err := d.clientset.CoreV1().Pods(d.cfg.Namespace).Get(ctx, ref, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("k8s: exec stream: get pod %q: %w", ref, err)
	}
	container := resolveExecContainer(pod)

	cmd := spec.Argv
	if len(spec.Env) > 0 {
		cmd = []string{"/bin/sh", "-c", envWrapScript(spec.Env, spec.Argv)}
	}

	exec, err := d.newExecutor(ref, container, cmd, true, spec.TTY)
	if err != nil {
		return nil, err
	}
	stream := newK8sStream(ctx, exec, spec.TTY)
	if spec.TTY && spec.Cols > 0 && spec.Rows > 0 {
		_ = stream.resize(spec.Cols, spec.Rows)
	}

	// Present, never nil, so callers can read it uniformly without a TTY
	// check — mirrors docker's ExecStream (see ExecSession.Stderr's doc).
	var stderr io.Reader = bytes.NewReader(nil)
	if stream.stderrR != nil {
		stderr = stream.stderrR
	}
	return &runner.ExecSession{
		Stdin:  stream.stdinW,
		Stdout: stream.stdoutR,
		Stderr: stderr,
		Resize: stream.resize,
		Wait:   stream.wait,
		Close:  stream.close,
	}, nil
}

// envWrapScript builds `export K='V' ...; exec 'argv0' 'argv1' ...` so
// exec-scoped env survives the exec subresource's lack of an Env field.
// Every value is single-quoted (POSIX-safe: an embedded single quote is
// closed, backslash-escaped, and reopened, the standard shQuote trick), so
// argv/env values containing spaces, globs, or shell metacharacters pass
// through literally.
func envWrapScript(env []string, argv []string) string {
	var b strings.Builder
	b.WriteString("export")
	for _, kv := range env {
		b.WriteByte(' ')
		if i := strings.IndexByte(kv, '='); i >= 0 {
			b.WriteString(kv[:i+1])
			b.WriteString(shQuote(kv[i+1:]))
		} else {
			b.WriteString(shQuote(kv)) // malformed (no '='): pass through quoted, harmless
		}
	}
	b.WriteString("; exec")
	for _, a := range argv {
		b.WriteByte(' ')
		b.WriteString(shQuote(a))
	}
	return b.String()
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// newExecutor builds the exec-subresource request and wraps it in a
// websocket-primary/SPDY-fallback Executor — the same recipe kubectl exec
// uses (NewFallbackExecutor(websocket, spdy), falling back on an upgrade
// failure).
func (d *Driver) newExecutor(podName, container string, cmd []string, stdin, tty bool) (remotecommand.Executor, error) {
	req := d.clientset.CoreV1().RESTClient().Post().
		Namespace(d.cfg.Namespace).
		Resource("pods").
		Name(podName).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			Stdin:     stdin,
			Stdout:    true,
			Stderr:    !tty, // TTY merges stderr onto stdout; no separate stream to request
			TTY:       tty,
		}, scheme.ParameterCodec)

	spdyExec, err := remotecommand.NewSPDYExecutor(d.restConfig, http.MethodPost, req.URL())
	if err != nil {
		return nil, fmt.Errorf("k8s: build spdy executor: %w", err)
	}
	wsExec, err := remotecommand.NewWebSocketExecutor(d.restConfig, http.MethodGet, req.URL().String())
	if err != nil {
		return nil, fmt.Errorf("k8s: build websocket executor: %w", err)
	}
	exec, err := remotecommand.NewFallbackExecutor(wsExec, spdyExec, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
	if err != nil {
		return nil, fmt.Errorf("k8s: build fallback executor: %w", err)
	}
	return exec, nil
}

// k8sStream bridges remotecommand's push-based StreamWithContext (it takes
// an io.Reader/io.Writer trio and blocks until the exec ends) to the
// pull-based io.Reader/io.WriteCloser shapes runner.Session and
// runner.ExecSession need — an io.Pipe per direction, fed by one background
// goroutine running the executor, exactly mirroring how docker's ExecStream
// bridges its hijacked connection via stdcopy into the same shape.
type k8sStream struct {
	stdinW  *io.PipeWriter
	stdoutR *io.PipeReader
	stderrR *io.PipeReader // nil when tty (PTY semantics merge stderr onto stdout)
	sizeQ   *termSizeQueue
	cancel  context.CancelFunc
	done    chan struct{}

	// exitCode/exitErr are set once, before done closes; safe to read after
	// <-done with no further synchronization (happens-before via the channel).
	exitCode int
	exitErr  error
}

// newK8sStream starts the background goroutine and returns immediately; the
// exec begins connecting asynchronously.
func newK8sStream(ctx context.Context, exec remotecommand.Executor, tty bool) *k8sStream {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	var stderrR *io.PipeReader
	var stderrW *io.PipeWriter
	if !tty {
		stderrR, stderrW = io.Pipe()
	}
	streamCtx, cancel := context.WithCancel(ctx)
	s := &k8sStream{
		stdinW:  stdinW,
		stdoutR: stdoutR,
		stderrR: stderrR,
		sizeQ:   newTermSizeQueue(),
		cancel:  cancel,
		done:    make(chan struct{}),
	}

	go func() {
		opts := remotecommand.StreamOptions{
			Stdin:             stdinR,
			Stdout:            stdoutW,
			Tty:               tty,
			TerminalSizeQueue: s.sizeQ,
		}
		if !tty {
			opts.Stderr = stderrW
		}
		err := exec.StreamWithContext(streamCtx, opts)

		var code int
		var streamErr error
		if err != nil {
			var codeErr k8sexec.CodeExitError
			if errors.As(err, &codeErr) {
				code = codeErr.Code
			} else {
				streamErr = err
			}
		}
		s.exitCode, s.exitErr = code, streamErr

		_ = stdoutW.CloseWithError(streamErr) // nil => readers see io.EOF
		if stderrW != nil {
			_ = stderrW.CloseWithError(streamErr)
		}
		close(s.done)
	}()
	return s
}

// wait blocks until the exec ends and returns its exit code (0 with a
// non-nil err on a genuine stream failure, e.g. a dropped connection —
// mirrors docker's pollExecExit contract).
func (s *k8sStream) wait() (int, error) {
	<-s.done
	return s.exitCode, s.exitErr
}

// resize pushes a new terminal size; degenerate sizes are ignored rather than
// erroring the stream (mirrors docker's Resize).
func (s *k8sStream) resize(cols, rows uint16) error {
	if cols == 0 || rows == 0 {
		return nil
	}
	s.sizeQ.push(cols, rows)
	return nil
}

// close cancels the streaming context (which unwinds StreamWithContext) and
// half-closes stdin. It tears down ONLY this exec stream, never the sandbox.
func (s *k8sStream) close() error {
	s.cancel()
	_ = s.stdinW.Close()
	s.sizeQ.stop()
	return nil
}

// termSizeQueue is a remotecommand.TerminalSizeQueue backed by a buffered
// channel: only the latest pushed size matters, so push drops any
// not-yet-consumed size before enqueueing the new one. stop signals Next to
// return nil (the TerminalSizeQueue contract for "monitoring has stopped")
// WITHOUT ever closing the data channel — push must stay panic-safe even
// after a concurrent close (Resize and Close can race from different
// goroutines), which closing q.ch outright would not be.
type termSizeQueue struct {
	ch        chan remotecommand.TerminalSize
	done      chan struct{}
	closeOnce sync.Once
}

func newTermSizeQueue() *termSizeQueue {
	return &termSizeQueue{ch: make(chan remotecommand.TerminalSize, 1), done: make(chan struct{})}
}

func (q *termSizeQueue) Next() *remotecommand.TerminalSize {
	select {
	case sz := <-q.ch:
		return &sz
	case <-q.done:
		return nil
	}
}

func (q *termSizeQueue) push(cols, rows uint16) {
	sz := remotecommand.TerminalSize{Width: cols, Height: rows}
	select {
	case <-q.ch: // drop a stale, not-yet-consumed size
	default:
	}
	select {
	case q.ch <- sz:
	case <-q.done:
	default:
	}
}

func (q *termSizeQueue) stop() { q.closeOnce.Do(func() { close(q.done) }) }
