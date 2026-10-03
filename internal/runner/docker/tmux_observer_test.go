// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// REAL-TMUX PROOF of the observer rule (term-t7): the real attach command, run
// under a PTY of a chosen size, against a real tmux server. A tmux that sizes
// its window to the LATEST client lets any new client resize the shared window;
// an observer attached with ignore-size must not. WARDYN_REQUIRE_TMUX=1 turns a
// missing tmux into a failure instead of a skip.

type tmuxRig struct {
	t       *testing.T
	scratch string
	sock    string
}

var tmuxVersionRE = regexp.MustCompile(`^tmux ([0-9]+)\.([0-9]+)`)

func newTmuxRig(t *testing.T) *tmuxRig {
	t.Helper()
	out, err := exec.Command("tmux", "-V").Output()
	if err != nil {
		if os.Getenv("WARDYN_REQUIRE_TMUX") == "1" {
			t.Fatalf("tmux not available and WARDYN_REQUIRE_TMUX=1: %v", err)
		}
		t.Skip("tmux not installed")
	}
	m := tmuxVersionRE.FindStringSubmatch(string(out))
	if m == nil {
		t.Skipf("unrecognised tmux version %q", out)
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major < 3 || (major == 3 && minor < 2) {
		t.Skipf("ignore-size needs tmux >= 3.2, have %s", strings.TrimSpace(string(out)))
	}
	if _, err := os.Stat("/etc/tmux.conf"); err == nil {
		t.Skip("host /etc/tmux.conf would be read by the test server")
	}
	scratch := t.TempDir()
	r := &tmuxRig{t: t, scratch: scratch, sock: filepath.Join(scratch, fmt.Sprintf("tmux-%d", os.Getuid()), "default")}
	t.Cleanup(func() { _, _ = r.tmux("kill-server") })
	return r
}

func (r *tmuxRig) tmux(args ...string) (string, error) {
	out, err := exec.Command("tmux", append([]string{"-S", r.sock}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (r *tmuxRig) window() string {
	out, _ := r.tmux("display-message", "-p", "-t", "wardyn", "#{window_width}x#{window_height}")
	return out
}

// waitWindow polls until the shared window is want.
func (r *tmuxRig) waitWindow(want string) {
	r.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if r.window() == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	r.t.Fatalf("window = %q, want %q", r.window(), want)
}

// holdsWindow fails if the window leaves want within d: an observer's effect
// (if any) lands within a frame or two of its attach.
func (r *tmuxRig) holdsWindow(want string, d time.Duration) {
	r.t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if got := r.window(); got != want {
			r.t.Fatalf("window = %q, want it to stay %q", got, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// client is one attach command under its own PTY.
type tmuxClient struct {
	ptmx *os.File
	cmd  *exec.Cmd
	mu   sync.Mutex
	out  strings.Builder
}

func (r *tmuxRig) attach(observer bool, cols, rows uint16) *tmuxClient {
	r.t.Helper()
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		r.t.Fatal(err)
	}
	var n uint32
	var unlock int32
	if err := ioctl(ptmx, syscall.TIOCGPTN, unsafe.Pointer(&n)); err != nil {
		r.t.Fatal(err)
	}
	if err := ioctl(ptmx, syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err != nil {
		r.t.Fatal(err)
	}
	pts, err := os.OpenFile("/dev/pts/"+strconv.Itoa(int(n)), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		r.t.Fatal(err)
	}
	defer pts.Close()
	if err := setWinsize(ptmx, cols, rows); err != nil {
		r.t.Fatal(err)
	}
	cmd := exec.Command(attachShellFor(observer)[0], attachShellFor(observer)[1:]...)
	cmd.Env = append(os.Environ(), "HOME="+r.scratch, "TMUX_TMPDIR="+r.scratch, "TERM=xterm-256color", "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "XDG_CONFIG_HOME="+r.scratch)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = pts, pts, pts
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		r.t.Fatal(err)
	}
	c := &tmuxClient{ptmx: ptmx, cmd: cmd}
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := ptmx.Read(buf)
			c.mu.Lock()
			c.out.Write(buf[:n])
			c.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	go func() { _ = cmd.Wait() }()
	r.t.Cleanup(c.close)
	return c
}

func (c *tmuxClient) close() {
	_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGHUP)
	_ = c.ptmx.Close()
}

func (c *tmuxClient) resize(cols, rows uint16) error { return setWinsize(c.ptmx, cols, rows) }

func (c *tmuxClient) write(s string) {
	_, _ = c.ptmx.WriteString(s)
}

func (c *tmuxClient) output() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out.String()
}

func ioctl(f *os.File, req uintptr, arg unsafe.Pointer) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), req, uintptr(arg)); e != 0 {
		return e
	}
	return nil
}

func setWinsize(f *os.File, cols, rows uint16) error {
	ws := struct{ Row, Col, X, Y uint16 }{Row: rows, Col: cols}
	if err := ioctl(f, syscall.TIOCSWINSZ, unsafe.Pointer(&ws)); err != nil {
		return errors.New("TIOCSWINSZ: " + err.Error())
	}
	return nil
}

// clients is `tmux list-clients` as "WxH flags" lines.
func (r *tmuxRig) clients() []string {
	out, _ := r.tmux("list-clients", "-F", "#{client_width}x#{client_height} #{client_flags}")
	return strings.Split(out, "\n")
}

// A writer at 120x40 resizes to 140x50; a delayed observer then attaches from a
// small 80x24 terminal. With ignore-size the window stays 140x50. The control
// below shows the same attach WITHOUT the flag does clamp it, so the assertion
// has teeth on this tmux.
func TestAttach_ObserverDoesNotClampTheWriterWindow(t *testing.T) {
	r := newTmuxRig(t)
	writer := r.attach(false, 120, 40)
	r.waitWindow("120x40")
	if err := writer.resize(140, 50); err != nil {
		t.Fatal(err)
	}
	r.waitWindow("140x50")

	observer := r.attach(true, 80, 24)
	_ = observer
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(strings.Join(r.clients(), "\n"), "ignore-size") {
		if time.Now().After(deadline) {
			t.Fatalf("the observer never attached with ignore-size: %v", r.clients())
		}
		time.Sleep(50 * time.Millisecond)
	}
	r.holdsWindow("140x50", 1500*time.Millisecond)

	// The observer's own terminal changing size does not move the window either.
	if err := observer.resize(60, 20); err != nil {
		t.Fatal(err)
	}
	r.holdsWindow("140x50", 1000*time.Millisecond)

	t.Run("control: a client without ignore-size clamps the window", func(t *testing.T) {
		r := newTmuxRig(t)
		r.attach(false, 120, 40)
		r.waitWindow("120x40")
		r.attach(false, 80, 24)
		r.waitWindow("80x24")
	})
}

// An 80x24 observer of a 120x40 writer is attached at the writer's 120x40 and
// sees a marker the writer printed at column 100; when the writer resizes to
// 140x50 and the observer's PTY follows, the observer is 140x50. An observer
// left at 80 columns never sees the marker (control).
func TestAttach_ObserverAtTheWritersSizeSeesColumn100AndFollowsResizes(t *testing.T) {
	r := newTmuxRig(t)
	writer := r.attach(false, 120, 40)
	r.waitWindow("120x40")
	follower := r.attach(true, 120, 40) // seeded from the writer's live size
	narrow := r.attach(true, 80, 24)    // left at its own size
	deadline := time.Now().Add(15 * time.Second)
	for strings.Count(strings.Join(r.clients(), "\n"), "ignore-size") < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the observers never attached with ignore-size: %v", r.clients())
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The marker is assembled by the shell so its literal never appears on the
	// command line the clients also draw. Column 101 (1-based) is column 100.
	writer.write(`printf '\033[3;101H%s%s' MKR 100` + "\r")
	// markerColumn is the 1-based column a client was told to draw the marker at.
	markerColumn := func(c *tmuxClient) int {
		m := regexp.MustCompile(`\x1b\[3;([0-9]+)H(?:\x1b\[[0-9;?]*[A-Za-z])*MKR100`).FindStringSubmatch(c.output())
		if m == nil {
			return 0
		}
		n, _ := strconv.Atoi(m[1])
		return n
	}
	deadline = time.Now().Add(15 * time.Second)
	for markerColumn(follower) == 0 || markerColumn(narrow) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the marker was not drawn (observer at 120 columns: %d, at 80: %d)", markerColumn(follower), markerColumn(narrow))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := markerColumn(follower); got != 101 {
		t.Errorf("the observer at the writer's 120x40 drew the marker at column %d, want 101 (column 100)", got)
	}
	// Control: an observer left at 80 columns is shown a horizontally scrolled
	// view, so its grid does not match the writer's.
	if got := markerColumn(narrow); got == 101 {
		t.Error("an observer left at 80 columns drew the marker at the writer's column: the control has no teeth")
	}

	if err := writer.resize(140, 50); err != nil {
		t.Fatal(err)
	}
	r.waitWindow("140x50")
	if err := follower.resize(140, 50); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(15 * time.Second)
	for {
		var sized bool
		for _, l := range r.clients() {
			sized = sized || (strings.HasPrefix(l, "140x50 ") && strings.Contains(l, "ignore-size"))
		}
		if sized {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the observer's client never reached 140x50: %v", r.clients())
		}
		time.Sleep(50 * time.Millisecond)
	}
	r.holdsWindow("140x50", 500*time.Millisecond)
}

// A promotion closes the observer's exec and opens a writer's at the newcomer's
// size: the window follows the promoted client, and the observers left behind
// still do not move it.
func TestAttach_PromotedWriterReplacedMidAttachSizesTheWindow(t *testing.T) {
	r := newTmuxRig(t)
	writer := r.attach(false, 120, 40)
	r.waitWindow("120x40")
	r.attach(true, 90, 25)
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(strings.Join(r.clients(), "\n"), "ignore-size") {
		if time.Now().After(deadline) {
			t.Fatalf("the observer never attached with ignore-size: %v", r.clients())
		}
		time.Sleep(50 * time.Millisecond)
	}

	writer.close() // the writer leaves; the observer is promoted
	r.attach(false, 100, 30)
	r.waitWindow("100x30")
	r.holdsWindow("100x30", 1000*time.Millisecond)
}
