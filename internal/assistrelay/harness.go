// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package assistrelay

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// allowTools lets the harness run the wardyn server's tools in print mode. With no allow-list the harness
// denies every MCP call.
const allowTools = "mcp__wardyn__*"

// StreamArgs is the stream-mode command line: one long-lived process per session, each person turn written to
// its stdin as a stream-json line. Built as argv, never through a shell.
func StreamArgs(claude, model, system, mcpConfig string) []string {
	return []string{
		claude, "-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--model", model,
		"--system-prompt", system,
		"--tools", "",
		"--mcp-config", mcpConfig,
		"--strict-mcp-config",
		"--allowedTools", allowTools,
	}
}

// ResumeArgs is the resume-mode command line: one process per person turn, the turn text on stdin and never in
// argv. The first turn of a harness session names it with --session-id; every later turn resumes it.
func ResumeArgs(claude, model, system, mcpConfig, session string, resume bool) []string {
	argv := []string{
		claude, "-p",
		"--output-format", "stream-json",
		"--verbose",
		"--model", model,
		"--system-prompt", system,
		"--tools", "",
		"--mcp-config", mcpConfig,
		"--strict-mcp-config",
		"--allowedTools", allowTools,
	}
	if resume {
		return append(argv, "--resume", session)
	}
	return append(argv, "--session-id", session)
}

// Launcher starts a harness process tree.
type Launcher interface {
	Start(ctx context.Context, argv, env []string, dir string, stdin io.Reader) (Proc, error)
}

// Proc is one running harness process tree: the harness and its MCP children.
type Proc interface {
	// Stdout is the harness's stream.
	Stdout() io.Reader
	// Stop terminates the whole tree by process group (SIGTERM, then SIGKILL after the grace period) and
	// returns only once every process in it has exited and the ones this process may reap are reaped.
	Stop() error
}

// ExecLauncher starts the harness as a real process in its own process group.
type ExecLauncher struct {
	// Grace is how long Stop waits after SIGTERM before SIGKILL; zero means 5 s.
	Grace time.Duration
}

// Start starts the harness. Its stderr is discarded: it may carry model output, which the relay never logs.
func (l ExecLauncher) Start(_ context.Context, argv, env []string, dir string, stdin io.Reader) (Proc, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty command line")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env, cmd.Dir, cmd.Stdin, cmd.Stderr = env, dir, stdin, io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	grace := l.Grace
	if grace <= 0 {
		grace = 5 * time.Second
	}
	return &execProc{cmd: cmd, out: out, grace: grace}, nil
}

type execProc struct {
	cmd   *exec.Cmd
	out   io.Reader
	grace time.Duration
	once  sync.Once
	err   error
}

func (p *execProc) Stdout() io.Reader { return p.out }

func (p *execProc) Stop() error {
	p.once.Do(func() { p.err = p.stop() })
	return p.err
}

// errTreeAlive is Stop's answer when a process of the tree outlives SIGKILL and the grace period after it.
var errTreeAlive = errors.New("a harness process did not exit")

func (p *execProc) stop() error {
	pgid := p.cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	waited := make(chan struct{})
	go func() { _ = p.cmd.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(p.grace):
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-waited
	}
	// The harness is reaped; its MCP children share its group. Kill what is left and wait until the group is
	// empty, reaping any member this process is the parent of.
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	deadline := time.Now().Add(p.grace)
	for {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return nil
		}
		var ws syscall.WaitStatus
		_, _ = syscall.Wait4(-pgid, &ws, syscall.WNOHANG, nil)
		if time.Now().After(deadline) {
			return errTreeAlive
		}
		time.Sleep(20 * time.Millisecond)
	}
}
