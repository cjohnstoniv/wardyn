// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build e2etmux && !docker && linux

// Package localtmux is a TEST-ONLY substrate for the chromium e2e gate. It
// opens the same interactive attach the Docker substrate opens (a tmux session
// named "wardyn" under a PTY), but against a tmux on this host, on a throwaway
// socket, so the production attach handler (internal/api/attach.go) and its
// holder and pump logic run unchanged against a real tmux.
//
// It is compiled only with `-tags e2etmux` (scripts/e2e-backend.sh builds it
// beside the normal wardynd), so a release build never links it.
package localtmux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"unsafe"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Name is both the -runner value and the runner target stored on runs.
// agent_runs.runner_target only admits "docker", "k8s" and "none", so the
// harness claims "docker"; the build tag excludes the real Docker substrate
// from the same binary, so the name is never registered twice.
const Name = "docker"

// attachScript is the tmux branch both real drivers run (runner.TmuxAttachSh,
// with its version-gated tmux settings). The docker package is behind the
// docker build tag and cannot be imported here; attach_script_test.go pins
// docker/session.go to the same fragment.
var attachScript = runner.TmuxAttachSh

func init() {
	substrate.Register(Name, func(substrate.Deps) (substrate.Substrate, error) {
		return New(os.Getenv("WARDYN_E2E_TMUX_SOCKET"), os.Getenv("WARDYN_E2E_TMUX_CONF"))
	})
}

// Substrate runs every attach against one tmux server on socket.
type Substrate struct {
	tmux, socket, dir string
}

// New fails, never degrades, when tmux, the socket name or the config is
// missing: a harness that silently skipped would turn the gate green with
// nothing tested.
func New(socket, conf string) (*Substrate, error) {
	if socket == "" || conf == "" {
		return nil, errors.New("localtmux: WARDYN_E2E_TMUX_SOCKET and WARDYN_E2E_TMUX_CONF are required")
	}
	if _, err := os.Stat(conf); err != nil {
		return nil, fmt.Errorf("localtmux: tmux config: %w", err)
	}
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		return nil, fmt.Errorf("localtmux: the real-tmux fixture is required but tmux is not installed: %w", err)
	}
	dir, err := os.MkdirTemp("", "wardyn-localtmux-")
	if err != nil {
		return nil, err
	}
	// The attach script runs a bare `tmux`; a shim on PATH adds the throwaway
	// socket and the production config, so the script stays verbatim.
	shim := fmt.Sprintf("#!/bin/sh\nexec %q -L %q -f %q \"$@\"\n", tmux, socket, conf)
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(shim), 0o755); err != nil {
		return nil, err
	}
	return &Substrate{tmux: tmux, socket: socket, dir: dir}, nil
}

func (s *Substrate) Name() string { return Name }

func (s *Substrate) Classes(context.Context) (substrate.ClassSupport, error) {
	return substrate.ClassSupport{Classes: []types.ConfinementClass{types.CC1}}, nil
}

// CreateSandbox never completes: the harness seeds its runs by SQL, and a
// dispatch that failed would race the seed's state rewrite.
func (s *Substrate) CreateSandbox(ctx context.Context, _ runner.SandboxSpec) (runner.Sandbox, error) {
	<-ctx.Done()
	return runner.Sandbox{}, ctx.Err()
}

func (s *Substrate) Exec(context.Context, string, []string) (string, error) {
	return "", errors.New("localtmux: exec unsupported")
}

func (s *Substrate) Wait(ctx context.Context, _ string) (int, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}

func (s *Substrate) ExecStream(context.Context, string, runner.ExecSpec) (*runner.ExecSession, error) {
	return nil, runner.ErrExecStreamUnsupported
}

func (s *Substrate) Status(context.Context, string) (runner.Status, error) {
	return runner.Status{State: types.RunRunning}, nil
}

func (s *Substrate) AgentStatus(ctx context.Context, ref, _ string) (runner.Status, error) {
	return s.Status(ctx, ref)
}

func (s *Substrate) StopSandbox(ctx context.Context, _ string) error { return s.kill(ctx) }
func (s *Substrate) KillSandbox(ctx context.Context, _ string) error { return s.kill(ctx) }

func (s *Substrate) kill(ctx context.Context) error {
	_ = exec.CommandContext(ctx, s.tmux, "-L", s.socket, "kill-server").Run()
	return nil
}

// Attach opens the tmux attach client under a fresh PTY. Like the Docker
// substrate it starts the PTY at the requested size, or at 0x0 when none is
// given (tmux then assumes 80x24).
func (s *Substrate) Attach(_ context.Context, _ string, opts runner.AttachOptions) (runner.Session, error) {
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	var n uint32
	var unlock int32
	if err := ioctl(ptmx, syscall.TIOCGPTN, unsafe.Pointer(&n)); err != nil {
		ptmx.Close()
		return nil, err
	}
	if err := ioctl(ptmx, syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err != nil {
		ptmx.Close()
		return nil, err
	}
	pts, err := os.OpenFile("/dev/pts/"+strconv.Itoa(int(n)), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		ptmx.Close()
		return nil, err
	}
	defer pts.Close()
	if opts.Cols > 0 && opts.Rows > 0 {
		if err := setSize(ptmx, opts.Cols, opts.Rows); err != nil {
			ptmx.Close()
			return nil, err
		}
	}

	cmd := exec.Command("/bin/sh", "-c", attachScript)
	cmd.Dir = s.dir
	cmd.Env = []string{
		"PATH=" + s.dir + ":" + os.Getenv("PATH"),
		"HOME=" + s.dir,
		"TERM=xterm-256color",
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = pts, pts, pts
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		ptmx.Close()
		return nil, err
	}
	go func() { _ = cmd.Wait() }()
	return &session{ptmx: ptmx, cmd: cmd}, nil
}

type session struct {
	ptmx *os.File
	cmd  *exec.Cmd
	once sync.Once
}

func (p *session) Read(b []byte) (int, error) {
	n, err := p.ptmx.Read(b)
	if err != nil && n == 0 && !errors.Is(err, io.EOF) {
		// The master read fails with EIO once the slave side is gone.
		return 0, io.EOF
	}
	return n, err
}

func (p *session) Write(b []byte) (int, error) { return p.ptmx.Write(b) }

func (p *session) Resize(_ context.Context, cols, rows uint16) error {
	if cols == 0 || rows == 0 {
		return nil
	}
	return setSize(p.ptmx, cols, rows)
}

// Close ends only this attach client; the tmux server and its session stay, as
// a detach does in a sandbox.
func (p *session) Close() error {
	p.once.Do(func() {
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGHUP)
		_ = p.ptmx.Close()
	})
	return nil
}

func ioctl(f *os.File, req uintptr, arg unsafe.Pointer) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), req, uintptr(arg)); e != 0 {
		return e
	}
	return nil
}

func setSize(f *os.File, cols, rows uint16) error {
	ws := struct{ Row, Col, X, Y uint16 }{Row: rows, Col: cols}
	return ioctl(f, syscall.TIOCSWINSZ, unsafe.Pointer(&ws))
}
