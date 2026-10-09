// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnertest

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Fake is an in-memory substrate.Substrate that implements every optional
// interface the 0.9 runner serves, and records what it was asked. It stands in
// for the local Docker substrate behind a runner.
type Fake struct {
	Support  substrate.ClassSupport
	ExitCode int // the agent's exit code, and an exec's

	mu         sync.Mutex
	sandboxes  map[string]uuid.UUID // local ref -> run id
	Calls      []string             // "verb ref"
	Created    []runner.SandboxSpec
	Resizes    [][2]uint16
	Orphans    []uuid.UUID // runs the sweep may remove
	execed     map[string]bool
	StoppedRef map[string]bool
}

// NewFake offers CC1 and CC2.
func NewFake() *Fake {
	return &Fake{
		Support:   substrate.ClassSupport{Classes: []types.ConfinementClass{types.CC1, types.CC2}, StructuralEgress: true},
		sandboxes: map[string]uuid.UUID{}, execed: map[string]bool{}, StoppedRef: map[string]bool{},
	}
}

var (
	_ substrate.Substrate    = (*Fake)(nil)
	_ runner.SandboxEnder    = (*Fake)(nil)
	_ runner.ProxyStopper    = (*Fake)(nil)
	_ runner.ProxyReviver    = (*Fake)(nil)
	_ runner.SandboxStarter  = (*Fake)(nil)
	_ runner.DriveProber     = (*Fake)(nil)
	_ runner.OutputRecoverer = (*Fake)(nil)
)

func (f *Fake) note(verb, ref string) {
	f.mu.Lock()
	f.Calls = append(f.Calls, verb+" "+ref)
	f.mu.Unlock()
}

// Called reports whether "verb ref" was seen.
func (f *Fake) Called(verb, ref string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.Calls {
		if c == verb+" "+ref {
			return true
		}
	}
	return false
}

func (f *Fake) Name() string { return "docker" }

func (f *Fake) Classes(context.Context) (substrate.ClassSupport, error) { return f.Support, nil }

func (f *Fake) CreateSandbox(_ context.Context, spec runner.SandboxSpec) (runner.Sandbox, error) {
	spec.NotifyWaiting("pulling image")
	spec.NotifyWaiting("starting sandbox")
	ref := "fake-" + spec.RunID.String()
	f.mu.Lock()
	f.sandboxes[ref] = spec.RunID
	f.Created = append(f.Created, spec)
	f.mu.Unlock()
	if spec.ExecOutput != nil {
		end := runner.BeginOutputDrain(spec.ExecOutput)
		go func() {
			_, err := io.WriteString(spec.ExecOutput, "agent output of "+ref+"\n")
			end(err)
		}()
	}
	return runner.Sandbox{Ref: ref, Driver: "docker", EnforcedClass: spec.ConfinementClass}, nil
}

func (f *Fake) exists(ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sandboxes[ref]; !ok || f.StoppedRef[ref] {
		return runner.ErrSandboxGone
	}
	return nil
}

func (f *Fake) Exec(_ context.Context, ref string, argv []string) (string, error) {
	f.note("exec", ref)
	if err := f.exists(ref); err != nil {
		return "", err
	}
	f.mu.Lock()
	f.execed[ref] = true
	f.mu.Unlock()
	return "exec-" + ref, nil
}

func (f *Fake) Wait(_ context.Context, ref string) (int, error) {
	f.note("wait", ref)
	f.mu.Lock()
	ok := f.execed[ref]
	f.mu.Unlock()
	if !ok {
		return 0, runner.ErrExecNeverStarted
	}
	return f.ExitCode, nil
}

func (f *Fake) Status(_ context.Context, ref string) (runner.Status, error) {
	f.note("status", ref)
	if err := f.exists(ref); err != nil {
		return runner.Status{}, err
	}
	return runner.Status{State: types.RunRunning, Message: "fake"}, nil
}

func (f *Fake) AgentStatus(ctx context.Context, ref, agentExecID string) (runner.Status, error) {
	f.note("agent_status "+agentExecID, ref)
	return f.Status(ctx, ref)
}

func (f *Fake) StopSandbox(_ context.Context, ref string) error {
	f.note("stop", ref)
	f.drop(ref)
	return nil
}
func (f *Fake) KillSandbox(_ context.Context, ref string) error {
	f.note("kill", ref)
	f.drop(ref)
	return nil
}

func (f *Fake) drop(ref string) {
	f.mu.Lock()
	delete(f.sandboxes, ref)
	f.mu.Unlock()
}

func (f *Fake) EndSandbox(_ context.Context, ref string) error {
	f.note("end", ref)
	f.mu.Lock()
	f.StoppedRef[ref] = true
	f.mu.Unlock()
	return nil
}

func (f *Fake) StopProxy(_ context.Context, ref string) error { f.note("stop_proxy", ref); return nil }

func (f *Fake) StartSandbox(_ context.Context, ref string) error {
	f.note("start", ref)
	f.mu.Lock()
	delete(f.StoppedRef, ref)
	f.mu.Unlock()
	return nil
}

func (f *Fake) CanReplaceProxy(_ context.Context, ref string) error {
	f.note("can_replace", ref)
	return nil
}
func (f *Fake) ReplaceProxy(_ context.Context, ref string, cfg []byte) error {
	f.note("replace_proxy "+string(cfg), ref)
	return nil
}
func (f *Fake) EnsureProxyImage(context.Context) error { f.note("ensure_proxy_image", ""); return nil }

func (f *Fake) ProbeDrive(_ context.Context, m types.DriveMount) (runner.DriveProbe, error) {
	return runner.DriveProbe{Result: runner.DriveProbeReadable, Detail: "fake"}, nil
}

func (f *Fake) RecoverOutput(_ context.Context, ref string, w io.Writer) error {
	f.note("recover_output", ref)
	_, err := io.WriteString(w, "recovered "+ref)
	return err
}

func (f *Fake) SweepOrphanedSandboxes(_ context.Context, _ time.Duration, isOrphan func(uuid.UUID) bool) (int, error) {
	f.mu.Lock()
	cands := append([]uuid.UUID(nil), f.Orphans...)
	f.mu.Unlock()
	n := 0
	for _, id := range cands {
		if isOrphan(id) {
			n++
			f.mu.Lock()
			f.Orphans = removeID(f.Orphans, id)
			f.mu.Unlock()
		}
	}
	return n, nil
}

func removeID(ids []uuid.UUID, id uuid.UUID) []uuid.UUID {
	out := ids[:0:0]
	for _, v := range ids {
		if v != id {
			out = append(out, v)
		}
	}
	return out
}

// Attach is an echo terminal: what is written is read back upper-cased by one.
func (f *Fake) Attach(_ context.Context, ref string, _ runner.AttachOptions) (runner.Session, error) {
	f.note("attach", ref)
	if err := f.exists(ref); err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	return &echoSession{pr: pr, pw: pw, f: f}, nil
}

type echoSession struct {
	pr *io.PipeReader
	pw *io.PipeWriter
	f  *Fake
}

func (e *echoSession) Read(b []byte) (int, error)  { return e.pr.Read(b) }
func (e *echoSession) Write(b []byte) (int, error) { return e.pw.Write(b) }
func (e *echoSession) Close() error                { _ = e.pw.Close(); return e.pr.Close() }
func (e *echoSession) Resize(_ context.Context, cols, rows uint16) error {
	e.f.mu.Lock()
	e.f.Resizes = append(e.f.Resizes, [2]uint16{cols, rows})
	e.f.mu.Unlock()
	return nil
}

// ExecStream is `cat` with a stderr line: stdin is echoed to stdout; stderr
// carries the argv; Wait answers ExitCode once stdin is closed.
func (f *Fake) ExecStream(_ context.Context, ref string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	f.note("exec_stream", ref)
	if err := f.exists(ref); err != nil {
		return nil, err
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(outW, inR)
		_ = outW.Close()
		close(done)
	}()
	go func() {
		_, _ = io.WriteString(errW, "argv:"+spec.Argv[0]+"\n")
		_ = errW.Close()
	}()
	sess := &runner.ExecSession{
		Stdin:  inW,
		Stdout: outR,
		Resize: func(cols, rows uint16) error {
			f.mu.Lock()
			f.Resizes = append(f.Resizes, [2]uint16{cols, rows})
			f.mu.Unlock()
			return nil
		},
		Wait:  func() (int, error) { <-done; return f.ExitCode, nil },
		Close: func() error { _ = inW.Close(); return outR.Close() },
	}
	if !spec.TTY {
		sess.Stderr = errR
	}
	return sess, nil
}
