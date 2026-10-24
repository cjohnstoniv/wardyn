// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
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
	"unsafe"

	"golang.org/x/sys/unix"
)

// Server is the macOS guest agent's HTTP handler: the Linux server's requests (server_linux.go) on
// macOS. It differs where macOS does: no virtiofs shares (folder mounts are refused), no sign-in
// tmpfs (the guest keeps its keychain and browser profiles on the run's disk, so Init answers
// SignInsRunDisk and refuses a read-only store it cannot make), the proxy CA reaches the tools through
// their environment, and the desktop is the guest's Screen Sharing (Desktop). The zero value is not
// usable; set every field except Desktop.
type Server struct {
	Version string
	// Cred is who execs run as; nil runs them as the agent's own user (tests only: in a guest the
	// command sets the agent user).
	Cred *syscall.Credential
	// Home is the exec user's home, its HOME and its working directory when it exists.
	Home string
	// User is the exec user's account name, for USER and LOGNAME.
	User string
	// OutDir holds detached execs' output files. macOS clears /var/run at boot.
	OutDir string
	// MarkerDir holds the marker of this boot's Init and the order of its pauses (PauseFence)
	// (/var/run: both outlive an agent restart and a reboot clears them).
	MarkerDir string
	// Desktop is the guest's Screen Sharing; nil serves no desktop requests (tests only).
	Desktop *Desktop

	// signal signals a process group; nil is kill(2) (tests hold or fail it).
	signal func(pgid int, sig unix.Signal) error

	mu      sync.Mutex
	fence   PauseFence // the order of pauses and resumes, as the marker directory keeps it
	inited  bool
	env     []string // Init's Env and SecretEnv, KEY=VALUE
	execs   map[string]*detached
	latest  string
	counter int
}

type detached struct {
	id   string
	pid  int // its own session and process group (Setsid), which pause and resume signal
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

// Handler serves the requests the package doc lists. It refuses a Server with no MarkerDir, whose
// marker would land in the working directory, where a reboot does not clear it.
func (s *Server) Handler() (http.Handler, error) {
	if s.MarkerDir == "" {
		return nil, errors.New("the guest agent has no marker directory")
	}
	fence, err := readFence(s.MarkerDir)
	if err != nil {
		return nil, fmt.Errorf("the guest agent's pause order: %w", err)
	}
	s.mu.Lock()
	s.fence = fence
	s.mu.Unlock()
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
	if s.Desktop != nil {
		s.Desktop.Register(mux)
	}
	mux.HandleFunc("POST /v1/agent/pause", func(w http.ResponseWriter, r *http.Request) { s.signalAgent(w, r, unix.SIGSTOP) })
	mux.HandleFunc("POST /v1/agent/resume", func(w http.ResponseWriter, r *http.Request) { s.signalAgent(w, r, unix.SIGCONT) })
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
	return mux, nil
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
	// Refused before the marker, so a corrected Init can follow.
	if len(in.Mounts) > 0 {
		http.Error(w, "a macOS guest does not mount folder shares", http.StatusUnprocessableEntity)
		return
	}
	// 0.10.0 cannot make a macOS keychain read-only, so a run that wants no sign-ins kept gets no
	// desktop: no password is set, and the tunnel refuses. It still runs agent-only. This is
	// first, so an Init that fails while writing leaves the desktop off.
	if in.SignInsOff && s.Desktop != nil {
		s.Desktop.Disable()
	}
	marker, err := os.OpenFile(filepath.Join(s.MarkerDir, "wardyn-guest-init"), os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		http.Error(w, "the sandbox is already initialised", http.StatusConflict)
		return
	}
	_ = marker.Close()
	res := InitResult{SignIns: SignInsRunDisk}
	if s.Desktop != nil {
		res.DesktopPasswordSet = new(bool)
	}
	for _, f := range in.Files {
		if err := s.writeFile(f); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	ca := caEnvFor(in.Files)
	if ca != nil {
		if err := s.buildCA(in.Files); err != nil {
			http.Error(w, "the CA bundle: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	s.env = initEnv(in.Env, in.SecretEnv, ca)
	if s.Desktop != nil {
		*res.DesktopPasswordSet = s.initDesktop(r.Context(), ca, in.DesktopPassword, !in.SignInsOff)
	}
	s.inited = true
	writeJSON(w, res)
}

// buildCA writes MacCAFullPath from the system roots and the CA Init has just written.
func (s *Server) buildCA(files []File) error {
	for _, f := range files {
		if f.Path == MacCABundlePath {
			return writeCAFull(MacSystemRoots, f.Content, MacCAFullPath)
		}
	}
	return nil
}

// initDesktop sets the run's VNC password (pw, from Init) and the CA variables in the desktop session,
// and says whether the password is set. Neither fails the Init: a run without them is agent-only or
// sees certificate errors in GUI apps, and InitResult.DesktopPasswordSet says which. The log lines name
// the step and never the password.
func (s *Server) initDesktop(ctx context.Context, ca []string, pw string, desktop bool) bool {
	set := false
	if desktop {
		if err := s.Desktop.SetRunPassword(ctx, pw); err != nil {
			slog.Warn("guest agent: no desktop password for this run", "err", err)
		} else {
			set = true
		}
	}
	if err := s.Desktop.SetSessionEnv(ctx, ca); err != nil {
		slog.Warn("guest agent: CA variables not set in the desktop session", "err", err)
	}
	return set
}

// signalAgent sends sig to the latest detached exec's process group: the harness and every process
// it started that stayed in its group. A process that left the group (setsid) is not reached. A request
// the fence drops (PauseFence) is answered 409 and signals nothing, as on Linux.
func (s *Server) signalAgent(w http.ResponseWriter, r *http.Request, sig unix.Signal) {
	var in AgentSignal
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if code, msg := s.signalLatest(in.Generation, sig); code != http.StatusOK {
		http.Error(w, msg, code)
		return
	}
	writeJSON(w, AgentSignalResult{FencedPause: in.Generation > 0})
}

// signalLatest holds one lock from the choice of the exec through the fence's check, its write and the
// signal, so no other pause or resume, and no new exec, comes between them. The fence is written before
// the signal: once a generation is let through, an older request stays dropped even when its signal
// fails, and the same request sent again acts.
func (s *Server) signalLatest(gen int64, sig unix.Signal) (int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.execs[s.latest]
	if d == nil || !d.state().Running {
		return http.StatusNotFound, "no agent is running"
	}
	next := s.fence
	if !next.Admit(gen, sig == unix.SIGCONT) {
		if gen == 0 {
			return http.StatusConflict, ReasonStaleUnfencedResume + ": a resume with no generation came after one with a generation; the agent stays paused"
		}
		return http.StatusConflict, "a newer pause or resume has reached the agent"
	}
	if gen == 0 && next.Generation > 0 {
		slog.Warn("guest agent: obeyed a pause with no generation after one with a generation", "generation", next.Generation)
	}
	if next != s.fence {
		if err := writeFence(s.MarkerDir, next); err != nil {
			return http.StatusInternalServerError, "keep the order of pauses: " + err.Error()
		}
		s.fence = next
	}
	signal := s.signal
	if signal == nil {
		signal = func(pgid int, sig unix.Signal) error { return unix.Kill(-pgid, sig) }
	}
	if err := signal(d.pid, sig); err != nil {
		return http.StatusInternalServerError, "signal the agent: " + err.Error()
	}
	return http.StatusOK, ""
}

// fenceFile, in the marker directory, keeps the PauseFence across a restart of the agent's service.
const fenceFile = "wardyn-pause-fence"

// readFence reads the fence the marker directory keeps; none is the zero fence of a fresh boot.
func readFence(dir string) (PauseFence, error) {
	var f PauseFence
	fh, err := os.OpenFile(filepath.Join(dir, fenceFile), os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	defer fh.Close()
	return f, json.NewDecoder(io.LimitReader(fh, 1<<10)).Decode(&f)
}

// writeFence replaces the kept fence with f, whole or not at all.
func writeFence(dir string, f PauseFence) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, fenceFile)
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	fh, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, err = fh.Write(b)
	if cerr := fh.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
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
	env := append([]string{"HOME=" + s.Home, "USER=" + s.User, "LOGNAME=" + s.User, "PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin", "LANG=en_US.UTF-8"}, s.env...)
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
	d := &detached{id: id, pid: cmd.Process.Pid, path: path, changed: make(chan struct{})}
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

// openPTY opens a pseudo-terminal pair the darwin way: /dev/ptmx, then grant, unlock and name the slave.
func openPTY(cols, rows uint16) (*os.File, *os.File, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	fd := master.Fd()
	if err = ptyIoctl(fd, unix.TIOCPTYGRANT, 0); err == nil {
		err = ptyIoctl(fd, unix.TIOCPTYUNLK, 0)
	}
	var name [128]byte
	if err == nil {
		err = ptyIoctl(fd, unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0])))
	}
	if err == nil && cols > 0 && rows > 0 {
		err = setWinsize(master, cols, rows)
	}
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	slave, err := os.OpenFile(unix.ByteSliceToString(name[:]), os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	return master, slave, nil
}

func ptyIoctl(fd, req, arg uintptr) error {
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, fd, req, arg); e != 0 {
		return e
	}
	return nil
}

func setWinsize(f *os.File, cols, rows uint16) error {
	return unix.IoctlSetWinsize(int(f.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Col: cols, Row: rows})
}
