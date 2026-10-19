// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// Server is the guest agent's HTTP handler. The zero value is not usable; set every field.
type Server struct {
	Version string
	// Cred is who execs run as; nil runs them as the agent's own user (tests only: in a guest the
	// command sets the agent user).
	Cred *syscall.Credential
	// Home is the exec user's home, its HOME and its working directory when it exists.
	Home string
	// OutDir holds detached execs' output files. In a guest it is a tmpfs.
	OutDir string
	// Mount mounts one virtiofs share; nil refuses an Init that names one.
	Mount func(Mount) error

	mu      sync.Mutex
	inited  bool
	env     []string // Init's Env and SecretEnv, KEY=VALUE
	execs   map[string]*detached
	latest  string
	counter int
}

type detached struct {
	id   string
	path string
	mu   sync.Mutex
	done bool
	code int
	// changed is closed and replaced whenever output is written or the process exits.
	changed chan struct{}
}

func (d *detached) state() ExecState {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := ExecState{ID: d.id, Running: !d.done}
	if d.done {
		code := d.code
		s.ExitCode = &code
	}
	return s
}

func (d *detached) notify() {
	d.mu.Lock()
	close(d.changed)
	d.changed = make(chan struct{})
	d.mu.Unlock()
}

// Handler serves the requests the package doc lists.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ready", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, Ready{Version: s.Version}) })
	mux.HandleFunc("POST /v1/init", s.handleInit)
	mux.HandleFunc("POST /v1/exec", s.handleExec)
	mux.HandleFunc("GET /v1/exec/{id}", func(w http.ResponseWriter, r *http.Request) {
		if d := s.lookup(r.PathValue("id")); d != nil {
			writeJSON(w, d.state())
			return
		}
		http.Error(w, "no such exec", http.StatusNotFound)
	})
	mux.HandleFunc("GET /v1/exec/{id}/output", s.handleOutput)
	mux.HandleFunc("GET /v1/agent", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		d := s.execs[s.latest]
		s.mu.Unlock()
		if d == nil {
			http.Error(w, "no exec started this boot", http.StatusNotFound)
			return
		}
		writeJSON(w, d.state())
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) lookup(id string) *detached {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.execs[id]
}

func (s *Server) handleInit(w http.ResponseWriter, r *http.Request) {
	var in Init
	if err := json.NewDecoder(io.LimitReader(r.Body, 32<<20)).Decode(&in); err != nil {
		http.Error(w, "bad init: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inited {
		http.Error(w, "the sandbox is already initialised", http.StatusConflict)
		return
	}
	for _, f := range in.Files {
		if err := s.writeFile(f); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	for _, m := range in.Mounts {
		if s.Mount == nil {
			http.Error(w, "this agent cannot mount shares", http.StatusInternalServerError)
			return
		}
		if err := s.Mount(m); err != nil {
			http.Error(w, fmt.Sprintf("mount %s at %s: %v", m.Tag, m.Target, err), http.StatusInternalServerError)
			return
		}
	}
	s.env = s.env[:0]
	for _, m := range []map[string]string{in.Env, in.SecretEnv} {
		for k, v := range m {
			s.env = append(s.env, k+"="+v)
		}
	}
	s.inited = true
	writeJSON(w, struct{}{})
}

// writeFile writes a managed file: created fresh (an existing one is replaced, never followed), root's
// unless AgentOwned, with exactly its mode.
func (s *Server) writeFile(f File) error {
	if !filepath.IsAbs(f.Path) || filepath.Clean(f.Path) != f.Path {
		return fmt.Errorf("managed file %q is not a clean absolute path", f.Path)
	}
	if strings.Contains(f.Path, "..") {
		return fmt.Errorf("managed file %q has \"..\" in its path", f.Path)
	}
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
		return err
	}
	tmp := f.Path + ".wardyn-tmp"
	_ = os.Remove(tmp)
	fh, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, err = fh.Write(f.Content)
	if err == nil && f.AgentOwned && s.Cred != nil {
		err = fh.Chown(int(s.Cred.Uid), int(s.Cred.Gid))
	}
	if err == nil {
		err = fh.Chmod(f.Mode.Perm())
	}
	if cerr := fh.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, f.Path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

func (s *Server) command(er ExecRequest) (*exec.Cmd, error) {
	if len(er.Argv) == 0 {
		return nil, errors.New("an exec needs argv")
	}
	s.mu.Lock()
	env := append([]string{"HOME=" + s.Home, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}, s.env...)
	s.mu.Unlock()
	if er.TTY {
		env = append(env, "TERM=xterm-256color")
	}
	cmd := exec.Command(er.Argv[0], er.Argv[1:]...)
	cmd.Env = append(env, er.Env...)
	if st, err := os.Stat(filepath.Join(s.Home, "work")); err == nil && st.IsDir() {
		cmd.Dir = filepath.Join(s.Home, "work")
	} else if s.Home != "" {
		cmd.Dir = s.Home
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Credential: s.Cred}
	return cmd, nil
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	typ, payload, err := ReadFrame(r.Body)
	if err != nil || typ != FrameSpec {
		http.Error(w, "the first frame must be the exec spec", http.StatusBadRequest)
		return
	}
	var er ExecRequest
	if err := json.Unmarshal(payload, &er); err != nil {
		http.Error(w, "bad exec spec: "+err.Error(), http.StatusBadRequest)
		return
	}
	cmd, err := s.command(er)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.EnableFullDuplex()
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()
	out := &frameWriter{w: w, rc: rc}
	if er.Detach {
		id, err := s.startDetached(cmd)
		if err != nil {
			_ = out.frame(FrameError, []byte(err.Error()))
			return
		}
		_ = out.frame(FrameStarted, []byte(id))
		return
	}
	code, err := s.runAttached(r, cmd, er, out)
	if err != nil {
		_ = out.frame(FrameError, []byte(err.Error()))
		return
	}
	_ = out.frame(FrameExit, int32Payload(code))
}

type frameWriter struct {
	mu sync.Mutex
	w  io.Writer
	rc *http.ResponseController
}

func (f *frameWriter) frame(typ byte, p []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := WriteFrame(f.w, typ, p); err != nil {
		return err
	}
	return f.rc.Flush()
}

// stream copies r to the session as frames of typ.
func (f *frameWriter) stream(typ byte, r io.Reader) {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 && f.frame(typ, buf[:n]) != nil {
			return
		}
		if err != nil {
			return
		}
	}
}

// runAttached runs cmd with the session's stdin and streams its output; a cancelled session (the
// runner closed it, or the connection went) kills the process group.
func (s *Server) runAttached(r *http.Request, cmd *exec.Cmd, er ExecRequest, out *frameWriter) (int, error) {
	var ptmx *os.File
	var stdin io.WriteCloser
	var copies sync.WaitGroup
	if er.TTY {
		master, slave, err := openPTY(er.Cols, er.Rows)
		if err != nil {
			return 0, err
		}
		ptmx = master
		defer master.Close()
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		cmd.SysProcAttr.Setctty = true
		err = cmd.Start()
		slave.Close()
		if err != nil {
			return 0, err
		}
		stdin = master
		copies.Add(1)
		go func() { defer copies.Done(); out.stream(FrameStdout, master) }()
	} else {
		in, err := cmd.StdinPipe()
		if err != nil {
			return 0, err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return 0, err
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			return 0, err
		}
		if err := cmd.Start(); err != nil {
			return 0, err
		}
		stdin = in
		copies.Add(2)
		go func() { defer copies.Done(); out.stream(FrameStdout, stdout) }()
		go func() { defer copies.Done(); out.stream(FrameStderr, stderr) }()
	}
	pgid := cmd.Process.Pid
	exited := make(chan struct{})
	go func() {
		select {
		case <-r.Context().Done():
			// Only while the process is ours: after Wait its group id could name someone else's.
			_ = unix.Kill(-pgid, unix.SIGKILL)
		case <-exited:
		}
	}()
	go func() {
		for {
			typ, p, err := ReadFrame(r.Body)
			if err != nil {
				if ptmx == nil {
					_ = stdin.Close()
				}
				return
			}
			switch typ {
			case FrameStdin:
				_, _ = stdin.Write(p)
			case FrameStdinClose:
				if ptmx != nil {
					_, _ = ptmx.Write([]byte{4}) // ^D: a terminal has no half-close
				} else {
					_ = stdin.Close()
				}
			case FrameResize:
				if cols, rows, err := readUint16Pair(p); err == nil && ptmx != nil {
					_ = setWinsize(ptmx, cols, rows)
				}
			}
		}
	}()
	if ptmx == nil {
		// Pipe readers must reach EOF before Wait closes them.
		copies.Wait()
	}
	err := cmd.Wait()
	close(exited)
	if ptmx != nil {
		// The master reads EIO once the last slave holder is gone; finish draining first.
		copies.Wait()
	}
	return exitCode(cmd, err)
}

func exitCode(cmd *exec.Cmd, err error) (int, error) {
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return 0, err
	}
	ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if ok && ws.Signaled() {
		return 128 + int(ws.Signal()), nil
	}
	return cmd.ProcessState.ExitCode(), nil
}

// startDetached starts cmd with its output (stdout and stderr together) in a file of OutDir.
func (s *Server) startDetached(cmd *exec.Cmd) (string, error) {
	s.mu.Lock()
	s.counter++
	id := strconv.Itoa(s.counter)
	s.mu.Unlock()
	path := filepath.Join(s.OutDir, "exec-"+id+".log")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		f.Close()
		return "", err
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		f.Close()
		pr.Close()
		pw.Close()
		return "", err
	}
	pw.Close()
	d := &detached{id: id, path: path, changed: make(chan struct{})}
	s.mu.Lock()
	if s.execs == nil {
		s.execs = map[string]*detached{}
	}
	s.execs[id] = d
	s.latest = id
	s.mu.Unlock()
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, rerr := pr.Read(buf)
			if n > 0 {
				if _, err := f.Write(buf[:n]); err != nil {
					slog.Error("guest agent: detached output write", "id", id, "err", err)
				}
				d.notify()
			}
			if rerr != nil {
				break
			}
		}
		pr.Close()
		f.Close()
		code, err := exitCode(cmd, cmd.Wait())
		if err != nil {
			code = -1
		}
		d.mu.Lock()
		d.done, d.code = true, code
		d.mu.Unlock()
		d.notify()
	}()
	return id, nil
}

// handleOutput streams a detached exec's output from offset until it has exited and all of it is sent.
func (s *Server) handleOutput(w http.ResponseWriter, r *http.Request) {
	d := s.lookup(r.PathValue("id"))
	if d == nil {
		http.Error(w, "no such exec", http.StatusNotFound)
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || offset < 0 {
		http.Error(w, "offset must be a byte count", http.StatusBadRequest)
		return
	}
	f, err := os.Open(d.path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rc := http.NewResponseController(w)
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()
	buf := make([]byte, 32<<10)
	for {
		d.mu.Lock()
		changed, done := d.changed, d.done
		d.mu.Unlock()
		for {
			n, err := f.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
				_ = rc.Flush()
			}
			if err != nil {
				break
			}
		}
		if done {
			return
		}
		select {
		case <-changed:
		case <-r.Context().Done():
			return
		}
	}
}

func openPTY(cols, rows uint16) (*os.File, *os.File, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	n, err := unix.IoctlGetUint32(int(master.Fd()), unix.TIOCGPTN)
	if err == nil {
		err = unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0)
	}
	if err == nil && cols > 0 && rows > 0 {
		err = setWinsize(master, cols, rows)
	}
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	slave, err := os.OpenFile("/dev/pts/"+strconv.FormatUint(uint64(n), 10), os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	return master, slave, nil
}

func setWinsize(f *os.File, cols, rows uint16) error {
	return unix.IoctlSetWinsize(int(f.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Col: cols, Row: rows})
}

// MountVirtiofs is Server.Mount in a guest: the share's tag at its target, nosuid and nodev, read-only
// when asked.
func MountVirtiofs(m Mount) error {
	if !strings.HasPrefix(m.Tag, "wardyn") || !filepath.IsAbs(m.Target) {
		return fmt.Errorf("refusing share %q at %q", m.Tag, m.Target)
	}
	if err := os.MkdirAll(m.Target, 0o755); err != nil {
		return err
	}
	flags := uintptr(unix.MS_NOSUID | unix.MS_NODEV)
	if m.ReadOnly {
		flags |= unix.MS_RDONLY
	}
	return unix.Mount(m.Tag, m.Target, "virtiofs", flags, "")
}
